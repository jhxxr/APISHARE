package main

import (
	"encoding/json"
	"testing"
)

func TestExplicitModelOptionsPreserveContextAndNativeExtensions(t *testing.T) {
	resetModelLimitsForTest(t)
	const raw = `{"model":"gpt-5.2","max_tokens":20000,"metadata":{"keep":"yes"},"messages":[{"role":"user","content":"hello"}]}`
	cr, err := parseClientRequest(FormatOpenAIChat, []byte(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	task := &relayTask{model: "gpt-5.2", requestedModel: "gpt-5.2(high,ctx=4096)", canon: cr}
	up := &Upstream{Type: "openai", OpenAIProtocol: "chat"}
	body := adaptModelRequestBody([]byte(raw), FormatOpenAIChat, up, "gpt-5.2", task, nil)
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result["reasoning_effort"] != "high" || object(result["metadata"])["keep"] != "yes" || cr.MaxTokens != 20000 || cr.ReasoningEffort != "" {
		t.Fatalf("explicit options lost data or mutated the original request: %s %+v", body, cr)
	}
	if output := readJSONInt(result["max_tokens"]); output <= 0 || estimateRequestContextTokens(cr)+output+256 > 4096 {
		t.Fatalf("explicit context window ignored: %s", body)
	}
	// The same explicit context limit must hold when converting to fixed-budget thinking.
	converted := *cr
	adaptModelRequestCanon(&converted, FormatAnthropic, &Upstream{Type: "anthropic"}, "claude-sonnet-4-5", task, nil)
	if converted.ThinkingBudget == nil || *converted.ThinkingBudget >= converted.MaxTokens || estimateRequestContextTokens(&converted)+converted.MaxTokens+256 > 4096 {
		t.Fatalf("converted thinking exceeds explicit context/output budget: %+v", converted)
	}
	if _, err := renderProtocolRequest(FormatAnthropic, &converted, "claude-sonnet-4-5", ""); err != nil {
		t.Fatalf("explicit model options no longer convert: %v", err)
	}
}

func TestModelContextOptionsDoNotInventThinking(t *testing.T) {
	cr := &canonReq{ReasoningEffort: "high", Messages: []canonMsg{{Role: "user", Text: "still broken, design a complex system"}}}
	for _, name := range []string{"custom-model(ctx=4096)", "custom-model(keep)", "gpt-5.2(auto)"} {
		plan := resolveModelThinking(&Upstream{Type: "openai"}, "custom-model", name, cr)
		copy := *cr
		applyModelThinking(&copy, plan)
		if copy.ReasoningEffort != "high" || copy.ThinkingBudget != nil || copy.ThinkingMode != "" {
			t.Fatalf("%s inferred or stripped thinking without an explicit override: %+v", name, copy)
		}
	}
}
