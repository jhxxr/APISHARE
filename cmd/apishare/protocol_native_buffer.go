package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// Buffer native SSE without passing through the portable representation: that
// would discard signatures, citations, hosted tools and provider usage details.
func collectNativeProtocolSSE(reader io.Reader, format clientFormat, info *relayInfo) ([]byte, error) {
	if format == FormatOpenAIResponse {
		return collectNativeResponsesSSE(reader, info)
	}
	const limit = 64 << 20
	wire := &io.LimitedReader{R: reader, N: limit + 1}
	b := nativeProtocolBuffer{format: format, root: map[string]any{}, entries: map[int]map[string]any{}, tools: map[int]map[int]map[string]any{}, arguments: map[int]string{}}
	err := readSSEFrames(wire, func(payload string) (bool, error) {
		if payload == "[DONE]" {
			b.terminal = b.terminal || format == FormatOpenAIChat
			return true, nil
		}
		event, err := decodeProtocolObject([]byte(payload))
		if err != nil {
			return false, err
		}
		done, err := nativeSSETerminalObject(format, event)
		if err != nil {
			return false, err
		}
		// Buffered for a non-streaming client: nothing is delivered incrementally,
		// so no first-token time is recorded and TPS uses the full duration.
		recordNativeEventObject(format, event, info)
		b.terminal = b.terminal || done
		switch format {
		case FormatOpenAIChat:
			err = b.chat(event)
		case FormatAnthropic:
			err = b.anthropic(event)
		case FormatGemini:
			err = b.gemini(event)
		default:
			err = fmt.Errorf("unsupported native stream format")
		}
		// Chat and Gemini may publish final usage after finish_reason.
		return done && format == FormatAnthropic, err
	})
	if err != nil {
		return nil, err
	}
	if wire.N <= 1 {
		return nil, fmt.Errorf("upstream buffered response too large")
	}
	if !b.terminal {
		return nil, io.ErrUnexpectedEOF
	}
	return b.finish()
}

type nativeProtocolBuffer struct {
	format    clientFormat
	root      map[string]any
	entries   map[int]map[string]any
	tools     map[int]map[int]map[string]any
	arguments map[int]string
	closed    map[int]bool
	terminal  bool
}

func nativeStreamIndex(value any) (int, error) {
	index := intFromAny(value)
	if index < 0 || index >= 65536 {
		return 0, fmt.Errorf("invalid upstream content index")
	}
	return index, nil
}

func mergeNativeFields(dst, src map[string]any) {
	for key, value := range src {
		dst[key] = value
	}
}

func mergeNativeUsage(dst map[string]any, field string, value any, format clientFormat) {
	if update := object(value); update != nil {
		usage := copyProtocolObject(object(dst[field]))
		_, hasInput := protocolTokenCount(update["input_tokens"])
		if format == FormatAnthropic && hasInput && update["prompt_tokens"] == nil && update["prompt_cache_miss_tokens"] == nil {
			delete(usage, "prompt_tokens")
			delete(usage, "prompt_cache_miss_tokens")
		}
		_, hasPrompt := protocolTokenCount(update["prompt_tokens"])
		_, hasOutput := protocolTokenCount(update["completion_tokens"])
		if format == FormatOpenAIChat && (hasPrompt || hasOutput) && update["total_tokens"] == nil {
			delete(usage, "total_tokens")
		}
		mergeNativeFields(usage, update)
		dst[field] = usage
	}
}

func appendNativeStrings(dst, src map[string]any, fields ...string) {
	for _, field := range fields {
		if value, ok := src[field].(string); ok {
			dst[field] = str(dst[field]) + value
		}
	}
}

func orderedNativeEntries(entries map[int]map[string]any) []any {
	indexes := make([]int, 0, len(entries))
	for index := range entries {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	out := make([]any, 0, len(indexes))
	for _, index := range indexes {
		out = append(out, entries[index])
	}
	return out
}

func (b *nativeProtocolBuffer) chat(event map[string]any) error {
	for field, value := range event {
		if field != "choices" && field != "usage" {
			b.root[field] = value
		}
	}
	mergeNativeUsage(b.root, "usage", event["usage"], b.format)
	for _, raw := range array(event["choices"]) {
		choice := object(raw)
		index, err := nativeStreamIndex(choice["index"])
		if err != nil {
			return err
		}
		entry := b.entries[index]
		if entry == nil {
			entry = map[string]any{"index": index, "message": map[string]any{"role": "assistant", "content": nil}, "finish_reason": nil}
			b.entries[index], b.tools[index] = entry, map[int]map[string]any{}
		}
		message := object(entry["message"])
		delta := object(choice["delta"])
		for field, value := range choice {
			if field != "delta" && field != "finish_reason" {
				entry[field] = value
			}
		}
		if choice["finish_reason"] != nil {
			entry["finish_reason"] = choice["finish_reason"]
		}
		previous := copyProtocolObject(message)
		mergeNativeFields(message, delta)
		for _, field := range []string{"content", "refusal", "reasoning_content", "reasoning"} {
			if value, ok := delta[field].(string); ok {
				message[field] = str(previous[field]) + value
			} else if delta[field] == nil && previous[field] != nil {
				message[field] = previous[field]
			}
		}
		if function := object(delta["function_call"]); function != nil {
			full := copyProtocolObject(object(previous["function_call"]))
			appendNativeStrings(full, function, "name", "arguments")
			message["function_call"] = full
		}
		if audio := object(delta["audio"]); audio != nil {
			full := copyProtocolObject(object(previous["audio"]))
			old := copyProtocolObject(full)
			mergeNativeFields(full, audio)
			for _, field := range []string{"data", "transcript"} {
				if value, ok := audio[field].(string); ok {
					full[field] = str(old[field]) + value
				}
			}
			message["audio"] = full
		}
		delete(message, "tool_calls")
		for _, raw := range array(delta["tool_calls"]) {
			call := object(raw)
			toolIndex, err := nativeStreamIndex(call["index"])
			if err != nil {
				return err
			}
			full := b.tools[index][toolIndex]
			if full == nil {
				full = map[string]any{"function": map[string]any{}}
				b.tools[index][toolIndex] = full
			}
			for field, value := range call {
				if field != "index" && field != "function" {
					full[field] = value
				}
			}
			appendNativeStrings(object(full["function"]), object(call["function"]), "name", "arguments")
		}
	}
	return nil
}

func (b *nativeProtocolBuffer) anthropic(event map[string]any) error {
	index, err := nativeStreamIndex(event["index"])
	if err != nil {
		return err
	}
	switch str(event["type"]) {
	case "message_start":
		message := object(event["message"])
		if message == nil {
			return fmt.Errorf("upstream message_start is missing message")
		}
		b.root = copyProtocolObject(message)
	case "content_block_start":
		block := object(event["content_block"])
		if block == nil || b.entries[index] != nil {
			return fmt.Errorf("invalid upstream content_block_start")
		}
		b.entries[index] = copyProtocolObject(block)
		if b.closed == nil {
			b.closed = map[int]bool{}
		}
	case "content_block_delta":
		block := b.entries[index]
		if block == nil || b.closed[index] {
			return fmt.Errorf("upstream delta has no content block")
		}
		delta := object(event["delta"])
		switch str(delta["type"]) {
		case "text_delta":
			appendNativeStrings(block, delta, "text")
		case "thinking_delta":
			appendNativeStrings(block, delta, "thinking")
		case "signature_delta":
			appendNativeStrings(block, delta, "signature")
		case "compaction_delta":
			appendNativeStrings(block, delta, "content")
		case "input_json_delta":
			// The initial input is a seed, not a JSON fragment. Deltas replace
			// it as one complete input object, preventing "{}{...}".
			b.arguments[index] += str(delta["partial_json"])
		case "citations_delta":
			block["citations"] = append(array(block["citations"]), delta["citation"])
		default:
			return unsupported("native Anthropic delta " + str(delta["type"]))
		}
	case "content_block_stop":
		if b.entries[index] == nil || b.closed[index] {
			return fmt.Errorf("invalid upstream content_block_stop")
		}
		b.closed[index] = true
	case "message_delta":
		mergeNativeFields(b.root, object(event["delta"]))
		mergeNativeUsage(b.root, "usage", event["usage"], b.format)
	}
	return nil
}

func (b *nativeProtocolBuffer) gemini(event map[string]any) error {
	for field, value := range event {
		if field != "candidates" && field != "usageMetadata" {
			b.root[field] = value
		}
	}
	mergeNativeUsage(b.root, "usageMetadata", event["usageMetadata"], b.format)
	for _, raw := range array(event["candidates"]) {
		candidate := object(raw)
		index, err := nativeStreamIndex(candidate["index"])
		if err != nil {
			return err
		}
		entry := b.entries[index]
		if entry == nil {
			entry = map[string]any{"index": index}
			b.entries[index] = entry
		}
		for field, value := range candidate {
			if field != "content" {
				entry[field] = value
			}
		}
		if incoming := object(candidate["content"]); incoming != nil {
			content := copyProtocolObject(object(entry["content"]))
			parts := array(content["parts"])
			// Each native part travels intact. Never coalesce signed parts or
			// detach thoughtSignature from its text/functionCall/image.
			parts = append(parts, array(incoming["parts"])...)
			mergeNativeFields(content, incoming)
			content["parts"], entry["content"] = parts, content
		}
	}
	return nil
}

func (b *nativeProtocolBuffer) finish() ([]byte, error) {
	entries := orderedNativeEntries(b.entries)
	switch b.format {
	case FormatOpenAIChat:
		if len(entries) == 0 {
			return nil, fmt.Errorf("upstream response is missing choices")
		}
		for _, raw := range entries {
			entry := object(raw)
			if str(entry["finish_reason"]) == "" {
				return nil, io.ErrUnexpectedEOF
			}
			if calls := orderedNativeEntries(b.tools[intFromAny(entry["index"])]); len(calls) > 0 {
				object(entry["message"])["tool_calls"] = calls
			}
		}
		b.root["object"], b.root["choices"] = "chat.completion", entries
	case FormatAnthropic:
		if str(b.root["type"]) != "message" || str(b.root["stop_reason"]) == "" {
			return nil, io.ErrUnexpectedEOF
		}
		for index := range b.entries {
			if !b.closed[index] {
				return nil, io.ErrUnexpectedEOF
			}
		}
		for index, arguments := range b.arguments {
			input, err := decodeProtocolObject([]byte(arguments))
			if err != nil {
				return nil, fmt.Errorf("invalid upstream tool input: %w", err)
			}
			b.entries[index]["input"] = input
		}
		b.root["content"] = entries
	case FormatGemini:
		if !geminiPromptBlocked(b.root) {
			if len(entries) == 0 {
				return nil, io.ErrUnexpectedEOF
			}
			for _, raw := range entries {
				if str(object(raw)["finishReason"]) == "" {
					return nil, io.ErrUnexpectedEOF
				}
			}
		}
		b.root["candidates"] = entries
	}
	return json.Marshal(b.root)
}
