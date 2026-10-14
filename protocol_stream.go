package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type streamDelta struct {
	Key   string
	Block responseBlock
}
type protocolDecoder struct {
	format        clientFormat
	tools         map[int]canonCall
	arguments     map[int]bool // Presence only; the writer owns the argument buffer.
	terminal      bool
	finish        string
	stopSequence  string
	pendingArgs   map[int]*strings.Builder
	announced     map[int]bool
	responseParts map[string]*strings.Builder
	responseBytes int
	reasonSource  map[int]string
	toolSeeds     map[int]string
}

func newProtocolDecoder(f clientFormat) *protocolDecoder {
	return &protocolDecoder{format: f, tools: map[int]canonCall{}, arguments: map[int]bool{}, pendingArgs: map[int]*strings.Builder{}, announced: map[int]bool{}, responseParts: map[string]*strings.Builder{}, reasonSource: map[int]string{}, toolSeeds: map[int]string{}, finish: "stop"}
}

func (d *protocolDecoder) appendResponsePart(key, text string) error {
	buffer := d.responseParts[key]
	if buffer == nil {
		buffer = &strings.Builder{}
		d.responseParts[key] = buffer
	}
	if buffer.Len()+len(text) > maxBodyBytes || d.responseBytes+len(text) > 64<<20 {
		return fmt.Errorf("upstream accumulated response too large")
	}
	buffer.WriteString(text)
	d.responseBytes += len(text)
	return nil
}

func (d *protocolDecoder) responseSuffix(key, complete string) (string, error) {
	if complete == "" {
		return "", nil
	}
	var previous string
	if buffer := d.responseParts[key]; buffer != nil {
		previous = buffer.String()
	}
	if previous != "" && strings.TrimSpace(complete) == "" {
		return "", nil
	}
	if !strings.HasPrefix(complete, previous) {
		return "", fmt.Errorf("upstream completed %s disagrees with streamed content", key)
	}
	return complete[len(previous):], nil
}

func (d *protocolDecoder) flushChatTools() ([]streamDelta, error) {
	var indexes []int
	for i := range d.tools {
		if !d.announced[i] {
			indexes = append(indexes, i)
		}
	}
	sort.Ints(indexes)
	var out []streamDelta
	for _, i := range indexes {
		call := d.tools[i]
		if call.Name == "" {
			return nil, fmt.Errorf("upstream tool call is missing name")
		}
		if buffer := d.pendingArgs[i]; buffer != nil {
			call.Arguments = buffer.String()
		}
		d.announced[i] = true
		out = append(out, streamDelta{Key: fmt.Sprintf("tool_%d", i), Block: responseBlock{Kind: "tool", Call: call}})
	}
	return out, nil
}
func (d *protocolDecoder) decode(payload string) ([]streamDelta, upstreamUsage, error) {
	if payload == "[DONE]" {
		if d.format != FormatOpenAIChat {
			return nil, upstreamUsage{}, fmt.Errorf("unexpected upstream DONE event")
		}
		d.terminal = true
		deltas, err := d.flushChatTools()
		return deltas, upstreamUsage{}, err
	}
	j, decodeErr := decodeProtocolObject([]byte(payload))
	if decodeErr != nil {
		return nil, upstreamUsage{}, fmt.Errorf("invalid upstream SSE JSON")
	}
	if j["error"] != nil || str(j["type"]) == "error" || str(j["type"]) == "response.failed" {
		return nil, upstreamUsage{}, fmt.Errorf("upstream stream returned an error")
	}
	out := []streamDelta{}
	u := upstreamUsage{}
	decodeErr = nil
	add := func(key, kind, text string) {
		if text != "" {
			if d.format == FormatOpenAIResponse {
				if err := d.appendResponsePart(key, text); err != nil {
					decodeErr = err
					return
				}
			}
			out = append(out, streamDelta{key, responseBlock{Kind: kind, Text: text}})
		}
	}
	tool := func(index int, id, name, args string) {
		c := d.tools[index]
		if id != "" {
			c.ID = id
		}
		if name != "" {
			if d.format == FormatOpenAIChat {
				c.Name += name
			} else if c.Name == "" || c.Name == name {
				c.Name = name
			} else {
				decodeErr = fmt.Errorf("upstream changed an active tool's name")
				return
			}
		}
		if d.format == FormatOpenAIResponse && args != "" {
			if err := d.appendResponsePart(fmt.Sprintf("arguments_%d", index), args); err != nil {
				decodeErr = err
				return
			}
		}
		d.tools[index] = c
		if args != "" {
			d.arguments[index] = true
		}
		if d.format == FormatOpenAIChat && !d.announced[index] {
			buffer := d.pendingArgs[index]
			if c.Name == "" || args == "" {
				if args != "" {
					if buffer == nil {
						buffer = &strings.Builder{}
						d.pendingArgs[index] = buffer
					}
					buffer.WriteString(args)
				}
				return
			}
			if buffer != nil {
				args = buffer.String() + args
				delete(d.pendingArgs, index)
			}
			d.announced[index] = true
		}
		out = append(out, streamDelta{fmt.Sprintf("tool_%d", index), responseBlock{Kind: "tool", Call: canonCall{c.ID, c.Name, args}}})
		if d.format == FormatOpenAIResponse {
			d.announced[index] = true
		}
	}
	completeText := func(key, kind, text string) error {
		suffix, err := d.responseSuffix(key, text)
		if err == nil {
			add(key, kind, suffix)
		}
		return err
	}
	// Some compatible providers only send done events or a terminal snapshot.
	// Fill unseen blocks without duplicating blocks already emitted as deltas.
	responseItem := func(index int, item map[string]any) error {
		switch str(item["type"]) {
		case "function_call":
			name := ""
			if d.tools[index].Name == "" {
				name = str(item["name"])
			}
			args, err := d.responseSuffix(fmt.Sprintf("arguments_%d", index), str(item["arguments"]))
			if err != nil {
				return err
			}
			if args != "" || !d.announced[index] {
				tool(index, str(item["call_id"]), name, args)
			}
		case "message":
			for partIndex, raw := range array(item["content"]) {
				part := object(raw)
				key := fmt.Sprintf("item_%d_%d", index, partIndex)
				text := str(part["text"])
				if str(part["type"]) == "refusal" {
					text = str(part["refusal"])
					d.finish = "content_filter"
				} else if str(part["type"]) != "output_text" {
					return unsupported("streamed Responses content " + str(part["type"]))
				}
				if err := completeText(key, "text", text); err != nil {
					return err
				}
			}
		case "reasoning":
			parts := array(item["summary"])
			if d.reasonSource[index] == "content" || len(parts) == 0 {
				parts = array(item["content"])
			}
			for partIndex, raw := range parts {
				key := fmt.Sprintf("reason_%d_%d", index, partIndex)
				if err := completeText(key, "thinking", str(object(raw)["text"])); err != nil {
					return err
				}
			}
		default:
			return unsupported("streamed Responses output " + str(item["type"]))
		}
		return nil
	}
	switch d.format {
	case FormatOpenAIChat:
		u = readUsage(object(j["usage"]), d.format)
		for _, v := range array(j["choices"]) {
			ch := object(v)
			if intFromAny(ch["index"]) != 0 {
				continue
			}
			b := object(ch["delta"])
			add("text", "text", str(b["content"]))
			add("thinking", "thinking", chatReasoningText(b))
			add("text", "text", str(b["refusal"]))
			for _, v := range array(b["tool_calls"]) {
				c := object(v)
				f := object(c["function"])
				if t := str(c["type"]); t != "" && t != "function" {
					return nil, u, unsupported("streamed non-function tools")
				}
				tool(intFromAny(c["index"]), str(c["id"]), str(f["name"]), str(f["arguments"]))
			}
			if s := str(ch["finish_reason"]); s != "" {
				d.finish = normalizeFinish(s)
				d.terminal = true
			}
		}
	case FormatAnthropic:
		i := intFromAny(j["index"])
		key := fmt.Sprintf("block_%d", i)
		switch str(j["type"]) {
		case "message_start":
			u = readUsage(object(object(j["message"])["usage"]), d.format)
		case "content_block_start":
			b := object(j["content_block"])
			switch str(b["type"]) {
			case "text":
				add(key, "text", str(b["text"]))
			case "thinking":
				add(key, "thinking", str(b["thinking"]))
			case "tool_use":
				if input := object(b["input"]); len(input) > 0 {
					d.toolSeeds[i] = jsonText(input)
				}
				tool(i, str(b["id"]), str(b["name"]), "")
			default:
				return nil, u, unsupported("streamed Anthropic block " + str(b["type"]))
			}
		case "content_block_delta":
			b := object(j["delta"])
			switch str(b["type"]) {
			case "text_delta":
				add(key, "text", str(b["text"]))
			case "thinking_delta":
				add(key, "thinking", str(b["thinking"]))
			case "input_json_delta":
				tool(i, "", "", str(b["partial_json"]))
			case "signature_delta":
			default:
				return nil, u, unsupported("Anthropic delta " + str(b["type"]))
			}
		case "content_block_stop":
			if seed := d.toolSeeds[i]; seed != "" && !d.arguments[i] {
				tool(i, "", "", seed)
			}
			delete(d.toolSeeds, i)
		case "message_delta":
			b := object(j["delta"])
			if str(b["stop_reason"]) == "pause_turn" {
				return nil, readUsage(object(j["usage"]), d.format), unsupported("upstream Anthropic paused turn")
			}
			if s := str(b["stop_reason"]); s != "" {
				d.finish = normalizeFinish(s)
			}
			d.stopSequence = str(b["stop_sequence"])
			u = readUsage(object(j["usage"]), d.format)
		case "message_stop":
			var indexes []int
			for index := range d.toolSeeds {
				indexes = append(indexes, index)
			}
			sort.Ints(indexes)
			for _, index := range indexes {
				if !d.arguments[index] {
					tool(index, "", "", d.toolSeeds[index])
				}
			}
			d.terminal = true
		}
	case FormatGemini:
		u = readUsage(object(j["usageMetadata"]), d.format)
		if geminiPromptBlocked(j) {
			return nil, u, fmt.Errorf("upstream Gemini prompt blocked: %s", str(object(j["promptFeedback"])["blockReason"]))
		}
		for _, v := range array(j["candidates"]) {
			c := object(v)
			if intFromAny(c["index"]) != 0 {
				continue
			}
			if err := geminiFinishError(str(c["finishReason"])); err != nil {
				return nil, u, err
			}
			for i, v := range array(object(c["content"])["parts"]) {
				p := object(v)
				if f := object(p["functionCall"]); f != nil {
					index := len(d.tools)
					tool(index, str(f["id"]), str(f["name"]), jsonText(f["args"]))
				} else if text, ok := p["text"].(string); ok {
					kind := "text"
					if p["thought"] == true {
						kind = "thinking"
					}
					add(fmt.Sprintf("%s_%d", kind, i), kind, text)
				} else if p["thoughtSignature"] != nil && len(p) == 1 {
					// Do not expose provider-specific signatures as text.
				} else {
					return nil, u, unsupported("streamed Gemini non-text part")
				}
			}
			if s := str(c["finishReason"]); s != "" {
				d.finish = normalizeFinish(s)
				d.terminal = true
			}
		}
	case FormatOpenAIResponse:
		i := intFromAny(j["output_index"])
		key := fmt.Sprintf("item_%d_%d", i, intFromAny(j["content_index"]))
		switch str(j["type"]) {
		case "response.output_item.added":
			b := object(j["item"])
			switch str(b["type"]) {
			case "function_call":
				tool(i, str(b["call_id"]), str(b["name"]), str(b["arguments"]))
			case "message", "reasoning":
			default:
				return nil, u, unsupported("streamed Responses output " + str(b["type"]))
			}
		case "response.output_text.delta":
			add(key, "text", str(j["delta"]))
		case "response.output_text.done":
			if err := completeText(key, "text", str(j["text"])); err != nil {
				return nil, u, err
			}
		case "response.refusal.delta":
			add(key, "text", str(j["delta"]))
			d.finish = "content_filter"
		case "response.refusal.done":
			if err := completeText(key, "text", str(j["refusal"])); err != nil {
				return nil, u, err
			}
			d.finish = "content_filter"
		case "response.content_part.done":
			part := object(j["part"])
			text := str(part["text"])
			if str(part["type"]) == "refusal" {
				text, d.finish = str(part["refusal"]), "content_filter"
			} else if str(part["type"]) != "output_text" {
				return nil, u, unsupported("streamed Responses content " + str(part["type"]))
			}
			if err := completeText(key, "text", text); err != nil {
				return nil, u, err
			}
		case "response.reasoning_summary_text.delta":
			if d.reasonSource[i] != "content" && str(j["delta"]) != "" {
				d.reasonSource[i] = "summary"
				add(fmt.Sprintf("reason_%d_%d", i, intFromAny(j["summary_index"])), "thinking", str(j["delta"]))
			}
		case "response.reasoning_summary_text.done":
			key := fmt.Sprintf("reason_%d_%d", i, intFromAny(j["summary_index"]))
			if d.reasonSource[i] != "content" {
				if err := completeText(key, "thinking", str(j["text"])); err != nil {
					return nil, u, err
				}
				if str(j["text"]) != "" {
					d.reasonSource[i] = "summary"
				}
			}
		case "response.reasoning_text.delta", "response.reasoning_text.done":
			if d.reasonSource[i] != "summary" {
				key := fmt.Sprintf("reason_%d_%d", i, intFromAny(j["content_index"]))
				if str(j["type"]) == "response.reasoning_text.delta" {
					add(key, "thinking", str(j["delta"]))
				} else if err := completeText(key, "thinking", str(j["text"])); err != nil {
					return nil, u, err
				}
				if str(j["delta"]) != "" || str(j["text"]) != "" {
					d.reasonSource[i] = "content"
				}
			}
		case "response.function_call_arguments.delta":
			tool(i, "", "", str(j["delta"]))
		case "response.function_call_arguments.done":
			args, err := d.responseSuffix(fmt.Sprintf("arguments_%d", i), str(j["arguments"]))
			if err != nil {
				return nil, u, err
			}
			if args != "" {
				name := ""
				if d.tools[i].Name == "" {
					name = str(j["name"])
				}
				tool(i, str(j["call_id"]), name, args)
			}
		case "response.output_item.done":
			if err := responseItem(i, object(j["item"])); err != nil {
				return nil, u, err
			}
		case "response.completed", "response.incomplete", "response.done":
			r := object(j["response"])
			if r == nil {
				r = j
			}
			if r["error"] != nil || str(r["status"]) == "failed" {
				return nil, u, fmt.Errorf("upstream response failed")
			}
			for index, raw := range array(r["output"]) {
				if err := responseItem(index, object(raw)); err != nil {
					return nil, u, err
				}
			}
			u = readUsage(object(r["usage"]), d.format)
			d.terminal = true
			if str(j["type"]) == "response.incomplete" || str(r["status"]) == "incomplete" {
				d.finish = "length"
				if str(object(r["incomplete_details"])["reason"]) == "content_filter" {
					d.finish = "content_filter"
				}
			}
		}
	}
	if decodeErr != nil {
		return nil, u, decodeErr
	}
	if d.format == FormatOpenAIChat && d.terminal {
		pending, err := d.flushChatTools()
		if err != nil {
			return nil, u, err
		}
		out = append(out, pending...)
	}
	return out, u, nil
}

// SSE frames can contain multiple data lines, comments and CRLF line endings.
func readSSEFrames(r io.Reader, consume func(string) (bool, error)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxBodyBytes)
	data := []string{}
	size := 0
	flush := func() (bool, error) {
		if len(data) == 0 {
			return false, nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		size = 0
		if strings.TrimSpace(payload) == "" {
			return false, nil
		}
		return consume(payload)
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			done, err := flush()
			if err != nil || done {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			size += len(value)
			if size > maxBodyBytes {
				return fmt.Errorf("upstream SSE frame too large")
			}
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	_, err := flush()
	return err
}

// Keep non-streaming conversions uncommitted until the upstream terminates.
// The limit applies to the entire wire response as well as accumulated output,
// so many small frames cannot bypass the per-frame bound.
func collectProtocolSSE(reader io.Reader, format clientFormat) (*canonResponse, error) {
	const limit = 64 << 20
	d := newProtocolDecoder(format)
	r := &canonResponse{}
	indexes := map[string]int{}
	var texts, arguments []*strings.Builder
	size := 0
	wire := &io.LimitedReader{R: reader, N: limit + 1}
	err := readSSEFrames(wire, func(payload string) (bool, error) {
		deltas, usage, err := d.decode(payload)
		if err != nil {
			return false, err
		}
		r.Usage.merge(usage)
		for _, delta := range deltas {
			size += len(delta.Block.Text) + len(delta.Block.Call.Arguments) + len(delta.Block.Call.Name)
			if size > limit || len(r.Blocks) > 65536 {
				return false, fmt.Errorf("upstream buffered response too large")
			}
			i, exists := indexes[delta.Key]
			if !exists {
				i = len(r.Blocks)
				indexes[delta.Key] = i
				b := delta.Block
				b.Text, b.Call.Arguments = "", ""
				r.Blocks = append(r.Blocks, b)
				texts = append(texts, &strings.Builder{})
				arguments = append(arguments, &strings.Builder{})
			}
			b := &r.Blocks[i]
			texts[i].WriteString(delta.Block.Text)
			arguments[i].WriteString(delta.Block.Call.Arguments)
			b.Text, b.Call.Arguments = texts[i].String(), arguments[i].String()
			if delta.Block.Call.ID != "" {
				b.Call.ID = delta.Block.Call.ID
			}
			if delta.Block.Call.Name != "" {
				b.Call.Name = delta.Block.Call.Name
			}
		}
		return payload == "[DONE]" || (format != FormatOpenAIChat && d.terminal), nil
	})
	if err != nil {
		return nil, err
	}
	if wire.N <= 1 {
		return nil, fmt.Errorf("upstream buffered response too large")
	}
	if !d.terminal {
		return nil, io.ErrUnexpectedEOF
	}
	r.Finish, r.StopSequence = d.finish, d.stopSequence
	return normalizeProtocolResponse(r)
}

type protocolStream struct {
	format             clientFormat
	model, id          string
	wantUsage, started bool
	sequence           int
	createdAt          int64
	blocks             []responseBlock
	textBuffers        []*strings.Builder
	argumentBuffers    []*strings.Builder
	indexes            map[string]int
	toolIndexes        map[int]int
	err                error
	responseTools      map[string]responseTool
	reasoningScope     responseReplayScope
	bufferBytes        int
	anthropicActive    int
	anthropicNextIndex int
	anthropicIndexes   map[int]int
}

func newProtocolStream(c *gin.Context, f clientFormat, model string, wantUsage bool) *protocolStream {
	return &protocolStream{format: f, model: model, id: protocolID(protocolIDPrefix(f)), createdAt: time.Now().Unix(), wantUsage: wantUsage, indexes: map[string]int{}, toolIndexes: map[int]int{}, anthropicActive: -1, anthropicIndexes: map[int]int{}}
}

func (w *protocolStream) closeAnthropicBlock(c *gin.Context) {
	if w.anthropicActive >= 0 {
		w.emit(c, map[string]any{"type": "content_block_stop", "index": w.anthropicIndexes[w.anthropicActive]})
		w.anthropicActive = -1
	}
}

func (w *protocolStream) openAnthropicBlock(c *gin.Context, i int) {
	if w.anthropicActive == i {
		return
	}
	w.closeAnthropicBlock(c)
	index := w.anthropicNextIndex
	w.anthropicNextIndex++
	w.anthropicActive = i
	w.anthropicIndexes[i] = index
	b := w.blocks[i]
	block := map[string]any{"type": "text", "text": ""}
	if b.Kind == "tool" {
		block = map[string]any{"type": "tool_use", "id": b.Call.ID, "name": b.Call.Name, "input": map[string]any{}}
	} else if b.Kind == "thinking" {
		block = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
	}
	w.emit(c, map[string]any{"type": "content_block_start", "index": index, "content_block": block})
}
func (w *protocolStream) emit(c *gin.Context, v map[string]any) {
	if w.err != nil {
		return
	}
	typ := str(v["type"])
	if w.format == FormatOpenAIResponse {
		v["sequence_number"] = w.sequence
		w.sequence++
		if response := object(v["response"]); response != nil {
			response["created_at"] = w.createdAt
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		w.err = err
		return
	}
	frame := ""
	if typ != "" && (w.format == FormatAnthropic || w.format == FormatOpenAIResponse) {
		frame = "event: " + typ + "\n"
	}
	frame += "data: " + string(b) + "\n\n"
	_, w.err = c.Writer.WriteString(frame)
	if w.err != nil {
		w.err = fmt.Errorf("%w: %w", errDownstreamWrite, w.err)
	}
	if w.err == nil {
		c.Writer.Flush()
	}
}
func (w *protocolStream) chunk(c *gin.Context, delta map[string]any, finish any) {
	w.emit(c, map[string]any{"id": w.id, "object": "chat.completion.chunk", "created": w.createdAt, "model": w.model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
}
func (w *protocolStream) start(c *gin.Context, u upstreamUsage) {
	if w.started {
		return
	}
	w.started = true
	c.Writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)
	switch w.format {
	case FormatAnthropic:
		w.emit(c, map[string]any{"type": "message_start", "message": map[string]any{"id": w.id, "type": "message", "role": "assistant", "model": w.model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": renderProtocolUsage(w.format, u)}})
	case FormatOpenAIResponse:
		r := renderProtocolResponse(w.format, w.model, w.id, &canonResponse{Finish: "stop"})
		r["status"] = "in_progress"
		r["usage"] = nil
		w.emit(c, map[string]any{"type": "response.created", "response": r})
		w.emit(c, map[string]any{"type": "response.in_progress", "response": r})
	case FormatOpenAIChat:
		w.chunk(c, map[string]any{"role": "assistant", "content": ""}, nil)
	}
}
func (w *protocolStream) item(i int, empty bool) map[string]any {
	b := w.blocks[i]
	if empty {
		b.Text = ""
		b.Call.Arguments = ""
	}
	r := renderProtocolResponse(FormatOpenAIResponse, w.model, w.id, &canonResponse{Blocks: []responseBlock{b}, Finish: "stop"})
	item := object(array(r["output"])[0])
	item["id"] = w.itemID(i)
	if empty {
		item["status"] = "in_progress"
		if b.Kind == "text" {
			item["content"] = []any{}
		}
		if b.Kind == "thinking" {
			item["summary"] = []any{}
		}
	}
	return item
}
func (w *protocolStream) itemID(i int) string {
	return responseItemID(w.id, i, w.blocks[i])
}
func (w *protocolStream) push(c *gin.Context, delta streamDelta, u upstreamUsage) error {
	buffered := len(delta.Block.Call.Arguments)
	if w.format == FormatOpenAIResponse {
		buffered += len(delta.Block.Text)
	}
	if w.bufferBytes+buffered > 64<<20 || len(w.blocks) >= 65536 {
		return fmt.Errorf("upstream accumulated response too large")
	}
	w.bufferBytes += buffered
	i, exists := w.indexes[delta.Key]
	if !exists {
		i = len(w.blocks)
		b := delta.Block
		if w.format == FormatOpenAIResponse {
			b = restoreResponseTool(b, w.responseTools)
		}
		b.Text = ""
		b.Call.Arguments = ""
		if b.Kind == "tool" {
			if b.Call.ID == "" {
				b.Call.ID = protocolID("call_")
			}
			if b.Call.Name == "" {
				return fmt.Errorf("upstream tool delta is missing name")
			}
			w.toolIndexes[i] = len(w.toolIndexes)
		}
		w.indexes[delta.Key] = i
		w.blocks = append(w.blocks, b)
		// Builders are pointers so growing these slices never copies a used builder.
		var textBuffer, argumentBuffer *strings.Builder
		if w.format == FormatOpenAIResponse {
			textBuffer = &strings.Builder{}
		}
		if b.Kind == "tool" {
			argumentBuffer = &strings.Builder{}
		}
		w.textBuffers = append(w.textBuffers, textBuffer)
		w.argumentBuffers = append(w.argumentBuffers, argumentBuffer)
		if w.format != FormatAnthropic || b.Kind != "tool" {
			w.start(c, u)
		}
		switch w.format {
		case FormatOpenAIResponse:
			w.emit(c, map[string]any{"type": "response.output_item.added", "output_index": i, "item": w.item(i, true)})
			if b.Kind == "text" {
				w.emit(c, map[string]any{"type": "response.content_part.added", "item_id": w.itemID(i), "output_index": i, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
			}
			if b.Kind == "thinking" {
				w.emit(c, map[string]any{"type": "response.reasoning_summary_part.added", "item_id": w.itemID(i), "output_index": i, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}})
			}
		case FormatOpenAIChat:
			if b.Kind == "tool" {
				w.chunk(c, map[string]any{"tool_calls": []any{map[string]any{"index": w.toolIndexes[i], "id": b.Call.ID, "type": "function", "function": map[string]any{"name": b.Call.Name, "arguments": ""}}}}, nil)
			}
		}
	}
	b := &w.blocks[i]
	if buffer := w.textBuffers[i]; buffer != nil {
		buffer.WriteString(delta.Block.Text)
		b.Text = buffer.String()
	}
	if buffer := w.argumentBuffers[i]; buffer != nil {
		buffer.WriteString(delta.Block.Call.Arguments)
		b.Call.Arguments = buffer.String()
	}
	text := delta.Block.Text
	if b.Kind == "tool" {
		text = delta.Block.Call.Arguments
	}
	if text == "" {
		return w.err
	}
	switch w.format {
	case FormatAnthropic:
		// Interleaved parallel tools are buffered until their arguments validate,
		// then emitted as complete sequential content blocks at finish.
		if b.Kind == "tool" {
			return w.err
		}
		w.openAnthropicBlock(c, i)
		typ, field := "text_delta", "text"
		if b.Kind == "thinking" {
			typ, field = "thinking_delta", "thinking"
		}
		w.emit(c, map[string]any{"type": "content_block_delta", "index": w.anthropicIndexes[i], "delta": map[string]any{"type": typ, field: text}})
	case FormatOpenAIResponse:
		// A Chat/Anthropic delta carries JSON fragments, not the custom tool's
		// raw text. Decode it after the entire argument object has arrived.
		if b.Kind == "tool" && (b.ToolType == "custom" || b.ToolType == "tool_search") {
			return w.err
		}
		typ := "response.output_text.delta"
		if b.Kind == "tool" {
			typ = "response.function_call_arguments.delta"
		}
		if b.Kind == "thinking" {
			typ = "response.reasoning_summary_text.delta"
		}
		event := map[string]any{"type": typ, "item_id": w.itemID(i), "output_index": i, "delta": text}
		if b.Kind == "text" {
			event["content_index"] = 0
		}
		if b.Kind == "thinking" {
			event["summary_index"] = 0
		}
		w.emit(c, event)
	case FormatGemini:
		// Gemini functionCall carries a complete JSON object, so only tool arguments are buffered.
		if b.Kind != "tool" {
			p := map[string]any{"text": text}
			if b.Kind == "thinking" {
				p["thought"] = true
			}
			w.emit(c, map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{p}}, "index": 0}}})
		}
	case FormatOpenAIChat:
		d := map[string]any{"content": text}
		if b.Kind == "thinking" {
			d = map[string]any{"reasoning_content": text}
		}
		if b.Kind == "tool" {
			d = map[string]any{"tool_calls": []any{map[string]any{"index": w.toolIndexes[i], "function": map[string]any{"arguments": text}}}}
		}
		w.chunk(c, d, nil)
	}
	return w.err
}

func (w *protocolStream) finish(c *gin.Context, u upstreamUsage, reason, stopSequence string) error {
	r := &canonResponse{Blocks: w.blocks, Finish: reason, StopSequence: stopSequence, Usage: u}
	if r.hasTools() && r.Finish == "stop" {
		r.Finish = "tool_calls"
	}
	for i := range w.blocks {
		b := &w.blocks[i]
		if b.Kind == "tool" {
			if b.Call.Arguments == "" {
				if w.format == FormatGemini {
					b.Call.Arguments = "{}"
				} else {
					if err := w.push(c, streamDelta{Key: findStreamKey(w.indexes, i), Block: responseBlock{Kind: "tool", Call: canonCall{Arguments: "{}"}}}, u); err != nil {
						return err
					}
				}
			}
			var args map[string]any
			if json.Unmarshal([]byte(b.Call.Arguments), &args) != nil || args == nil {
				return fmt.Errorf("upstream streamed tool arguments are not a JSON object")
			}
			if err := validateResponseToolArguments(*b); err != nil {
				return err
			}
		}
	}
	w.start(c, u)
	switch w.format {
	case FormatAnthropic:
		w.closeAnthropicBlock(c)
		for i, block := range w.blocks {
			if block.Kind != "tool" {
				continue
			}
			w.openAnthropicBlock(c, i)
			w.emit(c, map[string]any{"type": "content_block_delta", "index": w.anthropicIndexes[i], "delta": map[string]any{"type": "input_json_delta", "partial_json": block.Call.Arguments}})
			w.closeAnthropicBlock(c)
		}
		var stop any
		if stopSequence != "" {
			stop = stopSequence
		}
		w.emit(c, map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": anthropicStop(r), "stop_sequence": stop}, "usage": renderProtocolUsage(w.format, u)})
		w.emit(c, map[string]any{"type": "message_stop"})
	case FormatOpenAIResponse:
		for i, b := range w.blocks {
			id := w.itemID(i)
			switch b.Kind {
			case "text":
				w.emit(c, map[string]any{"type": "response.output_text.done", "item_id": id, "output_index": i, "content_index": 0, "text": b.Text})
				w.emit(c, map[string]any{"type": "response.content_part.done", "item_id": id, "output_index": i, "content_index": 0, "part": map[string]any{"type": "output_text", "text": b.Text, "annotations": []any{}}})
			case "tool":
				switch b.ToolType {
				case "custom":
					input := str(responsesToolItem(b, id)["input"])
					if input != "" {
						w.emit(c, map[string]any{"type": "response.custom_tool_call_input.delta", "item_id": id, "output_index": i, "delta": input})
					}
					w.emit(c, map[string]any{"type": "response.custom_tool_call_input.done", "item_id": id, "output_index": i, "input": input})
				case "tool_search":
					// Client tool search is materialized from output_item.done.
				default:
					w.emit(c, map[string]any{"type": "response.function_call_arguments.done", "item_id": id, "output_index": i, "name": b.Call.Name, "arguments": b.Call.Arguments})
				}
			case "thinking":
				w.emit(c, map[string]any{"type": "response.reasoning_summary_text.done", "item_id": id, "output_index": i, "summary_index": 0, "text": b.Text})
				w.emit(c, map[string]any{"type": "response.reasoning_summary_part.done", "item_id": id, "output_index": i, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": b.Text}})
			}
			w.emit(c, map[string]any{"type": "response.output_item.done", "output_index": i, "item": w.item(i, false)})
		}
		body := renderProtocolResponse(w.format, w.model, w.id, r)
		cacheResponseOutputReasoning(w.reasoningScope, body)
		typ := "response.completed"
		if body["status"] == "incomplete" {
			typ = "response.incomplete"
		}
		w.emit(c, map[string]any{"type": typ, "response": body})
	case FormatGemini:
		tools := []responseBlock{}
		for _, b := range w.blocks {
			if b.Kind == "tool" {
				tools = append(tools, b)
			}
		}
		body := renderProtocolResponse(w.format, w.model, w.id, &canonResponse{Blocks: tools, Finish: r.Finish, Usage: u})
		w.emit(c, body)
	default:
		w.chunk(c, map[string]any{}, chatStop(r))
		if w.wantUsage {
			w.emit(c, map[string]any{"id": w.id, "object": "chat.completion.chunk", "created": w.createdAt, "model": w.model, "choices": []any{}, "usage": renderProtocolUsage(w.format, u)})
		}
		if w.err == nil {
			_, w.err = c.Writer.WriteString("data: [DONE]\n\n")
			if w.err != nil {
				w.err = fmt.Errorf("%w: %w", errDownstreamWrite, w.err)
			} else {
				c.Writer.Flush()
			}
		}
	}
	return w.err
}
func findStreamKey(indexes map[string]int, index int) string {
	for k, i := range indexes {
		if i == index {
			return k
		}
	}
	return ""
}
func (w *protocolStream) fail(c *gin.Context) {
	// Before any client output, leave errors to the bounded upstream retry loop.
	if !w.started && !c.Writer.Written() {
		return
	}
	// Never emit a successful completion after an upstream error or truncated stream.
	if w.format == FormatOpenAIResponse {
		w.emit(c, map[string]any{"type": "response.failed", "response": map[string]any{"id": w.id, "object": "response", "status": "failed", "error": map[string]any{"code": "upstream_error", "message": "upstream stream interrupted"}}})
	} else if w.format == FormatAnthropic {
		w.emit(c, map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "upstream stream interrupted"}})
	} else {
		w.emit(c, map[string]any{"error": map[string]any{"message": "upstream stream interrupted", "code": 502}})
	}
}

func relayProtocolSSE(c *gin.Context, resp *http.Response, format clientFormat, task *relayTask, info *relayInfo) error {
	w := newProtocolStream(c, task.format, task.model, task.wantUsageChunk)
	if task.canon != nil {
		w.responseTools = task.canon.ResponseTools
	}
	w.reasoningScope = responseReplayScope{task.apiKeyID, info.UpstreamID, task.model, info.UpstreamModel}
	d := newProtocolDecoder(format)
	err := readSSEFrames(resp.Body, func(payload string) (bool, error) {
		deltas, u, err := d.decode(payload)
		info.mergeUsage(u)
		if err != nil {
			return false, err
		}
		for _, delta := range deltas {
			info.OutputText.WriteString(delta.Block.Text)
			info.OutputText.WriteString(delta.Block.Call.Arguments)
			if err := w.push(c, delta, info.protocolUsage()); err != nil {
				return false, err
			}
		}
		return payload == "[DONE]" || (format == FormatAnthropic && d.terminal) || (format == FormatOpenAIResponse && d.terminal), nil
	})
	if err == nil && !d.terminal {
		err = io.ErrUnexpectedEOF
	}
	if err == nil {
		err = w.finish(c, info.protocolUsage(), d.finish, d.stopSequence)
	}
	if err != nil {
		w.fail(c)
	}
	return err
}
