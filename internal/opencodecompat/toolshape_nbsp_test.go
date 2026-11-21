package opencodecompat

import (
	"reflect"
	"strings"
	"testing"
)

func TestClientToolDescriptionNBSP(t *testing.T) {
	for _, protocol := range []Protocol{Chat, Responses, Anthropic} {
		for _, v1 := range []bool{false, true} {
			t.Run(string(protocol)+map[bool]string{false: "/v2", true: "/v1"}[v1], func(t *testing.T) {
				schema := map[string]any{"description": "schema description unchanged", "properties": map[string]any{"arg": map[string]any{"type": "string", "description": "parameter description unchanged"}}}
				tool := map[string]any{"name": "client", "description": "Use this\u00a0tool\n  code example", "parameters": schema}
				if protocol == Anthropic {
					delete(tool, "parameters")
					tool["input_schema"] = schema
				}
				entry := tool
				if protocol == Chat {
					entry = map[string]any{"type": "function", "function": tool}
				} else if protocol == Responses {
					tool["type"] = "function"
				}
				payload := map[string]any{"tools": []any{entry}}
				renames, _ := renameAndCompleteTools(payload, protocol, v1, "")
				expected := "Use\u00a0this\u00a0tool\n\u00a0\u00a0code\u00a0example"
				if tool["description"] != expected {
					t.Fatalf("description = %q", tool["description"])
				}
				// The description transform itself is idempotent.
				if strings.ReplaceAll(tool["description"].(string), " ", "\u00a0") != expected {
					t.Fatal("not idempotent")
				}
				if schema["description"] != "schema description unchanged" || StringAt(schema, "properties", "arg", "description") != "parameter description unchanged" {
					t.Fatal("schema changed")
				}
				wire, _ := renames.Wire("client")
				if restored, ok := renames.Client(wire); !ok || restored != "client" {
					t.Fatal("name restoration changed")
				}
				appended := SliceAt(payload, "tools")[1:]
				if !reflect.DeepEqual(appended, officialToolset(protocol, v1, "")) {
					t.Fatal("appended official tools changed")
				}
			})
		}
	}
}

func TestLegacyToolDescriptionNBSP(t *testing.T) {
	for _, protocol := range []Protocol{Chat, Responses} {
		payload := map[string]any{"functions": []any{map[string]any{"name": "old", "description": "old function", "parameters": map[string]any{"description": "keep spaces"}}}}
		renameAndCompleteTools(payload, protocol, false, "")
		tool := SliceAt(payload, "tools")[0].(map[string]any)
		if protocol == Chat {
			tool = tool["function"].(map[string]any)
		}
		if tool["description"] != "old\u00a0function" || StringAt(tool, "parameters", "description") != "keep spaces" {
			t.Fatal("legacy conversion changed scope")
		}
	}
}
