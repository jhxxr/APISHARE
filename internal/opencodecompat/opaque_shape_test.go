package opencodecompat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func TestPrepareOpaqueClientToolsAndProtocolReferences(t *testing.T) {
	for _, tc := range []struct {
		protocol Protocol
		body     string
	}{
		{Chat, `{"model":"free","tools":[{"type":"function","function":{"name":"ReportFindings","description":"Keep this description","parameters":{"type":"object","properties":{"n":{"const":9007199254740993}}},"strict":true}},{"type":"function","function":{"name":"ScheduleWakeup","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"ScheduleWakeup"}},"messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"ReportFindings","arguments":"{\"name\":\"ScheduleWakeup\"}"}}]},{"role":"tool","tool_call_id":"c1","content":"{\"name\":\"ReportFindings\"}"}]}`},
		{Responses, `{"model":"free","tools":[{"type":"function","name":"ReportFindings","description":"Keep this description","parameters":{"type":"object","properties":{"n":{"const":9007199254740993}}},"strict":true},{"type":"function","name":"ScheduleWakeup","parameters":{"type":"object"}}],"tool_choice":{"type":"allowed_tools","tools":[{"type":"function","name":"ScheduleWakeup"}]},"input":[{"type":"function_call","call_id":"c1","name":"ReportFindings","arguments":"{\"name\":\"ScheduleWakeup\"}"},{"type":"function_call_output","call_id":"c1","output":"{\"name\":\"ReportFindings\"}"}]}`},
		{Anthropic, `{"model":"free","tools":[{"name":"ReportFindings","description":"Keep this description","input_schema":{"type":"object","properties":{"n":{"const":9007199254740993}}},"cache_control":{"type":"ephemeral"}},{"name":"ScheduleWakeup","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"ScheduleWakeup"},"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"ReportFindings","input":{"name":"ScheduleWakeup"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"{\"name\":\"ReportFindings\"}"}]}]}`},
	} {
		for _, legacy := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/legacy=%t", tc.protocol, legacy), func(t *testing.T) {
				payload, renames, err := Prepare([]byte(tc.body), tc.protocol, legacy)
				if err != nil {
					t.Fatal(err)
				}
				report, reportOK := renames.Wire("ReportFindings")
				wakeup, wakeupOK := renames.Wire("ScheduleWakeup")
				if !reportOK || !wakeupOK || !opaqueClientToolPattern.MatchString(report) || !opaqueClientToolPattern.MatchString(wakeup) {
					t.Fatalf("client identity leaked through aliases: %q %q", report, wakeup)
				}
				tools := SliceAt(payload, "tools")
				wantTools := 15
				if legacy {
					wantTools = 13
				}
				if len(tools) != wantTools {
					t.Fatalf("official set was changed: %d tools", len(tools))
				}
				for index, expected := range []string{report, wakeup} {
					tool := tools[index].(map[string]any)
					name := StringAt(tool, "name")
					if tc.protocol == Chat {
						name = StringAt(tool, "function", "name")
					}
					if name != expected {
						t.Fatalf("definition and rename map disagree: %q != %q", name, expected)
					}
				}
				encoded, _ := Marshal(payload)
				if !bytes.Contains(encoded, []byte(`"const":9007199254740993`)) || !bytes.Contains(encoded, []byte("Keep this description")) {
					t.Fatalf("description or schema changed: %s", encoded)
				}
				choice := payload["tool_choice"].(map[string]any)
				var historyName, argumentName, result string
				switch tc.protocol {
				case Chat:
					if StringAt(choice, "function", "name") != wakeup {
						t.Fatal("Chat named tool choice was not rewritten")
					}
					message := SliceAt(payload, "messages")[1].(map[string]any) // Official system prompt is prepended.
					call := SliceAt(message, "tool_calls")[0].(map[string]any)
					historyName = StringAt(call, "function", "name")
					argumentName = StringAt(call, "function", "arguments")
					result = StringAt(SliceAt(payload, "messages")[2].(map[string]any), "content")
				case Responses:
					if StringAt(SliceAt(choice, "tools")[0].(map[string]any), "name") != wakeup {
						t.Fatal("Responses allowed tools were not rewritten")
					}
					item := SliceAt(payload, "input")[0].(map[string]any)
					historyName, argumentName = StringAt(item, "name"), StringAt(item, "arguments")
					result = StringAt(SliceAt(payload, "input")[1].(map[string]any), "output")
				case Anthropic:
					if StringAt(choice, "name") != wakeup {
						t.Fatal("Messages named tool choice was not rewritten")
					}
					message := SliceAt(payload, "messages")[0].(map[string]any)
					block := SliceAt(message, "content")[0].(map[string]any)
					historyName = StringAt(block, "name")
					arguments, _ := json.Marshal(block["input"])
					argumentName = string(arguments)
					message = SliceAt(payload, "messages")[1].(map[string]any)
					result = StringAt(SliceAt(message, "content")[0].(map[string]any), "content")
				}
				if historyName != report || argumentName != `{"name":"ScheduleWakeup"}` || result != `{"name":"ReportFindings"}` {
					t.Fatalf("history reference or arbitrary tool data changed: name=%q arguments=%q result=%q", historyName, argumentName, result)
				}
			})
		}
	}
}
