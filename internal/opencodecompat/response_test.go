package opencodecompat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func restoredChatNames(t *testing.T, stream []byte) map[string]string {
	t.Helper()
	names := map[string]string{}
	for _, line := range strings.Split(string(stream), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "[DONE]" {
			continue
		}
		payload, err := Decode([]byte(raw))
		if err != nil {
			t.Fatalf("invalid restored frame: %s / %v", line, err)
		}
		for ci, rawChoice := range SliceAt(payload, "choices") {
			choice := rawChoice.(map[string]any)
			choiceID := indexOr(choice, ci)
			delta, _ := choice["delta"].(map[string]any)
			for ti, rawTool := range SliceAt(delta, "tool_calls") {
				tool := rawTool.(map[string]any)
				names[choiceID+":"+indexOr(tool, ti)] += StringAt(tool, "function", "name")
			}
			if function, ok := delta["function_call"].(map[string]any); ok {
				names[choiceID+":legacy"] += StringAt(function, "name")
			}
		}
	}
	return names
}

func TestChatStreamNamesWaitForCompletion(t *testing.T) {
	renames := NewToolRenames()
	renames.Add("read.file", "clint_read_file")
	renames.Add("read", "clint_read")
	renames.Add("repeat", "clint_clint_")
	for _, tc := range []struct {
		name      string
		fragments []string
		want      string
	}{
		{"unknown extension", []string{"clint_read_file", "_extra"}, "clint_read_file_extra"},
		{"short alias", []string{"clint_read"}, "read"},
		{"unique exact alias", []string{"clint_read_file"}, "read.file"},
		{"long alias", []string{"clint_read", "_file"}, "read.file"},
		{"repeated fragments", []string{"clint_", "clint_"}, "repeat"},
		{"unknown repeated fragments", []string{"clint_read", "clint_read"}, "clint_readclint_read"},
		{"unknown prefix", []string{"clint_"}, "clint_"},
		{"official name", []string{"read"}, "read"},
	} {
		for _, terminal := range []string{"arguments", "finish", "done"} {
			t.Run(tc.name+"/"+terminal, func(t *testing.T) {
				var stream strings.Builder
				for index, fragment := range tc.fragments {
					args := ""
					if terminal == "arguments" && index == len(tc.fragments)-1 {
						args = "{}"
					}
					payload := map[string]any{"id": "chatcmpl-x", "object": "chat.completion.chunk", "model": "free", "created": 42, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call-x", "function": map[string]any{"name": fragment, "arguments": args}}}}}}}
					encoded, _ := Marshal(payload)
					stream.WriteString("data: " + string(encoded) + "\r\n\r\n")
				}
				if terminal == "finish" {
					stream.WriteString("event: message\r\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"completion_tokens\":2}}\r\n\r\n")
				}
				stream.WriteString("data: [DONE]\r\n\r\n")
				body := NewResponseBody(io.NopCloser(strings.NewReader(stream.String())), Chat, renames, true)
				got, err := io.ReadAll(body)
				body.Close()
				if err != nil || restoredChatNames(t, got)["0:0"] != tc.want {
					t.Fatalf("name lost or misresolved: want %q got %s / %v", tc.want, got, err)
				}
				if !bytes.HasSuffix(got, []byte("data: [DONE]\r\n\r\n")) || !bytes.Contains(got, []byte(`"id":"call-x"`)) {
					t.Fatalf("terminal framing or call ID changed: %s", got)
				}
				if terminal == "finish" && (!bytes.Contains(got, []byte("event: message\r\n")) || !bytes.Contains(got, []byte(`"completion_tokens":2`))) {
					t.Fatalf("finish metadata lost: %s", got)
				}
				if terminal == "done" {
					if !bytes.Contains(got, []byte(`"object":"chat.completion.chunk"`)) || !bytes.Contains(got, []byte(`"created":42`)) {
						t.Fatalf("synthetic delta lost envelope: %s", got)
					}
				}
			})
		}
	}
}

func TestChatStreamTerminalFlushSeparatesChoicesCallsAndLegacy(t *testing.T) {
	renames := NewToolRenames()
	renames.Add("read.file", "clint_read_file")
	renames.Add("read", "clint_read")
	stream := "data: {\"choices\":[{\"index\":2,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"clint_read\"}},{\"index\":1,\"function\":{\"name\":\"clint_read_file\"}}]}},{\"index\":1,\"delta\":{\"function_call\":{\"name\":\"clint_read\"}}}]}\n\n" +
		"data: {\"choices\":[{\"index\":2,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" + "data: [DONE]\n\n"
	body := NewResponseBody(io.NopCloser(strings.NewReader(stream)), Chat, renames, true)
	got, err := io.ReadAll(body)
	body.Close()
	names := restoredChatNames(t, got)
	if err != nil || len(names) != 3 || names["2:0"] != "read" || names["2:1"] != "read.file" || names["1:legacy"] != "read" {
		t.Fatalf("terminal calls mixed: %+v / %v\n%s", names, err, got)
	}
}

func TestChatStreamNameStateIsBoundedAndEOFDoesNotComplete(t *testing.T) {
	renames := NewToolRenames()
	renames.Add("read", "clint_read")
	for _, tc := range []struct {
		name, stream string
		want         error
	}{
		{"pending EOF", "data: {\"choices\":[{\"delta\":{\"function_call\":{\"name\":\"clint_read\"}}}]}\n\n", io.ErrUnexpectedEOF},
		{"unterminated DONE", "data: {\"choices\":[{\"delta\":{\"function_call\":{\"name\":\"clint_read\"}}}]}\n\ndata: [DONE]", io.ErrUnexpectedEOF},
		{"unterminated finish", "data: {\"choices\":[{\"delta\":{\"function_call\":{\"name\":\"clint_read\"}}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n", io.ErrUnexpectedEOF},
		{"large name", "data: {\"choices\":[{\"delta\":{\"function_call\":{\"name\":\"" + strings.Repeat("x", responseNameLimit+1) + "\"}}}]}\n\n", nil},
		{"name after arguments", "data: {\"choices\":[{\"delta\":{\"function_call\":{\"name\":\"clint_read\",\"arguments\":\"{}\"}}}]}\n\ndata: {\"choices\":[{\"delta\":{\"function_call\":{\"name\":\"_extra\"}}}]}\n\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := NewResponseBody(io.NopCloser(strings.NewReader(tc.stream)), Chat, renames, true)
			got, err := io.ReadAll(body)
			body.Close()
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || bytes.Contains(got, []byte("[DONE]")) {
				t.Fatalf("invalid stream accepted or terminal invented: %s / %v", got, err)
			}
		})
	}
	body := NewResponseBody(io.NopCloser(strings.NewReader("")), Chat, renames, true).(*responseBody)
	for i := 0; i <= responseNameStates; i++ {
		body.restoreName(nameKey{"0", fmt.Sprint(i)}, map[string]any{"name": "clint_"}, 0, i)
	}
	if body.err == nil || len(body.names) > responseNameStates {
		t.Fatalf("tool state unbounded: %d / %v", len(body.names), body.err)
	}
	body.Close()
}

func TestChatStreamCarriageReturnFraming(t *testing.T) {
	renames := NewToolRenames()
	renames.Add("read", "clint_read")
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"function_call\":{\"name\":\"clint_read\"}}}]}\r\rdata: [DONE]\r\r"
	body := NewResponseBody(io.NopCloser(strings.NewReader(stream)), Chat, renames, true)
	got, err := io.ReadAll(body)
	body.Close()
	if err != nil || bytes.Contains(got, []byte("\n")) || !bytes.HasSuffix(got, []byte("data: [DONE]\r\r")) || restoredChatNames(t, bytes.ReplaceAll(got, []byte("\r"), []byte("\n")))["0:legacy"] != "read" {
		t.Fatalf("CR-only stream rejected or reframed: %s / %v", got, err)
	}
}

func TestChatStreamWhitespaceFieldDoesNotDispatchEvent(t *testing.T) {
	renames := NewToolRenames()
	renames.Add("read", "clint_read")
	stream := "data: {\"choices\":\n \ndata: [{\"index\":0,\"delta\":{\"function_call\":{\"name\":\"clint_read\",\"arguments\":\"{}\"}}}]}\n\ndata: [DONE]\n\n"
	body := NewResponseBody(io.NopCloser(strings.NewReader(stream)), Chat, renames, true)
	got, err := io.ReadAll(body)
	body.Close()
	if err != nil || restoredChatNames(t, got)["0:legacy"] != "read" || bytes.Contains(got, []byte("clint_")) {
		t.Fatalf("space-only field dispatched prematurely: %s / %v", got, err)
	}
}

func TestStructuredToolSSEPreservesInputsAndExactNames(t *testing.T) {
	renames := NewToolRenames()
	renames.Add("read.file", "clint_read_file")
	for _, tc := range []struct {
		protocol Protocol
		event    string
	}{
		{Responses, `{"type":"response.output_item.added","item":{"type":"function_call","name":"clint_read_file","arguments":"{\"name\":\"clint_read_file\"}"},"output":[{"type":"custom_tool_call","name":"clint_unknown"}]}`},
		{Anthropic, `{"type":"content_block_start","content_block":{"type":"tool_use","name":"clint_read_file","input":{"type":"tool_use","name":"clint_read_file"}},"content":[{"type":"tool_use","name":"clint_unknown"}]}`},
	} {
		stream := "event: named\r\nid: event-x\r\ndata: " + tc.event + "\r\n\r\n"
		body := NewResponseBody(io.NopCloser(strings.NewReader(stream)), tc.protocol, renames, true)
		got, err := io.ReadAll(body)
		body.Close()
		if err != nil || !bytes.Contains(got, []byte(`"name":"read.file"`)) || !bytes.Contains(got, []byte(`"name":"clint_unknown"`)) || !bytes.Contains(got, []byte("event: named\r\nid: event-x\r\n")) {
			t.Fatalf("structured restoration failed: %s / %v", got, err)
		}
		line := strings.Split(string(got), "data: ")[1]
		payload, err := Decode([]byte(strings.TrimSpace(line)))
		if err != nil {
			t.Fatal(err)
		}
		if tc.protocol == Anthropic && StringAt(payload, "content_block", "input", "name") != "clint_read_file" {
			t.Fatalf("arbitrary input renamed: %s", got)
		}
		if tc.protocol == Responses {
			var args map[string]any
			if json.Unmarshal([]byte(StringAt(payload, "item", "arguments")), &args) != nil || args["name"] != "clint_read_file" {
				t.Fatalf("arbitrary arguments renamed: %s", got)
			}
		}
	}
}
