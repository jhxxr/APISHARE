package requestredact

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestProcessRedactsProtocolTextAndPreservesStructure(t *testing.T) {
	cases := []struct {
		format string
		body   string
		want   int
		check  func(map[string]any) string
	}{
		{"openai-chat", `{"model":"gpt-4.1","messages":[{"role":"user","content":"Email jane@example.com, call 13812345678"}],"max_tokens":9007199254740993}`, 2, func(root map[string]any) string {
			return root["messages"].([]any)[0].(map[string]any)["content"].(string)
		}},
		{"openai-responses", `{"model":"gpt-4.1","instructions":"Contact jane@example.com","input":"Call 13812345678"}`, 2, func(root map[string]any) string { return root["instructions"].(string) + root["input"].(string) }},
		{"anthropic", `{"model":"claude","system":"Email jane@example.com","messages":[{"role":"user","content":[{"type":"text","text":"Call 13812345678"},{"type":"image","source":{"type":"base64","data":"opaque"}}]}]}`, 2, func(root map[string]any) string {
			return root["system"].(string) + root["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
		}},
		{"gemini", `{"contents":[{"parts":[{"text":"Email jane@example.com"}]}],"generationConfig":{"candidateCount":1}}`, 1, func(root map[string]any) string {
			return root["contents"].([]any)[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			result, err := Process(tc.format, []byte(tc.body), PersonalConfig())
			if err != nil {
				t.Fatal(err)
			}
			if !result.Changed || result.Matched != tc.want {
				t.Fatalf("expected %d replacements, got changed=%v matched=%d", tc.want, result.Changed, result.Matched)
			}
			var root map[string]any
			if err := json.Unmarshal(result.Body, &root); err != nil {
				t.Fatal(err)
			}
			text := tc.check(root)
			if strings.Contains(text, "jane@example.com") || strings.Contains(text, "13812345678") {
				t.Fatalf("sensitive text remains: %s", text)
			}
			if !strings.Contains(text, "[EMAIL]") {
				t.Fatalf("expected typed placeholders: %s", text)
			}
			if tc.want == 2 && !strings.Contains(text, "[PHONE]") {
				t.Fatalf("expected phone placeholder: %s", text)
			}
			if !strings.Contains(string(result.Body), "9007199254740993") && tc.format == "openai-chat" {
				t.Fatal("large JSON integer precision was lost")
			}
			if tc.format == "anthropic" && len(result.Unchecked) == 0 {
				t.Fatal("opaque image should be reported as unchecked")
			}
		})
	}
}

func TestProcessToolArgumentsAndCoverage(t *testing.T) {
	body := []byte(`{"model":"gpt-4.1","messages":[{"role":"assistant","tool_calls":[{"type":"function","function":{"arguments":"{\"email\":\"jane@example.com\"}"}}]}]}`)
	if _, err := Process("openai-chat", body, PersonalConfig()); err == nil {
		t.Fatal("sensitive tool argument must be rejected")
	} else {
		var redactErr *Error
		if !errors.As(err, &redactErr) || redactErr.Kind != "tool_argument" {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	strict := PersonalConfig()
	strict.Coverage = CoverageStrict
	unsupported := []byte(`{"model":"gpt-4.1","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"opaque"}}]}]}`)
	if _, err := Process("openai-chat", unsupported, strict); err == nil {
		t.Fatal("strict mode must reject opaque content")
	}
}

func TestProcessObserveAndDuplicateJSONKeys(t *testing.T) {
	cfg := PersonalConfig()
	cfg.Mode = ModeObserve
	body := []byte(`{"model":"gpt-4.1","messages":[{"role":"user","content":"jane@example.com"}]}`)
	result, err := Process("openai-chat", body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || string(result.Body) != string(body) || result.Matched != 1 {
		t.Fatalf("observe mode must report without changing body: %+v", result)
	}
	if _, err := Process("openai-chat", []byte(`{"model":"first","model":"second"}`), PersonalConfig()); err == nil {
		t.Fatal("duplicate JSON keys must be rejected")
	}
}

func TestBuiltinValidatorsReduceFalsePositives(t *testing.T) {
	cfg := PersonalConfig()
	cfg.Categories = []string{"id_card", "bank_card", "ipv4"}
	cfg.Replacements = map[string]string{"id_card": "[ID]", "bank_card": "[CARD]", "ipv4": "[IP]"}
	text, findings, err := ProcessText("id 11010519491231002X and bad 110105194912310021; cards 4111111111111111 and 1234567890123456; versions 1.2.3.4, ip 8.8.8.8", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "11010519491231002X") || strings.Contains(text, "4111111111111111") || strings.Contains(text, "8.8.8.8") {
		t.Fatalf("valid sensitive values were not redacted: %s", text)
	}
	if !strings.Contains(text, "110105194912310021") || !strings.Contains(text, "1234567890123456") || !strings.Contains(text, "1.2.3.4") {
		t.Fatalf("invalid candidates were changed: %s", text)
	}
	if len(findings) != 3 {
		t.Fatalf("expected three categories, got %+v", findings)
	}
}

func TestCompileRejectsEmptyMatchingRule(t *testing.T) {
	cfg := PersonalConfig()
	cfg.Rules = []Rule{{ID: "empty", Pattern: "a*", Replacement: "x"}}
	if _, err := Compile(cfg); err == nil {
		t.Fatal("empty-matching regex must be rejected")
	}
}
