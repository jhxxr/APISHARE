package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type streamDelta struct {
	Key   string
	Block responseBlock
}
type protocolDecoder struct {
	format       clientFormat
	tools        map[int]canonCall
	arguments    map[int]string
	terminal     bool
	finish       string
	stopSequence string
}

func newProtocolDecoder(f clientFormat) *protocolDecoder {
	return &protocolDecoder{format: f, tools: map[int]canonCall{}, arguments: map[int]string{}, finish: "stop"}
}
func (d *protocolDecoder) decode(payload string) ([]streamDelta, upstreamUsage, error) {
	if payload == "[DONE]" {
		if d.format != FormatOpenAIChat {
			return nil, upstreamUsage{}, fmt.Errorf("unexpected upstream DONE event")
		}
		d.terminal = true
		return nil, upstreamUsage{}, nil
	}
	var j map[string]any
	if json.Unmarshal([]byte(payload), &j) != nil || j == nil {
		return nil, upstreamUsage{}, fmt.Errorf("invalid upstream SSE JSON")
	}
	if j["error"] != nil || str(j["type"]) == "error" || str(j["type"]) == "response.failed" {
		return nil, upstreamUsage{}, fmt.Errorf("upstream stream returned an error")
	}
	out := []streamDelta{}
	u := upstreamUsage{}
	add := func(key, kind, text string) {
		if text != "" {
			out = append(out, streamDelta{key, responseBlock{Kind: kind, Text: text}})
		}
	}
	tool := func(index int, id, name, args string) {
		c := d.tools[index]
		if id != "" {
			c.ID = id
		}
		if name != "" {
			c.Name += name
		}
		d.tools[index] = c
		d.arguments[index] += args
		out = append(out, streamDelta{fmt.Sprintf("tool_%d", index), responseBlock{Kind: "tool", Call: canonCall{c.ID, c.Name, args}}})
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
			add("thinking", "thinking", str(b["reasoning_content"]))
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
				args := ""
				if input := object(b["input"]); len(input) > 0 {
					args = jsonText(input)
				}
				tool(i, str(b["id"]), str(b["name"]), args)
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
		case "message_delta":
			b := object(j["delta"])
			if s := str(b["stop_reason"]); s != "" {
				d.finish = normalizeFinish(s)
			}
			d.stopSequence = str(b["stop_sequence"])
			u = readUsage(object(j["usage"]), d.format)
		case "message_stop":
			d.terminal = true
		}
	case FormatGemini:
		u = readUsage(object(j["usageMetadata"]), d.format)
		for _, v := range array(j["candidates"]) {
			c := object(v)
			if intFromAny(c["index"]) != 0 {
				continue
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
		case "response.refusal.delta":
			add(key, "text", str(j["delta"]))
			d.finish = "content_filter"
		case "response.reasoning_summary_text.delta":
			add(fmt.Sprintf("reason_%d_%d", i, intFromAny(j["summary_index"])), "thinking", str(j["delta"]))
		case "response.function_call_arguments.delta":
			tool(i, "", "", str(j["delta"]))
		case "response.function_call_arguments.done":
			if d.arguments[i] == "" {
				tool(i, "", "", str(j["arguments"]))
			}
		case "response.completed", "response.incomplete":
			r := object(j["response"])
			u = readUsage(object(r["usage"]), d.format)
			d.terminal = true
			if str(j["type"]) == "response.incomplete" {
				d.finish = "length"
				if str(object(r["incomplete_details"])["reason"]) == "content_filter" {
					d.finish = "content_filter"
				}
			}
		}
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

type protocolStream struct {
	format             clientFormat
	model, id          string
	wantUsage, started bool
	sequence           int
	createdAt          int64
	blocks             []responseBlock
	indexes            map[string]int
	toolIndexes        map[int]int
	err                error
}

func newProtocolStream(c *gin.Context, f clientFormat, model string, wantUsage bool) *protocolStream {
	c.Writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	return &protocolStream{format: f, model: model, id: protocolID(protocolIDPrefix(f)), createdAt: time.Now().Unix(), wantUsage: wantUsage, indexes: map[string]int{}, toolIndexes: map[int]int{}}
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
	c.Writer.WriteHeader(http.StatusOK)
	switch w.format {
	case FormatAnthropic:
		w.emit(c, map[string]any{"type": "message_start", "message": map[string]any{"id": w.id, "type": "message", "role": "assistant", "model": w.model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": u.Prompt, "output_tokens": 0}}})
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
	item["id"] = fmt.Sprintf("%s_%d", w.id, i)
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
func (w *protocolStream) push(c *gin.Context, delta streamDelta, u upstreamUsage) error {
	w.start(c, u)
	i, exists := w.indexes[delta.Key]
	if !exists {
		i = len(w.blocks)
		b := delta.Block
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
		switch w.format {
		case FormatAnthropic:
			block := map[string]any{"type": "text", "text": ""}
			if b.Kind == "tool" {
				block = map[string]any{"type": "tool_use", "id": b.Call.ID, "name": b.Call.Name, "input": map[string]any{}}
			} else if b.Kind == "thinking" {
				block = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
			}
			w.emit(c, map[string]any{"type": "content_block_start", "index": i, "content_block": block})
		case FormatOpenAIResponse:
			w.emit(c, map[string]any{"type": "response.output_item.added", "output_index": i, "item": w.item(i, true)})
			if b.Kind == "text" {
				w.emit(c, map[string]any{"type": "response.content_part.added", "item_id": fmt.Sprintf("%s_%d", w.id, i), "output_index": i, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
			}
			if b.Kind == "thinking" {
				w.emit(c, map[string]any{"type": "response.reasoning_summary_part.added", "item_id": fmt.Sprintf("%s_%d", w.id, i), "output_index": i, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}})
			}
		case FormatOpenAIChat:
			if b.Kind == "tool" {
				w.chunk(c, map[string]any{"tool_calls": []any{map[string]any{"index": w.toolIndexes[i], "id": b.Call.ID, "type": "function", "function": map[string]any{"name": b.Call.Name, "arguments": ""}}}}, nil)
			}
		}
	}
	b := &w.blocks[i]
	b.Text += delta.Block.Text
	b.Call.Arguments += delta.Block.Call.Arguments
	text := delta.Block.Text
	if b.Kind == "tool" {
		text = delta.Block.Call.Arguments
	}
	if text == "" {
		return w.err
	}
	switch w.format {
	case FormatAnthropic:
		typ, field := "text_delta", "text"
		if b.Kind == "tool" {
			typ, field = "input_json_delta", "partial_json"
		}
		if b.Kind == "thinking" {
			typ, field = "thinking_delta", "thinking"
		}
		w.emit(c, map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": typ, field: text}})
	case FormatOpenAIResponse:
		typ := "response.output_text.delta"
		if b.Kind == "tool" {
			typ = "response.function_call_arguments.delta"
		}
		if b.Kind == "thinking" {
			typ = "response.reasoning_summary_text.delta"
		}
		event := map[string]any{"type": typ, "item_id": fmt.Sprintf("%s_%d", w.id, i), "output_index": i, "delta": text}
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
	w.start(c, u)
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
		}
	}
	switch w.format {
	case FormatAnthropic:
		for i := range w.blocks {
			w.emit(c, map[string]any{"type": "content_block_stop", "index": i})
		}
		var stop any
		if stopSequence != "" {
			stop = stopSequence
		}
		w.emit(c, map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": anthropicStop(r), "stop_sequence": stop}, "usage": map[string]any{"input_tokens": u.Prompt, "output_tokens": u.Completion}})
		w.emit(c, map[string]any{"type": "message_stop"})
	case FormatOpenAIResponse:
		for i, b := range w.blocks {
			id := fmt.Sprintf("%s_%d", w.id, i)
			switch b.Kind {
			case "text":
				w.emit(c, map[string]any{"type": "response.output_text.done", "item_id": id, "output_index": i, "content_index": 0, "text": b.Text})
				w.emit(c, map[string]any{"type": "response.content_part.done", "item_id": id, "output_index": i, "content_index": 0, "part": map[string]any{"type": "output_text", "text": b.Text, "annotations": []any{}}})
			case "tool":
				w.emit(c, map[string]any{"type": "response.function_call_arguments.done", "item_id": id, "output_index": i, "name": b.Call.Name, "arguments": b.Call.Arguments})
			case "thinking":
				w.emit(c, map[string]any{"type": "response.reasoning_summary_text.done", "item_id": id, "output_index": i, "summary_index": 0, "text": b.Text})
				w.emit(c, map[string]any{"type": "response.reasoning_summary_part.done", "item_id": id, "output_index": i, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": b.Text}})
			}
			w.emit(c, map[string]any{"type": "response.output_item.done", "output_index": i, "item": w.item(i, false)})
		}
		body := renderProtocolResponse(w.format, w.model, w.id, r)
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
			w.emit(c, map[string]any{"id": w.id, "object": "chat.completion.chunk", "created": w.createdAt, "model": w.model, "choices": []any{}, "usage": map[string]any{"prompt_tokens": u.Prompt, "completion_tokens": u.Completion, "total_tokens": u.Prompt + u.Completion}})
		}
		if w.err == nil {
			_, w.err = c.Writer.WriteString("data: [DONE]\n\n")
			c.Writer.Flush()
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
			if err := w.push(c, delta, upstreamUsage{Prompt: info.PromptTok, Completion: info.CompletionTok}); err != nil {
				return false, err
			}
		}
		return payload == "[DONE]" || (format == FormatAnthropic && d.terminal) || (format == FormatOpenAIResponse && d.terminal), nil
	})
	if err == nil && !d.terminal {
		err = io.ErrUnexpectedEOF
	}
	if err == nil {
		err = w.finish(c, upstreamUsage{Prompt: info.PromptTok, Completion: info.CompletionTok, InKnown: info.InKnown, OutKnown: info.OutKnown}, d.finish, d.stopSequence)
	}
	if err != nil {
		w.fail(c)
	}
	return err
}
