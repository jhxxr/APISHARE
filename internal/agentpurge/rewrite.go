// Package agentpurge bundles the original agent-purge persona catalog and
// rewrites only instruction fields. Other request payloads remain opaque.
package agentpurge

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"regexp"
	"strings"
)

// These identify the original template bundled by agent-purge. Prompt text is
// kept verbatim after removing the upstream YAML front matter.
const (
	PersonaID        = "neutral"
	PersonaName      = "中性模板"
	SourceRepository = "ZiChuanLan/agent-purge"
	SourceRevision   = "34a24f31bf4ec5d7668933b4135a9a2fc54b476e"
	SourcePath       = "lib/templates/personas/neutral.md"
	SourceURL        = "https://github.com/" + SourceRepository + "/blob/" + SourceRevision + "/" + SourcePath
)

//go:embed LICENSE
var License string

//go:embed NOTICE
var Notice string

// Prompt returns the bundled persona without YAML front matter.
func Prompt() string { return byID[PersonaID].Text }

// This identity-line heuristic follows agent-purge's Magpie hook. It cannot
// remove every identity statement; strict replaces the instruction fields.
var self = regexp.MustCompile(`(?i)^\s*(?:You are|You're|你是|您是|我(?:是|现在扮演|就是))`)
var named = regexp.MustCompile(`(?i)(Claude|ChatGPT|GPT-?[45]|Gemini|Codex|Copilot|DeepSeek|Qwen|Kimi|GLM|Cursor|Windsurf|AI (?:assistant|model)|语言模型|助手)`)

func stripIdentity(text string) string {
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if !self.MatchString(line) || !named.MatchString(line) {
			kept = append(kept, line)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func combined(host, mode, prompt string) string {
	if mode == "strict" {
		return prompt
	}
	if mode == "replace" {
		host = stripIdentity(host)
	}
	if host == "" {
		return prompt
	}
	return prompt + "\n\n" + host
}

func encode(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

// Rewrite applies append/replace/strict. Invalid, unsupported and off requests
// are returned byte-for-byte unchanged. Tools, cache markers, numeric values and
// unknown fields are retained as raw JSON. Each attempt starts from the original
// request, so retries never accumulate the template.
func Rewrite(body []byte, protocol, mode string) ([]byte, bool) {
	return RewriteWithPersona(body, protocol, mode, PersonaID)
}

// RewriteWithPersona applies an original bundled template. Unknown IDs are
// left unchanged rather than silently substituting a different persona.
func RewriteWithPersona(body []byte, protocol, mode, personaID string) ([]byte, bool) {
	persona, ok := Lookup(personaID)
	if !ok {
		return body, false
	}
	if mode != "append" && mode != "replace" && mode != "strict" {
		return body, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return body, false
	}
	var changed bool
	switch protocol {
	case "openai-chat":
		changed = rewriteChat(fields, mode, persona.Text)
	case "openai-responses":
		changed = rewriteResponses(fields, mode, persona.Text)
	case "anthropic":
		changed = rewriteAnthropic(fields, mode, persona.Text)
	case "gemini":
		changed = rewriteGemini(fields, mode, persona.Text)
	}
	if !changed {
		return body, false
	}
	return encode(fields), true
}

func rawObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var value map[string]json.RawMessage
	err := json.Unmarshal(raw, &value)
	return value, err == nil && value != nil
}

func missing(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func instructionMessage(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	message, ok := rawObject(raw)
	if !ok {
		return nil, false
	}
	var role string
	_ = json.Unmarshal(message["role"], &role)
	return message, role == "system" || role == "developer"
}

// stripContentIdentity retains arrays, non-text blocks and cache metadata.
func stripContentIdentity(raw json.RawMessage) (json.RawMessage, bool) {
	if missing(raw) {
		return raw, true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return encode(stripIdentity(text)), true
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return nil, false
	}
	kept := make([]json.RawMessage, 0, len(parts))
	for _, part := range parts {
		value, ok := rawObject(part)
		if !ok {
			return nil, false
		}
		if rawText, exists := value["text"]; exists {
			if json.Unmarshal(rawText, &text) != nil {
				return nil, false
			}
			cleaned := stripIdentity(text)
			if cleaned == "" {
				continue
			}
			value["text"] = encode(cleaned)
			part = encode(value)
		}
		kept = append(kept, part)
	}
	return encode(kept), true
}

func rewriteContent(raw json.RawMessage, mode, textType, prompt string) (json.RawMessage, bool) {
	if missing(raw) {
		return encode(prompt), true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return encode(combined(text, mode, prompt)), true
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return nil, false
	}
	if mode == "replace" {
		cleaned, ok := stripContentIdentity(raw)
		if !ok || json.Unmarshal(cleaned, &parts) != nil {
			return nil, false
		}
	}
	persona := encode(map[string]string{"type": textType, "text": prompt})
	return encode(append([]json.RawMessage{persona}, parts...)), true
}

func rewriteChat(fields map[string]json.RawMessage, mode, prompt string) bool {
	var messages []json.RawMessage
	if missing(fields["messages"]) || json.Unmarshal(fields["messages"], &messages) != nil {
		return false
	}
	first := -1
	for i, raw := range messages {
		message, instruction := instructionMessage(raw)
		if !instruction {
			continue
		}
		if first < 0 {
			first = i
		}
		if mode == "strict" {
			continue
		}
		if i != first {
			if mode != "replace" {
				continue
			}
			cleaned, ok := stripContentIdentity(message["content"])
			if !ok {
				return false
			}
			message["content"] = cleaned
		} else {
			content, ok := rewriteContent(message["content"], mode, "text", prompt)
			if !ok {
				return false
			}
			message["content"] = content
		}
		messages[i] = encode(message)
	}
	if mode == "strict" {
		kept := make([]json.RawMessage, 0, len(messages)+1)
		for _, raw := range messages {
			if _, instruction := instructionMessage(raw); !instruction {
				kept = append(kept, raw)
			}
		}
		messages = append([]json.RawMessage{encode(map[string]string{"role": "system", "content": prompt})}, kept...)
	} else if first < 0 {
		messages = append([]json.RawMessage{encode(map[string]string{"role": "system", "content": prompt})}, messages...)
	}
	fields["messages"] = encode(messages)
	return true
}

func rewriteResponses(fields map[string]json.RawMessage, mode, prompt string) bool {
	var host string
	if !missing(fields["instructions"]) && json.Unmarshal(fields["instructions"], &host) != nil {
		return false
	}
	// Responses also accepts instruction messages inside input. Keep all
	// conversation and tool items while handling these controllable entries.
	if mode == "strict" || mode == "replace" {
		var input []json.RawMessage
		if !missing(fields["input"]) && json.Unmarshal(fields["input"], &input) == nil {
			kept := make([]json.RawMessage, 0, len(input))
			for _, raw := range input {
				message, instruction := instructionMessage(raw)
				if instruction {
					if mode == "strict" {
						continue
					}
					content, ok := stripContentIdentity(message["content"])
					if !ok {
						return false
					}
					message["content"] = content
					raw = encode(message)
				}
				kept = append(kept, raw)
			}
			fields["input"] = encode(kept)
		}
	}
	fields["instructions"] = encode(combined(host, mode, prompt))
	return true
}

func rewriteAnthropic(fields map[string]json.RawMessage, mode, prompt string) bool {
	if mode == "strict" || mode == "replace" {
		var messages []json.RawMessage
		if !missing(fields["messages"]) {
			if json.Unmarshal(fields["messages"], &messages) != nil {
				return false
			}
			kept := make([]json.RawMessage, 0, len(messages))
			for _, raw := range messages {
				message, instruction := instructionMessage(raw)
				if instruction {
					if mode == "strict" {
						continue
					}
					content, ok := stripContentIdentity(message["content"])
					if !ok {
						return false
					}
					message["content"] = content
					raw = encode(message)
				}
				kept = append(kept, raw)
			}
			fields["messages"] = encode(kept)
		}
	}
	if mode == "strict" || missing(fields["system"]) {
		fields["system"] = encode([]any{map[string]string{"type": "text", "text": prompt}})
		return true
	}
	content, ok := rewriteContent(fields["system"], mode, "text", prompt)
	if ok {
		fields["system"] = content
	}
	return ok
}

func rewriteGemini(fields map[string]json.RawMessage, mode, prompt string) bool {
	key := "systemInstruction"
	if missing(fields[key]) && !missing(fields["system_instruction"]) {
		key = "system_instruction"
	}
	if mode == "strict" {
		delete(fields, "systemInstruction")
		delete(fields, "system_instruction")
		fields[key] = encode(map[string]any{"parts": []any{map[string]string{"text": prompt}}})
		return true
	}
	instruction := make(map[string]json.RawMessage)
	if !missing(fields[key]) {
		var ok bool
		instruction, ok = rawObject(fields[key])
		if !ok {
			return false
		}
	}
	var parts []json.RawMessage
	if !missing(instruction["parts"]) && json.Unmarshal(instruction["parts"], &parts) != nil {
		return false
	}
	if mode == "replace" && len(parts) > 0 {
		cleaned, ok := stripContentIdentity(encode(parts))
		if !ok || json.Unmarshal(cleaned, &parts) != nil {
			return false
		}
	}
	persona := encode(map[string]string{"text": prompt})
	instruction["parts"] = encode(append([]json.RawMessage{persona}, parts...))
	fields[key] = encode(instruction)
	return true
}
