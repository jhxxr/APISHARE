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

func anthropicSystemFixture(t *testing.T, stream bool) []byte {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(protocolRequests[FormatAnthropic]), &body); err != nil {
		t.Fatal(err)
	}
	body["stream"] = stream
	msgs := array(body["messages"])
	body["messages"] = []any{
		msgs[0],
		map[string]any{"role": "system", "content": "Use explicit types."},
		msgs[1],
		msgs[2],
		map[string]any{"role": "system", "content": []any{
			map[string]any{"type": "text", "text": "Keep answers concise.", "cache_control": map[string]any{"type": "ephemeral"}},
			map[string]any{"type": "text", "text": " Include examples."},
		}},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAnthropicSystemMessagesRelay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const wantSystem = "System instruction\nUse explicit types.\nKeep answers concise. Include examples."
	for _, out := range protocolFormats {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s_stream_%t", out, stream), func(t *testing.T) {
				body := anthropicSystemFixture(t, stream)
				cr, err := parseClientRequest(FormatAnthropic, body, "")
				if err != nil || cr.ConversionError != nil {
					t.Fatalf("parse system messages: %v / %v", err, cr.ConversionError)
				}
				if cr.System != wantSystem {
					t.Fatalf("system instructions lost or reordered: %q", cr.System)
				}
				if cr.InputText != "System instruction\n<u>Look" {
					t.Errorf("mid-conversation instructions changed session fingerprint: %q", cr.InputText)
				}
				upstreamCalled := false
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upstreamCalled = true
					b, _ := io.ReadAll(r.Body)
					if out == FormatAnthropic {
						if string(b) != string(body) {
							t.Error("native Anthropic request was modified")
						}
					} else {
						var wire map[string]any
						if err := json.Unmarshal(b, &wire); err != nil {
							t.Error(err)
							return
						}
						var system string
						switch out {
						case FormatOpenAIChat:
							system = contentText(object(array(wire["messages"])[0])["content"])
							object(array(wire["messages"])[0])["content"] = "System instruction"
						case FormatOpenAIResponse:
							system = str(wire["instructions"])
							wire["instructions"] = "System instruction"
						case FormatGemini:
							system = contentText(object(wire["systemInstruction"])["parts"])
							wire["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": "System instruction"}}}
						}
						if system != wantSystem {
							t.Errorf("outbound instructions lost: %q", system)
						}
						// Reuse independent image/tool wire checks after checking the added instructions.
						r.Body = io.NopCloser(strings.NewReader(jsonText(wire)))
						checkWireRequest(t, r, out)
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						if out == FormatAnthropic {
							for _, frame := range strings.Split(strings.TrimSpace(fixtureSSE(out)), "\r\n\r\n") {
								var event map[string]any
								if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &event); err != nil {
									t.Error(err)
									return
								}
								fmt.Fprintf(w, "event: %s\r\n%s\r\n\r\n", str(event["type"]), frame)
							}
						} else {
							fmt.Fprint(w, fixtureSSE(out))
						}
					} else {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, protocolResponses[out])
					}
				}))
				defer server.Close()
				u := fixtureUpstream(out, server.URL)
				if out == FormatAnthropic {
					u.ModelMap = ""
				}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
				task := &relayTask{format: FormatAnthropic, endpoint: "/v1/messages", model: "alias", body: body, canon: cr, stream: stream}
				info, wrote, err := doRelay(c, u, task)
				if err != nil || !wrote || !upstreamCalled || info.Status != http.StatusOK || rec.Code != http.StatusOK {
					t.Fatalf("relay failed: status=%d response=%s wrote=%t err=%v", info.Status, rec.Body.String(), wrote, err)
				}
				if stream {
					checkClientStream(t, FormatAnthropic, rec.Body.String())
				} else {
					response, err := parseProtocolResponse(FormatAnthropic, rec.Body.Bytes())
					if err != nil {
						t.Fatal(err)
					}
					checkToolResponse(t, response)
				}
			})
		}
	}
}

func TestAnthropicSystemMessageValidation(t *testing.T) {
	for _, tc := range []struct {
		name, message, wantError string
	}{
		{"tool_addition", `{"role":"system","content":[{"type":"tool_addition","definition":{"name":"new_tool"}}]}`, "content block tool_addition"},
		{"image", `{"role":"system","content":[{"type":"image","source":{"type":"url","url":"https://example.com/image.png"}}]}`, "system message content"},
		{"clear_at", `{"role":"system","clear_at":"next_user_message","content":"One turn only"}`, "system message clear_at"},
		{"output_config", `{"role":"system","output_config":{"effort":"high"},"content":[]}`, "system message output_config"},
		{"invalid_role", `{"role":"operator","content":"unknown role"}`, `invalid message role "operator"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"alias","messages":[{"role":"user","content":"hi"},` + tc.message + `]}`)
			cr, err := parseClientRequest(FormatAnthropic, body, "")
			if err != nil {
				t.Fatal(err)
			}
			if cr.ConversionError == nil || !strings.Contains(cr.ConversionError.Error(), tc.wantError) {
				t.Fatalf("unsupported feature discarded: %v", cr.ConversionError)
			}
			if _, err := renderProtocolRequest(FormatOpenAIChat, cr, "mapped"); err == nil {
				t.Error("invalid request was rendered")
			}
			if tc.name == "invalid_role" {
				return
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				if string(b) != string(body) {
					t.Error("native Anthropic feature was modified")
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, protocolResponses[FormatAnthropic])
			}))
			defer server.Close()
			u := fixtureUpstream(FormatAnthropic, server.URL)
			u.ModelMap = ""
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
			task := &relayTask{format: FormatAnthropic, endpoint: "/v1/messages", model: "alias", body: body, canon: cr}
			info, wrote, err := doRelay(c, u, task)
			if err != nil || !wrote || info.Status != http.StatusOK || rec.Code != http.StatusOK {
				t.Fatalf("native feature failed: status=%d response=%s err=%v", info.Status, rec.Body.String(), err)
			}
		})
	}
}

func TestAnthropicSystemMessagesWithoutTopLevelSystem(t *testing.T) {
	long := strings.Repeat("系统指令", 1500)
	body := []byte(fmt.Sprintf(`{"model":"alias","messages":[{"role":"system","content":"Initial instruction"},{"role":"user","content":"hi"},{"role":"system","content":[]},{"role":"system","clear_at":"never","content":%q}]}`, long))
	cr, err := parseClientRequest(FormatAnthropic, body, "")
	if err != nil || cr.ConversionError != nil {
		t.Fatalf("parse system messages: %v / %v", err, cr.ConversionError)
	}
	if cr.System != "Initial instruction\n"+long || len(cr.Messages) != 1 || cr.Messages[0].Role != "user" {
		t.Fatal("system text truncated, empty messages inserted, or user message lost")
	}
	if cr.InputText != "Initial instruction\n<u>hi" {
		t.Fatalf("unstable session fingerprint: %q", cr.InputText)
	}
}
