package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

func nativeSSETerminal(format clientFormat, payload string) (bool, error) {
	if payload == "[DONE]" {
		return format == FormatOpenAIChat, nil
	}
	var event map[string]any
	if json.Unmarshal([]byte(payload), &event) != nil {
		return false, fmt.Errorf("invalid upstream SSE JSON")
	}
	response := object(event["response"])
	if event["error"] != nil || str(event["type"]) == "error" || str(event["type"]) == "response.failed" || response["error"] != nil || str(response["status"]) == "failed" {
		return false, fmt.Errorf("upstream stream returned an error")
	}
	switch format {
	case FormatOpenAIResponse:
		typ := str(event["type"])
		return typ == "response.completed" || typ == "response.incomplete" || typ == "response.done", nil
	case FormatAnthropic:
		return str(event["type"]) == "message_stop", nil
	case FormatOpenAIChat:
		for _, raw := range array(event["choices"]) {
			choice := object(raw)
			if intFromAny(choice["index"]) == 0 && str(choice["finish_reason"]) != "" {
				return true, nil
			}
		}
	case FormatGemini:
		if geminiPromptBlocked(event) {
			// A blocked native response has no candidates by design. Preserve
			// its feedback instead of treating it as a truncated stream.
			return true, nil
		}
		for _, raw := range array(event["candidates"]) {
			candidate := object(raw)
			if intFromAny(candidate["index"]) != 0 {
				continue
			}
			if err := geminiFinishError(str(candidate["finishReason"])); err != nil {
				return false, err
			}
			if str(candidate["finishReason"]) != "" {
				return true, nil
			}
		}
	}
	return false, nil
}

// Restore only fields missing from a native terminal snapshot. Keep provider
// IDs, encrypted state and hosted output items instead of normalizing them.
func supplementNativeItem(final, accumulated map[string]any) map[string]any {
	if final == nil {
		return accumulated
	}
	if str(final["type"]) != "" && str(accumulated["type"]) != "" && final["type"] != accumulated["type"] {
		return final
	}
	out := copyProtocolObject(final)
	for _, field := range []string{"id", "call_id", "name", "namespace", "arguments", "input", "text", "refusal"} {
		text, isString := out[field].(string)
		if out[field] == nil || (isString && text == "") {
			if accumulated[field] != nil {
				out[field] = accumulated[field]
			}
		}
	}
	for _, field := range []string{"content", "summary"} {
		parts, previous := array(out[field]), array(accumulated[field])
		if len(parts) == 0 && len(previous) > 0 {
			out[field] = previous
		} else if len(parts) > 0 {
			parts = append([]any(nil), parts...)
			for index, raw := range parts {
				part := object(raw)
				if index < len(previous) && part != nil {
					part = copyProtocolObject(part)
					for _, textField := range []string{"text", "refusal"} {
						if strings.TrimSpace(str(part[textField])) == "" && str(object(previous[index])[textField]) != "" {
							part[textField] = object(previous[index])[textField]
						}
					}
					parts[index] = part
				}
			}
			out[field] = parts
		}
	}
	return out
}

func collectNativeResponsesSSE(reader io.Reader, info *relayInfo) ([]byte, error) {
	const limit = 64 << 20
	wire := &io.LimitedReader{R: reader, N: limit + 1}
	items := map[int]map[string]any{}
	var terminal map[string]any
	var baseResponse map[string]any
	err := readSSEFrames(wire, func(payload string) (bool, error) {
		done, err := nativeSSETerminal(FormatOpenAIResponse, payload)
		if err != nil {
			return false, err
		}
		recordNativeEvent(FormatOpenAIResponse, payload, info)
		event, err := decodeProtocolObject([]byte(payload))
		if err != nil {
			return false, err
		}
		typ := str(event["type"])
		index := intFromAny(event["output_index"])
		if index < 0 || index > 65536 {
			return false, fmt.Errorf("invalid upstream output index")
		}
		item := items[index]
		if item == nil {
			item = map[string]any{}
		}
		switch typ {
		case "response.created", "response.in_progress":
			if response := object(event["response"]); response != nil {
				baseResponse = response
			}
		case "response.output_item.added", "response.output_item.done":
			items[index] = supplementNativeItem(object(event["item"]), item)
		case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
			field, kind := "arguments", "function_call"
			if typ == "response.custom_tool_call_input.delta" {
				field, kind = "input", "custom_tool_call"
			}
			if item["type"] == nil {
				item["type"] = kind
			}
			item[field] = str(item[field]) + str(event["delta"])
			items[index] = item
		case "response.function_call_arguments.done", "response.custom_tool_call_input.done":
			field, kind := "arguments", "function_call"
			if typ == "response.custom_tool_call_input.done" {
				field, kind = "input", "custom_tool_call"
			}
			if item["type"] == nil {
				item["type"] = kind
			}
			if str(event[field]) != "" {
				item[field] = event[field]
			}
			for _, name := range []string{"item_id", "call_id", "name"} {
				if str(event[name]) != "" {
					if name == "item_id" {
						item["id"] = event[name]
					} else {
						item[name] = event[name]
					}
				}
			}
			items[index] = item
		case "response.output_text.delta", "response.output_text.done", "response.refusal.delta", "response.refusal.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.content_part.added", "response.content_part.done":
			field, textField, kind, partType := "content", "text", "message", "output_text"
			partIndex := intFromAny(event["content_index"])
			if strings.HasPrefix(typ, "response.reasoning_summary_") {
				field, kind, partType = "summary", "reasoning", "summary_text"
				partIndex = intFromAny(event["summary_index"])
			}
			if strings.HasPrefix(typ, "response.refusal.") {
				textField, partType = "refusal", "refusal"
			}
			if partIndex < 0 || partIndex > 65536 {
				return false, fmt.Errorf("invalid upstream content index")
			}
			parts := append([]any(nil), array(item[field])...)
			for len(parts) <= partIndex {
				parts = append(parts, nil)
			}
			part := object(parts[partIndex])
			if part == nil {
				part = map[string]any{"type": partType, textField: ""}
			}
			if strings.HasPrefix(typ, "response.content_part.") {
				part = supplementNativeItem(object(event["part"]), part)
			} else if strings.HasSuffix(typ, ".delta") {
				part[textField] = str(part[textField]) + str(event["delta"])
			} else if str(event[textField]) != "" {
				part[textField] = event[textField]
			}
			parts[partIndex] = part
			if item["type"] == nil {
				item["type"] = kind
			}
			if kind == "message" && item["role"] == nil {
				item["role"] = "assistant"
			}
			item[field], items[index] = parts, item
		}
		if done {
			terminal = object(event["response"])
			if terminal == nil {
				terminal = copyProtocolObject(event)
				delete(terminal, "type")
				delete(terminal, "sequence_number")
			}
		}
		return done, nil
	})
	if err != nil {
		return nil, err
	}
	if wire.N <= 1 {
		return nil, fmt.Errorf("upstream buffered response too large")
	}
	if terminal == nil {
		return nil, io.ErrUnexpectedEOF
	}
	terminal = copyProtocolObject(terminal)
	for _, field := range []string{"id", "object", "model", "created_at"} {
		if terminal[field] == nil && baseResponse[field] != nil {
			terminal[field] = baseResponse[field]
		}
	}
	output := append([]any(nil), array(terminal["output"])...)
	used := map[int]bool{}
	byID := map[string]int{}
	for index, item := range items {
		if id := str(item["id"]); id != "" {
			byID[id] = index
		}
	}
	for index, raw := range output {
		item := object(raw)
		match := -1
		if id := str(item["id"]); id != "" {
			if candidate, exists := byID[id]; exists {
				match = candidate
			}
		}
		if match < 0 && items[index] != nil && str(item["type"]) == str(items[index]["type"]) {
			match = index
		}
		if match >= 0 {
			output[index] = supplementNativeItem(item, items[match])
			used[match] = true
		}
	}
	var indexes []int
	for index := range items {
		if !used[index] && str(items[index]["type"]) != "" {
			indexes = append(indexes, index)
		}
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		output = append(output, items[index])
	}
	terminal["output"] = output
	return json.Marshal(terminal)
}
