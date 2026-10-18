package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestOpenCodeRetryAfterCooldown(t *testing.T) {
	modelsTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, value := range []string{"120", now.Add(2 * time.Minute).Format(http.TimeFormat)} {
		if got := openCodeRetryAfter(value, now); !got.Equal(now.Add(2 * time.Minute)) {
			t.Errorf("Retry-After %s: %s", value, got)
		}
	}
	for _, value := range []string{"", "-1", "garbage", "9999999999999999999999999", now.Add(-time.Hour).Format(http.TimeFormat)} {
		if got := openCodeRetryAfter(value, now); !got.IsZero() {
			t.Errorf("invalid Retry-After %s: %s", value, got)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":"rate limited"}`)
	}))
	defer server.Close()
	up := &Upstream{ID: 901, Type: "opencode", OpenAIProtocol: "chat", BaseURL: server.URL, APIKey: "key"}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	if info, wrote, err := doRelay(ctx, up, fixtureTask(t, FormatOpenAIChat, false)); err == nil || wrote || info.Status != 429 {
		t.Fatalf("rate limit did not fail over: %+v %v %v", info, wrote, err)
	}
	if healthAvailable(up.ID) {
		t.Fatal("rate-limited channel ignored Retry-After")
	}
	healthMarkFailure(up.ID)
	healthMarkFailure(up.ID)
	upHealth.Lock()
	until := upHealth.until[up.ID]
	upHealth.Unlock()
	if until.Before(time.Now().Add(115 * time.Second)) {
		t.Fatalf("normal backoff shortened Retry-After: %s", until)
	}
}

func TestOpenCodeStaleResponsesRepair(t *testing.T) {
	modelsTestDB(t)
	for _, tc := range []struct {
		name, message string
		retry         bool
	}{
		{"expired", "Referenced reasoning item rs_abc was not found or has expired", true},
		{"ordinary", "unknown reasoning field", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, session := 0, ""
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body, _ := io.ReadAll(r.Body)
				var payload map[string]json.RawMessage
				json.Unmarshal(body, &payload)
				if calls == 1 {
					session = r.Header.Get("x-opencode-session")
					w.WriteHeader(400)
					fmt.Fprint(w, tc.message)
					return
				}
				if calls > 2 {
					t.Error("stale reference repair replayed more than once")
				}
				if r.Header.Get("x-opencode-session") != session {
					t.Error("repair changed session identity")
				}
				if payload["previous_response_id"] != nil || strings.Contains(string(payload["input"]), `"type":"reasoning"`) {
					t.Errorf("stale references survived: %s", body)
				}
				if !strings.Contains(string(payload["input"]), "function_call_output") || payload["tools"] == nil || payload["metadata"] == nil {
					t.Errorf("repair removed client data or tools: %s", body)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, protocolResponses[FormatOpenAIResponse])
			}))
			defer server.Close()
			body := []byte(`{"model":"alias","previous_response_id":"resp_old","input":[{"type":"reasoning","id":"rs_old","summary":[]},{"role":"user","content":"hi"},{"type":"function_call_output","call_id":"call1","output":"ok"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"metadata":{"session_id":"original-session"}}`)
			cr, err := parseClientRequest(FormatOpenAIResponse, body, "alias")
			if err != nil {
				t.Fatal(err)
			}
			up := &Upstream{Type: "opencode", OpenAIProtocol: "responses", BaseURL: server.URL, APIKey: "key"}
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			task := &relayTask{format: FormatOpenAIResponse, endpoint: "/v1/responses", model: "alias", body: body, canon: cr}
			info, wrote, err := doRelay(ctx, up, task)
			if err != nil || !wrote {
				t.Fatalf("repair relay: %+v %v %v", info, wrote, err)
			}
			if tc.retry && (calls != 2 || info.Status != 200) || !tc.retry && (calls != 1 || info.Status != 400) {
				t.Fatalf("unexpected replay behavior: calls=%d status=%d", calls, info.Status)
			}
		})
	}
	noReferences := []byte(`{"model":"alias","input":"hi"}`)
	if _, changed := repairOpenCodeReasoning(noReferences, []byte("reasoning item expired")); changed {
		t.Fatal("repaired a request without stale references")
	}
}
