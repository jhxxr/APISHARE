package opencodecompat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestPromptToolsAndCallerInstructions(t *testing.T) {
	for _, protocol := range []Protocol{Chat, Responses, Anthropic} {
		for _, legacy := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/legacy=%t", protocol, legacy), func(t *testing.T) {
				body := `{"model":"free","messages":[{"role":"system","content":"caller-system"},{"role":"developer","content":[{"type":"text","text":"caller-developer"}]},{"role":"user","content":"hello"}],"metadata":{"n":9007199254740993}}`
				if protocol == Responses {
					body = `{"model":"free","instructions":"caller-system","input":"hello","metadata":{"n":9007199254740993}}`
				} else if protocol == Anthropic {
					body = `{"model":"free","system":[{"type":"text","text":"caller-system","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hello"}],"metadata":{"n":9007199254740993}}`
				}
				payload, renames, err := Prepare([]byte(body), protocol, legacy)
				if err != nil || !renames.Empty() {
					t.Fatalf("prepare: %v %+v", err, renames)
				}
				want := 13
				if legacy {
					want = 11
				}
				if got := len(SliceAt(payload, "tools")); got != want {
					t.Fatalf("official tools = %d, want %d", got, want)
				}
				encoded, _ := Marshal(payload)
				for _, text := range []string{opencodePromptMarker, "<env>", "<available_skills>", "caller-system", `"n":9007199254740993`} {
					if !bytes.Contains(encoded, []byte(text)) {
						t.Errorf("request lost %q", text)
					}
				}
				if protocol == Chat && !bytes.Contains(encoded, []byte("caller-developer")) {
					t.Fatal("developer instructions were lost")
				}
				if protocol == Anthropic && !bytes.Contains(encoded, []byte(`"cache_control":{"type":"ephemeral"}`)) {
					t.Fatal("system cache annotation was lost")
				}
				promptPayload, err := Decode(encoded)
				if err != nil || injectOfficialPrompt(promptPayload, protocol, legacy) {
					t.Fatal("official prompt was injected twice")
				}
			})
		}
	}
}

func TestClientToolsCollisionsLegacyAndReferences(t *testing.T) {
	body := []byte(`{"model":"free","messages":[{"role":"assistant","function_call":{"name":"legacy","arguments":"{}"},"tool_calls":[{"function":{"name":"read.file","arguments":"{\"name\":\"read.file\"}"}}]}],"tools":[{"type":"function","function":{"name":"read.file","parameters":{"type":"object","properties":{"n":{"const":9007199254740993}}},"strict":true}},{"type":"function","function":{"name":"read/file","parameters":{"type":"object"}}}],"functions":[{"name":"legacy","description":"old function","parameters":{"type":"object"}}],"tool_choice":{"type":"function","function":{"name":"read.file"}},"function_call":{"name":"legacy"}}`)
	payload, renames, err := Prepare(body, Chat, false)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := renames.Wire("read.file")
	b, _ := renames.Wire("read/file")
	legacy, _ := renames.Wire("legacy")
	if a == b || !opaqueClientToolPattern.MatchString(a) || !opaqueClientToolPattern.MatchString(b) || !opaqueClientToolPattern.MatchString(legacy) {
		t.Fatalf("non-invertible aliases: %q %q %q", a, b, legacy)
	}
	for index, tool := range SliceAt(payload, "tools")[:3] {
		entry := tool.(map[string]any)
		if name := StringAt(entry, "function", "name"); name != []string{a, b, legacy}[index] {
			t.Fatalf("tool was renamed more than once: %s", name)
		}
	}
	encoded, _ := Marshal(payload)
	for _, text := range []string{fmt.Sprintf(`"function":{"name":%q}`, a), fmt.Sprintf(`"function_call":{"name":%q}`, legacy), `"const":9007199254740993`, `"strict":true`, `\"name\":\"read.file\"`} {
		if !bytes.Contains(encoded, []byte(text)) {
			t.Errorf("lost reference, schema or argument: %s", text)
		}
	}
	if payload["functions"] != nil || renames.Count() != 3 {
		t.Fatalf("legacy conversion did not retain three client tools: %+v", renames)
	}
	used := map[string]bool{}
	for _, name := range []string{strings.Repeat("x", 300), strings.Repeat("x", 301), "中文", "---"} {
		wire := ClientToolWireName(name, used)
		if len(wire) > 64 || !strings.HasPrefix(wire, ClientToolPrefix) {
			t.Fatalf("invalid sanitized alias: %q", wire)
		}
	}
}

func TestResponseNamesAreExactAndNumbersStayIntact(t *testing.T) {
	renames := NewToolRenames()
	renames.Add("read.file", "clint_read_file")
	for _, tc := range []struct {
		protocol Protocol
		body     string
	}{
		{Chat, `{"choices":[{"message":{"tool_calls":[{"function":{"name":"clint_read_file","arguments":"{\"name\":\"clint_read_file\"}"}},{"function":{"name":"read","arguments":"{}"}}]}}],"n":9007199254740993}`},
		{Responses, `{"output":[{"type":"function_call","name":"clint_read_file","arguments":"{\"name\":\"clint_read_file\"}"},{"type":"custom_tool_call","name":"clint_unknown"}],"n":9007199254740993}`},
		{Anthropic, `{"content":[{"type":"tool_use","name":"clint_read_file","input":{"name":"clint_read_file"}},{"type":"tool_use","name":"read"}],"n":9007199254740993}`},
	} {
		reader := NewResponseBody(io.NopCloser(strings.NewReader(tc.body)), tc.protocol, renames, false)
		got, err := io.ReadAll(reader)
		reader.Close()
		if err != nil || !bytes.Contains(got, []byte(`"name":"read.file"`)) || !bytes.Contains(got, []byte(`9007199254740993`)) {
			t.Fatalf("response restoration failed: %s %v", got, err)
		}
		if tc.protocol == Anthropic && !bytes.Contains(got, []byte(`"input":{"name":"clint_read_file"}`)) {
			t.Fatalf("tool input was rewritten: %s", got)
		}
		if tc.protocol == Responses && !bytes.Contains(got, []byte(`"name":"clint_unknown"`)) {
			t.Fatalf("unmapped tool name was rewritten: %s", got)
		}
	}
}

func TestChatStreamSplitNamesChoicesAndLegacy(t *testing.T) {
	renames := NewToolRenames()
	renames.Add("read.file", "clint_read_file")
	renames.Add("read", "clint_read")
	stream := "event: message\r\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"clint_\",\"arguments\":\"\"}}]}},{\"index\":1,\"delta\":{\"function_call\":{\"name\":\"clint_read\",\"arguments\":\"\"}}}]}\r\n\r\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"read_file\",\"arguments\":\"{}\"}}]}},{\"index\":1,\"delta\":{\"function_call\":{\"arguments\":\"{}\"}}}]}\n\n" +
		"data: [DONE]\n\n"
	reader := NewResponseBody(io.NopCloser(strings.NewReader(stream)), Chat, renames, true)
	got, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Contains(got, []byte(`"name":"read.file"`)) || !bytes.Contains(got, []byte(`"name":"read"`)) || bytes.Contains(got, []byte("clint_")) {
		t.Fatalf("fragment restoration failed: %s %v", got, err)
	}
	if !bytes.Contains(got, []byte("event: message\r\n")) || !bytes.HasSuffix(got, []byte("data: [DONE]\n\n")) {
		t.Fatalf("SSE framing was lost: %s", got)
	}
	// Every rewritten frame remains JSON, even when tool names are withheld.
	for _, line := range strings.Split(string(got), "\n") {
		if strings.HasPrefix(line, "data: {") && !json.Valid([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data: ")))) {
			t.Fatalf("invalid rewritten event: %s", line)
		}
	}
}
