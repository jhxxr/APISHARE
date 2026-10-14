package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// Tool payloads can contain integer IDs larger than JavaScript's safe range.
// Preserve their JSON spelling while parsing ordinary protocol metadata.
func decodeProtocolObject(body []byte) (map[string]any, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("invalid JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, fmt.Errorf("invalid JSON object")
	}
	return object, nil
}

func protocolFloat(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case json.Number:
		result, err := number.Float64()
		return result, err == nil
	}
	return 0, false
}

// Providers expose the same plaintext reasoning under several field names.
// Prefer one representation, rather than concatenating duplicate aliases or
// mistaking opaque encrypted details for readable text.
func chatReasoningText(message map[string]any) string {
	for _, field := range []string{"reasoning_content", "reasoning"} {
		if text := str(message[field]); text != "" {
			return text
		}
	}
	var parts []string
	for _, raw := range array(message["reasoning_details"]) {
		item := object(raw)
		if str(item["type"]) == "reasoning.text" && str(item["text"]) != "" {
			parts = append(parts, str(item["text"]))
		}
	}
	return strings.Join(parts, "")
}

func geminiFinishError(reason string) error {
	switch reason {
	case "MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL", "MISSING_THOUGHT_SIGNATURE", "MALFORMED_RESPONSE", "NO_IMAGE", "TOO_MANY_TOOL_CALLS":
		return fmt.Errorf("upstream Gemini tool generation failed: %s", reason)
	}
	return nil
}

func geminiPromptBlocked(event map[string]any) bool {
	reason := str(object(event["promptFeedback"])["blockReason"])
	return reason != "" && reason != "BLOCK_REASON_UNSPECIFIED" && reason != "BLOCKED_REASON_UNSPECIFIED"
}

// The first array entry need not be the primary candidate.
func geminiPrimaryCandidate(candidates []any) map[string]any {
	for _, raw := range candidates {
		candidate := object(raw)
		if intFromAny(candidate["index"]) == 0 {
			return candidate
		}
	}
	return nil
}

// HTTP 200 alone does not establish success. Keep native content untouched,
// but reject explicit error envelopes before committing the downstream body.
func nativeProtocolResponseError(format clientFormat, body []byte) error {
	event, err := decodeProtocolObject(body)
	if err != nil {
		return fmt.Errorf("upstream returned invalid JSON: %w", err)
	}
	if event["error"] != nil || str(event["type"]) == "error" || str(event["status"]) == "failed" {
		return fmt.Errorf("upstream returned an error response")
	}
	if format == FormatGemini {
		for _, raw := range array(event["candidates"]) {
			if err := geminiFinishError(str(object(raw)["finishReason"])); err != nil {
				return err
			}
		}
	}
	return nil
}

func readProtocolBody(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("upstream response body too large")
	}
	return body, nil
}

// Add the function protocol's root constraint without flattening unions,
// removing references or mutating schemas reused by another upstream retry.
func functionParameterSchema(schema any) any {
	if schema == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if root := object(schema); root != nil && root["type"] == nil {
		out := copyProtocolObject(root)
		out["type"] = "object"
		return out
	}
	return schema
}

// Codex uses an unpaired tool-output-shaped envelope to bootstrap a separate
// task. Recognize this narrow shape; ordinary orphan results remain invalid.
func codexDelegationInput(item map[string]any) (string, bool) {
	if str(item["call_id"]) != "" {
		return "", false
	}
	if ns := str(item["namespace"]); ns != "codex_app" && ns != "codex_tui" {
		return "", false
	}
	if name := str(item["name"]); name != "create_thread" && name != "send_message_to_thread" {
		return "", false
	}
	text := str(item["output"])
	var envelope struct {
		XMLName xml.Name `xml:"codex_delegation"`
		Input   string   `xml:"input"`
	}
	if xml.Unmarshal([]byte(text), &envelope) != nil || strings.TrimSpace(envelope.Input) == "" {
		return "", false
	}
	return text, true
}
