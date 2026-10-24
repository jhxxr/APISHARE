package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
)

// The 2.1 Chat client writes this skeleton before optional provider fields.
var openCodeV2ChatKeyOrder = []string{"model", "messages", "tools", "stream", "stream_options", "max_completion_tokens"}

// Re-encode strings without Go's HTML escapes, including strings inside raw
// client fields. UseNumber retains opaque integer values beyond float64's
// precision. Used for shaped OpenCode free requests and stale-reference repair.
func encodeOpenCodeFreeBody(payload map[string]json.RawMessage, format clientFormat, legacy bool) ([]byte, error) {
	values := make(map[string]any, len(payload))
	for key, raw := range payload {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		values[key] = value
	}
	return encodeOpenCodeFreeValues(values, format, legacy)
}

func encodeOpenCodeFreeValues(payload map[string]any, format clientFormat, legacy bool) ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteByte('{')
	order := slices.Sorted(maps.Keys(payload))
	if !legacy && format == FormatOpenAIChat {
		order = append(slices.Clone(openCodeV2ChatKeyOrder), order...)
	}
	written := make(map[string]bool, len(payload))
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	for _, key := range order {
		value, exists := payload[key]
		if !exists || written[key] {
			continue
		}
		if len(written) > 0 {
			buffer.WriteByte(',')
		}
		if err := encoder.Encode(key); err != nil {
			return nil, err
		}
		buffer.Truncate(buffer.Len() - 1) // Encoder adds a newline.
		buffer.WriteByte(':')
		if err := encoder.Encode(value); err != nil {
			return nil, err
		}
		buffer.Truncate(buffer.Len() - 1)
		written[key] = true
	}
	buffer.WriteByte('}')
	return buffer.Bytes(), nil
}
