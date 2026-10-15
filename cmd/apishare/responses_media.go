package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

const toolImageMarker = "[Tool output images follow this tool result batch]"

func parseResponsesToolOutput(value any) ([]canonPart, error) {
	if text, ok := value.(string); ok {
		trimmed := strings.TrimSpace(text)
		if strings.HasPrefix(trimmed, "data:image/") && strings.Contains(trimmed, ";base64,") {
			return []canonPart{{Kind: "image", URL: trimmed}}, nil
		}
		// Some tools wrap their content array in a JSON string. Decode only
		// when recognized image nodes exist; ordinary JSON text stays exact.
		if json.Valid([]byte(text)) {
			decoder := json.NewDecoder(strings.NewReader(text))
			decoder.UseNumber()
			var decoded any
			if decoder.Decode(&decoded) == nil {
				rewritten, images, err := extractWrappedToolImages(decoded, 0)
				if err != nil {
					return nil, err
				}
				if len(images) > 0 {
					return append([]canonPart{{Kind: "text", Text: jsonText(rewritten)}}, images...), nil
				}
			}
		}
		return []canonPart{{Kind: "text", Text: text}}, nil
	}
	parts, err := parseCanonParts(FormatOpenAIResponse, value)
	if err == nil {
		return parts, nil
	}
	rewritten, images, mediaErr := extractWrappedToolImages(value, 0)
	if mediaErr != nil {
		return nil, mediaErr
	}
	if len(images) == 0 {
		return nil, err
	}
	return append([]canonPart{{Kind: "text", Text: jsonText(rewritten)}}, images...), nil
}

// Inspect content containers and typed image nodes, never arbitrary URL fields.
// Copy changed containers so rendering/retries cannot mutate source history.
func extractWrappedToolImages(value any, depth int) (any, []canonPart, error) {
	if depth > 32 {
		// Deeper arbitrary JSON remains text; do not reject media-free output
		// merely because it uses a deeply nested application-specific schema.
		return value, nil, nil
	}
	switch node := value.(type) {
	case []any:
		out := append([]any(nil), node...)
		var images []canonPart
		for i, child := range node {
			rewritten, media, err := extractWrappedToolImages(child, depth+1)
			if err != nil {
				return nil, nil, err
			}
			out[i] = rewritten
			images = append(images, media...)
		}
		return out, images, nil
	case map[string]any:
		typ := str(node["type"])
		if typ == "input_image" || typ == "image_url" || typ == "image" {
			parts, err := parseCanonParts(FormatOpenAIResponse, []any{node})
			if err != nil {
				return nil, nil, err
			}
			return map[string]any{"type": "input_text", "text": toolImageMarker}, parts, nil
		}
		if content, ok := node["content"]; ok {
			rewritten, images, err := extractWrappedToolImages(content, depth+1)
			if err != nil || len(images) == 0 {
				return value, images, err
			}
			out := make(map[string]any, len(node))
			for key, v := range node {
				out[key] = v
			}
			out["content"] = rewritten
			return out, images, nil
		}
	}
	return value, nil, nil
}

func liftToolResultImages(messages []canonMsg) []canonMsg {
	out := make([]canonMsg, 0, len(messages))
	var pending []canonPart
	flush := func() {
		if len(pending) > 0 {
			out = append(out, canonMsg{Role: "user", Parts: pending, Text: partsText(pending)})
			pending = nil
		}
	}
	for _, msg := range messages {
		if msg.Role != "tool" {
			flush()
			out = append(out, msg)
			continue
		}
		var text, images []canonPart
		for _, part := range messageParts(msg) {
			if part.Kind == "image" {
				images = append(images, part)
			} else {
				text = append(text, part)
			}
		}
		if len(images) > 0 {
			text = append(text, canonPart{Kind: "text", Text: toolImageMarker})
			msg.Parts, msg.Text = text, partsText(text)
			pending = append(pending, canonPart{Kind: "text", Text: fmt.Sprintf("[Tool output images for call %s]", msg.ToolID)})
			pending = append(pending, images...)
		}
		out = append(out, msg)
	}
	flush()
	return out
}
