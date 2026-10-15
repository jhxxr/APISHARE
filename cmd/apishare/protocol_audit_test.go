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

func TestAuditChatReasoningAliasesDoNotDuplicateOrReadCiphertext(t *testing.T) {
	for _, message := range []map[string]any{
		{"reasoning_content": "plan", "reasoning": "plan", "reasoning_details": []any{map[string]any{"type": "reasoning.text", "text": "plan"}}},
		{"reasoning": "plan"},
		{"reasoning_details": []any{map[string]any{"type": "reasoning.text", "text": "plan"}, map[string]any{"type": "reasoning.encrypted", "data": "secret"}}},
	} {
		if got := chatReasoningText(message); got != "plan" {
			t.Fatalf("alias reasoning: %q", got)
		}
		body := jsonText(map[string]any{"choices": []any{map[string]any{"message": message}}})
		response, err := parseProtocolResponse(FormatOpenAIChat, []byte(body))
		if err != nil || len(response.Blocks) != 1 || response.Blocks[0].Text != "plan" {
			t.Fatalf("JSON alias lost: %+v / %v", response, err)
		}
		deltas, _, err := newProtocolDecoder(FormatOpenAIChat).decode(jsonText(map[string]any{"choices": []any{map[string]any{"delta": message}}}))
		if err != nil || len(deltas) != 1 || deltas[0].Block.Text != "plan" {
			t.Fatalf("stream alias lost: %+v / %v", deltas, err)
		}
	}
	if chatReasoningText(map[string]any{"reasoning_details": []any{map[string]any{"type": "reasoning.encrypted", "data": "secret"}}}) != "" {
		t.Fatal("ciphertext was exposed as reasoning")
	}
}

func TestAuditAnthropicInitialOutputUsageIsCumulative(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	stream := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":8}}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"hello\"}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":20}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	info := &relayInfo{}
	err := relayProtocolSSE(c, &http.Response{Body: io.NopCloser(strings.NewReader(stream))}, FormatAnthropic, fixtureTask(t, FormatOpenAIResponse, true), info)
	if err != nil || info.PromptTok != 2 || info.CompletionTok != 20 || !info.OutKnown {
		t.Fatalf("start/final usage lost or added twice: %+v / %v", info, err)
	}
	if err := readSSEFrames(strings.NewReader("data:\n\ndata: [DONE]\n\n"), func(payload string) (bool, error) {
		if payload != "[DONE]" {
			t.Fatal("empty heartbeat interpreted as JSON")
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAuditAnthropicPlaintextThinkingReachesChatToolHistory(t *testing.T) {
	body := `{"model":"alias","max_tokens":100,"messages":[{"role":"user","content":"read"},{"role":"assistant","content":[{"type":"thinking","thinking":"read first","signature":"opaque"},{"type":"tool_use","id":"c","name":"read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":"done"}]}]}`
	cr, err := parseClientRequest(FormatAnthropic, []byte(body), "")
	if err != nil || cr.ConversionError != nil {
		t.Fatalf("parse: %v / %v", err, cr.ConversionError)
	}
	wire, err := renderProtocolRequest(FormatOpenAIChat, cr, "mapped")
	if err != nil || !strings.Contains(string(wire), `"reasoning_content":"read first"`) || strings.Contains(string(wire), "opaque") {
		t.Fatalf("thinking history lost: %s / %v", wire, err)
	}
}

func TestAuditFunctionSchemaKeepsRootUnionAndDefinitions(t *testing.T) {
	schema := map[string]any{"oneOf": []any{map[string]any{"type": "object", "properties": map[string]any{"mode": map[string]any{"const": "view"}}}}, "$defs": map[string]any{"mode": map[string]any{"type": "string"}}}
	before := jsonText(schema)
	cr := &canonReq{Tools: []canonTool{{Name: "automation_update", Schema: schema}}, Messages: []canonMsg{{Role: "user", Text: "read"}}}
	for _, format := range protocolFormats {
		body, err := renderProtocolRequest(format, cr, "mapped")
		if err != nil || !strings.Contains(string(body), `"oneOf"`) || !strings.Contains(string(body), `"$defs"`) || !strings.Contains(string(body), `"type":"object"`) {
			t.Fatalf("root union damaged: %s / %v", body, err)
		}
		if format == FormatOpenAIResponse && !strings.Contains(string(body), `"strict":false`) {
			t.Fatal("conversion strengthened a non-strict tool by default")
		}
	}
	if jsonText(schema) != before {
		t.Fatal("schema changed across retries")
	}
}

func TestAuditTopLevelToolNamesFitProviderLimitsAndRestoreIdentity(t *testing.T) {
	name := strings.Repeat("long.tool", 12)
	body := jsonText(map[string]any{"model": "alias", "input": "hello", "tools": []any{map[string]any{"type": "function", "name": name, "parameters": map[string]any{"type": "object"}}}})
	cr := parseCodexFixture(t, body)
	if len(cr.Tools) != 1 || len(cr.Tools[0].Name) > 64 || strings.Contains(cr.Tools[0].Name, ".") {
		t.Fatalf("invalid provider tool name: %+v", cr.Tools)
	}
	block := restoreResponseTool(responseBlock{Kind: "tool", Call: canonCall{"c", cr.Tools[0].Name, "{}"}}, cr.ResponseTools)
	if block.Call.Name != name || block.Namespace != "" {
		t.Fatal("tool alias lost the client's declared identity")
	}
}

func TestAuditCodexDelegationAndPlaintextAgentMessages(t *testing.T) {
	cr := parseCodexFixture(t, `{"model":"alias","input":[{"type":"function_call_output","name":"create_thread","namespace":"codex_app","output":"<codex_delegation><input>Read the files</input></codex_delegation>"},{"type":"agent_message","author":"/root","recipient":"/root/child","content":[{"type":"input_text","text":"Then patch them"}]}]}`)
	if len(cr.Messages) != 2 || cr.Messages[0].Role != "user" || cr.Messages[1].Text != "Then patch them" {
		t.Fatalf("delegation lost: %+v", cr.Messages)
	}
	for _, body := range []string{
		`{"model":"alias","input":[{"type":"function_call_output","name":"read","namespace":"codex_app","output":"<codex_delegation><input>bad</input></codex_delegation>"}]}`,
		`{"model":"alias","input":[{"type":"function_call_output","name":"create_thread","namespace":"other","output":"<codex_delegation><input>bad</input></codex_delegation>"}]}`,
		`{"model":"alias","input":[{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"opaque"}]}]}`,
	} {
		cr, err := parseClientRequest(FormatOpenAIResponse, []byte(body), "")
		if err != nil || cr.ConversionError == nil {
			t.Fatalf("invalid/opaque delegation accepted: %v / %+v", err, cr)
		}
	}
}

func TestAuditResponsesFinalSnapshotsCompletePartialDeltas(t *testing.T) {
	d := newProtocolDecoder(FormatOpenAIResponse)
	frames := []string{
		`{"type":"response.output_text.delta","output_index":0,"content_index":1,"delta":"Hel"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":1,"part":{"type":"output_text","text":"Hello"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"c","name":"read","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"path\":"}`,
		`{"type":"response.function_call_arguments.done","output_index":1,"arguments":"{\"path\":\"x.go\"}"}`,
		`{"type":"response.done","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":""},{"type":"output_text","text":"Hello"}]},{"type":"function_call","call_id":"c","name":"read","arguments":"{\"path\":\"x.go\"}"}],"usage":{"input_tokens":0,"output_tokens":0}}}`,
	}
	text, args := "", ""
	for _, frame := range frames {
		deltas, _, err := d.decode(frame)
		if err != nil {
			t.Fatal(err)
		}
		for _, delta := range deltas {
			text += delta.Block.Text
			args += delta.Block.Call.Arguments
		}
	}
	if !d.terminal || text != "Hello" || args != `{"path":"x.go"}` {
		t.Fatalf("partial final snapshot duplicated or lost: %q / %q", text, args)
	}
	if _, _, err := d.decode(`{"type":"response.output_text.done","output_index":0,"content_index":1,"text":"Different"}`); err == nil {
		t.Fatal("contradictory final text accepted")
	}
}

func TestAuditResponsesReasoningContentAndRefusalDone(t *testing.T) {
	r, err := collectProtocolSSE(strings.NewReader("data: {\"type\":\"response.reasoning_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"plan\"}\n\ndata: {\"type\":\"response.refusal.done\",\"output_index\":1,\"content_index\":0,\"refusal\":\"cannot do that\"}\n\ndata: {\"type\":\"response.done\",\"status\":\"completed\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}\n\n"), FormatOpenAIResponse)
	if err != nil || len(r.Blocks) != 2 || r.Blocks[0].Kind != "thinking" || r.Blocks[1].Text != "cannot do that" || r.Finish != "content_filter" || !r.Usage.OutKnown {
		t.Fatalf("content/refusal done lost: %+v / %v", r, err)
	}
}

func TestAuditGeminiInvalidFinishNeverLooksSuccessful(t *testing.T) {
	for _, reason := range []string{"MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL", "MISSING_THOUGHT_SIGNATURE", "MALFORMED_RESPONSE"} {
		body := jsonText(map[string]any{"candidates": []any{map[string]any{"finishReason": reason, "content": map[string]any{"parts": []any{map[string]any{"thoughtSignature": "opaque"}}}}}})
		if _, err := parseProtocolResponse(FormatGemini, []byte(body)); err == nil {
			t.Fatal("malformed Gemini JSON accepted")
		}
		if _, _, err := newProtocolDecoder(FormatGemini).decode(body); err == nil {
			t.Fatal("malformed Gemini SSE accepted")
		}
		if _, err := nativeSSETerminal(FormatGemini, body); err == nil {
			t.Fatal("native malformed Gemini counted as success")
		}
	}
}

func TestAuditAnthropicSeedArgumentsAreNotDuplicated(t *testing.T) {
	for _, withDelta := range []bool{false, true} {
		d := newProtocolDecoder(FormatAnthropic)
		frames := []string{`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c","name":"read","input":{"path":"x.go"}}}`}
		if withDelta {
			frames = append(frames, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"x.go\"}"}}`)
		}
		frames = append(frames, `{"type":"content_block_stop","index":0}`, `{"type":"message_stop"}`)
		args := ""
		for _, frame := range frames {
			deltas, _, err := d.decode(frame)
			if err != nil {
				t.Fatal(err)
			}
			for _, delta := range deltas {
				args += delta.Block.Call.Arguments
			}
		}
		if args != `{"path":"x.go"}` {
			t.Fatalf("initial input duplicated: %q", args)
		}
	}
}

func TestAuditAnthropicParallelToolsHaveSequentialBlockLifecycle(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	w := newProtocolStream(c, FormatAnthropic, "alias", false)
	for _, delta := range []streamDelta{
		{Key: "thinking", Block: responseBlock{Kind: "thinking", Text: "plan"}},
		{Key: "a", Block: responseBlock{Kind: "tool", Call: canonCall{"a", "read", `{"path":`}}},
		{Key: "text", Block: responseBlock{Kind: "text", Text: "reading"}},
		{Key: "b", Block: responseBlock{Kind: "tool", Call: canonCall{"b", "read", `{"path":"b.go"}`}}},
		{Key: "a", Block: responseBlock{Kind: "tool", Call: canonCall{Arguments: `"a.go"}`}}},
	} {
		if err := w.push(c, delta, upstreamUsage{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.finish(c, upstreamUsage{}, "tool_calls", ""); err != nil {
		t.Fatal(err)
	}
	open, next, tools := -1, 0, 0
	err := readSSEFrames(strings.NewReader(rec.Body.String()), func(payload string) (bool, error) {
		var event map[string]any
		json.Unmarshal([]byte(payload), &event)
		i := intFromAny(event["index"])
		switch str(event["type"]) {
		case "content_block_start":
			if open != -1 || i != next {
				t.Fatal("overlapping/out-of-order block")
			}
			open = i
			next++
			if object(event["content_block"])["type"] == "tool_use" {
				tools++
			}
		case "content_block_delta":
			if open != i {
				t.Fatal("delta targets unopened/closed block")
			}
		case "content_block_stop":
			if open != i {
				t.Fatal("wrong stop index")
			}
			open = -1
		}
		return false, nil
	})
	if err != nil || open != -1 || tools != 2 {
		t.Fatalf("invalid parallel lifecycle: %v / %s", err, rec.Body.String())
	}
	body, err := collectNativeProtocolSSE(strings.NewReader(rec.Body.String()), FormatAnthropic, &relayInfo{})
	if err != nil {
		t.Fatal(err)
	}
	var message map[string]any
	if err := json.Unmarshal(body, &message); err != nil {
		t.Fatal(err)
	}
	content := array(message["content"])
	if len(content) != 4 || str(object(content[0])["thinking"]) != "plan" || str(object(object(content[1])["input"])["path"]) != "a.go" || str(object(content[2])["text"]) != "reading" || str(object(object(content[3])["input"])["path"]) != "b.go" {
		t.Fatalf("streaming or deferred blocks lost/duplicated content: %s", body)
	}
}

func TestAuditInvalidStreamedAnthropicToolDoesNotComplete(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	w := newProtocolStream(c, FormatAnthropic, "alias", false)
	w.push(c, streamDelta{Key: "tool", Block: responseBlock{Kind: "tool", Call: canonCall{"c", "read", `{"path":`}}}, upstreamUsage{})
	if err := w.finish(c, upstreamUsage{}, "tool_calls", ""); err == nil {
		t.Fatal("invalid streamed tool was accepted")
	}
	w.fail(c)
	if !strings.Contains(rec.Body.String(), `"type":"error"`) || strings.Contains(rec.Body.String(), `"type":"message_stop"`) || strings.Contains(rec.Body.String(), `"type":"content_block_stop"`) {
		t.Fatal("invalid streamed tool was closed successfully or lost its error")
	}
}

func TestAuditNativeStreamsTrackTerminalWithoutChangingBytes(t *testing.T) {
	for _, tc := range []struct {
		stream    string
		wantError bool
	}{
		{"data: {\r\ndata: \"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\r\n\r\n", false},
		{"data: {\"type\":\"response.created\"}\n\n", true},
		{"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"overloaded\"}}}\n\n", true},
	} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		info := &relayInfo{}
		err := relaySSENative(c, &http.Response{Body: io.NopCloser(strings.NewReader(tc.stream))}, FormatOpenAIResponse, info)
		want := tc.stream
		if tc.wantError {
			want = "" // Initialization and early errors must remain retryable.
		}
		if (err != nil) != tc.wantError || rec.Body.String() != want {
			t.Fatalf("native bytes or terminal changed: %v / %q", err, rec.Body.String())
		}
		if !tc.wantError && (!info.InKnown || !info.OutKnown) {
			t.Fatal("native multiline zero usage lost")
		}
	}
}

func TestAuditNativeNonStreamingResponsesPreserveOpaqueOutputItems(t *testing.T) {
	stream := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_native\",\"object\":\"response\",\"model\":\"alias\"}}\n\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"compaction\",\"id\":\"cmp_native\",\"encrypted_content\":\"opaque\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, stream)
	}))
	defer server.Close()
	up := fixtureUpstream(FormatOpenAIResponse, server.URL)
	up.ModelMap = ""
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	_, wrote, err := doRelay(c, up, fixtureTask(t, FormatOpenAIResponse, false))
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if err != nil || !wrote || !strings.Contains(rec.Header().Get("Content-Type"), "application/json") || body["id"] != "resp_native" || len(array(body["output"])) != 1 || object(array(body["output"])[0])["encrypted_content"] != "opaque" {
		t.Fatalf("native opaque output lost: %v / %s", err, rec.Body.String())
	}
}

func TestAuditNativeSnapshotsKeepObjectArgumentsAndOutputIdentity(t *testing.T) {
	arguments := map[string]any{"query": "new"}
	final := map[string]any{"type": "tool_search_call", "arguments": arguments}
	prior := map[string]any{"type": "tool_search_call", "arguments": map[string]any{"query": "old"}}
	if !reflect.DeepEqual(supplementNativeItem(final, prior)["arguments"], arguments) {
		t.Fatal("native object arguments overwritten")
	}
	stream := "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_a\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}}\n\ndata: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"id\":\"fc_b\",\"call_id\":\"b\",\"name\":\"read\",\"arguments\":\"{}\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"function_call\",\"id\":\"fc_b\",\"call_id\":\"b\",\"name\":\"read\",\"arguments\":\"{}\"}]}}\n\n"
	body, err := collectNativeResponsesSSE(strings.NewReader(stream), &relayInfo{})
	var result map[string]any
	json.Unmarshal(body, &result)
	output := array(result["output"])
	if err != nil || len(output) != 2 || object(output[0])["id"] != "fc_b" || object(output[1])["id"] != "msg_a" || object(output[0])["content"] != nil {
		t.Fatalf("native items merged by wrong index: %v / %s", err, body)
	}
}

func TestAuditResponseBodyLimitRejectsTruncation(t *testing.T) {
	if body, err := readProtocolBody(strings.NewReader("1234"), 4); err != nil || string(body) != "1234" {
		t.Fatalf("exact body bound: %q / %v", body, err)
	}
	if _, err := readProtocolBody(strings.NewReader("12345"), 4); err == nil {
		t.Fatal("oversized response silently truncated")
	}
}

func TestAuditToolPayloadsPreserveLargeIntegerIDs(t *testing.T) {
	cr := parseCodexFixture(t, `{"model":"alias","input":[{"type":"function_call","call_id":"c","name":"lookup","arguments":{"id":9007199254740993}},{"type":"function_call_output","call_id":"c","output":"done"}]}`)
	if cr.Messages[0].Calls[0].Arguments != `{"id":9007199254740993}` {
		t.Fatal("object argument ID lost precision")
	}
	r, err := parseProtocolResponse(FormatAnthropic, []byte(`{"content":[{"type":"tool_use","id":"c","name":"lookup","input":{"id":9007199254740993}}],"stop_reason":"tool_use"}`))
	if err != nil || r.Blocks[0].Call.Arguments != `{"id":9007199254740993}` {
		t.Fatalf("upstream object ID lost precision: %+v / %v", r, err)
	}
}

func TestAuditNativeResponsesOperationsExcludeChatOnlyUpstreams(t *testing.T) {
	modelsTestDB(t)
	key := &ApiKey{Name: "compact", Key: "compact-test-key", Enabled: true, Models: "alias"}
	if err := dbInsertKey(key); err != nil {
		t.Fatal(err)
	}
	chatCalls, nativeCalls := 0, 0
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatCalls++
		t.Error("compact reached a Chat-only upstream")
	}))
	defer chat.Close()
	answer := `{"id":"resp_compact","object":"response.compaction","output":[{"type":"message","role":"user","content":[{"type":"input_text","text":"retained input"}]},{"type":"compaction","id":"cmp_keep","encrypted_content":"opaque"}],"usage":{"input_tokens":0,"output_tokens":0}}`
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeCalls++
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/v1/responses/compact" || body["model"] != "mapped" || body["instructions"] != "keep this" || object(body["metadata"])["keep"] != "unchanged" {
			t.Errorf("native compact modified input: %s / %+v", r.URL.Path, body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, answer)
	}))
	defer native.Close()
	chatUp, nativeUp := fixtureUpstream(FormatOpenAIChat, chat.URL), fixtureUpstream(FormatOpenAIResponse, native.URL)
	for _, up := range []*Upstream{chatUp, nativeUp} {
		up.Enabled = true
		up.Weight = 1
		up.Models = "alias"
		if err := dbInsertUpstream(up); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"model":"alias","instructions":"keep this","metadata":{"keep":"unchanged"},"input":"hello"}`
	cr := parseCodexFixture(t, body)
	router := gin.New()
	group := router.Group("/v1")
	group.Use(keyAuth())
	group.POST("/responses/compact", relayHandler(FormatOpenAIResponse, true))
	req := httptest.NewRequest("POST", "/v1/responses/compact", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key.Key)
	req.Header.Set("X-Session-Id", "compact-session")
	affinitySet(sessionKeyFromCanon("/v1/responses/compact", req.Header, cr), chatUp.ID)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 || chatCalls != 0 || nativeCalls != 1 || rec.Body.String() != answer {
		t.Fatalf("compact route failed: calls=%d/%d status=%d body=%s", chatCalls, nativeCalls, rec.Code, rec.Body.String())
	}
}
