package opencodecompat

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
)

// NewResponseBody restores request aliases before native forwarding or protocol
// conversion. No goroutine can outlive a disconnected client.
func NewResponseBody(source io.ReadCloser, protocol Protocol, renames *ToolRenames, sse bool) io.ReadCloser {
	if renames.Empty() {
		return source
	}
	return &responseBody{
		source: source, reader: bufio.NewReader(source), protocol: protocol,
		renames: renames, sse: sse, names: map[string]*nameState{},
	}
}

type nameState struct {
	pending string
	done    bool
	renamed bool
}

type responseBody struct {
	source   io.ReadCloser
	reader   *bufio.Reader
	protocol Protocol
	renames  *ToolRenames
	sse      bool
	names    map[string]*nameState
	pending  []byte
	err      error
}

const responseLimit = 64 << 20

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
		lineStart := 0
		for {
			part, err := b.reader.ReadSlice('\n')
			frame = append(frame, part...)
			if len(frame) > responseLimit {
				return 0, fmt.Errorf("OpenCode SSE event exceeds 64 MiB")
			}
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil {
				b.err = err
				break
			}
			if len(bytes.TrimSpace(frame[lineStart:])) == 0 {
				break
			}
			lineStart = len(frame)
		}
		b.pending = b.restoreFrame(frame)
	}
	n := copy(data, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *responseBody) Close() error { return b.source.Close() }

func (b *responseBody) restoreFrame(frame []byte) []byte {
	lines := bytes.SplitAfter(frame, []byte("\n"))
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
	restored := b.restoreJSON(raw, true)
	if bytes.Equal(raw, restored) {
		return frame
	}
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
					} else if b.restoreName(choiceID+":"+indexOr(call, callIndex), function) {
						changed = true
					}
				}
				if function, ok := container["function_call"].(map[string]any); ok {
					if streaming && key == "delta" {
						changed = b.restoreName(choiceID+":legacy", function) || changed
					} else {
						rename(function)
					}
				}
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

func indexOr(value map[string]any, fallback int) string {
	if index, ok := value["index"]; ok {
		return fmt.Sprint(index)
	}
	return fmt.Sprint(fallback)
}

func (b *responseBody) restoreName(key string, function map[string]any) bool {
	if function == nil {
		return false
	}
	state := b.names[key]
	if state == nil {
		state = &nameState{}
		b.names[key] = state
	}
	fragment := StringAt(function, "name")
	if state.done {
		if state.renamed && fragment != "" {
			delete(function, "name")
			return true
		}
		return false
	}
	if fragment != "" {
		switch {
		case state.pending == "", fragment == state.pending, strings.HasPrefix(fragment, state.pending):
			state.pending = fragment
		default:
			state.pending += fragment
		}
	}
	if state.pending == "" {
		return false
	}
	hasArguments := StringAt(function, "arguments") != ""
	client, exact := b.renames.Client(state.pending)
	if b.renames.HasLongerPrefix(state.pending) && !hasArguments {
		delete(function, "name")
		return fragment != ""
	}
	if exact {
		function["name"] = client
		state.done, state.renamed = true, true
		return true
	}
	function["name"] = state.pending
	state.done = true
	return fragment != state.pending
}
