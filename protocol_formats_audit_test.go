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

func auditFormatFrames(events ...string) string {
	return "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
}

func auditNativeChatFrames() string {
	return auditFormatFrames(
		`{"id":"chat_keep","object":"chat.completion.chunk","model":"native","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello ","tool_calls":[{"index":1,"id":"b","type":"function","function":{"name":"sum","arguments":"{\"id\":"}},{"index":0,"id":"a","type":"function","function":{"name":"read","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"world","tool_calls":[{"index":1,"function":{"arguments":"9007199254740993}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"content":null},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":4}}}`,
		`[DONE]`,
	)
}

func auditNativeAnthropicFrames() string {
	return auditFormatFrames(
		`{"type":"message_start","message":{"id":"msg_keep","type":"message","role":"assistant","model":"native","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1,"cache_read_input_tokens":9}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"signed_"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"state"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"c","name":"lookup","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"id\":9007199254740993}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":0}}`,
		`{"type":"message_stop"}`,
	)
}

func auditNativeGeminiFrames() string {
	return auditFormatFrames(
		`{"responseId":"gem_keep","modelVersion":"native","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Hello","thoughtSignature":"signed_text"}]}}]}`,
		`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"id":9007199254740993}},"thoughtSignature":"signed_call"},{"inlineData":{"mimeType":"image/png","data":"YQ=="}}]},"finishReason":"STOP","groundingMetadata":{"keep":true}}]}`,
		`{"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":0,"thoughtsTokenCount":2,"cachedContentTokenCount":4}}`,
	)
}

func TestFormatsAuditNativeChatAggregationKeepsParallelToolsAndTrailingUsage(t *testing.T) {
	body, err := collectNativeProtocolSSE(strings.NewReader(auditNativeChatFrames()), FormatOpenAIChat, &relayInfo{})
	if err != nil {
		t.Fatal(err)
	}
	result, _ := decodeProtocolObject(body)
	message := object(object(array(result["choices"])[0])["message"])
	calls := array(message["tool_calls"])
	if result["id"] != "chat_keep" || result["object"] != "chat.completion" || message["content"] != "Hello world" || len(calls) != 2 || object(calls[0])["id"] != "a" || object(object(calls[1])["function"])["arguments"] != `{"id":9007199254740993}` || object(object(result["usage"])["prompt_tokens_details"])["cached_tokens"] != json.Number("4") {
		t.Fatalf("native Chat aggregation lost content or usage: %s", body)
	}
}

func TestFormatsAuditNativeAnthropicAggregationPreservesSignatureAndToolObject(t *testing.T) {
	body, err := collectNativeProtocolSSE(strings.NewReader(auditNativeAnthropicFrames()), FormatAnthropic, &relayInfo{})
	if err != nil {
		t.Fatal(err)
	}
	result, _ := decodeProtocolObject(body)
	blocks := array(result["content"])
	if len(blocks) != 2 || object(blocks[0])["signature"] != "signed_state" || object(blocks[0])["thinking"] != "plan" || object(object(blocks[1])["input"])["id"] != json.Number("9007199254740993") || object(result["usage"])["output_tokens"] != json.Number("0") || object(result["usage"])["cache_read_input_tokens"] != json.Number("9") {
		t.Fatalf("native Messages signature or tool input lost: %s", body)
	}
}

func TestFormatsAuditNativeGeminiAggregationKeepsSignedPartsAndImages(t *testing.T) {
	body, err := collectNativeProtocolSSE(strings.NewReader(auditNativeGeminiFrames()), FormatGemini, &relayInfo{})
	if err != nil {
		t.Fatal(err)
	}
	result, _ := decodeProtocolObject(body)
	candidate := object(array(result["candidates"])[0])
	parts := array(object(candidate["content"])["parts"])
	if len(parts) != 3 || object(parts[0])["thoughtSignature"] != "signed_text" || object(parts[1])["thoughtSignature"] != "signed_call" || object(parts[2])["inlineData"] == nil || object(candidate["groundingMetadata"])["keep"] != true || object(object(object(parts[1])["functionCall"])["args"])["id"] != json.Number("9007199254740993") || object(result["usageMetadata"])["thoughtsTokenCount"] != json.Number("2") {
		t.Fatalf("native Gemini parts detached or dropped: %s", body)
	}
}

func TestFormatsAuditNativeNonStreamingClientsReceiveJSONForSSEUpstreams(t *testing.T) {
	for format, frames := range map[clientFormat]string{FormatOpenAIChat: auditNativeChatFrames(), FormatAnthropic: auditNativeAnthropicFrames(), FormatGemini: auditNativeGeminiFrames()} {
		t.Run(string(format), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, frames)
			}))
			defer server.Close()
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest("POST", "/", nil)
			_, wrote, err := doRelay(c, fixtureUpstream(format, server.URL), fixtureTask(t, format, false))
			if err != nil || !wrote || !strings.Contains(recorder.Header().Get("Content-Type"), "application/json") || !json.Valid(recorder.Body.Bytes()) {
				t.Fatalf("non-stream native relay: %v / %s", err, recorder.Body.String())
			}
		})
	}
}

func TestFormatsAuditBufferedTruncationAndMalformedInputsDoNotSucceed(t *testing.T) {
	for _, sample := range []struct {
		format clientFormat
		frames string
	}{
		{FormatOpenAIChat, auditFormatFrames(`{"choices":[{"index":0,"delta":{"content":"partial"}}]}`, `[DONE]`)},
		{FormatAnthropic, strings.Replace(auditNativeAnthropicFrames(), `9007199254740993}`, `9007199254740993`, 1)},
		{FormatAnthropic, strings.ReplaceAll(auditNativeAnthropicFrames(), `data: {"type":"content_block_stop","index":1}`+"\n\n", "")},
		{FormatGemini, auditFormatFrames(`{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`)},
	} {
		if _, err := collectNativeProtocolSSE(strings.NewReader(sample.frames), sample.format, &relayInfo{}); err == nil {
			t.Fatalf("incomplete %s stream accepted", sample.format)
		}
	}
}

func TestFormatsAuditGeminiSamplingAndNativeOnlySettings(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"topP":0.7,"responseMimeType":"text/plain","stopSequences":["END"]}}`)
	cr, err := parseClientRequest(FormatGemini, body, "alias")
	if err != nil || cr.ConversionError != nil || cr.TopP == nil || *cr.TopP != 0.7 {
		t.Fatalf("Gemini sampling lost: %+v / %v", cr, err)
	}
	wire, err := renderProtocolRequest(FormatOpenAIChat, cr, "mapped")
	request, _ := decodeProtocolObject(wire)
	if err != nil || request["top_p"] != json.Number("0.7") || str(array(request["stop"])[0]) != "END" {
		t.Fatalf("sampling conversion: %s / %v", wire, err)
	}
	for _, cfg := range []string{`"thinkingConfig":{"thinkingBudget":1024}`, `"candidateCount":2`, `"topK":40`, `"responseMimeType":"application/json"`} {
		cr, err := parseClientRequest(FormatGemini, []byte(`{"contents":[{"parts":[{"text":"hello"}]}],"generationConfig":{`+cfg+`}}`), "alias")
		if err != nil || cr.ConversionError == nil {
			t.Fatalf("unsupported setting silently dropped: %s", cfg)
		}
	}
	chat, _ := parseClientRequest(FormatOpenAIChat, []byte(`{"model":"alias","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"text"}}`), "")
	if chat.ConversionError != nil {
		t.Fatal(chat.ConversionError)
	}
}

func TestFormatsAuditGeminiPrimaryCandidateAndPromptBlockSemantics(t *testing.T) {
	r, err := parseProtocolResponse(FormatGemini, []byte(`{"candidates":[{"index":1,"content":{"parts":[{"text":"wrong"}]}},{"index":0,"content":{"parts":[{"text":"right"}]},"finishReason":"STOP"}]}`))
	if err != nil || r.text() != "right" {
		t.Fatalf("wrong primary candidate: %+v / %v", r, err)
	}
	blocked := `{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":5}}`
	if _, err := parseProtocolResponse(FormatGemini, []byte(blocked)); err == nil {
		t.Fatal("converted prompt block accepted as an answer")
	}
	if _, err := collectProtocolSSE(strings.NewReader(auditFormatFrames(blocked)), FormatGemini); err == nil {
		t.Fatal("streamed prompt block accepted as an answer")
	}
	if body, err := collectNativeProtocolSSE(strings.NewReader(auditFormatFrames(blocked)), FormatGemini, &relayInfo{}); err != nil || !strings.Contains(string(body), `"blockReason":"SAFETY"`) {
		t.Fatalf("native feedback lost: %s / %v", body, err)
	}
}

func TestFormatsAuditToolChoiceReferencesAndDuplicateDeclarations(t *testing.T) {
	for _, body := range []string{
		`{"model":"alias","messages":[],"tools":[{"type":"function","function":{"name":"known"}}],"tool_choice":{"type":"function","function":{"name":"unknown"}}}`,
		`{"model":"alias","messages":[],"tools":[{"type":"function","function":{"name":"same"}},{"type":"function","function":{"name":"same"}}]}`,
		`{"model":"alias","messages":[],"tool_choice":"required"}`,
	} {
		cr, err := parseClientRequest(FormatOpenAIChat, []byte(body), "")
		if err != nil || cr.ConversionError == nil {
			t.Fatalf("invalid tool declaration accepted: %s", body)
		}
	}
}

func TestFormatsAuditAnthropicParallelHistoryKeepsBatchOwnership(t *testing.T) {
	body := `{"model":"alias","max_tokens":32,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"read","input":{}},{"type":"tool_use","id":"b","name":"sum","input":{}}]},{"role":"user","content":[{"type":"text","text":"continue"},{"type":"tool_result","tool_use_id":"b","content":"two"},{"type":"tool_result","tool_use_id":"a","content":"one"}]},{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"next","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"new"}]}]}`
	cr, err := parseClientRequest(FormatAnthropic, []byte(body), "")
	if err != nil || cr.ConversionError != nil {
		t.Fatalf("history parse: %v / %v", err, cr.ConversionError)
	}
	if len(cr.Messages) != 6 || cr.Messages[1].ToolID != "a" || cr.Messages[1].Text != "one" || cr.Messages[2].ToolID != "b" || cr.Messages[3].Text != "continue" || cr.Messages[5].ToolName != "next" || cr.Messages[5].Text != "new" {
		t.Fatalf("tool batches reordered across turns: %+v", cr.Messages)
	}
}

func TestFormatsAuditLargeIDsSurviveEveryObjectRenderingPath(t *testing.T) {
	call := canonCall{"c", "lookup", `{"id":9007199254740993}`}
	cr := &canonReq{Messages: []canonMsg{{Role: "assistant", Calls: []canonCall{call}}, {Role: "tool", ToolID: "c", ToolName: "lookup", Text: call.Arguments}}}
	for _, format := range []clientFormat{FormatAnthropic, FormatGemini} {
		wire, err := renderProtocolRequest(format, cr, "mapped")
		if err != nil || !strings.Contains(string(wire), `9007199254740993`) || strings.Contains(string(wire), `9007199254740992`) {
			t.Fatalf("request rounded ID: %s / %v", wire, err)
		}
		response := jsonText(renderProtocolResponse(format, "mapped", "", &canonResponse{Blocks: []responseBlock{{Kind: "tool", Call: call}}, Finish: "tool_calls"}))
		if !strings.Contains(response, `9007199254740993`) || strings.Contains(response, `9007199254740992`) {
			t.Fatalf("response rounded ID: %s", response)
		}
	}
}

func TestFormatsAuditNativeHTTP200ErrorsRemainUncommitted(t *testing.T) {
	for _, format := range []clientFormat{FormatOpenAIChat, FormatAnthropic, FormatGemini} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"error":{"message":"upstream failed"}}`)
		}))
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest("POST", "/", nil)
		_, wrote, err := doRelay(c, fixtureUpstream(format, server.URL), fixtureTask(t, format, false))
		server.Close()
		if err == nil || wrote || recorder.Body.Len() != 0 {
			t.Fatalf("native %s error logged as success: %v / %s", format, err, recorder.Body.String())
		}
	}
}
