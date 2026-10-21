package main

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestOptimizationNativeJSONStreamsPreserveProviderState(t *testing.T) {
	samples := map[clientFormat]string{
		FormatOpenAIChat:     `{"id":"chat_native","object":"chat.completion","model":"native","choices":[{"index":0,"message":{"role":"assistant","content":"hello","tool_calls":[{"id":"c","type":"function","function":{"name":"lookup","arguments":"{\"id\":9007199254740993}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":3}}}`,
		FormatAnthropic:      `{"id":"msg_native","type":"message","model":"native","role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"opaque_signature"},{"type":"tool_use","id":"c","name":"lookup","input":{"id":9007199254740993}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":0,"cache_read_input_tokens":3}}`,
		FormatGemini:         `{"responseId":"gem_native","modelVersion":"native","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hello","thoughtSignature":"opaque_signature"},{"inlineData":{"mimeType":"image/png","data":"YQ=="}}]},"finishReason":"STOP","groundingMetadata":{"keep":true}}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":0,"thoughtsTokenCount":3}}`,
		FormatOpenAIResponse: `{"id":"resp_native","object":"response","model":"native","status":"completed","output":[{"id":"rs_keep","type":"reasoning","summary":[{"type":"summary_text","text":"plan"}],"encrypted_content":"opaque"},{"id":"ctc_keep","type":"custom_tool_call","call_id":"c","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"},{"id":"cmp_keep","type":"compaction","encrypted_content":"opaque_compaction"}],"usage":{"input_tokens":5,"output_tokens":0}}`,
	}
	for format, body := range samples {
		t.Run(string(format), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest("POST", "/", nil)
			if err := relayNativeJSONStream(c, format, []byte(body), true); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(recorder.Header().Get("Content-Type"), "text/event-stream") {
				t.Fatal("native streaming client received JSON headers")
			}
			aggregated, err := collectNativeProtocolSSE(strings.NewReader(recorder.Body.String()), format, &relayInfo{})
			if err != nil {
				t.Fatalf("wrapper generated invalid native lifecycle: %v / %s", err, recorder.Body.String())
			}
			original, _ := decodeProtocolObject([]byte(body))
			restored, _ := decodeProtocolObject(aggregated)
			field := map[clientFormat]string{FormatOpenAIChat: "choices", FormatAnthropic: "content", FormatGemini: "candidates", FormatOpenAIResponse: "output"}[format]
			if !reflect.DeepEqual(original[field], restored[field]) || !reflect.DeepEqual(original["usage"], restored["usage"]) || !reflect.DeepEqual(original["usageMetadata"], restored["usageMetadata"]) {
				t.Fatalf("provider state changed: %s / %s", body, aggregated)
			}
		})
	}
}

func TestOptimizationNativeWrappersValidateBeforeWriting(t *testing.T) {
	for format, body := range map[clientFormat]string{
		FormatOpenAIChat:     `{"choices":[{"message":{"tool_calls":[{"type":"function","function":{"arguments":"{broken"}}]},"finish_reason":"tool_calls"}]}`,
		FormatAnthropic:      `{"content":[{"type":"tool_use","input":null}],"stop_reason":"tool_use"}`,
		FormatOpenAIResponse: `{"output":[],"status":"in_progress"}`,
		FormatGemini:         `{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`,
	} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest("POST", "/", nil)
		if err := relayNativeJSONStream(c, format, []byte(body), true); err == nil || c.Writer.Written() || recorder.Body.Len() != 0 {
			t.Fatalf("invalid %s wrapper committed a response", format)
		}
	}
}

func TestOptimizationThinkingModesAndBudgetBoundaries(t *testing.T) {
	disabled, _ := parseClientRequest(FormatAnthropic, []byte(`{"model":"alias","max_tokens":4096,"thinking":{"type":"disabled"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`), "")
	wire, err := renderProtocolRequest(FormatOpenAIChat, disabled, "deepseek-v4-flash")
	request, _ := decodeProtocolObject(wire)
	if err != nil || request["reasoning_effort"] != "none" {
		t.Fatalf("disabled thinking lost precedence: %s / %v", wire, err)
	}
	fixed, _ := parseClientRequest(FormatAnthropic, []byte(`{"model":"alias","max_tokens":4096,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`), "")
	wire, err = renderProtocolRequest(FormatGemini, fixed, "gemini-2.5-flash")
	request, _ = decodeProtocolObject(wire)
	if err != nil || object(object(request["generationConfig"])["thinkingConfig"])["thinkingBudget"] != json.Number("1024") {
		t.Fatalf("fixed budget lost: %s / %v", wire, err)
	}
	wire, err = renderProtocolRequest(FormatOpenAIChat, fixed, "deepseek-v4-flash")
	request, _ = decodeProtocolObject(wire)
	if err != nil || request["reasoning_effort"] != "low" {
		t.Fatalf("fixed budget did not convert into effort: %s / %v", wire, err)
	}
	if _, err := renderProtocolRequest(FormatGemini, disabled, "gemini-2.5-pro"); err == nil {
		t.Fatal("always-on model received thinking-off")
	}
	tooSmall, _ := parseClientRequest(FormatAnthropic, []byte(`{"model":"alias","max_tokens":1024,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[]}`), "")
	if tooSmall.ConversionError == nil {
		t.Fatal("budget consumes the complete output allowance")
	}
	emptyConfig, _ := parseClientRequest(FormatAnthropic, []byte(`{"model":"alias","max_tokens":4096,"thinking":{},"messages":[]}`), "")
	if emptyConfig.ConversionError == nil {
		t.Fatal("thinking without a mode was silently dropped")
	}
	adaptive, _ := parseClientRequest(FormatGemini, []byte(`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"medium"}},"contents":[{"parts":[{"text":"hi"}]}]}`), "alias")
	wire, err = renderProtocolRequest(FormatAnthropic, adaptive, "claude-opus-4-8")
	request, _ = decodeProtocolObject(wire)
	if err != nil || object(request["thinking"])["type"] != "adaptive" || object(request["output_config"])["effort"] != "medium" {
		t.Fatalf("adaptive effort lost: %s / %v", wire, err)
	}
	if _, err := renderProtocolRequest(FormatGemini, &canonReq{ReasoningEffort: "low"}, "gemini-3.1-flash-lite-image"); err == nil {
		t.Fatal("unsupported image-model effort accepted")
	}
}

func TestOptimizationUsageTotalsAndPartialUpdates(t *testing.T) {
	usage := readUsage(map[string]any{"input_tokens": 10, "output_tokens": 2, "cache_read_input_tokens": 30, "cache_creation_input_tokens": 5}, FormatAnthropic)
	usage.merge(readUsage(map[string]any{"output_tokens": 0}, FormatAnthropic))
	chat := renderProtocolUsage(FormatOpenAIChat, usage)
	if chat["prompt_tokens"] != 45 || chat["completion_tokens"] != 0 || object(chat["prompt_tokens_details"])["cached_tokens"] != 30 || usage.Prompt != 10 {
		t.Fatalf("cache totals or existing billing counters changed: %+v / %+v", usage, chat)
	}
	gemini := readUsage(map[string]any{"promptTokenCount": 100, "candidatesTokenCount": 20, "cachedContentTokenCount": 30, "thoughtsTokenCount": 5, "toolUsePromptTokenCount": 2}, FormatGemini)
	anthropic := renderProtocolUsage(FormatAnthropic, gemini)
	if anthropic["input_tokens"] != 72 || anthropic["cache_read_input_tokens"] != 30 || anthropic["output_tokens"] != 25 || anthropic["cache_creation_input_tokens"] != nil {
		t.Fatalf("Gemini cache counted twice or write category invented: %+v", anthropic)
	}
	openai := readUsage(map[string]any{"prompt_tokens": 100, "completion_tokens": 25, "prompt_tokens_details": map[string]any{"cached_tokens": 30}, "completion_tokens_details": map[string]any{"reasoning_tokens": 5}}, FormatOpenAIChat)
	response := renderProtocolUsage(FormatOpenAIResponse, openai)
	if response["output_tokens"] != 25 || object(response["output_tokens_details"])["reasoning_tokens"] != 5 {
		t.Fatal("inclusive OpenAI reasoning added twice")
	}
	metadata := renderProtocolUsage(FormatGemini, gemini)
	if metadata["promptTokenCount"] != 100 || metadata["toolUsePromptTokenCount"] != 2 || metadata["totalTokenCount"] != 127 {
		t.Fatalf("Gemini category totals: %+v", metadata)
	}
}

func TestOptimizationStreamingUsageMatchesBufferedUsage(t *testing.T) {
	frames := auditFormatFrames(
		`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":30}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hello"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`,
		`{"type":"message_stop"}`,
	)
	result, err := collectProtocolSSE(strings.NewReader(frames), FormatAnthropic)
	if err != nil || result.Usage.CacheRead != 30 || renderProtocolUsage(FormatOpenAIResponse, result.Usage)["input_tokens"] != 40 {
		t.Fatalf("buffered details lost: %+v / %v", result, err)
	}
	info := relayInfo{}
	readSSEFrames(strings.NewReader(frames), func(payload string) (bool, error) {
		recordNativeEvent(FormatAnthropic, payload, &info)
		return false, nil
	})
	if !reflect.DeepEqual(renderProtocolUsage(FormatOpenAIResponse, result.Usage), renderProtocolUsage(FormatOpenAIResponse, info.protocolUsage())) {
		t.Fatalf("native and converted accounting disagree: %+v / %+v", result.Usage, info.protocolUsage())
	}
}

func TestOptimizationPausedTurnsAreNotConvertedToNormalCompletion(t *testing.T) {
	if _, err := parseProtocolResponse(FormatAnthropic, []byte(`{"content":[{"type":"text","text":"waiting"}],"stop_reason":"pause_turn"}`)); err == nil {
		t.Fatal("paused turn was converted into end_turn")
	}
	if normalizeFinish("model_context_window_exceeded") != "length" {
		t.Fatal("context exhaustion was reported as a normal finish")
	}
}
