package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestThinkingDisplayCrossFormatRelay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const thought = "private reasoning"
	for _, display := range []string{"updates", "summarized", "omitted"} {
		for _, mode := range []string{"enabled", "adaptive"} {
			for _, target := range []clientFormat{FormatOpenAIChat, FormatOpenAIResponse, FormatGemini} {
				for _, stream := range []bool{false, true} {
					for _, upstreamSSE := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/%s/%s/stream=%t/sse=%t", display, mode, target, stream, upstreamSSE), func(t *testing.T) {
							thinking := map[string]any{"type": mode, "display": display}
							payload := map[string]any{
								"model": "alias", "max_tokens": 4096, "stream": stream, "thinking": thinking,
								"messages": []any{map[string]any{"role": "user", "content": "hi"}},
							}
							model := "custom-model"
							if mode == "enabled" {
								thinking["budget_tokens"] = 1024
								if target == FormatGemini {
									model = "gemini-2.5-flash"
								}
							} else {
								payload["output_config"] = map[string]any{"effort": "high"}
								if target == FormatGemini {
									model = "gemini-3.8-flash"
								}
							}
							body := []byte(jsonText(payload))
							cr, err := parseClientRequest(FormatAnthropic, body, "")
							if err != nil {
								t.Fatal(err)
							}
							task := &relayTask{format: FormatAnthropic, endpoint: "/v1/messages", model: "alias", body: body, canon: cr, stream: stream}
							calls := 0
							server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								calls++
								var request map[string]any
								if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
									t.Error(err)
								}
								if request["thinking"] != nil || request["display"] != nil {
									t.Errorf("Anthropic display leaked into target request: %+v", request)
								}
								if target == FormatGemini {
									config := object(object(request["generationConfig"])["thinkingConfig"])
									if mode == "enabled" && config["thinkingBudget"] != float64(1024) ||
										mode == "adaptive" && config["thinkingLevel"] != "high" {
										t.Errorf("thinking configuration lost: %+v", config)
									}
								} else {
									want := "high"
									if mode == "enabled" {
										want = "low"
									}
									if got := reasoningTestWireEffort(request, target); got != want {
										t.Errorf("reasoning effort = %q, want %q", got, want)
									}
								}
								if upstreamSSE {
									frame := map[clientFormat]string{
										FormatOpenAIChat:     `{"choices":[{"index":0,"delta":{"reasoning_content":"private reasoning"}}]}`,
										FormatOpenAIResponse: `{"type":"response.reasoning_summary_text.delta","output_index":3,"summary_index":0,"delta":"private reasoning"}`,
										FormatGemini:         `{"candidates":[{"index":0,"content":{"parts":[{"thought":true,"text":"private reasoning"}]}}]}`,
									}[target]
									fixture := fixtureSSE(target)
									index := strings.Index(fixture, "\r\n\r\n") + len("\r\n\r\n")
									w.Header().Set("Content-Type", "text/event-stream")
									fmt.Fprint(w, fixture[:index]+"data: "+frame+"\r\n\r\n"+fixture[index:])
								} else {
									response, _ := decodeProtocolObject([]byte(protocolResponses[target]))
									switch target {
									case FormatOpenAIChat:
										object(object(array(response["choices"])[0])["message"])["reasoning_content"] = thought
									case FormatOpenAIResponse:
										response["output"] = append([]any{map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": thought}}}}, array(response["output"])...)
									case FormatGemini:
										content := object(object(array(response["candidates"])[0])["content"])
										content["parts"] = append([]any{map[string]any{"thought": true, "text": thought}}, array(content["parts"])...)
									}
									w.Header().Set("Content-Type", "application/json")
									fmt.Fprint(w, jsonText(response))
								}
							}))
							defer server.Close()
							up := fixtureUpstream(target, server.URL)
							up.ModelMap = jsonText(map[string]string{"alias": model})
							rec := httptest.NewRecorder()
							c, _ := gin.CreateTestContext(rec)
							c.Request = httptest.NewRequest(http.MethodPost, task.endpoint, nil)
							info, wrote, err := doRelay(c, up, task)
							if err != nil || !wrote || info.Status != http.StatusOK || calls != 1 {
								t.Fatalf("relay: status=%d wrote=%t calls=%d err=%v body=%s", info.Status, wrote, calls, err, rec.Body.String())
							}
							if got := strings.Contains(rec.Body.String(), thought); got != (display == "summarized") {
								t.Fatalf("thinking display %q: %s", display, rec.Body.String())
							}
							if stream {
								checkClientStream(t, FormatAnthropic, rec.Body.String())
							} else {
								result, err := parseProtocolResponse(FormatAnthropic, rec.Body.Bytes())
								if err != nil {
									t.Fatal(err)
								}
								checkToolResponse(t, result)
							}
							if !strings.Contains(rec.Body.String(), "Hello") || info.PromptTok != 17 || info.CompletionTok != 0 {
								t.Fatalf("response content or usage lost: %+v / %s", info, rec.Body.String())
							}
							if string(task.body) != string(body) || task.canon.ThinkingMode != mode || task.canon.ThinkingDisplay != display ||
								mode == "enabled" && (task.canon.ThinkingBudget == nil || *task.canon.ThinkingBudget != 1024) ||
								mode == "adaptive" && task.canon.ReasoningEffort != "high" {
								t.Fatal("relay changed the original thinking request")
							}
						})
					}
				}
			}
		}
	}
}

func TestThinkingDisplayValidation(t *testing.T) {
	for _, tc := range []struct {
		name, thinking string
		wantError      bool
	}{
		{"default", `{"type":"enabled","budget_tokens":1024}`, false},
		{"null", `{"type":"enabled","budget_tokens":1024,"display":null}`, false},
		{"unknown", `{"type":"enabled","budget_tokens":1024,"display":"future"}`, true},
		{"empty", `{"type":"enabled","budget_tokens":1024,"display":""}`, true},
		{"non_string", `{"type":"enabled","budget_tokens":1024,"display":true}`, true},
		{"disabled", `{"type":"disabled","display":"updates"}`, true},
		{"unknown_control", `{"type":"enabled","budget_tokens":1024,"block_binding":"opaque"}`, true},
		{"missing_mode", `{"display":"updates"}`, true},
		{"missing_budget", `{"type":"enabled","display":"updates"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"alias","max_tokens":4096,"thinking":%s,"messages":[{"role":"user","content":"hi"}]}`, tc.thinking))
			cr, err := parseClientRequest(FormatAnthropic, body, "")
			if err != nil {
				t.Fatal(err)
			}
			_, err = renderProtocolRequest(FormatOpenAIChat, cr, "custom-model")
			if (err != nil) != tc.wantError {
				t.Fatalf("thinking=%s conversion error=%v, want error=%t", tc.thinking, err, tc.wantError)
			}
		})
	}
}
