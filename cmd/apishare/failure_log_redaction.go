package main

import (
	"encoding/json"
	"mime"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"
)

const failureRedacted = "[REDACTED]"

func failureRequestHeaders(headers http.Header) map[string]string {
	out := make(map[string]string)
	// Only allow known protocol header values; custom headers may contain
	// credentials even if their names don't look like credential headers.
	for _, name := range []string{"Accept", "Openai-Beta", "Anthropic-Version", "Anthropic-Beta", "User-Agent", "Content-Type"} {
		if value := headers.Get(name); value != "" {
			if name == "Content-Type" {
				value, _, _ = mime.ParseMediaType(value)
			}
			out[strings.ToLower(name)] = failurePrefix(value, 128)
		}
	}
	for name := range headers {
		if len(out) == 24 {
			break
		}
		name = failurePrefix(strings.ToLower(name), 64)
		if _, allowed := out[name]; !allowed {
			out[name] = failureRedacted
		}
	}
	return out
}

func failureContentKey(key string) bool {
	switch strings.ToLower(key) {
	case "messages", "prompt", "input", "contents", "content", "system", "system_instruction", "systeminstruction", "instructions", "tools", "tool_choice", "tool_calls", "arguments", "output", "text", "file", "audio", "image", "query", "stop", "stop_sequences", "stopsequences", "suffix", "metadata", "user":
		return true
	}
	return false
}

// User text never enters the request snapshot, not even for short inputs.
func summarizeFailureRequest(root map[string]any) (map[string]any, bool) {
	if root == nil {
		return nil, false // Never fall back to saving raw/non-JSON input.
	}
	out := make(map[string]any)
	truncated := false
	for key, value := range root {
		if failureContentKey(key) {
			if list, ok := value.([]any); ok {
				summary := map[string]any{"count": len(list), "redacted": true}
				switch strings.ToLower(key) {
				case "messages", "input", "contents":
					items := make([]map[string]any, 0, min(len(list), 6))
					for _, item := range list[:cap(items)] {
						meta := map[string]any{"text_chars": failureTextChars(item)}
						switch role := str(object(item)["role"]); role {
						case "system", "developer", "user", "assistant", "tool", "function", "model":
							meta["role"] = role
						}
						items = append(items, meta)
					}
					summary["items"] = items
					truncated = truncated || len(items) < len(list)
				}
				out[key] = summary
			} else {
				out[key] = map[string]any{"text_chars": failureTextChars(value), "redacted": true}
			}
			continue
		}
		switch key {
		case "stream", "max_tokens", "max_completion_tokens", "max_output_tokens", "temperature", "top_p", "top_k", "seed", "n", "dimensions", "frequency_penalty", "presence_penalty":
			switch value := value.(type) {
			case bool:
				out[key] = value
			case json.Number:
				if len(value) <= 32 {
					out[key] = value
				}
			}
		case "reasoning_effort", "encoding_format":
			switch str(value) {
			case "none", "minimal", "low", "medium", "high", "xhigh", "float", "base64":
				out[key] = value
			}
		case "generationConfig":
			params := make(map[string]any)
			for _, field := range []string{"maxOutputTokens", "temperature", "topP", "topK", "candidateCount", "seed"} {
				if n, ok := object(value)[field].(json.Number); ok && len(n) <= 32 {
					params[field] = n
				}
			}
			out[key] = params
		}
	}
	return out, truncated
}

func failureTextChars(value any) int {
	switch v := value.(type) {
	case string:
		return utf8.RuneCountInString(v)
	case []any:
		n := 0
		for _, item := range v {
			n += failureTextChars(item)
		}
		return n
	case map[string]any:
		n := 0
		for _, key := range []string{"content", "parts", "text", "input", "arguments", "output"} {
			n += failureTextChars(v[key])
		}
		return n
	}
	return 0
}

func (r *failureResponse) setBody(reply failureReply, request map[string]any, secrets []string) {
	r.Truncated = reply.truncated
	if len(reply.data) == 0 {
		return
	}
	text := strings.ToValidUTF8(string(reply.data), "\uFFFD")
	// Providers sometimes quote a credential or a prompt inside a free-form
	// error message. Scrub known literals and their JSON-escaped forms, even
	// when the original reply wasn't JSON or was too large to parse.
	literals := make(map[string]bool)
	for _, secret := range secrets {
		if secret = strings.TrimSpace(secret); secret != "" {
			literals[secret] = true
			if _, token, ok := strings.Cut(secret, " "); ok && token != "" {
				literals[token] = true
			}
		}
	}
	var collect func(any)
	collect = func(value any) {
		if len(literals) > 256 {
			return
		}
		switch v := value.(type) {
		case string:
			if v != "" {
				literals[v] = true
			}
		case []any:
			for _, item := range v {
				collect(item)
			}
		case map[string]any:
			for key, item := range v {
				// Roles and MIME/types describe content; they are not its text.
				if key != "role" && key != "type" && key != "mimeType" {
					collect(item)
				}
			}
		}
	}
	for key, value := range request {
		if failureContentKey(key) {
			collect(value)
		}
	}
	if len(literals) > 256 {
		r.Redacted = true
		return // Too many literals to safely inspect within the diagnostic budget.
	}
	pairs := make([]string, 0, 2*len(literals))
	ordered := make([]string, 0, len(literals))
	for literal := range literals {
		ordered = append(ordered, literal)
	}
	slices.SortFunc(ordered, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	for _, literal := range ordered {
		// The full literal may have been cut at the capture boundary. If an
		// identifiable prefix survives without the rest, omit the reply body.
		if len(literal) > 32 && !strings.Contains(text, literal) {
			prefix := failurePrefix(literal, 32)
			escaped, _ := json.Marshal(prefix)
			if strings.Contains(text, prefix) || strings.Contains(text, string(escaped[1:len(escaped)-1])) {
				r.Redacted = true
				return
			}
		}
		if len(literal) <= len(text) {
			pairs = append(pairs, literal, failureRedacted)
		}
	}
	replacer := strings.NewReplacer(pairs...)
	if root, err := decodeProtocolObject(reply.data); err == nil {
		// Scrub decoded strings so Unicode escaping cannot hide a literal.
		// Re-encode so overwritten duplicate JSON fields cannot leak raw.
		clean, changed := scrubFailureResponse(root, replacer)
		root = object(clean)
		encoded, err := json.Marshal(root)
		if err != nil {
			r.Redacted = true
			return
		}
		text, r.Redacted = string(encoded), changed
		for _, env := range []map[string]any{object(root["error"]), object(object(root["response"])["error"]), root} {
			for _, key := range []string{"type", "code", "status"} {
				if value := str(env[key]); failureLogCode.MatchString(value) {
					r.ErrorType = value
					break
				}
			}
			if r.ErrorType != r.Kind {
				break
			}
		}
	} else {
		// Plain-text errors are also useful, but don't persist partial JSON:
		// it cannot be decoded and may contain incomplete credential fields.
		prefix := strings.TrimSpace(text)
		if strings.HasPrefix(prefix, "{") || strings.HasPrefix(prefix, "[") {
			r.Redacted = true
			return
		}
		clean := replacer.Replace(text)
		r.Redacted = clean != text
		text = clean
	}
	r.Body = text
}

func scrubFailureResponse(value any, replacer *strings.Replacer) (any, bool) {
	changed := false
	switch v := value.(type) {
	case string:
		clean := replacer.Replace(v)
		return clean, clean != v
	case map[string]any:
		for key, item := range v {
			name := strings.ToLower(key)
			switch name {
			case "authorization", "proxy-authorization", "cookie", "api_key", "apikey", "x-api-key", "x-goog-api-key", "token", "access_token", "refresh_token", "input_text", "output_text", "data":
				v[key], changed = failureRedacted, true
			default:
				if failureContentKey(name) {
					v[key], changed = failureRedacted, true
				} else if cleanKey := replacer.Replace(key); cleanKey != key {
					delete(v, key)
					v[failureRedacted], changed = failureRedacted, true
				} else if clean, replaced := scrubFailureResponse(item, replacer); replaced {
					v[key], changed = clean, true
				}
			}
		}
	case []any:
		for i, item := range v {
			if clean, replaced := scrubFailureResponse(item, replacer); replaced {
				v[i], changed = clean, true
			}
		}
	}
	return value, changed
}

// fullContent mode is an explicit admin opt-in. Request headers are then stored
// verbatim (including Authorization) because reproducing a failing call needs
// them; the console masks well-known credential headers when rendering.
func failureRawHeaders(headers http.Header) map[string]string {
	out := make(map[string]string, len(headers))
	for name, values := range headers {
		if len(values) == 0 {
			continue
		}
		out[failurePrefix(strings.ToLower(name), 64)] = failurePrefix(strings.Join(values, ", "), 4096)
	}
	return out
}

// failureRawBody keeps the client payload readable. JSON is wrapped in the
// decoded object so the UI can pretty-print it; anything else stays as text.
func failureRawBody(body []byte, limit int) (map[string]any, string) {
	if root, err := decodeProtocolObject(body); err == nil {
		return root, ""
	}
	return nil, failurePrefix(strings.ToValidUTF8(string(body), "\uFFFD"), limit)
}

// failureScrubText replaces known credential literals inside a free-form string.
// Used for the error summary, which never goes through setBody.
func failureScrubText(text string, secrets []string) string {
	pairs := make([]string, 0, 2*len(secrets))
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret == "" {
			continue
		}
		pairs = append(pairs, secret, failureRedacted)
		if _, token, ok := strings.Cut(secret, " "); ok && token != "" {
			pairs = append(pairs, token, failureRedacted)
		}
	}
	if len(pairs) == 0 {
		return text
	}
	return strings.NewReplacer(pairs...).Replace(text)
}
