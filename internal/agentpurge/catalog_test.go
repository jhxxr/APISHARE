package agentpurge

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestCompleteBundledCatalog(t *testing.T) {
	want := []string{"neutral", "ctf", "dafeiyu", "eni-chatgpt-policy-54", "eni-deepseek-flash", "eni-gemini-36", "eni-gemini-argon-4", "eni-glm-53", "eni-grok-47", "eni-kimi-k3", "eni-mimo-26", "eni-minimax-m31", "eni-opus-5", "eni-sonnet-46", "l1b3rt4s-deepseek", "l1b3rt4s-openai", "mistral-large-4", "nemotron-3-ultra", "qwen-37", "xiaomajiang", "zcode-agents-53"}
	items := Personas()
	if len(items) != len(want) {
		t.Fatalf("bundled %d templates, want all %d", len(items), len(want))
	}
	for _, id := range want {
		item, ok := Lookup(id)
		if !ok || item.Name == "" || item.Text == "" || item.Characters <= 0 || len(item.SHA256) != 64 || item.Source.URL == "" {
			t.Errorf("incomplete original template: %s", id)
		}
		if strings.HasPrefix(item.Text, "---\n") {
			t.Errorf("front matter leaked into prompt: %s", id)
		}
		if id != PersonaID && (item.Source.Repository != "ZiChuanLan/some-magic" || item.Source.License != "not_declared") {
			t.Errorf("template provenance incorrectly attributed: %s", id)
		}
	}
	items[0].Text = "changed"
	if original, _ := Lookup(items[0].ID); original.Text == "changed" {
		t.Fatal("a caller mutated the compiled catalog")
	}
	for _, id := range []string{"../neutral", "personas/xiaomajiang.md", "unknown-persona"} {
		if _, ok := Lookup(id); ok {
			t.Errorf("unknown/path-like ID accepted: %s", id)
		}
		body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
		if after, changed := RewriteWithPersona(body, "openai-chat", "strict", id); changed || string(after) != string(body) {
			t.Errorf("unknown ID silently used a different template: %s", id)
		}
	}
}

func TestEveryTemplateEveryProtocolAndMode(t *testing.T) {
	fixtures := map[string]string{
		"openai-chat":      `{"model":"alias","messages":[{"role":"system","content":"Old instructions"},{"role":"user","content":"Hello"}],"tools":[{"n":9007199254740993}]}`,
		"openai-responses": `{"model":"alias","instructions":"Old instructions","input":"Hello","tools":[{"n":9007199254740993}]}`,
		"anthropic":        `{"model":"alias","system":"Old instructions","messages":[{"role":"user","content":"Hello"}],"tools":[{"n":9007199254740993}]}`,
		"gemini":           `{"systemInstruction":{"parts":[{"text":"Old instructions"}]},"contents":[{"role":"user","parts":[{"text":"Hello"}]}],"tools":[{"n":9007199254740993}]}`,
	}
	for _, item := range Personas() {
		for protocol, fixture := range fixtures {
			for _, mode := range []string{"append", "replace", "strict"} {
				t.Run(item.ID+"/"+protocol+"/"+mode, func(t *testing.T) {
					after, changed := RewriteWithPersona([]byte(fixture), protocol, mode, item.ID)
					if !changed || !json.Valid(after) {
						t.Fatal("template rewrite failed")
					}
					before, decoded := jsonValue(t, []byte(fixture)), jsonValue(t, after)
					if !reflect.DeepEqual(before["tools"], decoded["tools"]) {
						t.Fatal("opaque tool schema was modified")
					}
					var text string
					switch protocol {
					case "openai-chat":
						text = decoded["messages"].([]any)[0].(map[string]any)["content"].(string)
					case "openai-responses":
						text = decoded["instructions"].(string)
					case "anthropic":
						if mode == "strict" {
							text = decoded["system"].([]any)[0].(map[string]any)["text"].(string)
						} else {
							text = decoded["system"].(string)
						}
					case "gemini":
						text = decoded["systemInstruction"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string)
					}
					if !strings.HasPrefix(text, item.Text) {
						t.Fatal("selected template body was changed or replaced with the default")
					}
					if mode == "strict" && text != item.Text {
						t.Fatal("strict contains extra instruction text")
					}
				})
			}
		}
	}
}
