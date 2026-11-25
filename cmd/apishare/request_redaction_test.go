package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"apishare/internal/requestredact"
	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

func enableTestRedaction(t *testing.T, mode, coverage string) {
	t.Helper()
	policy := defaultRequestRedactionPolicy()
	policy.Mode = mode
	policy.Coverage = coverage
	if _, err := saveRequestRedactionPolicy(policy, 0); err != nil {
		t.Fatal(err)
	}
}

func TestRequestRedactionPersonalIsolationAndRevision(t *testing.T) {
	modelsTestDB(t)
	key := &ApiKey{Name: "personal", Key: "personal-redaction", Models: "alias", Enabled: true}
	if err := dbInsertKey(key); err != nil {
		t.Fatal(err)
	}
	agentPurgeTestUser(t, key, "redaction-user")
	bound, err := dbGetKeyByID(key.ID)
	if err != nil {
		t.Fatal(err)
	}
	enableTestRedaction(t, "enforce", "strict")
	snapshot, err := resolveRequestRedaction(*bound, "alias")
	if err != nil || snapshot.Source != "user" || snapshot.Config.Mode != "off" {
		t.Fatalf("personal off inherited global: %+v %v", snapshot, err)
	}
	if _, err = saveUserRequestRedactionConfig("redaction-user", true, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = saveUserRequestRedactionConfig("redaction-user", false, 0); err != errRequestRedactionRevisionConflict {
		t.Fatal("stale revision accepted")
	}
	if err = settingsSet(requestRedactionPolicyKey, "corrupt"); err != nil {
		t.Fatal(err)
	}
	snapshot, err = resolveRequestRedaction(*bound, "alias")
	if err != nil || snapshot.Config.Mode != "enforce" || snapshot.Config.Coverage != "text" {
		t.Fatalf("personal policy depends on global: %+v %v", snapshot, err)
	}
	if _, err = resolveRequestRedaction(ApiKey{ID: 999}, "alias"); err == nil {
		t.Fatal("corrupt global silently disabled")
	}
}

func TestRequestRedactionUserRoutes(t *testing.T) {
	modelsTestDB(t)
	if err := settingsSet("session_secret", "redaction-test-secret"); err != nil {
		t.Fatal(err)
	}
	key := &ApiKey{Name: "personal", Key: "personal-redaction", Models: "alias", Enabled: true}
	if err := dbInsertKey(key); err != nil {
		t.Fatal(err)
	}
	session := agentPurgeTestUser(t, key, "redaction-route")
	router := newRouter()
	request := func(method, path, body string, auth, csrf bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/user/"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if auth {
			req.AddCookie(&http.Cookie{Name: xauthUserSessionCookie, Value: session})
		}
		if csrf {
			req.Header.Set(xauthUserRequestHeader, "user-console")
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	if w := request("GET", "request-processing", "", false, false); w.Code != 401 {
		t.Fatalf("unauth %d", w.Code)
	}
	for _, tc := range []struct {
		body string
		csrf bool
		want int
	}{
		{`{"enabled":true,"revision":0}`, false, 403},
		{`{"enabled":true,"revision":0,"sub":"other"}`, true, 400},
		{`{"enabled":true,"revision":0}`, true, 200},
		{`{"enabled":false,"revision":0}`, true, 409},
	} {
		if w := request("PUT", "request-processing/redaction", tc.body, true, tc.csrf); w.Code != tc.want {
			t.Fatalf("save %d want %d %s", w.Code, tc.want, w.Body.String())
		}
	}
	w := request("POST", "request-processing/redaction/dry-run", `{"format":"openai-chat","body":{"messages":[{"content":"jane@example.com"}]}}`, true, true)
	if w.Code != 200 || strings.Contains(w.Body.String(), "jane@example.com") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("dry-run %d %s", w.Code, w.Body.String())
	}
}

func TestRequestRedactionResponsesWS(t *testing.T) {
	key, _, gateway := responsesWSTestGateway(t)
	agentPurgeTestUser(t, key, "redaction-ws")
	if _, err := saveUserRequestRedactionConfig("redaction-ws", true, 0); err != nil {
		t.Fatal(err)
	}
	received := make(chan string, 2)
	fixture := responsesWSFixture(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		for i := 0; i < 2; i++ {
			_, body, err := conn.Read(ctx)
			if err != nil {
				return
			}
			received <- string(body)
			responsesWSWrite(t, ctx, conn, responsesWSTerminal(fmt.Sprintf("redacted-%d", i), "", 1, 1))
		}
		_, _, _ = conn.Read(ctx)
	})
	responsesWSTestUpstream(t, fixture.URL, "openai", 0)
	ctx, conn := responsesWSDial(t, gateway, key, nil)
	for i, typ := range []string{"response.create", "response.append"} {
		responsesWSWrite(t, ctx, conn, fmt.Sprintf(`{"type":%q,"model":"ws-alias","input":"jane@example.com"}`, typ))
		responsesWSRead(t, ctx, conn)
		if body := <-received; strings.Contains(body, "jane@example.com") || !strings.Contains(body, "[EMAIL]") {
			t.Fatalf("WS bypass turn %d: %s", i, body)
		}
		if i == 0 {
			responsesWSWrite(t, ctx, conn, `{"type":"response.create","input":[{"type":"function_call","arguments":"{\"email\":\"jane@example.com\"}"}]}`)
			if event := responsesWSRead(t, ctx, conn); !strings.Contains(string(event), "request_redaction_rejected") {
				t.Fatalf("expected local rejection: %s", event)
			}
		}
	}
	if _, err := saveUserRequestRedactionConfig("redaction-ws", false, 1); err != nil {
		t.Fatal(err)
	}
	responsesWSWrite(t, ctx, conn, `{"type":"response.create","input":"after settings change"}`)
	if event := responsesWSRead(t, ctx, conn); !strings.Contains(string(event), "reconnect") {
		t.Fatalf("policy changed on old handshake: %s", event)
	}
}

func TestRequestRedactionSearchEgress(t *testing.T) {
	for _, provider := range []string{"brave", "tavily", "exa"} {
		t.Run(provider, func(t *testing.T) {
			modelsTestDB(t)
			enableTestRedaction(t, "enforce", "text")
			snapshot, err := resolveRequestRedaction(ApiKey{ID: 1}, "alias")
			if err != nil {
				t.Fatal(err)
			}
			previous := webSearchHTTPClient.Transport
			t.Cleanup(func() { webSearchHTTPClient.Transport = previous })
			called := false
			webSearchHTTPClient.Transport = jobRoundTripper(func(r *http.Request) (*http.Response, error) {
				called = true
				query := r.URL.Query().Get("q")
				if provider != "brave" {
					var payload map[string]any
					_ = json.NewDecoder(r.Body).Decode(&payload)
					query = str(payload["query"])
				}
				if query != "find [EMAIL]" {
					t.Errorf("unredacted search query: %q", query)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"type":"search","web":{"results":[]},"results":[]}`))}, nil
			})
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			task := &relayTask{model: "alias", requestRedaction: snapshot}
			info, _, err := relayResponsesSearchFallback(c, task, webSearchSettings{Provider: provider, APIKey: "provider-key"}, &responsesSearchRequest{query: "find jane@example.com"})
			if err != nil || !called || info.requestRedaction == nil || strings.Contains(recorder.Body.String(), "jane@example.com") {
				t.Fatalf("search redaction failed: %v %s", err, recorder.Body.String())
			}
		})
	}
}

func TestRequestRedactionHeadersAndObserveOverlay(t *testing.T) {
	cfg := requestredact.PersonalConfig()
	snapshot, err := compileRedactionSnapshot("global", 1, "builtin-v1", cfg, requestRedactionHeaders{true, true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	incoming := http.Header{"Authorization": {"Bearer short-client-secret"}}
	for _, raw := range []string{`{"Authorization":true}`, `{"Authorization":"{client_header:Authorization}"}`, `{"X-Secret":"{client_header:Authorization}"}`} {
		if err := checkRedactionHeaders(snapshot, &Upstream{HeaderOverrides: raw}, incoming, http.Header{}); err == nil {
			t.Fatalf("credential copy accepted: %s", raw)
		}
	}
	headers := http.Header{"Authorization": {"Bearer provider-key"}, "Cookie": {"session=secret"}, "X-Forwarded-For": {"1.2.3.4"}}
	if err := checkRedactionHeaders(snapshot, &Upstream{}, incoming, headers); err != nil || headers.Get("Cookie") != "" || headers.Get("Authorization") != "Bearer provider-key" {
		t.Fatalf("header policy %v %+v", err, headers)
	}
	cfg.Mode = requestredact.ModeObserve
	snapshot, err = compileRedactionSnapshot("global", 1, "builtin-v1", cfg, requestRedactionHeaders{}, []requestRedactionChannelOverlay{{UpstreamID: 1, Rules: []requestredact.Rule{{ID: "employee", Pattern: "EMP-123", Replacement: "[EMP]"}}}})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"input":"EMP-123 jane@example.com"}`)
	result, err := snapshot.process(FormatOpenAIResponse, body, 1)
	if err != nil || result.Changed || string(result.Body) != string(body) {
		t.Fatalf("observe overlay enforced redaction: %s %v", result.Body, err)
	}
}
