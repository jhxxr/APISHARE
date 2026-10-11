package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The intermediate representation retains structured content; Text is only for accounting.
type canonPart struct{ Kind, Text, URL, MIME, Data string }
type canonCall struct{ ID, Name, Arguments string }
type canonTool struct {
	Name, Description string
	Schema            any
	Strict            *bool
}

func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func array(v any) []any           { a, _ := v.([]any); return a }
func str(v any) string            { s, _ := v.(string); return s }
func jsonText(v any) string       { b, _ := json.Marshal(v); return string(b) }
func unsupported(feature string) error {
	return fmt.Errorf("cross-format conversion does not support %s; use a native upstream for this feature", feature)
}

// OpenAIProtocol fixes the outbound endpoint independently of the client's endpoint.
// Empty/auto preserves the existing Chat/Responses native behavior.
func (u *Upstream) wireFormat(in clientFormat) clientFormat {
	switch u.Type {
	case "anthropic":
		return FormatAnthropic
	case "gemini":
		return FormatGemini
	default:
		switch u.OpenAIProtocol {
		case "chat":
			return FormatOpenAIChat
		case "responses":
			return FormatOpenAIResponse
		default:
			if in == FormatOpenAIResponse {
				return in
			}
			return FormatOpenAIChat
		}
	}
}

func validOpenAIProtocol(p string) bool {
	return p == "" || p == "auto" || p == "chat" || p == "responses"
}

func enrichCanonRequest(cr *canonReq, format clientFormat, body []byte) error {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil || m == nil {
		return fmt.Errorf("invalid JSON object")
	}
	cr.Messages = nil
	cr.System = ""
	if v, ok := m["top_p"].(float64); ok {
		cr.TopP = &v
	}
	if v, ok := m["parallel_tool_calls"].(bool); ok {
		cr.Parallel = &v
	}
	cr.ReasoningEffort = str(m["reasoning_effort"])
	if r := object(m["reasoning"]); r != nil {
		cr.ReasoningEffort = str(r["effort"])
	}
	if config := object(m["output_config"]); config != nil {
		if config["format"] != nil {
			return unsupported("Anthropic structured output format")
		}
		if effort := str(config["effort"]); effort != "" {
			cr.ReasoningEffort = effort
		}
	}
	for _, key := range []string{"response_format", "context_management", "functions", "function_call", "audio", "modalities"} {
		if m[key] != nil {
			return unsupported(key)
		}
	}
	if cfg := object(m["text"]); cfg["format"] != nil {
		return unsupported("Responses structured output format")
	}
	if cfg := object(m["generationConfig"]); cfg["responseSchema"] != nil || cfg["responseJsonSchema"] != nil || cfg["responseMimeType"] != nil {
		return unsupported("Gemini structured output format")
	}
	stopKey := "stop"
	if format == FormatAnthropic {
		stopKey = "stop_sequences"
	}
	if s := str(m[stopKey]); s != "" {
		cr.Stop = []string{s}
	} else {
		for _, v := range array(m[stopKey]) {
			cr.Stop = append(cr.Stop, str(v))
		}
	}
	for _, key := range []string{"previous_response_id", "conversation"} {
		if v := m[key]; v != nil && v != "" {
			return unsupported(key + " (send the full conversation when converting)")
		}
	}
	if n, _ := m["n"].(float64); n > 1 {
		return unsupported("multiple choices (n > 1)")
	}
	if err := parseCanonTools(cr, format, m); err != nil {
		return err
	}
	toolNames := map[string]string{}
	usedGeminiToolIDs := map[string]bool{}
	appendMessage := func(msg canonMsg) {
		for _, c := range msg.Calls {
			toolNames[c.ID] = c.Name
		}
		if msg.ToolID != "" && msg.ToolName == "" {
			msg.ToolName = toolNames[msg.ToolID]
		}
		cr.Messages = append(cr.Messages, msg)
	}
	switch format {
	case FormatOpenAIChat:
		for _, v := range array(m["messages"]) {
			j := object(v)
			role := str(j["role"])
			if role == "system" || role == "developer" {
				cr.System = joinSystem(cr.System, contentText(j["content"]))
				continue
			}
			msg := canonMsg{Role: role, ToolID: str(j["tool_call_id"]), ToolName: str(j["name"])}
			parts, err := parseCanonParts(format, j["content"])
			if err != nil {
				return err
			}
			msg.Parts, msg.Text = parts, partsText(parts)
			if role == "tool" && msg.ToolID == "" {
				return fmt.Errorf("tool result is missing tool_call_id")
			}
			for _, call := range array(j["tool_calls"]) {
				c := object(call)
				f := object(c["function"])
				if str(c["type"]) != "function" || f == nil {
					return unsupported("non-function tool calls")
				}
				msg.Calls = append(msg.Calls, canonCall{str(c["id"]), str(f["name"]), str(f["arguments"])})
			}
			if j["function_call"] != nil {
				return unsupported("legacy function_call")
			}
			appendMessage(msg)
		}
	case FormatOpenAIResponse:
		cr.System = str(m["instructions"])
		if s, ok := m["input"].(string); ok {
			appendMessage(canonMsg{Role: "user", Text: s, Parts: []canonPart{{Kind: "text", Text: s}}})
		} else {
			for _, v := range array(m["input"]) {
				j := object(v)
				switch str(j["type"]) {
				case "", "message":
					role := str(j["role"])
					if role == "system" || role == "developer" {
						cr.System = joinSystem(cr.System, contentText(j["content"]))
						continue
					}
					parts, err := parseCanonParts(format, j["content"])
					if err != nil {
						return err
					}
					appendMessage(canonMsg{Role: role, Parts: parts, Text: partsText(parts)})
				case "function_call":
					appendMessage(canonMsg{Role: "assistant", Calls: []canonCall{{str(j["call_id"]), str(j["name"]), str(j["arguments"])}}})
				case "function_call_output":
					parts, err := parseCanonParts(format, j["output"])
					if err != nil {
						return err
					}
					appendMessage(canonMsg{Role: "tool", ToolID: str(j["call_id"]), Parts: parts, Text: partsText(parts)})
				default:
					return unsupported("Responses input item " + str(j["type"]))
				}
			}
		}
	case FormatAnthropic:
		cr.System = contentText(m["system"])
		for _, v := range array(m["messages"]) {
			j := object(v)
			role := str(j["role"])
			if s, ok := j["content"].(string); ok {
				appendMessage(canonMsg{Role: role, Text: s, Parts: []canonPart{{Kind: "text", Text: s}}})
				continue
			}
			msg := canonMsg{Role: role}
			flush := func() {
				if len(msg.Parts) > 0 || len(msg.Calls) > 0 {
					msg.Text = partsText(msg.Parts)
					appendMessage(msg)
					msg = canonMsg{Role: role}
				}
			}
			for _, v := range array(j["content"]) {
				b := object(v)
				switch str(b["type"]) {
				case "tool_use":
					msg.Calls = append(msg.Calls, canonCall{str(b["id"]), str(b["name"]), jsonText(b["input"])})
				case "tool_result":
					flush()
					parts, err := parseCanonParts(format, b["content"])
					if err != nil {
						return err
					}
					if b["is_error"] == true {
						parts = append([]canonPart{{Kind: "text", Text: "Tool error: "}}, parts...)
					}
					appendMessage(canonMsg{Role: "tool", ToolID: str(b["tool_use_id"]), Parts: parts, Text: partsText(parts)})
				case "thinking", "redacted_thinking":
					// Provider signatures cannot be replayed on another provider; omit only replayed thinking.
				default:
					parts, err := parseCanonParts(format, []any{b})
					if err != nil {
						return err
					}
					msg.Parts = append(msg.Parts, parts...)
				}
			}
			flush()
		}
	case FormatGemini:
		cr.System = contentText(object(m["systemInstruction"])["parts"])
		gc := object(m["generationConfig"])
		if v, ok := gc["topP"].(float64); ok {
			cr.TopP = &v
		}
		for _, v := range array(gc["stopSequences"]) {
			cr.Stop = append(cr.Stop, str(v))
		}
		for _, v := range array(m["contents"]) {
			j := object(v)
			role := "user"
			if str(j["role"]) == "model" {
				role = "assistant"
			}
			msg := canonMsg{Role: role}
			flush := func() {
				if len(msg.Parts) > 0 || len(msg.Calls) > 0 {
					msg.Text = partsText(msg.Parts)
					appendMessage(msg)
					msg = canonMsg{Role: role}
				}
			}
			for _, v := range array(j["parts"]) {
				b := object(v)
				if f := object(b["functionCall"]); f != nil {
					id := str(f["id"])
					if id == "" {
						id = fmt.Sprintf("call_%d_%d", len(cr.Messages), len(msg.Calls))
					}
					msg.Calls = append(msg.Calls, canonCall{id, str(f["name"]), jsonText(f["args"])})
					continue
				}
				if f := object(b["functionResponse"]); f != nil {
					flush()
					if f["parts"] != nil {
						return unsupported("Gemini image functionResponse parts")
					}
					id := str(f["id"])
					name := str(f["name"])
					if id == "" {
						for i := len(cr.Messages) - 1; i >= 0 && id == ""; i-- {
							for _, c := range cr.Messages[i].Calls {
								if c.Name == name && !usedGeminiToolIDs[c.ID] {
									id = c.ID
									break
								}
							}
						}
					}
					if id == "" {
						return fmt.Errorf("Gemini functionResponse %q has no matching functionCall", name)
					}
					usedGeminiToolIDs[id] = true
					s := jsonText(f["response"])
					appendMessage(canonMsg{Role: "tool", ToolID: id, ToolName: name, Text: s, Parts: []canonPart{{Kind: "text", Text: s}}})
					continue
				}
				if b["thought"] == true {
					continue
				}
				parts, err := parseCanonParts(format, []any{b})
				if err != nil {
					return err
				}
				msg.Parts = append(msg.Parts, parts...)
			}
			flush()
		}
	}
	for _, msg := range cr.Messages {
		if msg.Role != "user" && msg.Role != "assistant" && msg.Role != "tool" {
			return fmt.Errorf("invalid message role %q", msg.Role)
		}
		if msg.Role == "tool" && msg.ToolID == "" {
			return fmt.Errorf("tool result is missing call ID")
		}
		for _, call := range msg.Calls {
			var arguments map[string]any
			if call.ID == "" || call.Name == "" || json.Unmarshal([]byte(call.Arguments), &arguments) != nil || arguments == nil {
				return fmt.Errorf("invalid function call id, name or arguments")
			}
		}
	}
	if len(cr.Messages) > 0 || cr.System != "" {
		cr.InputText = buildInputText(cr.System, cr.Messages)
	}
	return nil
}

func joinSystem(a, b string) string {
	if a == "" {
		return b
	}
	return a + "\n" + b
}
func contentText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	var b strings.Builder
	for _, v := range array(v) {
		j := object(v)
		b.WriteString(str(j["text"]))
	}
	return b.String()
}
func partsText(parts []canonPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Kind == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func parseCanonParts(format clientFormat, v any) ([]canonPart, error) {
	if v == nil {
		return nil, nil
	}
	if s, ok := v.(string); ok {
		return []canonPart{{Kind: "text", Text: s}}, nil
	}
	a, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("content must be a string or an array")
	}
	var out []canonPart
	for _, v := range a {
		j := object(v)
		typ := str(j["type"])
		switch {
		case typ == "text" || typ == "input_text" || typ == "output_text" || (format == FormatGemini && j["text"] != nil):
			out = append(out, canonPart{Kind: "text", Text: str(j["text"])})
		case typ == "image_url" || typ == "input_image":
			u := str(j["image_url"])
			if o := object(j["image_url"]); o != nil {
				u = str(o["url"])
			}
			if u == "" {
				return nil, unsupported("image file IDs")
			}
			out = append(out, canonPart{Kind: "image", URL: u})
		case typ == "image":
			s := object(j["source"])
			p := canonPart{Kind: "image"}
			if str(s["type"]) == "url" {
				p.URL = str(s["url"])
			} else if str(s["type"]) == "base64" {
				p.MIME = str(s["media_type"])
				p.Data = str(s["data"])
			} else {
				return nil, unsupported("image source " + str(s["type"]))
			}
			out = append(out, p)
		case format == FormatGemini && j["inlineData"] != nil:
			s := object(j["inlineData"])
			if !strings.HasPrefix(str(s["mimeType"]), "image/") {
				return nil, unsupported("non-image inlineData")
			}
			out = append(out, canonPart{Kind: "image", MIME: str(s["mimeType"]), Data: str(s["data"])})
		case format == FormatGemini && j["fileData"] != nil:
			s := object(j["fileData"])
			if !strings.HasPrefix(str(s["mimeType"]), "image/") {
				return nil, unsupported("non-image fileData")
			}
			out = append(out, canonPart{Kind: "image", URL: str(s["fileUri"]), MIME: str(s["mimeType"])})
		default:
			return nil, unsupported("content block " + typ)
		}
	}
	return out, nil
}

func parseCanonTools(cr *canonReq, format clientFormat, m map[string]any) error {
	for _, v := range array(m["tools"]) {
		j := object(v)
		if format == FormatGemini {
			decls := array(j["functionDeclarations"])
			if len(decls) == 0 {
				return unsupported("Gemini built-in tools")
			}
			for _, v := range decls {
				f := object(v)
				schema := f["parametersJsonSchema"]
				if schema == nil {
					schema = f["parameters"]
				}
				if str(f["name"]) == "" {
					return fmt.Errorf("tool name is required")
				}
				cr.Tools = append(cr.Tools, canonTool{Name: str(f["name"]), Description: str(f["description"]), Schema: normalizeGeminiSchema(schema)})
			}
			continue
		}
		f := j
		schemaKey := "parameters"
		switch format {
		case FormatOpenAIChat:
			if str(j["type"]) != "function" {
				return unsupported("non-function tools")
			}
			f = object(j["function"])
		case FormatOpenAIResponse:
			if str(j["type"]) != "function" {
				return unsupported("Responses built-in tools")
			}
		case FormatAnthropic:
			if typ := str(j["type"]); typ != "" && typ != "custom" {
				return unsupported("Anthropic server tools")
			}
			schemaKey = "input_schema"
		}
		t := canonTool{Name: str(f["name"]), Description: str(f["description"]), Schema: f[schemaKey]}
		if strict, ok := f["strict"].(bool); ok {
			t.Strict = &strict
		}
		if t.Name == "" {
			return fmt.Errorf("tool name is required")
		}
		cr.Tools = append(cr.Tools, t)
	}
	c := m["tool_choice"]
	if s, ok := c.(string); ok {
		cr.ToolChoice = s
	} else if j := object(c); j != nil {
		switch format {
		case FormatAnthropic:
			cr.ToolChoice = str(j["type"])
			if cr.ToolChoice == "any" {
				cr.ToolChoice = "required"
			}
			if cr.ToolChoice == "tool" {
				cr.ToolChoice = "function"
				cr.ToolName = str(j["name"])
			}
			if v, ok := j["disable_parallel_tool_use"].(bool); ok {
				p := !v
				cr.Parallel = &p
			}
		case FormatOpenAIResponse:
			cr.ToolChoice = "function"
			cr.ToolName = str(j["name"])
		default:
			cr.ToolChoice = "function"
			cr.ToolName = str(object(j["function"])["name"])
		}
	}
	if format == FormatGemini {
		fc := object(object(m["toolConfig"])["functionCallingConfig"])
		switch str(fc["mode"]) {
		case "AUTO":
			cr.ToolChoice = "auto"
		case "NONE":
			cr.ToolChoice = "none"
		case "ANY":
			cr.ToolChoice = "required"
		}
		names := array(fc["allowedFunctionNames"])
		if len(names) == 1 {
			cr.ToolChoice = "function"
			cr.ToolName = str(names[0])
		} else if len(names) > 1 {
			return unsupported("multiple allowedFunctionNames")
		}
	}
	switch cr.ToolChoice {
	case "", "auto", "none", "required":
	case "function":
		if cr.ToolName == "" {
			return fmt.Errorf("tool_choice is missing function name")
		}
	default:
		return unsupported("tool_choice " + cr.ToolChoice)
	}
	return nil
}

// Gemini's Schema uses uppercase enum values; JSON Schema expects lowercase types.
func normalizeGeminiSchema(v any) any {
	switch j := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, value := range j {
			out[key] = value
			switch key {
			case "properties", "$defs", "definitions":
				props := map[string]any{}
				for name, schema := range object(value) {
					props[name] = normalizeGeminiSchema(schema)
				}
				out[key] = props
			case "items", "anyOf", "oneOf", "allOf", "not", "additionalProperties":
				out[key] = normalizeGeminiSchema(value)
			}
		}
		if typ, ok := j["type"].(string); ok {
			out["type"] = strings.ToLower(typ)
		}
		return out
	case []any:
		out := make([]any, len(j))
		for i, value := range j {
			out[i] = normalizeGeminiSchema(value)
		}
		return out
	default:
		return v
	}
}

func messageParts(msg canonMsg) []canonPart {
	if msg.Parts != nil {
		return msg.Parts
	}
	if msg.Text != "" {
		return []canonPart{{Kind: "text", Text: msg.Text}}
	}
	return nil
}
func imageURL(p canonPart) string {
	if p.URL != "" {
		return p.URL
	}
	return "data:" + p.MIME + ";base64," + p.Data
}
func imageData(p canonPart) (string, string, bool) {
	if p.Data != "" {
		return p.MIME, p.Data, true
	}
	if strings.HasPrefix(p.URL, "data:") {
		head, data, ok := strings.Cut(strings.TrimPrefix(p.URL, "data:"), ";base64,")
		return head, data, ok
	}
	return "", "", false
}

func renderCanonParts(format clientFormat, parts []canonPart) ([]any, error) {
	out := []any{}
	for _, p := range parts {
		if p.Kind == "text" {
			typ := "text"
			if format == FormatOpenAIResponse {
				typ = "input_text"
			}
			if format == FormatGemini {
				out = append(out, map[string]any{"text": p.Text})
			} else {
				out = append(out, map[string]any{"type": typ, "text": p.Text})
			}
			continue
		}
		switch format {
		case FormatOpenAIChat:
			out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL(p)}})
		case FormatOpenAIResponse:
			out = append(out, map[string]any{"type": "input_image", "image_url": imageURL(p)})
		case FormatAnthropic:
			s := map[string]any{"type": "url", "url": p.URL}
			if mime, data, ok := imageData(p); ok {
				s = map[string]any{"type": "base64", "media_type": mime, "data": data}
			}
			out = append(out, map[string]any{"type": "image", "source": s})
		case FormatGemini:
			if mime, data, ok := imageData(p); ok {
				out = append(out, map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": data}})
			} else {
				if p.MIME == "" {
					return nil, unsupported("remote image without MIME type for Gemini; use a base64 image")
				}
				out = append(out, map[string]any{"fileData": map[string]any{"mimeType": p.MIME, "fileUri": p.URL}})
			}
		}
	}
	return out, nil
}

func renderProtocolRequest(format clientFormat, cr *canonReq, model string) ([]byte, error) {
	if cr.ConversionError != nil {
		return nil, cr.ConversionError
	}
	if cr.ReasoningEffort != "" && (format == FormatAnthropic || format == FormatGemini) {
		return nil, unsupported("reasoning effort on this upstream protocol")
	}
	m := map[string]any{"model": model, "stream": cr.Stream}
	msgs := []any{}
	if cr.System != "" && format == FormatOpenAIChat {
		msgs = append(msgs, map[string]any{"role": "system", "content": cr.System})
	}
	for _, msg := range cr.Messages {
		parts, err := renderCanonParts(format, messageParts(msg))
		if err != nil {
			return nil, err
		}
		switch format {
		case FormatOpenAIChat:
			var content any = parts
			if len(parts) == 0 {
				content = nil
			} else if len(parts) == 1 && len(messageParts(msg)) == 1 && messageParts(msg)[0].Kind == "text" {
				content = msg.Text
				if msg.Parts != nil {
					content = msg.Parts[0].Text
				}
			}
			j := map[string]any{"role": msg.Role, "content": content}
			if msg.Role == "tool" {
				for _, p := range messageParts(msg) {
					if p.Kind != "text" {
						return nil, unsupported("image tool results in Chat Completions")
					}
				}
				j["content"] = partsText(messageParts(msg))
				j["tool_call_id"] = msg.ToolID
			}
			if len(msg.Calls) > 0 {
				calls := []any{}
				for _, c := range msg.Calls {
					calls = append(calls, map[string]any{"id": c.ID, "type": "function", "function": map[string]any{"name": c.Name, "arguments": c.Arguments}})
				}
				j["tool_calls"] = calls
			}
			msgs = append(msgs, j)
		case FormatOpenAIResponse:
			if msg.Role == "tool" {
				msgs = append(msgs, map[string]any{"type": "function_call_output", "call_id": msg.ToolID, "output": parts})
				continue
			}
			if len(parts) > 0 {
				if msg.Role == "assistant" {
					for _, v := range parts {
						j := object(v)
						if str(j["type"]) == "input_text" {
							j["type"] = "output_text"
						}
					}
				}
				msgs = append(msgs, map[string]any{"type": "message", "role": msg.Role, "content": parts})
			}
			for _, c := range msg.Calls {
				msgs = append(msgs, map[string]any{"type": "function_call", "call_id": c.ID, "name": c.Name, "arguments": c.Arguments})
			}
		case FormatAnthropic:
			role := msg.Role
			if role == "tool" {
				role = "user"
				parts = []any{map[string]any{"type": "tool_result", "tool_use_id": msg.ToolID, "content": parts}}
			}
			for _, c := range msg.Calls {
				var input any
				if json.Unmarshal([]byte(c.Arguments), &input) != nil {
					return nil, fmt.Errorf("invalid tool arguments")
				}
				parts = append(parts, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": input})
			}
			if len(msgs) > 0 && str(object(msgs[len(msgs)-1])["role"]) == role {
				last := object(msgs[len(msgs)-1])
				last["content"] = append(array(last["content"]), parts...)
			} else {
				msgs = append(msgs, map[string]any{"role": role, "content": parts})
			}
		case FormatGemini:
			role := "user"
			if msg.Role == "assistant" {
				role = "model"
			}
			if msg.Role == "tool" {
				if msg.ToolName == "" {
					return nil, fmt.Errorf("tool result %q has no matching function name", msg.ToolID)
				}
				var result any
				if json.Unmarshal([]byte(msg.Text), &result) != nil || object(result) == nil {
					result = map[string]any{"result": msg.Text}
				}
				parts = []any{map[string]any{"functionResponse": map[string]any{"id": msg.ToolID, "name": msg.ToolName, "response": result}}}
			}
			for _, c := range msg.Calls {
				var args any
				json.Unmarshal([]byte(c.Arguments), &args)
				parts = append(parts, map[string]any{"functionCall": map[string]any{"id": c.ID, "name": c.Name, "args": args}})
			}
			msgs = append(msgs, map[string]any{"role": role, "parts": parts})
		}
	}
	gc := map[string]any{}
	switch format {
	case FormatAnthropic:
		m["messages"] = msgs
		m["max_tokens"] = cr.MaxTokens
		if cr.MaxTokens <= 0 {
			m["max_tokens"] = 4096
		}
		if cr.System != "" {
			m["system"] = cr.System
		}
		if len(cr.Stop) > 0 {
			m["stop_sequences"] = cr.Stop
		}
	case FormatOpenAIResponse:
		m["input"] = msgs
		if cr.System != "" {
			m["instructions"] = cr.System
		}
		if cr.MaxTokens > 0 {
			m["max_output_tokens"] = cr.MaxTokens
		}
		if len(cr.Stop) > 0 {
			return nil, unsupported("stop sequences on a Responses upstream")
		}
		if cr.ReasoningEffort != "" {
			m["reasoning"] = map[string]any{"effort": cr.ReasoningEffort}
		}
	case FormatGemini:
		delete(m, "model")
		delete(m, "stream")
		m["contents"] = msgs
		if cr.System != "" {
			m["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": cr.System}}}
		}
		if cr.MaxTokens > 0 {
			gc["maxOutputTokens"] = cr.MaxTokens
		}
		if len(cr.Stop) > 0 {
			gc["stopSequences"] = cr.Stop
		}
	default:
		m["messages"] = msgs
		if cr.MaxTokens > 0 {
			field := "max_tokens"
			// Reasoning model families use the current Chat token limit field.
			for _, prefix := range []string{"o1", "o3", "o4", "gpt-5", "gpt-6"} {
				if strings.HasPrefix(model, prefix) {
					field = "max_completion_tokens"
					break
				}
			}
			m[field] = cr.MaxTokens
		}
		if cr.Stream {
			m["stream_options"] = map[string]any{"include_usage": true}
		}
		if len(cr.Stop) > 0 {
			m["stop"] = cr.Stop
		}
		if cr.ReasoningEffort != "" {
			m["reasoning_effort"] = cr.ReasoningEffort
		}
	}
	if format == FormatGemini {
		if cr.Temperature != nil {
			gc["temperature"] = *cr.Temperature
		}
		if cr.TopP != nil {
			gc["topP"] = *cr.TopP
		}
		if len(gc) > 0 {
			m["generationConfig"] = gc
		}
	} else {
		if cr.Temperature != nil {
			m["temperature"] = *cr.Temperature
		}
		if cr.TopP != nil {
			m["top_p"] = *cr.TopP
		}
	}
	if len(cr.Tools) > 0 {
		tools := []any{}
		for _, t := range cr.Tools {
			schema := t.Schema
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			f := map[string]any{"name": t.Name, "description": t.Description, "parameters": schema}
			switch format {
			case FormatAnthropic:
				delete(f, "parameters")
				f["input_schema"] = schema
			case FormatOpenAIChat:
				if t.Strict != nil {
					f["strict"] = *t.Strict
				}
			case FormatOpenAIResponse:
				f["type"] = "function"
				if t.Strict != nil {
					f["strict"] = *t.Strict
				}
			case FormatGemini:
				delete(f, "parameters")
				f["parametersJsonSchema"] = schema
			}
			if format == FormatOpenAIChat {
				tools = append(tools, map[string]any{"type": "function", "function": f})
			} else {
				tools = append(tools, f)
			}
		}
		if format == FormatGemini {
			m["tools"] = []any{map[string]any{"functionDeclarations": tools}}
		} else {
			m["tools"] = tools
		}
	}
	if cr.ToolChoice != "" {
		switch format {
		case FormatAnthropic:
			typ := cr.ToolChoice
			if typ == "required" {
				typ = "any"
			}
			if typ == "function" {
				typ = "tool"
			}
			choice := map[string]any{"type": typ}
			if typ == "tool" {
				choice["name"] = cr.ToolName
			}
			if cr.Parallel != nil {
				choice["disable_parallel_tool_use"] = !*cr.Parallel
			}
			m["tool_choice"] = choice
		case FormatGemini:
			mode := map[string]string{"auto": "AUTO", "none": "NONE", "required": "ANY", "function": "ANY"}[cr.ToolChoice]
			choice := map[string]any{"mode": mode}
			if cr.ToolName != "" {
				choice["allowedFunctionNames"] = []string{cr.ToolName}
			}
			m["toolConfig"] = map[string]any{"functionCallingConfig": choice}
		default:
			var choice any = cr.ToolChoice
			if cr.ToolChoice == "function" {
				if format == FormatOpenAIChat {
					choice = map[string]any{"type": "function", "function": map[string]any{"name": cr.ToolName}}
				} else {
					choice = map[string]any{"type": "function", "name": cr.ToolName}
				}
			}
			m["tool_choice"] = choice
		}
	}
	if format == FormatAnthropic && cr.ToolChoice == "" && cr.Parallel != nil && len(cr.Tools) > 0 {
		m["tool_choice"] = map[string]any{"type": "auto", "disable_parallel_tool_use": !*cr.Parallel}
	}
	if format == FormatGemini && cr.Parallel != nil && !*cr.Parallel {
		return nil, unsupported("disabling parallel tool calls on a Gemini upstream")
	}
	if cr.Parallel != nil && (format == FormatOpenAIChat || format == FormatOpenAIResponse) {
		m["parallel_tool_calls"] = *cr.Parallel
	}
	return json.Marshal(m)
}
