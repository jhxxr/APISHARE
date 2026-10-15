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
	"time"
)

func TestModelTestDefaultsAndCustomMessagesAcrossProtocols(t *testing.T) {
	for _, tc := range []struct {
		name, typ, protocol, path, response string
		format                              clientFormat
	}{
		{"chat", "openai", "chat", "/v1/chat/completions", `{"choices":[{"message":{"content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`, FormatOpenAIChat},
		{"responses", "openai", "responses", "/v1/responses", `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":2,"output_tokens":3}}`, FormatOpenAIResponse},
		{"anthropic", "anthropic", "", "/v1/messages", `{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`, FormatAnthropic},
		{"gemini", "gemini", "", "/v1beta/models/real-model:generateContent", `{"candidates":[{"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3}}`, FormatGemini},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := testProxyAdminRouter(t)
			wantMessage, wantMax := "hi", 4096
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != tc.path || r.Method != "POST" {
					t.Errorf("incorrect model endpoint: %s %s", r.Method, r.URL.Path)
				}
				body, _ := io.ReadAll(r.Body)
				cr, err := parseClientRequest(tc.format, body, "real-model")
				if err != nil || len(cr.Messages) != 1 || cr.Messages[0].Text != wantMessage || cr.MaxTokens != wantMax || cr.Model != "real-model" || cr.Stream {
					t.Errorf("message/default/mapping lost: %s (%v)", body, err)
				}
				if r.Header.Get("X-Tenant") != "saved" || r.Header.Get("X-Admin") != "" {
					t.Error("saved headers missing or administrator headers leaked")
				}
				switch tc.typ {
				case "anthropic":
					if r.Header.Get("x-api-key") != "channel-key" || r.Header.Get("anthropic-version") != "2023-06-01" {
						t.Error("incorrect Anthropic authentication")
					}
				case "gemini":
					if r.Header.Get("x-goog-api-key") != "channel-key" {
						t.Error("incorrect Gemini authentication")
					}
				default:
					if r.Header.Get("Authorization") != "Bearer channel-key" {
						t.Error("incorrect OpenAI authentication")
					}
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.response)
			}))
			defer server.Close()
			up := &Upstream{Type: tc.typ, OpenAIProtocol: tc.protocol, BaseURL: server.URL, APIKey: "channel-key", Models: "alias", ModelMap: `{"alias":"real-model"}`, HeaderOverrides: `{"*":true,"X-Tenant":"saved","X-Admin":"{client_header:Authorization}"}`}
			if err := dbInsertUpstream(up); err != nil {
				t.Fatal(err)
			}
			path := fmt.Sprintf("upstreams/%d/model-test", up.ID)
			healthMarkFailure(up.ID)
			fails := healthFails(up.ID)
			for _, body := range []string{`{"model":"alias"}`, `{"model":"alias","message":" \n "}`, `{"model":"alias","message":"  请测试：\n返回 你好 👋  ","max_tokens":128}`} {
				if strings.Contains(body, "128") {
					wantMessage, wantMax = "  请测试：\n返回 你好 👋  ", 128
				}
				w := proxyAdminRequest(router, "POST", path, body, true)
				var result map[string]any
				if json.Unmarshal(w.Body.Bytes(), &result) != nil || w.Code != 200 || result["ok"] != true || result["reply"] != "hello" || result["upstream_model"] != "real-model" || result["input_tokens"] != float64(2) || result["output_tokens"] != float64(3) {
					t.Fatalf("incorrect model test result: %d %s", w.Code, w.Body.String())
				}
			}
			if healthFails(up.ID) != fails {
				t.Error("manual testing changed routing health state")
			}
			if w := proxyAdminRequest(router, "POST", path, `{"model":"alias"}`, false); w.Code != 401 || calls != 3 {
				t.Fatal("unauthenticated model testing reached upstream")
			}
			for _, body := range []string{`{`, `{}`, `{"model":" "}`, `{"model":"alias","max_tokens":-1}`, `{"model":"alias","max_tokens":32769}`, `{"model":"alias","message":1}`, `{"model":"alias","message":"` + strings.Repeat("x", 65537) + `"}`} {
				if w := proxyAdminRequest(router, "POST", path, body, true); w.Code != 400 || calls != 3 {
					t.Fatalf("invalid request reached upstream: %d %s", w.Code, w.Body.String())
				}
			}
			if w := proxyAdminRequest(router, "POST", "upstreams/999999/model-test", `{"model":"alias"}`, true); w.Code != 404 {
				t.Fatal("unknown upstream was not rejected")
			}
		})
	}
}

func TestModelTestErrorsStreamsAndProxy(t *testing.T) {
	testProxyAdminRouter(t)
	for _, tc := range []struct {
		name, body, contentType string
		status                  int
		ok                      bool
	}{
		{"http-error", `{"error":{"message":"invalid channel-key"}}`, "application/json", 401, false},
		{"invalid-json", `<html>bad gateway</html>`, "text/html", 200, false},
		{"error-envelope", `{"error":{"message":"model unavailable"}}`, "application/json", 200, false},
		{"missing-message", `{"choices":[]}`, "application/json", 200, false},
		{"stream", "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", "text/event-stream", 200, true},
		{"incomplete-stream", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n", "text/event-stream", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			result, status, err := testUpstreamModel(context.Background(), &Upstream{Type: "openai", BaseURL: server.URL, APIKey: "channel-key"}, "model", "hi", 128)
			if (err == nil) != tc.ok || status != tc.status || (tc.ok && result.text() != "hello") {
				t.Fatalf("result=%+v status=%d err=%v", result, status, err)
			}
			if err != nil && strings.Contains(err.Error(), "channel-key") {
				t.Fatal("upstream API key leaked in test error")
			}
		})
	}
	proxyCalls, redirectCalls := 0, 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirectCalls++ }))
	defer destination.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls++
		if r.URL.Host != "model-test.invalid" || r.Header.Get("Authorization") != "Bearer channel-key" {
			t.Error("model test did not use the configured proxy")
		}
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(307)
	}))
	defer proxy.Close()
	p := testSavedProxy(t, proxy.URL, "", "")
	up := &Upstream{Type: "openai", BaseURL: "http://model-test.invalid", APIKey: "channel-key", ProxyID: p.ID}
	_, status, err := testUpstreamModel(context.Background(), up, "model", "hi", 128)
	if err == nil || status != 307 || proxyCalls != 1 || redirectCalls != 0 {
		t.Fatalf("proxy/redirect isolation failed: status=%d calls=%d redirected=%d err=%v", status, proxyCalls, redirectCalls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, _, err = testUpstreamModel(ctx, up, "model", "hi", 128)
	if err == nil || time.Since(start) > time.Second {
		t.Fatal("cancelled tests must stop promptly")
	}
}
