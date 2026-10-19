package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

var protocolFormats = []clientFormat{FormatOpenAIChat, FormatOpenAIResponse, FormatAnthropic, FormatGemini}

// Independent wire fixtures include an image and a complete tool round trip.
var protocolRequests = map[clientFormat]string{
	FormatOpenAIChat:     `{"model":"alias","max_tokens":99,"temperature":0.2,"tools":[{"type":"function","function":{"name":"weather","description":"Forecast","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],"tool_choice":{"type":"function","function":{"name":"weather"}},"messages":[{"role":"system","content":"System instruction"},{"role":"user","content":[{"type":"text","text":"Look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]},{"role":"assistant","tool_calls":[{"id":"call_old","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}}]},{"role":"tool","tool_call_id":"call_old","content":"{\"result\":1}"},{"role":"user","content":"Continue"}]}`,
	FormatOpenAIResponse: `{"model":"alias","instructions":"System instruction","max_output_tokens":99,"temperature":0.2,"tools":[{"type":"function","name":"weather","description":"Forecast","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}],"tool_choice":{"type":"function","name":"weather"},"input":[{"role":"user","content":[{"type":"input_text","text":"Look"},{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]},{"type":"function_call","call_id":"call_old","name":"weather","arguments":"{\"city\":\"Paris\"}"},{"type":"function_call_output","call_id":"call_old","output":"{\"result\":1}"},{"role":"user","content":"Continue"}]}`,
	FormatAnthropic:      `{"model":"alias","system":[{"type":"text","text":"System instruction"}],"max_tokens":99,"temperature":0.2,"tools":[{"name":"weather","description":"Forecast","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],"tool_choice":{"type":"tool","name":"weather"},"messages":[{"role":"user","content":[{"type":"text","text":"Look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]},{"role":"assistant","content":[{"type":"tool_use","id":"call_old","name":"weather","input":{"city":"Paris"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_old","content":"{\"result\":1}"},{"type":"text","text":"Continue"}]}]}`,
	FormatGemini:         `{"systemInstruction":{"parts":[{"text":"System instruction"}]},"generationConfig":{"maxOutputTokens":99,"temperature":0.2},"tools":[{"functionDeclarations":[{"name":"weather","description":"Forecast","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["weather"]}},"contents":[{"role":"user","parts":[{"text":"Look"},{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}]},{"role":"model","parts":[{"functionCall":{"id":"call_old","name":"weather","args":{"city":"Paris"}}}]},{"role":"user","parts":[{"functionResponse":{"id":"call_old","name":"weather","response":{"result":1}}},{"text":"Continue"}]}]}`,
}

var protocolResponses = map[clientFormat]string{
	FormatOpenAIChat:     `{"id":"chatcmpl-up","choices":[{"index":0,"message":{"role":"assistant","content":"Hello","tool_calls":[{"id":"call_a","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}},{"id":"call_b","type":"function","function":{"name":"sum","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":17,"completion_tokens":0}}`,
	FormatOpenAIResponse: `{"id":"resp_up","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Hello"}]},{"type":"function_call","call_id":"call_a","name":"weather","arguments":"{\"city\":\"Paris\"}"},{"type":"function_call","call_id":"call_b","name":"sum","arguments":"{\"a\":1}"}],"usage":{"input_tokens":17,"output_tokens":0}}`,
	FormatAnthropic:      `{"id":"msg_up","type":"message","content":[{"type":"text","text":"Hello"},{"type":"tool_use","id":"call_a","name":"weather","input":{"city":"Paris"}},{"type":"tool_use","id":"call_b","name":"sum","input":{"a":1}}],"stop_reason":"tool_use","usage":{"input_tokens":17,"output_tokens":0}}`,
	FormatGemini:         `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Hello"},{"functionCall":{"id":"call_a","name":"weather","args":{"city":"Paris"}}},{"functionCall":{"id":"call_b","name":"sum","args":{"a":1}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":17,"candidatesTokenCount":0}}`,
}

func fixtureSSE(format clientFormat) string {
	frames := []string{}
	switch format {
	case FormatOpenAIChat:
		frames = []string{
			`{"choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"weather","arguments":"{\"city\":"}},{"index":1,"id":"call_b","type":"function","function":{"name":"sum","arguments":"{\"a\":"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"1}"}},{"index":0,"function":{"arguments":"\"Paris\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":17,"completion_tokens":0}}`, `[DONE]`,
		}
	case FormatAnthropic:
		frames = []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":17,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_a","name":"weather","input":{}}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}`,
			`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_b","name":"sum","input":{}}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":0}}`,
			`{"type":"message_stop"}`,
		}
	case FormatOpenAIResponse:
		frames = []string{
			`{"type":"response.created","response":{"id":"resp_up","status":"in_progress"}}`,
			`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Hello"}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_a","name":"weather","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"city\":"}`,
			`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","call_id":"call_b","name":"sum","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"a\":1}"}`,
			`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"\"Paris\"}"}`,
			`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":17,"output_tokens":0}}}`,
		}
	case FormatGemini:
		frames = []string{
			`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Hello"}]}}]}`,
			protocolResponses[FormatGemini],
		}
		// The second frame contains only tool parts, not a repeated text delta.
		frames[1] = strings.Replace(frames[1], `{"text":"Hello"},`, "", 1)
	}
	return "data: " + strings.Join(frames, "\r\n\r\ndata: ") + "\r\n\r\n"
}

func fixtureUpstream(format clientFormat, url string) *Upstream {
	u := &Upstream{Type: format.nativeUpstreamType(), BaseURL: url, APIKey: "upstream-key", ModelMap: `{"alias":"mapped"}`}
	if format == FormatOpenAIChat {
		u.OpenAIProtocol = "chat"
	}
	if format == FormatOpenAIResponse {
		u.OpenAIProtocol = "responses"
	}
	return u
}
func fixtureTask(t *testing.T, format clientFormat, stream bool) *relayTask {
	t.Helper()
	body := []byte(protocolRequests[format])
	cr, err := parseClientRequest(format, body, "alias")
	if err != nil || cr.ConversionError != nil {
		t.Fatalf("fixture parse: %v / %v", err, cr.ConversionError)
	}
	cr.Stream = stream
	if format != FormatGemini {
		var j map[string]any
		json.Unmarshal(body, &j)
		j["stream"] = stream
		body, _ = json.Marshal(j)
	}
	return &relayTask{format: format, endpoint: nativeEndpoint(format, cr), model: "alias", body: body, canon: cr, stream: stream, wantUsageChunk: true}
}

func checkWireRequest(t *testing.T, r *http.Request, format clientFormat) {
	t.Helper()
	if r.URL.Path != strings.Split(nativeEndpoint(format, &canonReq{Model: "mapped", Stream: strings.Contains(r.URL.Path, "streamGenerateContent")}), "?")[0] {
		t.Errorf("wrong upstream path: %s", r.URL.Path)
	}
	if format == FormatGemini && strings.Contains(r.URL.Path, "streamGenerateContent") && r.URL.Query().Get("alt") != "sse" {
		t.Error("Gemini stream missing alt=sse")
	}
	if format.nativeUpstreamType() == "openai" && (r.Header.Get("Authorization") != "Bearer upstream-key" || r.Header.Get("x-api-key") != "") {
		t.Error("wrong upstream credentials")
	}
	body, _ := io.ReadAll(r.Body)
	j := map[string]any{}
	if json.Unmarshal(body, &j) != nil {
		t.Fatal("invalid outbound JSON")
	}
	switch format {
	case FormatOpenAIChat:
		if j["model"] != "mapped" || object(array(j["messages"])[0])["role"] != "system" {
			t.Error("missing mapped model or system")
		}
		msgs := array(j["messages"])
		if object(array(object(msgs[2])["tool_calls"])[0])["id"] != "call_old" || object(msgs[3])["tool_call_id"] != "call_old" {
			t.Error("lost Chat tool round trip")
		}
		if str(object(array(object(msgs[1])["content"])[1])["type"]) != "image_url" {
			t.Error("lost Chat image")
		}
		if str(object(object(array(j["tools"])[0])["function"])["name"]) != "weather" {
			t.Error("lost Chat tool definition")
		}
	case FormatOpenAIResponse:
		if j["model"] != "mapped" || j["instructions"] != "System instruction" {
			t.Error("lost Responses model/system")
		}
		input := array(j["input"])
		if object(input[1])["call_id"] != "call_old" || object(input[2])["call_id"] != "call_old" {
			t.Error("lost Responses tool round trip")
		}
		if str(object(array(object(input[0])["content"])[1])["type"]) != "input_image" {
			t.Error("lost Responses image")
		}
	case FormatAnthropic:
		if j["model"] != "mapped" || contentText(j["system"]) != "System instruction" {
			t.Error("lost Anthropic model/system")
		}
		msgs := array(j["messages"])
		if object(array(object(msgs[1])["content"])[0])["id"] != "call_old" || object(array(object(msgs[2])["content"])[0])["tool_use_id"] != "call_old" {
			t.Error("lost Anthropic tool round trip")
		}
		if str(object(array(object(msgs[0])["content"])[1])["type"]) != "image" {
			t.Error("lost Anthropic image")
		}
	case FormatGemini:
		if contentText(object(j["systemInstruction"])["parts"]) != "System instruction" {
			t.Error("lost Gemini system")
		}
		msgs := array(j["contents"])
		if object(object(array(object(msgs[1])["parts"])[0])["functionCall"])["id"] != "call_old" || object(object(array(object(msgs[2])["parts"])[0])["functionResponse"])["name"] != "weather" {
			t.Error("lost Gemini tool round trip")
		}
		if object(array(object(msgs[0])["parts"])[1])["inlineData"] == nil {
			t.Error("lost Gemini image")
		}
	}
}

func TestProtocolRelayMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, in := range protocolFormats {
		for _, out := range protocolFormats {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s_to_%s_stream_%t", in, out, stream), func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						checkWireRequest(t, r, out)
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, fixtureSSE(out))
						} else {
							w.Header().Set("Content-Type", "application/json")
							fmt.Fprint(w, protocolResponses[out])
						}
					}))
					defer server.Close()
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest("POST", "/", nil)
					c.Request.Header.Set("x-api-key", "client-key")
					info, wrote, err := doRelay(c, fixtureUpstream(out, server.URL), fixtureTask(t, in, stream))
					if err != nil || !wrote || rec.Code != 200 {
						t.Fatalf("relay status=%d wrote=%t err=%v body=%s", rec.Code, wrote, err, rec.Body.String())
					}
					if info.PromptTok != 17 || info.CompletionTok != 0 || !info.InKnown || !info.OutKnown {
						t.Errorf("usage lost: %+v", info)
					}
					if in == out {
						return
					} // Native bytes are verified separately; inspect conversion output.
					if stream {
						checkClientStream(t, in, rec.Body.String())
					} else {
						result, err := parseProtocolResponse(in, rec.Body.Bytes())
						if err != nil {
							t.Fatal(err)
						}
						checkToolResponse(t, result)
						var body map[string]any
						json.Unmarshal(rec.Body.Bytes(), &body)
						if in != FormatGemini && body["model"] != "alias" {
							t.Error("response leaked mapped model")
						}
					}
				})
			}
		}
	}
}

func checkToolResponse(t *testing.T, r *canonResponse) {
	t.Helper()
	if r.Finish != "tool_calls" {
		t.Errorf("wrong finish reason %s", r.Finish)
	}
	calls := []canonCall{}
	for _, b := range r.Blocks {
		if b.Kind == "tool" {
			calls = append(calls, b.Call)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("missing calls: %+v", calls)
	}
	if calls[0].ID != "call_a" || calls[0].Name != "weather" || calls[0].Arguments != `{"city":"Paris"}` || calls[1].ID != "call_b" || calls[1].Arguments != `{"a":1}` {
		t.Errorf("corrupt tools: %+v", calls)
	}
}

// Reconstruct client events independently of the production stream decoder.
func checkClientStream(t *testing.T, format clientFormat, body string) {
	t.Helper()
	r := &canonResponse{Finish: "stop"}
	blocks := map[int]responseBlock{}
	terminal := false
	sequence := -1
	frames := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n")
	for _, frame := range frames {
		if strings.TrimSpace(frame) == "" {
			continue
		}
		event, payload := "", ""
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "event: ") {
				event = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				payload = strings.TrimPrefix(line, "data: ")
			}
		}
		if payload == "[DONE]" {
			terminal = true
			continue
		}
		var j map[string]any
		if json.Unmarshal([]byte(payload), &j) != nil {
			t.Fatalf("invalid SSE %s", frame)
		}
		if format == FormatAnthropic || format == FormatOpenAIResponse {
			if event != str(j["type"]) {
				t.Errorf("missing/wrong event name %s", frame)
			}
		}
		switch format {
		case FormatAnthropic:
			i := intFromAny(j["index"])
			switch event {
			case "content_block_start":
				b := object(j["content_block"])
				if b["type"] == "tool_use" {
					blocks[i] = responseBlock{Kind: "tool", Call: canonCall{ID: str(b["id"]), Name: str(b["name"])}}
				} else {
					blocks[i] = responseBlock{Kind: "text"}
				}
			case "content_block_delta":
				b := blocks[i]
				d := object(j["delta"])
				if b.Kind == "tool" {
					b.Call.Arguments += str(d["partial_json"])
				} else {
					b.Text += str(d["text"])
				}
				blocks[i] = b
			case "message_delta":
				r.Finish = normalizeFinish(str(object(j["delta"])["stop_reason"]))
				r.Usage = readUsage(object(j["usage"]), format)
			case "message_stop":
				terminal = true
			}
		case FormatOpenAIResponse:
			seq := intFromAny(j["sequence_number"])
			if seq != sequence+1 {
				t.Error("invalid Responses sequence")
			}
			sequence = seq
			if event == "response.completed" {
				terminal = true
				resp := object(j["response"])
				parsed, err := parseProtocolResponse(format, []byte(jsonText(resp)))
				if err != nil {
					t.Fatal(err)
				}
				r = parsed
			}
			if strings.HasSuffix(event, ".delta") && j["item_id"] == nil {
				t.Error("Responses delta missing item_id")
			}
		case FormatGemini:
			for _, v := range array(j["candidates"]) {
				cand := object(v)
				if str(cand["finishReason"]) != "" {
					terminal = true
				}
				for _, v := range array(object(cand["content"])["parts"]) {
					p := object(v)
					if call := object(p["functionCall"]); call != nil {
						r.Blocks = append(r.Blocks, responseBlock{Kind: "tool", Call: canonCall{str(call["id"]), str(call["name"]), jsonText(call["args"])}})
					}
				}
			}
			if terminal {
				r.Finish = "tool_calls"
			}
		case FormatOpenAIChat:
			for _, v := range array(j["choices"]) {
				ch := object(v)
				if str(ch["finish_reason"]) != "" {
					r.Finish = str(ch["finish_reason"])
				}
				for _, v := range array(object(ch["delta"])["tool_calls"]) {
					call := object(v)
					i := intFromAny(call["index"])
					b := blocks[i]
					b.Kind = "tool"
					if id := str(call["id"]); id != "" {
						b.Call.ID = id
					}
					f := object(call["function"])
					b.Call.Name += str(f["name"])
					b.Call.Arguments += str(f["arguments"])
					blocks[i] = b
				}
			}
		}
	}
	if !terminal {
		t.Fatal("missing terminal client event")
	}
	if format == FormatAnthropic || format == FormatOpenAIChat {
		for i := 0; i < len(blocks); i++ {
			r.Blocks = append(r.Blocks, blocks[i])
		}
	}
	checkToolResponse(t, r)
}

func TestProtocolUnsupportedAndNativePreservation(t *testing.T) {
	for _, body := range []string{
		`{"model":"alias","input":"hello","previous_response_id":"resp_previous"}`,
		`{"model":"alias","tools":[{"type":"web_search"}],"input":"hello"}`,
		`{"model":"alias","input":[{"role":"user","content":[{"type":"input_audio","data":"a"}]}]}`,
	} {
		cr, err := parseClientRequest(FormatOpenAIResponse, []byte(body), "")
		if err != nil || cr.ConversionError == nil {
			t.Fatal("unsupported feature not identified")
		}
		if _, err = renderProtocolRequest(FormatOpenAIChat, cr, "mapped"); err == nil {
			t.Fatal("unsupported feature silently discarded")
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			if string(b) != body {
				t.Error("native body was modified")
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, protocolResponses[FormatOpenAIResponse])
		}))
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		u := fixtureUpstream(FormatOpenAIResponse, server.URL)
		u.ModelMap = ""
		task := &relayTask{format: FormatOpenAIResponse, endpoint: "/v1/responses", model: "alias", body: []byte(body), canon: cr}
		_, wrote, err := doRelay(c, u, task)
		server.Close()
		if !wrote || err != nil {
			t.Fatalf("native feature failed: %v", err)
		}
	}
}

func TestProtocolTruncatedAndErrorStreams(t *testing.T) {
	for _, suffix := range []string{"", "data: {\"error\":{\"message\":\"upstream-secret\"}}\n\n", "data: broken\n\n"} {
		for _, in := range protocolFormats {
			if in == FormatOpenAIChat {
				continue
			}
			t.Run(string(in)+suffix, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				resp := &http.Response{Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n" + suffix))}
				info := &relayInfo{}
				err := relayProtocolSSE(c, resp, FormatOpenAIChat, fixtureTask(t, in, true), info)
				if err == nil {
					t.Fatal("interrupted stream accepted")
				}
				body := rec.Body.String()
				if strings.Contains(body, "upstream-secret") || strings.Contains(body, "message_stop") || strings.Contains(body, "response.completed") {
					t.Fatal("leaked error or emitted successful terminal event")
				}
				if !strings.Contains(body, "error") {
					t.Fatal("missing client error event")
				}
			})
		}
	}
}

func TestProtocolSSEFrames(t *testing.T) {
	input := ": heartbeat\r\nevent: message\r\ndata: {\r\ndata: \"choices\": []}\r\n\r\ndata: [DONE]"
	var got []string
	err := readSSEFrames(strings.NewReader(input), func(p string) (bool, error) { got = append(got, p); return p == "[DONE]", nil })
	if err != nil || len(got) != 2 || got[0] != "{\n\"choices\": []}" {
		t.Fatalf("bad SSE frames: %q %v", got, err)
	}
}

func TestProtocolFinishReasonsAndJSONStreamFallback(t *testing.T) {
	for _, in := range protocolFormats {
		if in == FormatOpenAIChat {
			continue
		}
		for _, finish := range []string{"length", "stop", "content_filter"} {
			t.Run(string(in)+finish, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"choices":[{"message":{"content":"text"},"finish_reason":%q}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`, finish)
				}))
				defer server.Close()
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest("POST", "/", nil)
				info, wrote, err := doRelay(c, fixtureUpstream(FormatOpenAIChat, server.URL), fixtureTask(t, in, true))
				if !wrote || err != nil || !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") || !info.InKnown || !info.OutKnown {
					t.Fatalf("JSON fallback failed %v %s", err, rec.Body.String())
				}
				reason := finish
				if in == FormatAnthropic {
					reason = anthropicStop(&canonResponse{Finish: finish})
				}
				if in == FormatGemini {
					reason = geminiStop(&canonResponse{Finish: finish})
				}
				if in == FormatOpenAIResponse && finish == "length" {
					reason = "max_output_tokens"
				}
				if in == FormatOpenAIResponse && finish == "stop" {
					reason = "completed"
				}
				if !strings.Contains(rec.Body.String(), reason) {
					t.Errorf("lost finish %s: %s", finish, rec.Body.String())
				}
			})
		}
	}
}

func TestOpenAIProtocolPersistenceAndMigration(t *testing.T) {
	r := testProxyAdminRouter(t)
	created := proxyAdminRequest(r, "POST", "upstreams", `{"name":"fixed","type":"openai","openai_protocol":"chat","base_url":"https://example.invalid","api_key":"saved-key"}`, true)
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	var result struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(created.Body.Bytes(), &result)
	u, err := dbGetUpstream(result.ID)
	if err != nil || u.OpenAIProtocol != "chat" {
		t.Fatal("protocol not persisted")
	}
	path := fmt.Sprintf("upstreams/%d", result.ID)
	patched := proxyAdminRequest(r, "PATCH", path, `{"openai_protocol":"responses"}`, true)
	if patched.Code != 200 {
		t.Fatal(patched.Body.String())
	}
	proxyAdminRequest(r, "PATCH", path, `{"name":"renamed"}`, true)
	u, _ = dbGetUpstream(result.ID)
	if u.OpenAIProtocol != "responses" {
		t.Fatal("omitted protocol was overwritten")
	}
	bad := proxyAdminRequest(r, "PATCH", path, `{"openai_protocol":"invalid"}`, true)
	if bad.Code != 400 {
		t.Fatal("invalid protocol accepted")
	}
	if _, err = db.Exec(`ALTER TABLE upstreams DROP COLUMN openai_protocol`); err != nil {
		t.Fatal(err)
	}
	if err = migrate(); err != nil {
		t.Fatal(err)
	}
	u, err = dbGetUpstream(result.ID)
	if err != nil || u.APIKey != "saved-key" || u.OpenAIProtocol != "" {
		t.Fatal("legacy migration lost credentials or changed defaults")
	}
	if u.wireFormat(FormatOpenAIResponse) != FormatOpenAIResponse || u.wireFormat(FormatAnthropic) != FormatOpenAIChat {
		t.Fatal("legacy behavior changed")
	}
}

func TestGeminiStreamRouteAndFixedOpenAIEndpoint(t *testing.T) {
	modelsTestDB(t)
	key := &ApiKey{Name: "protocol", Key: "sk-test-protocol", Enabled: true, Models: "alias"}
	if err := dbInsertKey(key); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("wrong endpoint %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var j map[string]any
		json.Unmarshal(body, &j)
		if j["stream"] != true {
			t.Error("Gemini URL stream action not reflected outbound")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, fixtureSSE(FormatOpenAIChat))
	}))
	defer server.Close()
	up := fixtureUpstream(FormatOpenAIChat, server.URL)
	up.Weight = 1
	up.Enabled = true
	up.Models = "alias"
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.POST("/v1beta/models/*action", func(c *gin.Context) { c.Set("apiKey", *key) }, relayHandler(FormatGemini, false))
	req := httptest.NewRequest("POST", "/v1beta/models/alias:streamGenerateContent?alt=sse", strings.NewReader(protocolRequests[FormatGemini]))
	req.Header.Set("X-Session-Id", "protocol-route")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("Gemini route failed %d %s", rec.Code, rec.Body.String())
	}
	checkClientStream(t, FormatGemini, rec.Body.String())
}

func TestProtocolGatewayAuthRoutingAndBilling(t *testing.T) {
	for _, in := range []clientFormat{FormatAnthropic, FormatOpenAIResponse, FormatGemini} {
		t.Run(string(in), func(t *testing.T) {
			modelsTestDB(t)
			previous := getSettings()
			settingsMu.Lock()
			settingsCache.Pricing = map[string]Price{"alias": {In: 2, Out: 100}}
			settingsMu.Unlock()
			defer func() { settingsMu.Lock(); settingsCache = previous; settingsMu.Unlock() }()
			key := &ApiKey{Name: "cross-protocol", Key: "sk-protocol-auth", Enabled: true, QPS: 100, Concurrency: 5, Models: "alias"}
			if err := dbInsertKey(key); err != nil {
				t.Fatal(err)
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				checkWireRequest(t, r, FormatOpenAIChat)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, protocolResponses[FormatOpenAIChat])
			}))
			defer server.Close()
			up := fixtureUpstream(FormatOpenAIChat, server.URL)
			up.Name = "fixed chat"
			up.Enabled = true
			up.Weight = 1
			up.Models = "alias"
			if err := dbInsertUpstream(up); err != nil {
				t.Fatal(err)
			}
			r := gin.New()
			group := r.Group("/v1")
			group.Use(keyAuth(), keyRateLimit())
			group.POST("/messages", relayHandler(FormatAnthropic, false))
			group.POST("/responses", relayHandler(FormatOpenAIResponse, false))
			gemini := r.Group("/v1beta")
			gemini.Use(keyAuth(), keyRateLimit())
			gemini.POST("/models/*action", relayHandler(FormatGemini, false))
			path := nativeEndpoint(in, &canonReq{Model: "alias"})
			req := httptest.NewRequest("POST", path, strings.NewReader(protocolRequests[in]))
			switch in {
			case FormatAnthropic:
				req.Header.Set("x-api-key", key.Key)
			case FormatGemini:
				req.Header.Set("x-goog-api-key", key.Key)
			default:
				req.Header.Set("Authorization", "Bearer "+key.Key)
			}
			req.Header.Set("X-Session-Id", "auth-"+string(in))
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != 200 || calls != 1 {
				t.Fatalf("gateway failed %d calls=%d body=%s", rec.Code, calls, rec.Body.String())
			}
			result, err := parseProtocolResponse(in, rec.Body.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			checkToolResponse(t, result)
			logs, err := dbListLogs(10, 0)
			if err != nil || len(logs) != 1 {
				t.Fatal("missing call log")
			}
			entry := logs[0]
			if entry.Endpoint != string(in) || entry.UpstreamID != up.ID || entry.PromptTokens != 17 || entry.CompletionTokens != 0 || entry.Status != 200 || math.Abs(entry.CostUSD-34.0/1e6) > 1e-12 {
				t.Errorf("wrong cross-protocol log: %+v", entry)
			}
			saved, _ := dbGetKeyByID(key.ID)
			if math.Abs(saved.UsedUSD-entry.CostUSD) > 1e-12 {
				t.Error("usage not billed to gateway key")
			}
			bad := httptest.NewRequest("POST", path, strings.NewReader(protocolRequests[in]))
			denied := httptest.NewRecorder()
			r.ServeHTTP(denied, bad)
			if denied.Code != 401 || calls != 1 {
				t.Error("gateway auth was bypassed")
			}
		})
	}
}

func TestProtocolRequestOptionsAndLongSystem(t *testing.T) {
	long := strings.Repeat("系统提示", 2000)
	body := []byte(fmt.Sprintf(`{"model":"alias","system":%q,"max_tokens":64,"top_p":0.7,"stop_sequences":["END"],"tool_choice":{"type":"auto","disable_parallel_tool_use":true},"tools":[{"name":"weather","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`, long))
	cr, err := parseClientRequest(FormatAnthropic, body, "")
	if err != nil || cr.ConversionError != nil {
		t.Fatal("failed valid request")
	}
	rendered, err := renderProtocolRequest(FormatOpenAIChat, cr, "mapped")
	if err != nil {
		t.Fatal(err)
	}
	var j map[string]any
	json.Unmarshal(rendered, &j)
	if object(array(j["messages"])[0])["content"] != long || j["parallel_tool_calls"] != false || j["tool_choice"] != "auto" || j["top_p"] != 0.7 || str(array(j["stop"])[0]) != "END" {
		t.Error("lost options or truncated actual system prompt")
	}
	if _, err := renderProtocolRequest(FormatOpenAIResponse, cr, "mapped"); err == nil {
		t.Error("Responses stop sequence silently dropped")
	}
	for _, model := range []string{"o3-mini", "gpt-5", "gpt-6-astra"} {
		modern, err := renderProtocolRequest(FormatOpenAIChat, cr, model)
		if err != nil {
			t.Fatal(err)
		}
		var request map[string]any
		json.Unmarshal(modern, &request)
		if request["max_completion_tokens"] != float64(64) || request["max_tokens"] != nil {
			t.Error("wrong reasoning-model token limit field")
		}
	}
	body = []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"weather","parameters":{"type":"OBJECT","properties":{"city":{"type":"STRING"},"type":{"type":"STRING"}},"default":{"type":"KeepCASE"}}}]}]}`)
	cr, _ = parseClientRequest(FormatGemini, body, "alias")
	rendered, err = renderProtocolRequest(FormatOpenAIChat, cr, "mapped")
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(rendered, &j)
	schema := object(object(object(array(j["tools"])[0])["function"])["parameters"])
	if schema["type"] != "object" || object(object(schema["properties"])["city"])["type"] != "string" || object(schema["default"])["type"] != "KeepCASE" {
		t.Error("invalid Gemini Schema conversion")
	}
}

func TestProtocolEmptyToolArgumentsAndIncrementalDelivery(t *testing.T) {
	for _, format := range []clientFormat{FormatAnthropic, FormatOpenAIResponse, FormatGemini} {
		t.Run(string(format), func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			info := &relayInfo{}
			resp := &http.Response{Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_empty\",\"function\":{\"name\":\"ping\",\"arguments\":\"\"}}]}}]}\n\ndata: [DONE]\n\n"))}
			if err := relayProtocolSSE(c, resp, FormatOpenAIChat, fixtureTask(t, format, true), info); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(rec.Body.String(), "call_empty") {
				t.Error("empty-argument tool lost")
			}
		})
	}
	// The downstream receives the first tool argument before the upstream releases its final frame.
	first := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_partial\",\"function\":{\"name\":\"weather\",\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n\n")
		w.(http.Flusher).Flush()
		close(first)
		<-release
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"Paris\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	defer close(release)
	r := gin.New()
	r.POST("/v1/messages", func(c *gin.Context) {
		_, _, _ = doRelay(c, fixtureUpstream(FormatOpenAIChat, upstream.URL), fixtureTask(t, FormatAnthropic, true))
	})
	gateway := httptest.NewServer(r)
	defer gateway.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	req := httptest.NewRequest("POST", gateway.URL+"/v1/messages", nil)
	req.RequestURI = ""
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-first
	err = readSSEFrames(resp.Body, func(payload string) (bool, error) {
		var j map[string]any
		json.Unmarshal([]byte(payload), &j)
		return str(object(j["delta"])["type"]) == "input_json_delta", nil
	})
	if err != nil {
		t.Fatalf("first argument was buffered until completion: %v", err)
	}
}

func TestFixedOpenAIProtocolKeepsEmbeddingsNative(t *testing.T) {
	modelsTestDB(t)
	key := &ApiKey{Name: "embedding", Key: "sk-embedding", Enabled: true, Models: "embedding-alias"}
	if err := dbInsertKey(key); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"embedding-alias","input":["hello world","second input"]}`
	cr, _ := parseClientRequest(FormatOpenAIChat, []byte(body), "")
	if !strings.Contains(cr.InputText, "hello world") {
		t.Error("embedding token estimate lost input")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("embedding dispatched to %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != body {
			t.Error("embedding payload converted")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":4,"total_tokens":4}}`)
	}))
	defer server.Close()
	wrong := &Upstream{Name: "wrong sticky type", Type: "anthropic", BaseURL: server.URL, APIKey: "key", Models: "embedding-alias", Weight: 1, Enabled: true}
	if err := dbInsertUpstream(wrong); err != nil {
		t.Fatal(err)
	}
	up := &Upstream{Name: "fixed Responses", Type: "openai", OpenAIProtocol: "responses", BaseURL: server.URL, APIKey: "key", Models: "embedding-alias", Weight: 1, Enabled: true}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	affinitySet(scopedSessionKey(key.ID, "sid:embedding-protocol"), wrong.ID)
	r := gin.New()
	r.POST("/v1/embeddings", func(c *gin.Context) { c.Set("apiKey", *key) }, relayHandler(FormatOpenAIChat, true))
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(body))
	req.Header.Set("X-Session-Id", "embedding-protocol")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "embedding") {
		t.Fatalf("embedding failed %d %s", rec.Code, rec.Body.String())
	}
	logs, _ := dbListLogs(1, 0)
	if len(logs) != 1 || logs[0].UpstreamID != up.ID {
		t.Fatal("embedding used incompatible sticky upstream")
	}
}
