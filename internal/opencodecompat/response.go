package opencodecompat

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"sort"
)

// NewResponseBody restores request aliases before native forwarding or protocol
// conversion. No goroutine can outlive a disconnected client.
func NewResponseBody(source io.ReadCloser, protocol Protocol, renames *ToolRenames, sse bool) io.ReadCloser {
	if renames.Empty() {
		return source
	}
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 4096), responseLimit+2)
	scanner.Split(splitResponseLine)
	return &responseBody{
		source: source, scanner: scanner, protocol: protocol,
		renames: renames, sse: sse, names: map[nameKey]*nameState{},
	}
}

type nameKey struct {
	choice string
	call   string
}

type nameState struct {
	pending     string
	done        bool
	renamed     bool
	choiceIndex any
	callIndex   any
}

type responseBody struct {
	source       io.ReadCloser
	scanner      *bufio.Scanner
	protocol     Protocol
	renames      *ToolRenames
	sse          bool
	names        map[nameKey]*nameState
	chatEnvelope map[string]any
	pending      []byte
	err          error
}

const responseLimit = 64 << 20
const responseNameLimit = 4096
const responseNameStates = 4096

func (b *responseBody) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	for len(b.pending) == 0 {
		if b.err != nil {
			return 0, b.err
		}
		if !b.sse {
			raw, err := io.ReadAll(io.LimitReader(b.source, responseLimit+1))
			if err != nil {
				return 0, err
			}
			if len(raw) > responseLimit {
				return 0, fmt.Errorf("OpenCode response exceeds 64 MiB")
			}
			b.pending, b.err = b.restoreJSON(raw, false), io.EOF
			continue
		}
		var frame []byte
		complete := false
		for b.scanner.Scan() {
			line := b.scanner.Bytes()
			frame = append(frame, line...)
			if len(frame) > responseLimit {
				return 0, fmt.Errorf("OpenCode SSE event exceeds 64 MiB")
			}
			if bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) || bytes.Equal(line, []byte("\r")) {
				complete = true
				break
			}
		}
		if !complete {
			b.err = b.scanner.Err()
			if b.err == nil {
				b.err = io.EOF
				if len(frame) > 0 || b.hasPendingNames() {
					b.err = io.ErrUnexpectedEOF
				}
			}
			return 0, b.err
		}
		b.pending = b.restoreFrame(frame)
	}
	n := copy(data, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

// Keep line delimiters so rewritten and untouched SSE frames preserve framing.
func splitResponseLine(data []byte, atEOF bool) (int, []byte, error) {
	if index := bytes.IndexAny(data, "\r\n"); index >= 0 {
		end := index + 1
		if data[index] == '\r' {
			if end == len(data) && !atEOF {
				return 0, nil, nil
			}
			if end < len(data) && data[end] == '\n' {
				end++
			}
		}
		return end, data[:end], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func (b *responseBody) Close() error { return b.source.Close() }

func (b *responseBody) restoreFrame(frame []byte) []byte {
	var lines [][]byte
	for remaining := frame; len(remaining) > 0; {
		end, line, _ := splitResponseLine(remaining, true)
		lines = append(lines, line)
		remaining = remaining[end:]
	}
	var data [][]byte
	first := -1
	for index, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			if first < 0 {
				first = index
			}
			data = append(data, bytes.TrimPrefix(bytes.TrimRight(line[5:], "\r\n"), []byte(" ")))
		}
	}
	if first < 0 {
		return frame
	}
	raw := bytes.Join(data, []byte("\n"))
	if b.protocol == Chat && bytes.Equal(raw, []byte("[DONE]")) && b.hasPendingNames() {
		payload := map[string]any{}
		for key, value := range b.chatEnvelope {
			payload[key] = value
		}
		var choices []any
		seen := map[string]bool{}
		for _, key := range b.pendingNameKeys("") {
			if seen[key.choice] {
				continue
			}
			seen[key.choice] = true
			delta := map[string]any{}
			choice := map[string]any{"index": b.names[key].choiceIndex, "delta": delta, "finish_reason": nil}
			b.flushNames(key.choice, delta)
			choices = append(choices, choice)
		}
		payload["choices"] = choices
		encoded, err := Marshal(payload)
		if err != nil {
			b.err = err
			return nil
		}
		newline := "\n"
		if bytes.Contains(frame, []byte("\r\n")) {
			newline = "\r\n"
		} else if bytes.Contains(frame, []byte("\r")) && !bytes.Contains(frame, []byte("\n")) {
			newline = "\r"
		}
		return append([]byte("data: "+string(encoded)+newline+newline), frame...)
	}
	restored := b.restoreJSON(raw, true)
	if b.err != nil && b.err != io.EOF {
		return nil
	}
	if bytes.Equal(raw, restored) {
		return frame
	}
	var result bytes.Buffer
	for index, line := range lines {
		if index == first {
			result.WriteString("data: ")
			result.Write(restored)
			if bytes.HasSuffix(line, []byte("\r\n")) {
				result.WriteString("\r\n")
			} else if bytes.HasSuffix(line, []byte("\r")) {
				result.WriteByte('\r')
			} else {
				result.WriteByte('\n')
			}
		} else if !bytes.HasPrefix(line, []byte("data:")) {
			result.Write(line)
		}
	}
	return result.Bytes()
}

func (b *responseBody) restoreJSON(raw []byte, streaming bool) []byte {
	payload, err := Decode(raw)
	if err != nil {
		return raw
	}
	changed := false
	rename := func(container map[string]any) {
		if client, ok := b.renames.Client(StringAt(container, "name")); ok {
			container["name"] = client
			changed = true
		}
	}
	switch b.protocol {
	case Chat:
		if streaming {
			if b.chatEnvelope == nil {
				b.chatEnvelope = map[string]any{}
			}
			for _, key := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
				if value, ok := payload[key]; ok {
					b.chatEnvelope[key] = value
				}
			}
		}
		for choiceIndex, rawChoice := range SliceAt(payload, "choices") {
			choice, _ := rawChoice.(map[string]any)
			choiceID := indexOr(choice, choiceIndex)
			for _, key := range []string{"message", "delta"} {
				container, _ := choice[key].(map[string]any)
				for callIndex, rawCall := range SliceAt(container, "tool_calls") {
					call, _ := rawCall.(map[string]any)
					function, _ := call["function"].(map[string]any)
					if !streaming || key == "message" {
						rename(function)
					} else if b.restoreName(nameKey{choiceID, indexOr(call, callIndex)}, function, indexValue(choice, choiceIndex), indexValue(call, callIndex)) {
						changed = true
					}
				}
				if function, ok := container["function_call"].(map[string]any); ok {
					if streaming && key == "delta" {
						changed = b.restoreName(nameKey{choiceID, "legacy"}, function, indexValue(choice, choiceIndex), nil) || changed
					} else {
						rename(function)
					}
				}
			}
			if streaming && StringAt(choice, "finish_reason") != "" {
				delta, _ := choice["delta"].(map[string]any)
				if delta == nil {
					delta = map[string]any{}
					choice["delta"] = delta
				}
				changed = b.flushNames(choiceID, delta) || changed
			}
		}
	case Responses, Anthropic:
		var visit func(any)
		visit = func(value any) {
			switch node := value.(type) {
			case []any:
				for _, child := range node {
					visit(child)
				}
			case map[string]any:
				kind := StringAt(node, "type")
				if kind == "function_call" || kind == "custom_tool_call" || kind == "tool_use" {
					rename(node)
					return // Tool inputs are arbitrary client data.
				}
				for _, key := range []string{"item", "output", "response", "content", "content_block"} {
					visit(node[key])
				}
			}
		}
		visit(payload)
	}
	if !changed {
		return raw
	}
	encoded, err := Marshal(payload)
	if err != nil {
		return raw
	}
	return encoded
}

func indexValue(value map[string]any, fallback int) any {
	if index, ok := value["index"]; ok {
		return index
	}
	return fallback
}

func indexOr(value map[string]any, fallback int) string {
	return fmt.Sprint(indexValue(value, fallback))
}

func (b *responseBody) restoreName(key nameKey, function map[string]any, choiceIndex, callIndex any) bool {
	if function == nil {
		return false
	}
	fragment := StringAt(function, "name")
	state := b.names[key]
	if state == nil {
		if fragment == "" {
			return false
		}
		if len(b.names) >= responseNameStates || len(key.choice)+len(key.call) > responseNameLimit {
			b.err = fmt.Errorf("OpenCode tool-name state exceeds limit")
			return false
		}
		state = &nameState{choiceIndex: choiceIndex, callIndex: callIndex}
		b.names[key] = state
	}
	if state.done {
		if state.renamed && fragment != "" {
			b.err = fmt.Errorf("OpenCode tool name continued after arguments")
		}
		return false
	}
	if len(state.pending)+len(fragment) > responseNameLimit {
		b.err = fmt.Errorf("OpenCode tool name exceeds limit")
		return false
	}
	// Chat names are deltas, not snapshots; even repeated fragments concatenate.
	state.pending += fragment
	if state.pending == "" {
		return false
	}
	if b.renames.HasPrefix(state.pending) && StringAt(function, "arguments") == "" {
		delete(function, "name")
		return fragment != ""
	}
	name := state.pending
	if client, exact := b.renames.Client(name); exact {
		name, state.renamed = client, true
	}
	function["name"] = name
	state.pending, state.done = "", true
	return name != fragment
}

func (b *responseBody) pendingNameKeys(choice string) []nameKey {
	var keys []nameKey
	for key, state := range b.names {
		if state.pending != "" && (choice == "" || key.choice == choice) {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].choice != keys[j].choice {
			return keys[i].choice < keys[j].choice
		}
		return keys[i].call < keys[j].call
	})
	return keys
}

func (b *responseBody) hasPendingNames() bool {
	for _, state := range b.names {
		if state.pending != "" {
			return true
		}
	}
	return false
}

func (b *responseBody) flushNames(choice string, delta map[string]any) bool {
	changed := false
	for _, key := range b.pendingNameKeys(choice) {
		state := b.names[key]
		name := state.pending
		if client, exact := b.renames.Client(name); exact {
			name, state.renamed = client, true
		}
		if key.call == "legacy" {
			function, _ := delta["function_call"].(map[string]any)
			if function == nil {
				function = map[string]any{}
				delta["function_call"] = function
			}
			function["name"] = name
		} else {
			calls := SliceAt(delta, "tool_calls")
			var target map[string]any
			for index, rawCall := range calls {
				call, _ := rawCall.(map[string]any)
				if indexOr(call, index) == key.call {
					target = call
					break
				}
			}
			if target == nil {
				target = map[string]any{"index": state.callIndex}
				delta["tool_calls"] = append(calls, target)
			}
			function, _ := target["function"].(map[string]any)
			if function == nil {
				function = map[string]any{}
				target["function"] = function
			}
			function["name"] = name
		}
		state.pending, state.done = "", true
		changed = true
	}
	return changed
}
