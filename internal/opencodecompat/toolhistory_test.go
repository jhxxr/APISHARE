package opencodecompat

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
)

func testClientTools(protocol Protocol, names ...string) []any {
	var tools []any
	for _, name := range names {
		tool := map[string]any{"name": name, "description": "client tool"}
		if protocol == Anthropic {
			tool["input_schema"] = map[string]any{"type": "object"}
		} else {
			tool["parameters"] = map[string]any{"type": "object"}
			if protocol == Chat {
				tool = map[string]any{"type": "function", "function": tool}
			} else {
				tool["type"] = "function"
			}
		}
		tools = append(tools, tool)
	}
	return tools
}

func TestOfficialToolNamesRestoreOnlyDeclaredEquivalents(t *testing.T) {
	for _, protocol := range []Protocol{Chat, Responses, Anthropic} {
		for _, v1 := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/v1=%t", protocol, v1), func(t *testing.T) {
				request := map[string]any{"tools": testClientTools(protocol, "Bash", "Read", "Edit", "Write", "Glob", "Grep", "Agent", "AskUserQuestion", "TodoWrite", "WebFetch", "WebSearch", "Skill")}
				body, _ := Marshal(request)
				_, renames, err := Prepare(body, protocol, v1)
				if err != nil {
					t.Fatal(err)
				}
				pairs := map[string]string{"read": "Read", "edit": "Edit", "write": "Write", "glob": "Glob", "grep": "Grep", "webfetch": "WebFetch", "websearch": "WebSearch", "skill": "Skill", "execute": "execute", "unknown": "unknown", "clit_bad": "clit_bad", "clnt_bad": "clnt_bad", "mcp_unknown": "mcp_unknown"}
				if v1 {
					pairs["bash"], pairs["task"], pairs["todowrite"] = "Bash", "Agent", "TodoWrite"
				} else {
					pairs["shell"], pairs["subagent"], pairs["question"] = "Bash", "Agent", "AskUserQuestion"
				}
				for wire, want := range pairs {
					if got := renames.Restore(wire); got != want {
						t.Errorf("restore %q = %q, want %q", wire, got, want)
					}
					var response string
					switch protocol {
					case Chat:
						response = fmt.Sprintf(`{"choices":[{"message":{"tool_calls":[{"function":{"name":%q,"arguments":"{}"}}]}}]}`, wire)
					case Responses:
						response = fmt.Sprintf(`{"output":[{"type":"function_call","name":%q,"arguments":"{}"}]}`, wire)
					case Anthropic:
						response = fmt.Sprintf(`{"content":[{"type":"tool_use","name":%q,"input":{"name":"shell"}}]}`, wire)
					}
					for _, sse := range []bool{false, true} {
						data := response
						if sse {
							data = "data: " + data + "\n\n"
						}
						reader := NewResponseBody(io.NopCloser(strings.NewReader(data)), protocol, renames, sse)
						got, err := io.ReadAll(reader)
						reader.Close()
						if err != nil || !bytes.Contains(got, []byte(fmt.Sprintf(`"name":%q`, want))) {
							t.Fatalf("%q response sse=%t: %s / %v", wire, sse, got, err)
						}
					}
				}
			})
		}
	}
}

func TestToolRenamesResolveUnambiguousSpellings(t *testing.T) {
	var renames ToolRenames // The documented zero value must also support Add.
	for _, name := range []string{"Read", "mcp_Read", "Todo_Write", "a-b", "a_b", "Edit", "EDIT", "---"} {
		renames.Add(name, ClientToolWireName(name, nil))
	}
	for wire, want := range map[string]string{"read": "Read", "todo.write": "Todo_Write", "TODOWRITE": "Todo_Write", "mcp_Read": "mcp_Read", "mcp_TODOWRITE": "Todo_Write", "ab": "ab", "edit": "edit", "a_b": "a_b", "...": "...", "mcp_deadbeef1234": "mcp_deadbeef1234"} {
		if got := renames.Restore(wire); got != want {
			t.Errorf("resolve %q = %q, want %q", wire, got, want)
		}
	}
}

func TestToolHistoryDropsGhostCallsAndPairedResults(t *testing.T) {
	for _, protocol := range []Protocol{Chat, Responses, Anthropic} {
		for _, v1 := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/v1=%t", protocol, v1), func(t *testing.T) {
				official := "shell"
				if v1 {
					official = "bash"
				}
				var history string
				switch protocol {
				case Chat:
					history = fmt.Sprintf(`{"messages":[{"role":"assistant","content":"keep text","tool_calls":[{"id":"good","function":{"name":%q,"arguments":"{\"name\":\"ghost\"}"}},{"id":"bad","function":{"name":"ghost","arguments":"{}"}}]},{"role":"tool","tool_call_id":"bad","content":"drop-result"},{"role":"tool","tool_call_id":"good","content":"keep-result"},{"role":"assistant","content":null,"tool_calls":[{"id":"empty","function":{"name":"execute"}}]},{"role":"tool","tool_call_id":"empty","content":"drop-empty"}],"tool_choice":{"type":"function","function":{"name":"ghost"}}}`, official)
				case Responses:
					history = fmt.Sprintf(`{"input":[{"role":"user","content":"keep text"},{"type":"function_call","call_id":"good","name":%q,"arguments":"{\"name\":\"ghost\"}"},{"type":"custom_tool_call","call_id":"bad","name":"ghost","input":"raw"},{"type":"function_call_output","call_id":"good","output":"keep-result"},{"type":"custom_tool_call_output","call_id":"bad","output":"drop-result"},{"type":"function_call","call_id":"empty","name":"execute"},{"type":"function_call_output","call_id":"empty","output":"drop-empty"}],"tool_choice":{"type":"function","name":"ghost"}}`, official)
				case Anthropic:
					history = fmt.Sprintf(`{"messages":[{"role":"assistant","content":[{"type":"text","text":"keep text"},{"type":"tool_use","id":"good","name":%q,"input":{"name":"ghost"}},{"type":"tool_use","id":"bad","name":"ghost","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"bad","content":"drop-result"},{"type":"tool_result","tool_use_id":"good","content":"keep-result"}]},{"role":"assistant","content":[{"type":"tool_use","id":"empty","name":"execute","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"empty","content":"drop-empty"}]}],"tool_choice":{"type":"tool","name":"ghost"}}`, official)
				}
				request, _ := Decode([]byte(history))
				request["tools"] = testClientTools(protocol, "Bash")
				body, _ := Marshal(request)
				payload, renames, err := Prepare(body, protocol, v1)
				if err != nil {
					t.Fatal(err)
				}
				field := "messages"
				if protocol == Responses {
					field = "input"
				}
				got, _ := Marshal(payload[field])
				wire, _ := renames.Wire("Bash")
				for _, want := range []string{"keep text", "keep-result", wire, "ghost"} {
					if !bytes.Contains(got, []byte(want)) {
						t.Errorf("history lost %q: %s", want, got)
					}
				}
				for _, removed := range []string{`"bad"`, `"empty"`, "drop-result", "drop-empty", `"name":"execute"`} {
					if bytes.Contains(got, []byte(removed)) {
						t.Errorf("history retained %q: %s", removed, got)
					}
				}
				if payload["tool_choice"] != nil {
					t.Fatal("ghost tool_choice survived")
				}
			})
		}
	}
}

func TestToolHistoryPreservesResponsesStringAndLegacyFunctions(t *testing.T) {
	request := []byte(`{"input":"keep scalar input","tools":[{"type":"function","name":"Read"}]}`)
	payload, _, err := Prepare(request, Responses, false)
	if err != nil || payload["input"] != "keep scalar input" {
		t.Fatalf("scalar input changed: %+v / %v", payload, err)
	}
	request = []byte(`{"functions":[{"name":"Bash"}],"function_call":{"name":"ghost"},"messages":[{"role":"assistant","function_call":{"name":"ghost","arguments":"{}"}},{"role":"function","name":"ghost","content":"drop-result"},{"role":"assistant","function_call":{"name":"bash","arguments":"{}"}},{"role":"function","name":"bash","content":"keep-result"}]}`)
	payload, renames, err := Prepare(request, Chat, true)
	if err != nil {
		t.Fatal(err)
	}
	history, _ := Marshal(payload["messages"])
	wire, _ := renames.Wire("Bash")
	if bytes.Contains(history, []byte("ghost")) || bytes.Contains(history, []byte("drop-result")) || !bytes.Contains(history, []byte(wire)) || !bytes.Contains(history, []byte("keep-result")) || payload["function_call"] != nil {
		t.Fatalf("legacy history not cleaned: %s", history)
	}
}
