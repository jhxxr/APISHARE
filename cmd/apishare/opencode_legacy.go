package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// The legacy Zen gate only requires a read tool. Preserve the client's schema
// and alias an existing tool, preferring a file reader. Even a differently named
// tool must remain executable by the caller once its original name is restored.
func shapeLegacyOpenCodeTools(payload map[string]json.RawMessage, tools []json.RawMessage, format clientFormat) (string, error) {
	candidate, alias := -1, ""
	fallback, fallbackName := -1, ""
	for index, raw := range tools {
		name, _ := openCodeToolContract(raw, format)
		if name == "read" {
			return "", nil
		}
		if fallback < 0 && name != "" {
			fallback, fallbackName = index, name
		}
		if candidate < 0 && openCodeReadToolName(name) {
			candidate, alias = index, name
		}
	}
	if candidate < 0 {
		candidate, alias = fallback, fallbackName
	}
	if candidate < 0 {
		if len(tools) != 0 {
			return "", fmt.Errorf("OpenCode Zen 免费模型需要至少一个客户端函数工具，当前工具类型不支持兼容别名")
		}
		stub := openCodeToolDefinition{
			Description: "Disabled compatibility declaration. Do not invoke this tool. Use the caller's available tools instead.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		}
		tools = append(tools, renderOpenCodeTool("read", stub, format))
		payload["tools"], _ = json.Marshal(tools)
		// The declaration is only for the legacy gate. With no client tools,
		// prohibit tool selection instead of relying on its description alone.
		payload["tool_choice"] = json.RawMessage(`"none"`)
		if format == FormatAnthropic {
			payload["tool_choice"] = json.RawMessage(`{"type":"none"}`)
		}
		return "", nil
	}
	var tool map[string]json.RawMessage
	if json.Unmarshal(tools[candidate], &tool) != nil {
		return "", fmt.Errorf("无法读取 OpenCode read 工具")
	}
	if format == FormatOpenAIChat {
		var function map[string]json.RawMessage
		if json.Unmarshal(tool["function"], &function) != nil {
			return "", fmt.Errorf("无法读取 OpenCode read 工具")
		}
		function["name"] = json.RawMessage(`"read"`)
		tool["function"], _ = json.Marshal(function)
	} else {
		tool["name"] = json.RawMessage(`"read"`)
	}
	tools[candidate], _ = json.Marshal(tool)
	payload["tools"], _ = json.Marshal(tools)
	for _, field := range []string{"tool_choice", "messages", "input"} {
		if len(payload[field]) == 0 {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(payload[field]))
		decoder.UseNumber()
		var value any
		if decoder.Decode(&value) != nil {
			continue
		}
		rewriteOpenCodeToolNames(value, format, alias, "read")
		payload[field], _ = json.Marshal(value)
	}
	return alias, nil
}

func openCodeReadToolName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	normalized := strings.NewReplacer("_", "", "-", "", ".", "").Replace(name)
	switch normalized {
	case "read", "readfile", "fileread", "readfiles", "readtext", "readtextfile", "readcontent", "viewfile":
		return true
	}
	return strings.HasPrefix(name, "read_") || strings.HasPrefix(name, "read-") || strings.HasSuffix(name, "_read") || strings.HasSuffix(name, "-read")
}

// Visit protocol envelopes only. Function arguments and tool-result content
// can contain user JSON with identical names and must never be rewritten.
func rewriteOpenCodeToolNames(value any, format clientFormat, from, to string) {
	if values, ok := value.([]any); ok {
		for _, entry := range values {
			rewriteOpenCodeToolNames(entry, format, from, to)
		}
		return
	}
	entry, ok := value.(map[string]any)
	if !ok {
		return
	}
	kind := str(entry["type"])
	if (kind == "function" || kind == "function_call" || kind == "tool" || kind == "tool_use" || strings.HasPrefix(kind, "response.function_call_arguments.")) && str(entry["name"]) == from {
		entry["name"] = to
	}
	if format == FormatOpenAIChat {
		if function := object(entry["function"]); function != nil && str(function["name"]) == from {
			function["name"] = to
		}
	}
	for _, name := range []string{"choices", "message", "delta", "tool_calls", "messages", "input", "item", "response", "output", "content", "content_block"} {
		// Anthropic tool_use.input is arbitrary user data rather than an envelope.
		if name == "input" && kind == "tool_use" {
			continue
		}
		if name == "output" && (kind == "function_call_output" || kind == "tool_result") {
			continue
		}
		if name == "content" && kind == "tool_result" {
			continue
		}
		if child, exists := entry[name]; exists {
			rewriteOpenCodeToolNames(child, format, from, to)
		}
	}
}

func restoreOpenCodeToolJSON(body []byte, format clientFormat, alias string) []byte {
	value, err := decodeProtocolObject(body)
	if err != nil {
		return body
	}
	rewriteOpenCodeToolNames(value, format, "read", alias)
	encoded, err := json.Marshal(value)
	if err != nil {
		return body
	}
	return encoded
}

// Restore an aliased read tool in native JSON and complete SSE events, while
// retaining every other event field and the streaming/backpressure behavior.
type openCodeAliasBody struct {
	source  io.ReadCloser
	reader  *bufio.Reader
	format  clientFormat
	alias   string
	sse     bool
	pending []byte
	err     error
}

func newOpenCodeAliasBody(source io.ReadCloser, format clientFormat, alias string, sse bool) io.ReadCloser {
	return &openCodeAliasBody{source: source, reader: bufio.NewReader(source), format: format, alias: alias, sse: sse}
}

func (body *openCodeAliasBody) Read(output []byte) (int, error) {
	if len(output) == 0 {
		return 0, nil
	}
	if len(body.pending) == 0 && body.err != nil {
		return 0, body.err
	}
	if len(body.pending) == 0 {
		if !body.sse {
			raw, err := readProtocolBody(body.source, 64<<20)
			if err != nil {
				return 0, err
			}
			body.pending = restoreOpenCodeToolJSON(raw, body.format, body.alias)
			body.err = io.EOF
		} else {
			var frame []byte
			lineStart := 0
			for {
				fragment, err := body.reader.ReadSlice('\n')
				frame = append(frame, fragment...)
				if len(frame) > 64<<20 {
					return 0, fmt.Errorf("OpenCode SSE event is too large")
				}
				if err == bufio.ErrBufferFull {
					continue
				}
				if err != nil {
					body.err = err
					break
				}
				if len(bytes.TrimSpace(frame[lineStart:])) == 0 {
					break
				}
				lineStart = len(frame)
			}
			body.pending = body.restoreFrame(frame)
		}
	}
	if len(body.pending) == 0 && body.err != nil {
		return 0, body.err
	}
	n := copy(output, body.pending)
	body.pending = body.pending[n:]
	return n, nil
}

func (body *openCodeAliasBody) restoreFrame(frame []byte) []byte {
	lines := bytes.SplitAfter(frame, []byte("\n"))
	var data [][]byte
	first := -1
	for index, line := range lines {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		if first < 0 {
			first = index
		}
		value := bytes.TrimRight(line[5:], "\r\n")
		value = bytes.TrimPrefix(value, []byte(" "))
		data = append(data, value)
	}
	if first < 0 {
		return frame
	}
	raw := bytes.Join(data, []byte("\n"))
	if !json.Valid(raw) {
		return frame
	}
	restored := restoreOpenCodeToolJSON(raw, body.format, body.alias)
	var result bytes.Buffer
	for index, line := range lines {
		if index == first {
			result.WriteString("data: ")
			result.Write(restored)
			result.WriteByte('\n')
		} else if !bytes.HasPrefix(line, []byte("data:")) {
			result.Write(line)
		}
	}
	return result.Bytes()
}

func (body *openCodeAliasBody) Close() error { return body.source.Close() }
