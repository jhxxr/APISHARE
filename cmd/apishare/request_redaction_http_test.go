package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequestRedactionPreservesExplicitModelOptions(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(fmt.Sprintf("scoped=%v", scoped), func(t *testing.T) {
			modelsTestDB(t)
			const requestedModel = "gpt-5.2(high)"
			key := &ApiKey{Name: "model-options", Key: "model-options-key", Models: requestedModel, Enabled: true, QPS: 1000, Concurrency: 10}
			if err := dbInsertKey(key); err != nil {
				t.Fatal(err)
			}
			policy := defaultRequestRedactionPolicy()
			policy.Mode = "enforce"
			if scoped {
				policy.Scope.Models = []string{requestedModel}
			}
			if _, err := saveRequestRedactionPolicy(policy, 0); err != nil {
				t.Fatal(err)
			}
			called := false
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				encoded, _ := json.Marshal(body)
				if strings.Contains(string(encoded), "jane@example.com") || !strings.Contains(string(encoded), "[EMAIL]") {
					t.Errorf("model-scoped redaction bypass: %s", encoded)
				}
				if body["model"] != "gpt-5.2" || body["reasoning_effort"] != "high" {
					t.Errorf("explicit model options lost: %s", encoded)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			}))
			defer provider.Close()
			up := &Upstream{Name: "provider", Type: "openai", OpenAIProtocol: "chat", BaseURL: provider.URL, Models: "gpt-5.2", Enabled: true, Weight: 1}
			if err := dbInsertUpstream(up); err != nil {
				t.Fatal(err)
			}
			router := newRouter()
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.2(high)","messages":[{"role":"user","content":"jane@example.com"}]}`))
			req.Header.Set("Authorization", "Bearer "+key.Key)
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			if !called || recorder.Code != 200 {
				t.Fatalf("relay %d %s", recorder.Code, recorder.Body.String())
			}
			logs, err := dbListLogs(10, 0)
			if err != nil || len(logs) != 1 || logs[0].Model != "gpt-5.2" || logs[0].Billing == nil || logs[0].Billing.RequestRedaction == nil || logs[0].Billing.RequestRedaction.Status != "redacted" {
				t.Fatalf("normalized model or redaction receipt lost: %+v %v", logs, err)
			}
		})
	}
}

func TestRequestRedactionHTTPAndDiagnosticProtection(t *testing.T) {
	for _, format := range []clientFormat{FormatOpenAIChat, FormatOpenAIResponse, FormatAnthropic, FormatGemini} {
		t.Run(string(format), func(t *testing.T) {
			modelsTestDB(t)
			store := testFailureLogs(t, 64<<10)
			store.fullContent = true
			key := &ApiKey{Name: "redaction", Key: "short-gateway-secret", Models: "alias", Enabled: true, QPS: 1000, Concurrency: 10}
			if err := dbInsertKey(key); err != nil {
				t.Fatal(err)
			}
			enableTestRedaction(t, "enforce", "text")
			called := false
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				body, _ := io.ReadAll(r.Body)
				if strings.Contains(string(body), "jane@example.com") || !strings.Contains(string(body), "[EMAIL]") {
					t.Errorf("HTTP redaction bypass: %s", body)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":{"message":"echo jane@example.com and short-provider-secret"}}`)
			}))
			defer provider.Close()
			kind := "openai"
			protocol := "chat"
			if format == FormatOpenAIResponse {
				protocol = "responses"
			}
			if format == FormatAnthropic {
				kind = "anthropic"
			}
			if format == FormatGemini {
				kind = "gemini"
			}
			up := &Upstream{Name: "provider", Type: kind, OpenAIProtocol: protocol, BaseURL: provider.URL, APIKey: "short-provider-secret", Models: "alias", Enabled: true, Weight: 1}
			if err := dbInsertUpstream(up); err != nil {
				t.Fatal(err)
			}
			body := `{"model":"alias","messages":[{"role":"user","content":"jane@example.com"}]}`
			path := "/v1/chat/completions"
			switch format {
			case FormatOpenAIResponse:
				path = "/v1/responses"
				body = `{"model":"alias","input":"jane@example.com"}`
			case FormatAnthropic:
				path = "/v1/messages"
				body = `{"model":"alias","max_tokens":20,"messages":[{"role":"user","content":"jane@example.com"}]}`
			case FormatGemini:
				path = "/v1beta/models/alias:generateContent"
				body = `{"contents":[{"role":"user","parts":[{"text":"jane@example.com"}]}]}`
			}
			router := newRouter()
			req := httptest.NewRequest("POST", path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+key.Key)
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			if !called || recorder.Code != 400 {
				t.Fatalf("relay %d %s", recorder.Code, recorder.Body.String())
			}
			_, raw := readTestFailureLog(t, store)
			for _, secret := range []string{"jane@example.com", "short-gateway-secret", "short-provider-secret"} {
				if strings.Contains(raw, secret) {
					t.Fatalf("diagnostic leaked %q", secret)
				}
			}
			logs, err := dbListLogs(10, 0)
			if err != nil || len(logs) != 1 || logs[0].Billing == nil || logs[0].Billing.RequestRedaction == nil || logs[0].Billing.RequestRedaction.Status != "redacted" {
				t.Fatalf("receipt missing: %+v %v", logs, err)
			}
		})
	}
}

func TestRequestRedactionStrictUnsupportedEndpoints(t *testing.T) {
	modelsTestDB(t)
	key := &ApiKey{Name: "strict", Key: "strict-key", Models: "alias", Enabled: true, QPS: 1000, Concurrency: 10}
	if err := dbInsertKey(key); err != nil {
		t.Fatal(err)
	}
	enableTestRedaction(t, "enforce", "strict")
	router := newRouter()
	for _, tc := range []struct{ method, path, body, contentType string }{
		{"POST", "/v1/audio/speech", `{"model":"alias","input":"hello","voice":"alloy"}`, "application/json"},
		{"POST", "/v1/realtime/calls?model=alias", "v=0\r\n", "application/sdp"},
		{"GET", "/v1/realtime?model=alias", "", ""},
		{"POST", "/v1/embeddings", `{"model":"alias","input":"hello"}`, "application/json"},
		{"POST", "/v1/responses/compact", `{"model":"alias","input":"hello"}`, "application/json"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+key.Key)
		req.Header.Set("Content-Type", tc.contentType)
		if tc.method == "GET" {
			req.Header.Set("Upgrade", "websocket")
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		if recorder.Code != 400 {
			t.Fatalf("unsupported strict endpoint %s: %d %s", tc.path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestRequestRedactionLocalRejectionDoesNotReachUpstream(t *testing.T) {
	modelsTestDB(t)
	key := &ApiKey{Name: "local", Key: "local-key", Models: "alias", Enabled: true, QPS: 1000, Concurrency: 10}
	if err := dbInsertKey(key); err != nil {
		t.Fatal(err)
	}
	enableTestRedaction(t, "enforce", "text")
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer server.Close()
	up := &Upstream{Type: "openai", BaseURL: server.URL, Models: "alias", Enabled: true, Weight: 1}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.POST("/v1/chat/completions", keyAuth(), relayHandler(FormatOpenAIChat, false))
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"mail","arguments":"{\"to\":\"jane@example.com\"}"}}]}]}`))
	req.Header.Set("Authorization", "Bearer "+key.Key)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != 400 || called || !strings.Contains(recorder.Body.String(), "request_redaction_rejected") {
		t.Fatalf("local reject bypass: %d %s", recorder.Code, recorder.Body.String())
	}
}
