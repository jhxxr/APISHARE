package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestResponsesCustomAliasesKeepDeclaredOwnership(t *testing.T) {
	tools := map[string]responseTool{
		"apply_patch":     {Type: "custom", Name: "apply_patch"},
		"functions__wait": {Type: "function", Namespace: "functions", Name: "wait"},
	}
	block := responseBlock{Kind: "tool", Call: canonCall{"call_patch", "functions__apply_patch", `{"input":"patch"}`}}
	restored := restoreResponseTool(block, tools)
	if restored.ToolType != "custom" || restored.Call.Name != "apply_patch" || restored.Namespace != "" {
		t.Fatalf("custom alias was not restored: %+v", restored)
	}
	tools["functions__apply_patch"] = responseTool{Type: "function", Name: "functions__apply_patch"}
	restored = restoreResponseTool(block, tools)
	if restored.ToolType != "function" || restored.Call.Name != "functions__apply_patch" {
		t.Fatal("custom alias hijacked an explicitly declared function")
	}
	delete(tools, "functions__apply_patch")
	tools["a__exec"] = responseTool{Type: "custom", Name: "exec", Namespace: "a"}
	tools["b__exec"] = responseTool{Type: "custom", Name: "exec", Namespace: "b"}
	if _, _, ok := resolveResponseTool("exec", tools); ok {
		t.Fatal("ambiguous bare custom tool name was guessed")
	}
	delete(tools, "b__exec")
	if _, meta, ok := resolveResponseTool("exec", tools); !ok || meta.Namespace != "a" {
		t.Fatal("unambiguous missing namespace was not restored")
	}
}

func TestResponsesCustomHistoryAliasAndWrapperInstructions(t *testing.T) {
	body := `{"model":"alias","tools":[{"type":"custom","name":"apply_patch","description":"Do not wrap the patch in JSON."},{"type":"namespace","name":"functions","tools":[{"type":"function","name":"wait","parameters":{"type":"object"}}]}],"input":[{"role":"user","content":"edit"},{"type":"custom_tool_call","call_id":"c","name":"functions__apply_patch","input":"patch text"},{"type":"custom_tool_call_output","call_id":"c","output":"applied"}]}`
	cr := parseCodexFixture(t, body)
	if cr.Messages[1].Calls[0].Name != "apply_patch" {
		t.Fatal("custom history kept the model's wrong namespace alias")
	}
	if !strings.Contains(cr.Tools[0].Description, "argument envelope") || !strings.Contains(cr.Tools[0].Description, "Do not wrap the patch in JSON.") {
		t.Fatal("wrapper instructions did not clarify the retained raw tool instructions")
	}
}

func TestResponsesHistoryReordersResultsAndRemovesInterruptedCalls(t *testing.T) {
	body := `{"model":"alias","input":[{"role":"user","content":"work"},{"type":"function_call","call_id":"a","name":"read","arguments":"{}"},{"type":"function_call","call_id":"b","name":"read","arguments":"{}"},{"type":"function_call","call_id":"c","name":"read","arguments":"{}"},{"role":"user","content":"approval notice"},{"type":"function_call_output","call_id":"b","output":"B"},{"type":"function_call_output","call_id":"a","output":"partial A"},{"type":"function_call_output","call_id":"a","output":"final A"},{"type":"function_call_output","call_id":"orphan","output":"unmatched"},{"role":"user","content":"continue"},{"type":"function_call","call_id":"unfinished","name":"read","arguments":"{}"}]}`
	cr := parseCodexFixture(t, body)
	if len(cr.Messages) != 6 || len(cr.Messages[1].Calls) != 2 {
		t.Fatalf("incorrect repaired history: %+v", cr.Messages)
	}
	if cr.Messages[2].ToolID != "a" || cr.Messages[2].Text != "final A" || cr.Messages[3].ToolID != "b" || cr.Messages[4].Text != "approval notice" || cr.Messages[5].Text != "continue" {
		t.Fatal("tool results were not placed directly after their call batch")
	}
	for _, format := range []clientFormat{FormatOpenAIChat, FormatAnthropic, FormatGemini} {
		wire, err := renderProtocolRequest(format, cr, "mapped")
		if err != nil || strings.Contains(string(wire), "unfinished") || strings.Contains(string(wire), "unmatched") {
			t.Fatalf("invalid repaired %s history: %s / %v", format, wire, err)
		}
	}
}

func TestResponsesHistoryDoesNotConfuseReusedCallIDs(t *testing.T) {
	body := `{"model":"alias","input":[{"role":"user","content":"first"},{"type":"function_call","call_id":"same","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"same","output":"first result"},{"role":"user","content":"second"},{"type":"function_call","call_id":"same","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"same","output":"second result"}]}`
	cr := parseCodexFixture(t, body)
	if len(cr.Messages) != 6 || cr.Messages[2].Text != "first result" || cr.Messages[5].Text != "second result" {
		t.Fatal("a later tool result was attached to an earlier call with the same ID")
	}
}

func TestResponsesTruncatedHistoricalArgumentsRecover(t *testing.T) {
	body := `{"model":"alias","input":[{"role":"user","content":"old"},{"type":"function_call","call_id":"broken","name":"read","arguments":"{\"path\":"},{"type":"function_call_output","call_id":"broken","output":"interrupted"},{"role":"user","content":"try again"}]}`
	cr := parseCodexFixture(t, body)
	if len(cr.Messages) != 2 || cr.Messages[1].Text != "try again" {
		t.Fatal("a partial historical call poisoned the next user turn")
	}
}

func TestResponsesReasoningPrefersCachedSpanThenSameTurnFallback(t *testing.T) {
	resetResponseReasoningForTest(t)
	scope := responseReplayScope{1, 2, "alias", "deepseek"}
	cacheResponseReasoning(scope, "rs_new", "new reasoning")
	messages := []canonMsg{
		{Role: "user", Text: "first"},
		{Role: "assistant", Reasoning: "old reasoning", ReasoningIDs: []string{"rs_old"}},
		{Role: "tool", ToolID: "a"},
		{Role: "assistant", ReasoningIDs: []string{"rs_new"}},
		{Role: "tool", ToolID: "b"},
		{Role: "assistant", ReasoningIDs: []string{"rs_missing"}},
		{Role: "user", Text: "second"},
		{Role: "assistant", ReasoningIDs: []string{"rs_missing"}},
	}
	got := replayResponseReasoning(messages, scope)
	if got[3].Reasoning != "new reasoning" || got[5].Reasoning != "new reasoning" || got[7].Reasoning != "" {
		t.Fatalf("incorrect replay spans: %+v", got)
	}
	if lookupResponseReasoning(scope, "rs_missing") != "" {
		t.Fatal("fallback reasoning was misattributed to an unknown reasoning ID")
	}
	if messages[3].Reasoning != "" {
		t.Fatal("replay mutated the source transcript")
	}
}

func TestResponsesReasoningContentFallbackAndSlidingRetention(t *testing.T) {
	resetResponseReasoningForTest(t)
	body := `{"model":"alias","input":[{"role":"user","content":"work"},{"type":"reasoning","id":"rs_content","summary":[],"content":[{"type":"reasoning_text","text":"content plan"}]},{"role":"assistant","content":"answer"}]}`
	cr := parseCodexFixture(t, body)
	if cr.Messages[1].Reasoning != "content plan" {
		t.Fatal("reasoning content fallback was lost")
	}
	scope := responseReplayScope{1, 2, "alias", "deepseek"}
	cacheResponseReasoning(scope, "rs_content", "content plan")
	responseReasoningCache.Lock()
	e := responseReasoningCache.Items[responseReasoningKey{scope, "rs_content"}]
	entry := e.Value.(responseReasoningEntry)
	entry.Expires = time.Now().Add(time.Minute)
	e.Value = entry
	responseReasoningCache.Unlock()
	if lookupResponseReasoning(scope, "rs_content") != "content plan" {
		t.Fatal("reasoning lookup failed")
	}
	responseReasoningCache.Lock()
	expires := e.Value.(responseReasoningEntry).Expires
	responseReasoningCache.Unlock()
	if time.Until(expires) < 6*24*time.Hour {
		t.Fatal("active reasoning cache lifetime was not refreshed")
	}
	wire, err := renderProtocolRequest(FormatOpenAIChat, cr, "mapped")
	if err != nil {
		t.Fatal(err)
	}
	var j map[string]any
	if json.Unmarshal(wire, &j) != nil || str(object(array(j["messages"])[1])["reasoning_content"]) != "content plan" {
		t.Fatal("reasoning did not reach Chat history")
	}
}
