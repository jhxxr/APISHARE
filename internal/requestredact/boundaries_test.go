package requestredact

import (
	"errors"
	"strings"
	"testing"
)

func TestRedactionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		format, body string
		reject       bool
	}{
		{"anthropic", `{"messages":[{"content":[{"type":"tool_use","input":{"email":"jane@example.com"}}]}]}`, true},
		{"gemini", `{"contents":[{"parts":[{"functionCall":{"args":{"email":"jane@example.com"}}}]}]}`, true},
		{"openai-chat", `{"tools":[{"function":{"parameters":{"enum":["jane@example.com"]}}}]}`, true},
		{"openai-responses", `{"input":[{"type":"function_call_output","output":{"email":"jane@example.com"}}]}`, false},
		{"gemini", `{"contents":[{"parts":[{"functionResponse":{"response":{"email":"jane@example.com"}}}]}]}`, false},
		{"openai-chat", `{"metadata":{"contact":"jane@example.com"},"user":"jane@example.com"}`, false},
	} {
		result, err := Process(tc.format, []byte(tc.body), PersonalConfig())
		if tc.reject {
			if err == nil {
				t.Fatalf("expected rejection: %s", tc.body)
			}
			continue
		}
		if err != nil || strings.Contains(string(result.Body), "jane@example.com") {
			t.Fatalf("unchecked text: %s %v", result.Body, err)
		}
	}
}

func TestStrictRejectsRemoteAndUnknownContent(t *testing.T) {
	cfg := PersonalConfig()
	cfg.Coverage = CoverageStrict
	for _, body := range []string{
		`{"input":"hello","previous_response_id":"resp_old"}`,
		`{"input":"hello","conversation":{"id":"conv_old"}}`,
		`{"contents":[{"parts":[{"inlineData":{"data":"opaque"}}]}]}`,
		`{"input":"hello","new_input":"opaque"}`,
		`{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`,
	} {
		format := "openai-responses"
		if strings.Contains(body, "contents") {
			format = "gemini"
		}
		_, err := Process(format, []byte(body), cfg)
		var problem *Error
		if !errors.As(err, &problem) || problem.Kind != "unsupported" {
			t.Fatalf("strict bypass: %s %v", body, err)
		}
	}
}

func TestReplacementConflictsAndObserveLimits(t *testing.T) {
	cfg := PersonalConfig()
	cfg.Replacements["email"] = "again@example.com"
	if _, err := Compile(cfg); err == nil {
		t.Fatal("sensitive replacement accepted")
	}
	cfg = PersonalConfig()
	cfg.Mode = ModeObserve
	body := []byte(`{"input":"` + strings.Repeat("x", (1<<20)+1) + `"}`)
	result, err := Process("openai-responses", body, cfg)
	if err != nil || result.Changed || len(result.Unchecked) == 0 {
		t.Fatalf("observe rejected or changed oversized field: %+v %v", result.Unchecked, err)
	}
	cfg.Mode = ModeOff
	text, hits, err := ProcessText("jane@example.com", cfg)
	if text != "jane@example.com" || len(hits) != 0 || err != nil {
		t.Fatal("disabled text scanner was active")
	}
}
