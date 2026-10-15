package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

type nativeJSONEvent struct {
	name string
	body map[string]any
}

// Prepare and validate the entire native envelope before committing headers.
// Opaque blocks travel intact; only fields with documented deltas are split.
func nativeJSONEvents(format clientFormat, body []byte, wantUsage bool) ([]nativeJSONEvent, error) {
	root, err := decodeProtocolObject(body)
	if err != nil {
		return nil, err
	}
	if err := nativeProtocolResponseError(format, body); err != nil {
		return nil, err
	}
	var events []nativeJSONEvent
	add := func(name string, body map[string]any) {
		events = append(events, nativeJSONEvent{name, body})
	}
	switch format {
	case FormatOpenAIChat:
		choices := array(root["choices"])
		if len(choices) == 0 || len(choices) > 65536 {
			return nil, fmt.Errorf("invalid native Chat choices")
		}
		base := copyProtocolObject(root)
		delete(base, "usage")
		base["object"] = "chat.completion.chunk"
		var content, endings []any
		for index, raw := range choices {
			choice := object(raw)
			message := object(choice["message"])
			if message == nil || str(choice["finish_reason"]) == "" {
				return nil, fmt.Errorf("native Chat choice is incomplete")
			}
			delta := copyProtocolObject(message)
			if calls := array(message["tool_calls"]); len(calls) > 0 {
				indexed := make([]any, 0, len(calls))
				for toolIndex, call := range calls {
					item := copyProtocolObject(object(call))
					if str(item["type"]) == "function" {
						if _, err := decodeProtocolObject([]byte(str(object(item["function"])["arguments"]))); err != nil {
							return nil, fmt.Errorf("invalid native Chat tool arguments")
						}
					}
					item["index"] = toolIndex
					indexed = append(indexed, item)
				}
				delta["tool_calls"] = indexed
			}
			part := copyProtocolObject(choice)
			delete(part, "message")
			if part["index"] == nil {
				part["index"] = index
			}
			part["delta"], part["finish_reason"] = delta, nil
			content = append(content, part)
			endings = append(endings, map[string]any{"index": part["index"], "delta": map[string]any{}, "finish_reason": choice["finish_reason"]})
		}
		chunk := copyProtocolObject(base)
		chunk["choices"] = content
		add("", chunk)
		chunk = copyProtocolObject(base)
		chunk["choices"] = endings
		add("", chunk)
		if wantUsage && root["usage"] != nil {
			chunk = copyProtocolObject(base)
			chunk["choices"], chunk["usage"] = []any{}, root["usage"]
			add("", chunk)
		}
	case FormatAnthropic:
		blocks, ok := root["content"].([]any)
		if !ok || len(blocks) > 65536 || str(root["stop_reason"]) == "" {
			return nil, fmt.Errorf("native Messages response is incomplete")
		}
		initial := copyProtocolObject(root)
		initial["content"], initial["stop_reason"], initial["stop_sequence"] = []any{}, nil, nil
		initialUsage := copyProtocolObject(object(root["usage"]))
		initialUsage["output_tokens"] = 0
		initial["usage"] = initialUsage
		add("message_start", map[string]any{"type": "message_start", "message": initial})
		for index, raw := range blocks {
			block := object(raw)
			if str(block["type"]) == "" {
				return nil, fmt.Errorf("invalid native Messages content block")
			}
			seed := copyProtocolObject(block)
			var deltas []map[string]any
			switch str(block["type"]) {
			case "text":
				seed["text"] = ""
				deltas = append(deltas, map[string]any{"type": "text_delta", "text": block["text"]})
				if citations := array(block["citations"]); len(citations) > 0 {
					seed["citations"] = []any{}
					for _, citation := range citations {
						deltas = append(deltas, map[string]any{"type": "citations_delta", "citation": citation})
					}
				}
			case "thinking":
				seed["thinking"], seed["signature"] = "", ""
				deltas = append(deltas, map[string]any{"type": "thinking_delta", "thinking": str(block["thinking"])})
				if signature := str(block["signature"]); signature != "" {
					deltas = append(deltas, map[string]any{"type": "signature_delta", "signature": signature})
				}
			case "tool_use", "server_tool_use":
				if object(block["input"]) == nil {
					return nil, fmt.Errorf("native Messages tool input is not an object")
				}
				seed["input"] = map[string]any{}
				deltas = append(deltas, map[string]any{"type": "input_json_delta", "partial_json": jsonText(block["input"])})
			}
			add("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": seed})
			for _, delta := range deltas {
				add("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": delta})
			}
			add("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		}
		add("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": root["stop_reason"], "stop_sequence": root["stop_sequence"]}, "usage": root["usage"]})
		add("message_stop", map[string]any{"type": "message_stop"})
	case FormatGemini:
		if !geminiPromptBlocked(root) {
			if len(array(root["candidates"])) == 0 {
				return nil, fmt.Errorf("native Gemini response is missing candidates")
			}
			for _, raw := range array(root["candidates"]) {
				if str(object(raw)["finishReason"]) == "" {
					return nil, fmt.Errorf("native Gemini candidate is incomplete")
				}
			}
		}
		add("", root)
	case FormatOpenAIResponse:
		status := str(root["status"])
		if status != "completed" && status != "incomplete" {
			return nil, fmt.Errorf("native Responses response is not terminal")
		}
		items, ok := root["output"].([]any)
		if !ok || len(items) > 65536 {
			return nil, fmt.Errorf("native Responses response is missing output")
		}
		initial := copyProtocolObject(root)
		initial["status"], initial["output"], initial["usage"] = "in_progress", []any{}, nil
		initial["completed_at"], initial["incomplete_details"] = nil, nil
		sequence := 0
		responseEvent := func(typ string, data map[string]any) {
			data["type"], data["sequence_number"] = typ, sequence
			sequence++
			add(typ, data)
		}
		responseEvent("response.created", map[string]any{"response": initial})
		responseEvent("response.in_progress", map[string]any{"response": initial})
		for index, raw := range items {
			item := object(raw)
			if str(item["type"]) == "" {
				return nil, fmt.Errorf("invalid native Responses output item")
			}
			seed := copyProtocolObject(item)
			if item["status"] != nil {
				seed["status"] = "in_progress"
			}
			switch str(item["type"]) {
			case "message":
				seed["content"] = []any{}
			case "reasoning":
				seed["summary"] = []any{}
			case "function_call":
				seed["arguments"] = ""
			case "custom_tool_call":
				seed["input"] = ""
			}
			responseEvent("response.output_item.added", map[string]any{"output_index": index, "item": seed})
			if str(item["type"]) == "message" {
				for partIndex, rawPart := range array(item["content"]) {
					part := object(rawPart)
					partSeed := copyProtocolObject(part)
					if part["type"] == "output_text" {
						partSeed["text"], partSeed["annotations"] = "", []any{}
					} else if part["type"] == "refusal" {
						partSeed["refusal"] = ""
					}
					partEvent := func(typ string, data map[string]any) {
						data["item_id"], data["output_index"], data["content_index"] = item["id"], index, partIndex
						responseEvent(typ, data)
					}
					partEvent("response.content_part.added", map[string]any{"part": partSeed})
					switch str(part["type"]) {
					case "output_text":
						partEvent("response.output_text.delta", map[string]any{"delta": part["text"]})
						partEvent("response.output_text.done", map[string]any{"text": part["text"]})
					case "refusal":
						partEvent("response.refusal.delta", map[string]any{"delta": part["refusal"]})
						partEvent("response.refusal.done", map[string]any{"refusal": part["refusal"]})
					}
					partEvent("response.content_part.done", map[string]any{"part": part})
				}
			} else if item["type"] == "reasoning" {
				for summaryIndex, rawSummary := range array(item["summary"]) {
					part := object(rawSummary)
					partSeed := copyProtocolObject(part)
					partSeed["text"] = ""
					summaryEvent := func(typ string, data map[string]any) {
						data["item_id"], data["output_index"], data["summary_index"] = item["id"], index, summaryIndex
						responseEvent(typ, data)
					}
					summaryEvent("response.reasoning_summary_part.added", map[string]any{"part": partSeed})
					summaryEvent("response.reasoning_summary_text.delta", map[string]any{"delta": part["text"]})
					summaryEvent("response.reasoning_summary_text.done", map[string]any{"text": part["text"]})
					summaryEvent("response.reasoning_summary_part.done", map[string]any{"part": part})
				}
			} else if item["type"] == "function_call" || item["type"] == "custom_tool_call" {
				field, stem := "arguments", "response.function_call_arguments"
				if item["type"] == "custom_tool_call" {
					field, stem = "input", "response.custom_tool_call_input"
				} else if _, err := decodeProtocolObject([]byte(str(item[field]))); err != nil {
					return nil, fmt.Errorf("invalid native Responses tool arguments")
				}
				responseEvent(stem+".delta", map[string]any{"item_id": item["id"], "output_index": index, "delta": item[field]})
				responseEvent(stem+".done", map[string]any{"item_id": item["id"], "output_index": index, field: item[field]})
			}
			responseEvent("response.output_item.done", map[string]any{"output_index": index, "item": item})
		}
		responseEvent("response."+status, map[string]any{"response": root})
	default:
		return nil, fmt.Errorf("unsupported native JSON stream format")
	}
	for _, event := range events {
		encoded, err := json.Marshal(event.body)
		if err != nil || len(encoded) > maxBodyBytes {
			return nil, fmt.Errorf("native SSE event is invalid or too large")
		}
	}
	return events, nil
}

func relayNativeJSONStream(c *gin.Context, format clientFormat, body []byte, wantUsage bool) error {
	events, err := nativeJSONEvents(format, body, wantUsage)
	if err != nil {
		return err
	}
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	commitRelayResponse(c)
	for _, event := range events {
		if c.Request != nil && c.Request.Context().Err() != nil {
			return c.Request.Context().Err()
		}
		frame := "data: " + jsonText(event.body) + "\n\n"
		if event.name != "" {
			frame = "event: " + event.name + "\n" + frame
		}
		if _, err := c.Writer.WriteString(frame); err != nil {
			return fmt.Errorf("%w: %w", errDownstreamWrite, err)
		}
		c.Writer.Flush()
	}
	if format == FormatOpenAIChat {
		if _, err := c.Writer.WriteString("data: [DONE]\n\n"); err != nil {
			return fmt.Errorf("%w: %w", errDownstreamWrite, err)
		}
		c.Writer.Flush()
	}
	return nil
}
