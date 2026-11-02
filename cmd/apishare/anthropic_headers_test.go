package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAnthropicThinkingDisplayBetaRelay(t *testing.T) {
	modelsTestDB(t)
	const updatesBeta = "thinking-display-updates-2026-08-18"
	for _, channel := range []struct {
		name, kind, path, defaultBeta string
	}{
		{name: "anthropic", kind: "anthropic"},
		{name: "opencode_v1", kind: "opencode", path: "/zen"},
		{name: "opencode_v2", kind: "opencode", path: "/zen/go", defaultBeta: "interleaved-thinking-2025-05-14"},
		{name: "cliproxy", kind: "cliproxy"},
	} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				name, thinking string
				incoming, want []string
			}{
				{name: "adaptive_updates", thinking: `{"type":"adaptive","display":"updates"}`, want: []string{updatesBeta}},
				{name: "enabled_updates", thinking: `{"type":"enabled","budget_tokens":1024,"display":"updates"}`, want: []string{updatesBeta}},
				{name: "preserve_betas", thinking: `{"type":"adaptive","display":"updates"}`, incoming: []string{"context-1,context-2", "context-3"}, want: []string{"context-1", "context-2", "context-3", updatesBeta}},
				{name: "existing_updates_beta", thinking: `{"type":"adaptive","display":"updates"}`, incoming: []string{"context-1, " + updatesBeta, updatesBeta + ",context-2"}, want: []string{"context-1", updatesBeta, "context-2"}},
				{name: "summarized", thinking: `{"type":"adaptive","display":"summarized"}`, incoming: []string{"context-1", "context-2"}, want: []string{"context-1", "context-2"}},
				{name: "omitted", thinking: `{"type":"adaptive","display":"omitted"}`},
				{name: "default_display", thinking: `{"type":"adaptive"}`},
				{name: "no_thinking", incoming: []string{"context-1", "context-2"}, want: []string{"context-1", "context-2"}},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", channel.name, stream, tc.name), func(t *testing.T) {
					payload := map[string]any{
						"model": "alias", "max_tokens": 4096, "stream": stream,
						"messages": []any{map[string]any{"role": "user", "content": "hi"}},
					}
					if tc.thinking != "" {
						payload["thinking"] = json.RawMessage(tc.thinking)
					}
					body, err := json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
					calls := 0
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls++
						forwarded, err := io.ReadAll(r.Body)
						if err != nil || string(forwarded) != string(body) {
							t.Errorf("native body changed: %s / %v", forwarded, err)
						}
						want := append([]string(nil), tc.want...)
						if channel.defaultBeta != "" {
							want = append([]string{channel.defaultBeta}, want...)
						}
						var got []string
						for _, value := range r.Header.Values("anthropic-beta") {
							for _, beta := range strings.Split(value, ",") {
								if beta = strings.TrimSpace(beta); beta != "" {
									got = append(got, beta)
								}
							}
						}
						if !reflect.DeepEqual(got, want) {
							t.Errorf("upstream beta flags = %v, want %v", got, want)
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusBadRequest)
							fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"invalid Claude request: thinking: invalid thinking display: updates"}}`)
							return
						}
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, fixtureSSE(FormatAnthropic))
						} else {
							w.Header().Set("Content-Type", "application/json")
							fmt.Fprint(w, protocolResponses[FormatAnthropic])
						}
					}))
					defer server.Close()
					up := &Upstream{Type: channel.kind, BaseURL: server.URL + channel.path, APIKey: "upstream-key", OpenAIProtocol: "anthropic"}
					cr, err := parseClientRequest(FormatAnthropic, body, "")
					if err != nil {
						t.Fatal(err)
					}
					task := &relayTask{format: FormatAnthropic, endpoint: "/v1/messages", model: "alias", body: body, canon: cr, stream: stream}
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
					for _, value := range tc.incoming {
						c.Request.Header.Add("anthropic-beta", value)
					}
					incoming := c.Request.Header.Clone()
					info, wrote, err := doRelay(c, up, task)
					if err != nil || !wrote || info.Status != http.StatusOK || calls != 1 {
						t.Fatalf("relay failed: status=%d wrote=%t calls=%d err=%v", info.Status, wrote, calls, err)
					}
					if !reflect.DeepEqual(c.Request.Header, incoming) || string(task.body) != string(body) {
						t.Fatal("relay modified the incoming request")
					}
				})
			}
		}
	}
}

func TestAnthropicThinkingDisplayBetaHeaderOverrides(t *testing.T) {
	modelsTestDB(t)
	for _, tc := range []struct {
		name, overrides, want string
	}{
		{"passthrough", `{"*":true}`, "client-beta,thinking-display-updates-2026-08-18"},
		{"explicit", `{"anthropic-beta":"admin-beta"}`, "admin-beta,thinking-display-updates-2026-08-18"},
		{"removed", `{"anthropic-beta":false}`, "thinking-display-updates-2026-08-18"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("anthropic-beta"); got != tc.want {
					t.Errorf("upstream beta header = %q, want %q", got, tc.want)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, protocolResponses[FormatAnthropic])
			}))
			defer server.Close()
			body := []byte(`{"model":"alias","max_tokens":4096,"thinking":{"type":"adaptive","display":"updates"},"messages":[{"role":"user","content":"hi"}]}`)
			cr, err := parseClientRequest(FormatAnthropic, body, "")
			if err != nil {
				t.Fatal(err)
			}
			up := &Upstream{Type: "anthropic", BaseURL: server.URL, APIKey: "upstream-key", HeaderOverrides: tc.overrides}
			compileUpstreamConfig(up)
			task := &relayTask{format: FormatAnthropic, endpoint: "/v1/messages", model: "alias", body: body, canon: cr}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			c.Request.Header.Set("anthropic-beta", "client-beta")
			info, wrote, err := doRelay(c, up, task)
			if err != nil || !wrote || info.Status != http.StatusOK {
				t.Fatalf("relay failed: status=%d wrote=%t err=%v", info.Status, wrote, err)
			}
		})
	}
}
