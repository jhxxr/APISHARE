package agentpurge

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestBundledNeutralMatchesPinnedOriginal(t *testing.T) {
	// SHA-256 of the original neutral.md body at SourceRevision, after only
	// stripping its YAML front matter and surrounding whitespace.
	const want = "938e246419abfcb1a8ad4bb0862a5828f283c82ae1246beb1894368b7f41e25a"
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(Prompt()))); got != want {
		t.Fatalf("bundled project template differs from pinned upstream: got %s", got)
	}
	if PersonaID != "neutral" || PersonaName != "中性模板" {
		t.Fatal("bundled template metadata differs from the original project")
	}
}

func jsonValue(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestRewriteProtocolsAndModes(t *testing.T) {
	fixtures := map[string]string{
		"openai-chat":      `{"model":"alias","messages":[{"role":"system","content":[{"type":"text","text":"You are Claude.\nKeep tool instructions.","cache_control":{"type":"ephemeral"}}]},{"role":"developer","content":"You are Codex.\nKeep workspace rules."},{"role":"user","content":"Do the work."}],"tools":[{"type":"function","function":{"name":"run","parameters":{"const":9007199254740993}}}],"metadata":{"n":9007199254740993}}`,
		"openai-responses": `{"model":"alias","instructions":"You are Codex.\nKeep tool instructions.","input":[{"role":"developer","content":"You are Claude.\nKeep workspace rules."},{"type":"function_call_output","call_id":"keep","output":"original"},{"role":"user","content":"Do the work."}],"tools":[{"type":"function","name":"run","parameters":{"const":9007199254740993}}],"metadata":{"n":9007199254740993}}`,
		"anthropic":        `{"model":"alias","system":[{"type":"text","text":"You are Claude.\nKeep tool instructions.","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"keep","content":"original"}]}],"tools":[{"name":"run","input_schema":{"const":9007199254740993}}],"metadata":{"n":9007199254740993}}`,
		"gemini":           `{"systemInstruction":{"role":"system","parts":[{"text":"You are Gemini.\nKeep tool instructions.","custom":"keep"}]},"contents":[{"role":"user","parts":[{"functionResponse":{"id":"keep","name":"run","response":{"n":9007199254740993}}}]}],"tools":[{"functionDeclarations":[{"name":"run","parameters":{"const":9007199254740993}}]}],"metadata":{"n":9007199254740993}}`,
	}
	for protocol, fixture := range fixtures {
		for _, mode := range []string{"append", "replace", "strict"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				original := []byte(fixture)
				body, changed := Rewrite(original, protocol, mode)
				if !changed || string(original) != fixture {
					t.Fatal("request did not change or original bytes were mutated")
				}
				before, after := jsonValue(t, original), jsonValue(t, body)
				for _, field := range []string{"tools", "metadata", "model", "contents"} {
					if !reflect.DeepEqual(before[field], after[field]) {
						t.Fatalf("opaque field %s changed: %s", field, body)
					}
				}
				decoded := string(body)
				if !strings.Contains(decoded, "系统提示词 · 起步模板") || strings.Count(decoded, "系统提示词 · 起步模板") != 1 {
					t.Fatal("persona missing or repeated")
				}
				if mode == "append" && !strings.Contains(decoded, "You are") {
					t.Fatal("append removed caller instructions")
				}
				if mode != "append" && strings.Contains(decoded, "You are") {
					t.Fatalf("identity survived %s: %s", mode, body)
				}
				if mode == "strict" && strings.Contains(decoded, "Keep tool instructions") {
					t.Fatal("strict retained old instruction text")
				}
				if mode != "strict" && !strings.Contains(decoded, "Keep tool instructions") {
					t.Fatal("non-strict mode removed useful instructions")
				}
				if protocol == "anthropic" && mode != "strict" && !strings.Contains(decoded, "ephemeral") {
					t.Fatal("Anthropic cache control lost")
				}
				if protocol == "openai-responses" && !strings.Contains(decoded, `"call_id":"keep"`) {
					t.Fatal("Responses tool history lost")
				}
			})
		}
	}
}

func TestRewriteMissingInstructionAndFailOpen(t *testing.T) {
	for _, tc := range []struct{ protocol, body string }{
		{"gemini", `{"contents":[]}`},
		{"gemini", `{"system_instruction":{"parts":[]},"contents":[]}`},
		{"anthropic", `{"messages":[]}`},
		{"anthropic", `{"system":"original","messages":[]}`},
		{"openai-chat", `{"messages":[{"role":"user","content":"hello"}]}`},
		{"openai-responses", `{"input":"hello"}`},
	} {
		for _, mode := range []string{"append", "replace", "strict"} {
			body, changed := Rewrite([]byte(tc.body), tc.protocol, mode)
			if !changed || !bytes.Contains(body, []byte("系统提示词")) {
				t.Fatalf("missing instructions %s %s: %s", tc.protocol, mode, body)
			}
		}
	}
	for _, tc := range []struct{ protocol, body, mode string }{
		{"unknown", `{"input":"hi"}`, "append"},
		{"openai-chat", `{"messages":[]}`, "off"},
		{"openai-chat", `{"messages":[]}`, "invalid"},
		{"openai-chat", `{"messages":{}}`, "append"},
		{"openai-chat", `{"messages":[{"role":"system","content":42}]}`, "append"},
		{"openai-responses", `{"instructions":[]}`, "append"},
		{"gemini", `{"systemInstruction":42}`, "append"},
		{"anthropic", `null`, "append"},
		{"anthropic", `{} {}`, "append"},
	} {
		body, changed := Rewrite([]byte(tc.body), tc.protocol, tc.mode)
		if changed || string(body) != tc.body {
			t.Errorf("invalid/off request changed: %s", tc.body)
		}
	}
}
