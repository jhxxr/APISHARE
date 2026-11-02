package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestInternAIAnthropicCompatibilityScope(t *testing.T) {
	body := []byte(`{ "model":"alias", "thinking":{"type":"adaptive","display":"updates"}, "messages":[{"role":"user","content":[{"type":"text","text":""},{"type":"text","text":"hi"}]}] }`)
	for _, tc := range []struct {
		name, target string
		format       clientFormat
		changed      bool
	}{
		{"internai", "https://discovery-api.intern-ai.org.cn/v1/messages", FormatAnthropic, true},
		{"case_and_port", "https://DISCOVERY-API.INTERN-AI.ORG.CN:443/v1/messages", FormatAnthropic, true},
		{"official", "https://api.anthropic.com/v1/messages", FormatAnthropic, false},
		{"other_proxy", "https://example.com/v1/messages", FormatAnthropic, false},
		{"suffix", "https://discovery-api.intern-ai.org.cn.example.com/v1/messages", FormatAnthropic, false},
		{"chat", "https://discovery-api.intern-ai.org.cn/v1/chat/completions", FormatOpenAIChat, false},
		{"responses", "https://discovery-api.intern-ai.org.cn/v1/responses", FormatOpenAIResponse, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{}
			headers.Add("anthropic-beta", "other-beta,"+anthropicThinkingDisplayUpdatesBeta)
			headers.Add("anthropic-beta", "extra-beta,"+anthropicThinkingDisplayUpdatesBeta)
			originalHeaders := headers.Clone()
			normalizeInternAIAnthropicHeaders(headers, tc.format, tc.target)
			if tc.changed {
				if got := headers.Get("anthropic-beta"); got != "other-beta,extra-beta" {
					t.Fatalf("wrong provider beta normalization: %q", got)
				}
			} else if !reflect.DeepEqual(headers, originalHeaders) {
				t.Fatal("headers outside the provider adapter were changed")
			}
			got, err := normalizeInternAIAnthropicRequest(body, tc.format, tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.changed {
				if string(got) != string(body) {
					t.Fatal("request outside the provider adapter was changed")
				}
				return
			}
			payload, err := decodeProtocolObject(got)
			if err != nil || object(payload["thinking"])["display"] != nil ||
				len(array(object(array(payload["messages"])[0])["content"])) != 1 {
				t.Fatalf("provider fields were not normalized: %s / %v", got, err)
			}
		})
	}
}

func TestInternAIAnthropicCompatibilityPreservesHistory(t *testing.T) {
	body := []byte(`{
		"model":"alias","max_tokens":4096,
		"thinking":{"type":"enabled","budget_tokens":1024,"display":"updates"},
		"system":[{"type":"text","text":""},{"type":"text","text":"Keep <code>& intact.","cache_control":{"type":"ephemeral"}}],
		"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"large":{"type":"integer"}}}}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":""}]},
			{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]},
			{"role":"assistant","content":[{"type":"text","text":""}]},
			{"role":"assistant","content":[{"type":"text"},{"type":"thinking","thinking":"","signature":"opaque-signed-state"},{"type":"redacted_thinking","data":"opaque"},{"type":"tool_use","id":"call_1","name":"lookup","input":{"large":9007199254740993,"content":[{"type":"text","text":""}]}}]},
			{"role":"user","content":[{"type":"text","text":null},{"type":"tool_result","tool_use_id":"call_1","is_error":false,"content":[{"type":"text","text":""},{"type":"text","text":"<result>&"}]},{"type":"text","text":" "}]}
		],
		"metadata":{"large":9007199254740993,"session_id":"original"}
	}`)
	got, err := normalizeInternAIAnthropicRequest(body, FormatAnthropic, "https://discovery-api.intern-ai.org.cn/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := decodeProtocolObject(got)
	if err != nil {
		t.Fatal(err)
	}
	thinking := object(payload["thinking"])
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != json.Number("1024") || thinking["display"] != nil {
		t.Fatalf("thinking mode or budget changed: %s", got)
	}
	messages := array(payload["messages"])
	if len(messages) != 3 {
		t.Fatalf("empty turns were not removed: %s", got)
	}
	assistant := array(object(messages[1])["content"])
	if len(assistant) != 3 || object(assistant[0])["signature"] != "opaque-signed-state" ||
		object(assistant[1])["data"] != "opaque" || object(assistant[2])["id"] != "call_1" {
		t.Fatalf("signed thinking or tool call changed: %s", got)
	}
	input := object(object(assistant[2])["input"])
	if input["large"] != json.Number("9007199254740993") ||
		object(array(input["content"])[0])["text"] != "" {
		t.Fatalf("opaque tool input or number precision changed: %s", got)
	}
	last := array(object(messages[2])["content"])
	result := object(last[0])
	if result["tool_use_id"] != "call_1" || result["is_error"] != false ||
		len(array(result["content"])) != 1 || object(last[1])["text"] != " " {
		t.Fatalf("tool result or whitespace text changed: %s", got)
	}
	if object(array(payload["system"])[0])["cache_control"] == nil || payload["tools"] == nil ||
		object(payload["metadata"])["large"] != json.Number("9007199254740993") ||
		!strings.Contains(string(got), "Keep <code>& intact.") {
		t.Fatalf("system, schema, metadata or text encoding changed: %s", got)
	}
	again, err := normalizeInternAIAnthropicRequest(got, FormatAnthropic, "https://discovery-api.intern-ai.org.cn/v1/messages")
	if err != nil || string(again) != string(got) {
		t.Fatalf("normalization was not stable: %s / %v", again, err)
	}
}

func TestInternAIAnthropicCompatibilityLeavesValidOrUnrecoverableRequests(t *testing.T) {
	for _, body := range []string{
		`{"model":"alias","thinking":{"type":"adaptive","display":"omitted"},"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"alias","thinking":{"type":"adaptive","display":"summarized"},"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"alias","messages":[{"role":"user","content":[{"type":"text","text":" "}]}]}`,
		`{"model":"alias","messages":[{"role":"user","content":[{"type":"text","text":123}]}]}`,
		`{"model":"alias","messages":[{"role":"assistant","content":"old answer"},{"role":"user","content":[{"type":"text","text":""}]}]}`,
		`{"model":"alias","messages":[{"role":"assistant","content":[{"type":"text","text":""}]}]}`,
		`{"model":"alias",`,
	} {
		got, err := normalizeInternAIAnthropicRequest([]byte(body), FormatAnthropic, "https://discovery-api.intern-ai.org.cn/v1/messages")
		if err != nil || string(got) != body {
			t.Fatalf("unnecessary rewrite: %s / %v", got, err)
		}
	}
}

func TestInternAIAnthropicCompatibilityRelay(t *testing.T) {
	modelsTestDB(t)
	previous := upstreamHTTPClient
	t.Cleanup(func() { upstreamHTTPClient = previous })
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			calls := 0
			upstreamHTTPClient = &http.Client{Transport: openCodeRoutingTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				body, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				payload, err := decodeProtocolObject(body)
				if err != nil || req.URL.Hostname() != "discovery-api.intern-ai.org.cn" || payload["model"] != "deepseek-v4.1-flash-1017" {
					return nil, fmt.Errorf("wrong provider or model: %s / %v", body, err)
				}
				if strings.Contains(req.Header.Get("anthropic-beta"), anthropicThinkingDisplayUpdatesBeta) {
					return nil, fmt.Errorf("unsupported updates beta survived normalization")
				}
				// Mirror the provider's actual validator, which rejects updates
				// regardless of the supplied beta header.
				message := ""
				if object(payload["thinking"])["display"] == "updates" {
					message = "invalid Claude request: thinking: invalid thinking display: updates"
				}
				for _, raw := range array(payload["messages"]) {
					for _, block := range array(object(raw)["content"]) {
						if b := object(block); str(b["type"]) == "text" && str(b["text"]) == "" {
							message = "invalid Claude request: message[3]: content[0]: text type must have non-empty text field"
						}
					}
				}
				if message != "" {
					return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(jsonText(map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": message}})))}, nil
				}
				response := protocolResponses[FormatAnthropic]
				contentType := "application/json"
				if stream {
					response, contentType = fixtureSSE(FormatAnthropic), "text/event-stream"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(response))}, nil
			})}
			body := []byte(fmt.Sprintf(`{"model":"alias","max_tokens":4096,"stream":%t,"thinking":{"type":"adaptive","display":"updates"},"messages":[{"role":"user","content":[{"type":"text","text":""},{"type":"text","text":"hi"}]}]}`, stream))
			cr, err := parseClientRequest(FormatAnthropic, body, "")
			if err != nil {
				t.Fatal(err)
			}
			up := &Upstream{Type: "anthropic", BaseURL: "https://discovery-api.intern-ai.org.cn", APIKey: "test-key", ModelMap: `{"alias":"deepseek-v4.1-flash-1017"}`}
			task := &relayTask{format: FormatAnthropic, endpoint: "/v1/messages", model: "alias", body: body, canon: cr, stream: stream}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			c.Request.Header.Set("anthropic-beta", anthropicThinkingDisplayUpdatesBeta)
			incoming := c.Request.Header.Clone()
			info, wrote, err := doRelay(c, up, task)
			if err != nil || !wrote || info.Status != 200 || calls != 1 {
				t.Fatalf("provider relay failed: status=%d calls=%d wrote=%t err=%v", info.Status, calls, wrote, err)
			}
			if !reflect.DeepEqual(c.Request.Header, incoming) || string(task.body) != string(body) {
				t.Fatal("provider normalization changed the original request")
			}
		})
	}
}

// Opt-in live coverage verifies the real provider rather than assuming that
// accepting the beta header implies support for its thinking display value.
func TestInternAIAnthropicCompatibilityLive(t *testing.T) {
	key := os.Getenv("APISHARE_INTERNAI_TEST_KEY")
	if key == "" {
		t.Skip("set APISHARE_INTERNAI_TEST_KEY to verify the real provider")
	}
	modelsTestDB(t)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"alias","max_tokens":32,"stream":%t,"thinking":{"type":"adaptive","display":"updates"},"messages":[{"role":"user","content":"Hello."},{"role":"assistant","content":"OK."},{"role":"user","content":"Again."},{"role":"assistant","content":[{"type":"text","text":""},{"type":"text","text":"OK."}]},{"role":"user","content":"Reply OK."}]}`, stream))
			cr, err := parseClientRequest(FormatAnthropic, body, "")
			if err != nil {
				t.Fatal(err)
			}
			up := &Upstream{Type: "anthropic", BaseURL: "https://discovery-api.intern-ai.org.cn", APIKey: key, ModelMap: `{"alias":"deepseek-v4.1-flash-1017"}`}
			task := &relayTask{format: FormatAnthropic, endpoint: "/v1/messages", model: "alias", body: body, canon: cr, stream: stream}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
			c.Request.Header.Set("anthropic-beta", anthropicThinkingDisplayUpdatesBeta)
			info, wrote, err := doRelay(c, up, task)
			if err != nil || !wrote || info.Status != http.StatusOK || recorder.Code != http.StatusOK {
				t.Fatalf("live provider relay failed: status=%d wrote=%t err=%v response=%s", info.Status, wrote, err, strings.ReplaceAll(recorder.Body.String(), key, "[REDACTED]"))
			}
			t.Logf("real InternAI Messages request succeeded: stream=%t, HTTP %d", stream, info.Status)
		})
	}
}
