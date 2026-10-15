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
			wantStream := true
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				wantPath := tc.path
				if tc.format == FormatGemini && wantStream {
					wantPath = strings.Replace(wantPath, ":generateContent", ":streamGenerateContent", 1)
					if r.URL.Query().Get("alt") != "sse" {
						t.Error("missing Gemini SSE query")
					}
				}
				if r.URL.Path != wantPath || r.Method != "POST" {
					t.Errorf("incorrect model endpoint: %s %s", r.Method, r.URL.Path)
				}
				body, _ := io.ReadAll(r.Body)
				cr, err := parseClientRequest(tc.format, body, "real-model")
				if err != nil || len(cr.Messages) != 1 || cr.Messages[0].Text != wantMessage || cr.MaxTokens != wantMax || cr.Model != "real-model" || (tc.format != FormatGemini && cr.Stream != wantStream) {
					t.Errorf("message/default/mapping lost: %s (%v)", body, err)
				}
				if r.Header.Get("X-Tenant") != "saved" || r.Header.Get("X-Admin") != "" {
					t.Error("saved headers missing or administrator headers leaked")
				}
				if wantStream && (r.Header.Get("Accept") != "text/event-stream" || r.Header.Get("Accept-Encoding") != "identity") {
					t.Error("stream test must use the same streaming headers as normal calls")
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
			for _, body := range []string{`{"model":"alias"}`, `{"model":"alias","message":" \n "}`, `{"model":"alias","message":"  请测试：\n返回 你好 👋  ","max_tokens":128}`, `{"model":"alias","stream":false}`} {
				wantMessage, wantMax = "hi", 4096
				wantStream = !strings.Contains(body, `"stream":false`)
				if strings.Contains(body, "128") {
					wantMessage, wantMax = "  请测试：\n返回 你好 👋  ", 128
				}
				w := proxyAdminRequest(router, "POST", path, body, true)
				var result map[string]any
				if json.Unmarshal(w.Body.Bytes(), &result) != nil || w.Code != 200 || result["ok"] != true || result["reply"] != "hello" || result["upstream_model"] != "real-model" || result["input_tokens"] != float64(2) || result["output_tokens"] != float64(3) || result["stream"] != wantStream {
					t.Fatalf("incorrect model test result: %d %s", w.Code, w.Body.String())
				}
			}
			if healthFails(up.ID) != fails {
				t.Error("manual testing changed routing health state")
			}
			if w := proxyAdminRequest(router, "POST", path, `{"model":"alias"}`, false); w.Code != 401 || calls != 4 {
				t.Fatal("unauthenticated model testing reached upstream")
			}
			for _, body := range []string{`{`, `{}`, `{"model":" "}`, `{"model":"alias","max_tokens":-1}`, `{"model":"alias","max_tokens":32769}`, `{"model":"alias","message":1}`, `{"model":"alias","message":"` + strings.Repeat("x", 65537) + `"}`} {
				if w := proxyAdminRequest(router, "POST", path, body, true); w.Code != 400 || calls != 4 {
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
			result, status, err := testUpstreamModel(context.Background(), &Upstream{Type: "openai", BaseURL: server.URL, APIKey: "channel-key"}, "model", "hi", 128, true)
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
	_, status, err := testUpstreamModel(context.Background(), up, "model", "hi", 128, true)
	if err == nil || status != 307 || proxyCalls != 1 || redirectCalls != 0 {
		t.Fatalf("proxy/redirect isolation failed: status=%d calls=%d redirected=%d err=%v", status, proxyCalls, redirectCalls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, _, err = testUpstreamModel(ctx, up, "model", "hi", 128, true)
	if err == nil || time.Since(start) > time.Second {
		t.Fatal("cancelled tests must stop promptly")
	}
}

func TestModelTestDetectsActualResponseEncoding(t *testing.T) {
	for _, format := range protocolFormats {
		for _, contentType := range []string{"application/json", "text/event-stream", "Text/Event-Stream; charset=utf-8", ""} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", format, contentType, stream), func(t *testing.T) {
					body := protocolResponses[format]
					if stream {
						body = fixtureSSE(format)
					}
					// Proxy-added whitespace and UTF-8 BOM must not decide encoding.
					result, err := readModelTestResponse(strings.NewReader(" \r\n\xef\xbb\xbf"+body), contentType, format)
					if err != nil || !strings.HasPrefix(result.text(), "Hello") || !result.Usage.InKnown || result.Usage.Prompt != 17 {
						t.Fatalf("mislabeled response was rejected: %+v %v", result, err)
					}
				})
			}
		}
	}
	for _, prefix := range []string{": keepalive\n\n", "event: message\n", "id: first\n", "retry: 1000\n"} {
		result, err := readModelTestResponse(strings.NewReader(prefix+fixtureSSE(FormatOpenAIChat)), "application/json", FormatOpenAIChat)
		if err != nil || !strings.HasPrefix(result.text(), "Hello") {
			t.Fatalf("SSE prelude was rejected: %+v %v", result, err)
		}
	}
	oversized := `{"choices":[{"message":{"content":"` + strings.Repeat("x", 4<<20) + `"}}]}`
	if _, err := readModelTestResponse(strings.NewReader(oversized), "application/json", FormatOpenAIChat); err == nil {
		t.Fatal("oversized response was accepted")
	}
}

func TestModelTestStreamingDefaultAndUpstreamErrorDiagnostics(t *testing.T) {
	router := testProxyAdminRouter(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		if request.Stream {
			// A usable streamed model can have a broken non-streaming endpoint,
			// and some gateways return SSE under a JSON Content-Type.
			fmt.Fprint(w, fixtureSSE(FormatOpenAIChat))
		} else {
			fmt.Fprint(w, `{"code":"unavailable","message":"Non-stream temporarily unavailable; key=channel-key","request":{"prompt":"private-prompt"}}`)
		}
	}))
	defer server.Close()
	up := &Upstream{Type: "openai", BaseURL: server.URL, APIKey: "channel-key"}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("upstreams/%d/model-test", up.ID)
	for _, stream := range []string{"", `,"stream":false`} {
		w := proxyAdminRequest(router, "POST", path, `{"model":"test"`+stream+`}`, true)
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 {
			t.Fatalf("model test failed: %d %s", w.Code, w.Body.String())
		}
		if stream == "" {
			if result["ok"] != true || result["stream"] != true || !strings.HasPrefix(str(result["reply"]), "Hello") {
				t.Fatalf("stream-capable model was incorrectly reported as failing: %v", result)
			}
		} else {
			message := str(result["error"])
			if result["ok"] != false || !strings.Contains(message, "Non-stream temporarily unavailable") || !strings.Contains(message, "HTTP 200") || strings.Contains(message, "channel-key") || strings.Contains(message, "private-prompt") {
				t.Fatalf("upstream error was lost or leaked sensitive data: %v", result)
			}
		}
	}
	if calls != 2 {
		t.Fatal("model tests must not silently retry billable requests")
	}
	for _, body := range []string{`{"error":{"message":"provider busy"}}`, `{"message":"provider busy"}`, `{"msg":"provider busy"}`, `{"detail":"provider busy"}`} {
		if _, err := readModelTestResponse(strings.NewReader(body), "text/event-stream", FormatOpenAIChat); err == nil || !strings.Contains(err.Error(), "provider busy") {
			t.Fatalf("missing upstream error details: %v", err)
		}
	}
	if _, err := readModelTestResponse(strings.NewReader(`{"output":[],"status":"completed"}`), "application/json", FormatOpenAIChat); err == nil || !strings.Contains(err.Error(), "output, status") {
		t.Fatalf("missing response shape diagnostics: %v", err)
	}
}
