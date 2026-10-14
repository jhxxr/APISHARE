package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type responseBlock struct {
	Kind, Text          string
	Call                canonCall
	ToolType, Namespace string
}
type canonResponse struct {
	Blocks               []responseBlock
	Finish, StopSequence string
	Usage                upstreamUsage
}

func protocolID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(b[:])
}

func protocolIDPrefix(format clientFormat) string {
	return map[clientFormat]string{FormatAnthropic: "msg_", FormatOpenAIChat: "chatcmpl-", FormatOpenAIResponse: "resp_", FormatGemini: "conv_"}[format]
}

// Output item IDs and tool call IDs have different lifetimes. Keep call IDs
// untouched, but use the item type's prefix when clients replay output items.
func responseItemID(responseID string, index int, block responseBlock) string {
	prefix := "msg_"
	switch block.Kind {
	case "thinking":
		prefix = "rs_"
	case "tool":
		prefix = "fc_"
		if block.ToolType == "custom" {
			prefix = "ctc_"
		} else if block.ToolType == "tool_search" {
			prefix = "tsc_"
		}
	}
	return fmt.Sprintf("%s%s_%d", prefix, strings.TrimPrefix(responseID, "resp_"), index)
}
func readUsage(j map[string]any, format clientFormat) upstreamUsage {
	u := upstreamUsage{}
	if j == nil {
		return u
	}
	in, out := "prompt_tokens", "completion_tokens"
	if format == FormatAnthropic || format == FormatOpenAIResponse {
		in, out = "input_tokens", "output_tokens"
	}
	if format == FormatGemini {
		in, out = "promptTokenCount", "candidatesTokenCount"
	}
	if v, ok := j[in]; ok {
		u.Prompt = intFromAny(v)
		u.InKnown = true
	}
	if v, ok := j[out]; ok {
		u.Completion = intFromAny(v)
		u.OutKnown = true
	}
	return u
}
func normalizeFinish(reason string) string {
	switch reason {
	case "max_tokens", "MAX_TOKENS", "length":
		return "length"
	case "tool_use", "tool_calls", "function_call":
		return "tool_calls"
	case "SAFETY", "RECITATION", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION", "ESCALATION", "content_filter", "refusal":
		return "content_filter"
	case "stop_sequence":
		return "stop_sequence"
	default:
		return "stop"
	}
}
func (r *canonResponse) text() string {
	s := ""
	for _, b := range r.Blocks {
		if b.Kind != "tool" {
			s += b.Text
		} else {
			s += b.Call.Arguments
		}
	}
	return s
}
func (r *canonResponse) hasTools() bool {
	for _, b := range r.Blocks {
		if b.Kind == "tool" {
			return true
		}
	}
	return false
}

func parseProtocolResponse(format clientFormat, body []byte) (*canonResponse, error) {
	j, decodeErr := decodeProtocolObject(body)
	if decodeErr != nil {
		return nil, fmt.Errorf("upstream returned invalid JSON")
	}
	if j["error"] != nil {
		return nil, fmt.Errorf("upstream returned an error response")
	}
	r := &canonResponse{Finish: "stop"}
	switch format {
	case FormatOpenAIChat:
		choices := array(j["choices"])
		if len(choices) == 0 {
			return nil, fmt.Errorf("upstream response is missing choices")
		}
		ch := object(choices[0])
		msg := object(ch["message"])
		if msg == nil {
			return nil, fmt.Errorf("upstream response is missing message")
		}
		if text := chatReasoningText(msg); text != "" {
			r.Blocks = append(r.Blocks, responseBlock{Kind: "thinking", Text: text})
		}
		if text := contentText(msg["content"]); text != "" {
			r.Blocks = append(r.Blocks, responseBlock{Kind: "text", Text: text})
		}
		if text := str(msg["refusal"]); text != "" {
			r.Blocks = append(r.Blocks, responseBlock{Kind: "text", Text: text})
			r.Finish = "content_filter"
		}
		for _, v := range array(msg["tool_calls"]) {
			c := object(v)
			f := object(c["function"])
			if str(c["type"]) != "function" || f == nil {
				return nil, unsupported("upstream non-function tool call")
			}
			r.Blocks = append(r.Blocks, responseBlock{Kind: "tool", Call: canonCall{str(c["id"]), str(f["name"]), str(f["arguments"])}})
		}
		if str(ch["finish_reason"]) != "" {
			r.Finish = normalizeFinish(str(ch["finish_reason"]))
		}
		r.Usage = readUsage(object(j["usage"]), format)
	case FormatAnthropic:
		if _, ok := j["content"].([]any); !ok {
			return nil, fmt.Errorf("upstream response is missing content")
		}
		for _, v := range array(j["content"]) {
			b := object(v)
			switch str(b["type"]) {
			case "text":
				r.Blocks = append(r.Blocks, responseBlock{Kind: "text", Text: str(b["text"])})
			case "thinking":
				r.Blocks = append(r.Blocks, responseBlock{Kind: "thinking", Text: str(b["thinking"])})
			case "tool_use":
				r.Blocks = append(r.Blocks, responseBlock{Kind: "tool", Call: canonCall{str(b["id"]), str(b["name"]), jsonText(b["input"])}})
			default:
				return nil, unsupported("upstream content block " + str(b["type"]))
			}
		}
		r.Finish = normalizeFinish(str(j["stop_reason"]))
		r.StopSequence = str(j["stop_sequence"])
		r.Usage = readUsage(object(j["usage"]), format)
	case FormatGemini:
		if geminiPromptBlocked(j) {
			return nil, fmt.Errorf("upstream Gemini prompt blocked: %s", str(object(j["promptFeedback"])["blockReason"]))
		}
		cs := array(j["candidates"])
		c := geminiPrimaryCandidate(cs)
		if c == nil {
			return nil, fmt.Errorf("upstream response is missing candidates")
		}
		if err := geminiFinishError(str(c["finishReason"])); err != nil {
			return nil, err
		}
		for _, v := range array(object(c["content"])["parts"]) {
			p := object(v)
			if f := object(p["functionCall"]); f != nil {
				r.Blocks = append(r.Blocks, responseBlock{Kind: "tool", Call: canonCall{str(f["id"]), str(f["name"]), jsonText(f["args"])}})
			} else if s, ok := p["text"].(string); ok {
				kind := "text"
				if p["thought"] == true {
					kind = "thinking"
				}
				r.Blocks = append(r.Blocks, responseBlock{Kind: kind, Text: s})
			} else if p["thoughtSignature"] != nil && len(p) == 1 {
				// A signature-only trailer carries no portable output content.
			} else {
				return nil, unsupported("upstream Gemini non-text part")
			}
		}
		r.Finish = normalizeFinish(str(c["finishReason"]))
		r.Usage = readUsage(object(j["usageMetadata"]), format)
	case FormatOpenAIResponse:
		if _, ok := j["output"].([]any); !ok {
			return nil, fmt.Errorf("upstream response is missing output")
		}
		if str(j["status"]) == "failed" {
			return nil, fmt.Errorf("upstream response failed")
		}
		for _, v := range array(j["output"]) {
			o := object(v)
			switch str(o["type"]) {
			case "message":
				for _, v := range array(o["content"]) {
					p := object(v)
					switch str(p["type"]) {
					case "output_text":
						r.Blocks = append(r.Blocks, responseBlock{Kind: "text", Text: str(p["text"])})
					case "refusal":
						r.Blocks = append(r.Blocks, responseBlock{Kind: "text", Text: str(p["refusal"])})
						r.Finish = "content_filter"
					default:
						return nil, unsupported("upstream Responses content")
					}
				}
			case "function_call":
				r.Blocks = append(r.Blocks, responseBlock{Kind: "tool", Call: canonCall{str(o["call_id"]), str(o["name"]), str(o["arguments"])}})
			case "reasoning":
				parts := array(o["summary"])
				if len(parts) == 0 {
					parts = array(o["content"])
				}
				for _, v := range parts {
					r.Blocks = append(r.Blocks, responseBlock{Kind: "thinking", Text: str(object(v)["text"])})
				}
			default:
				return nil, unsupported("upstream Responses output " + str(o["type"]))
			}
		}
		if str(j["status"]) == "incomplete" {
			r.Finish = "length"
			if str(object(j["incomplete_details"])["reason"]) == "content_filter" {
				r.Finish = "content_filter"
			}
		}
		r.Usage = readUsage(object(j["usage"]), format)
	}
	return normalizeProtocolResponse(r)
}

func normalizeProtocolResponse(r *canonResponse) (*canonResponse, error) {
	if r.hasTools() && r.Finish == "stop" {
		r.Finish = "tool_calls"
	}
	for i := range r.Blocks {
		b := &r.Blocks[i]
		if b.Kind == "tool" {
			if b.Call.ID == "" {
				b.Call.ID = protocolID("call_")
			}
			if b.Call.Name == "" {
				return nil, fmt.Errorf("upstream tool call is missing name")
			}
			if b.Call.Arguments == "" {
				b.Call.Arguments = "{}"
			}
			var args map[string]any
			if json.Unmarshal([]byte(b.Call.Arguments), &args) != nil || args == nil {
				return nil, fmt.Errorf("upstream tool arguments are not a JSON object")
			}
		}
	}
	return r, nil
}

func anthropicStop(r *canonResponse) string {
	switch r.Finish {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "stop_sequence":
		return "stop_sequence"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}
func geminiStop(r *canonResponse) string {
	switch r.Finish {
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	default:
		return "STOP"
	}
}
func chatStop(r *canonResponse) string {
	if r.Finish == "stop_sequence" {
		return "stop"
	}
	return r.Finish
}

func renderProtocolResponse(format clientFormat, model, id string, r *canonResponse) map[string]any {
	if id == "" {
		id = protocolID(protocolIDPrefix(format))
	}
	p, q := r.Usage.Prompt, r.Usage.Completion
	switch format {
	case FormatAnthropic:
		blocks := []any{}
		for _, b := range r.Blocks {
			switch b.Kind {
			case "text":
				blocks = append(blocks, map[string]any{"type": "text", "text": b.Text})
			case "thinking":
				blocks = append(blocks, map[string]any{"type": "thinking", "thinking": b.Text, "signature": ""})
			case "tool":
				args, _ := decodeProtocolObject([]byte(b.Call.Arguments))
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": b.Call.ID, "name": b.Call.Name, "input": args})
			}
		}
		var stop any
		if r.StopSequence != "" {
			stop = r.StopSequence
		}
		return map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": blocks, "stop_reason": anthropicStop(r), "stop_sequence": stop, "usage": map[string]any{"input_tokens": p, "output_tokens": q}}
	case FormatGemini:
		parts := []any{}
		for _, b := range r.Blocks {
			if b.Kind == "tool" {
				args, _ := decodeProtocolObject([]byte(b.Call.Arguments))
				parts = append(parts, map[string]any{"functionCall": map[string]any{"id": b.Call.ID, "name": b.Call.Name, "args": args}})
			} else {
				j := map[string]any{"text": b.Text}
				if b.Kind == "thinking" {
					j["thought"] = true
				}
				parts = append(parts, j)
			}
		}
		return map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": parts}, "index": 0, "finishReason": geminiStop(r)}}, "usageMetadata": map[string]any{"promptTokenCount": p, "candidatesTokenCount": q, "totalTokenCount": p + q}, "modelVersion": model}
	case FormatOpenAIResponse:
		output := []any{}
		for i, b := range r.Blocks {
			itemID := responseItemID(id, i, b)
			switch b.Kind {
			case "text":
				output = append(output, map[string]any{"id": itemID, "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": b.Text, "annotations": []any{}}}})
			case "tool":
				output = append(output, responsesToolItem(b, itemID))
			case "thinking":
				output = append(output, map[string]any{"id": itemID, "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": b.Text}}})
			}
		}
		status := "completed"
		var incomplete any
		if r.Finish == "length" || r.Finish == "content_filter" {
			status = "incomplete"
			reason := "max_output_tokens"
			if r.Finish == "content_filter" {
				reason = "content_filter"
			}
			incomplete = map[string]any{"reason": reason}
		}
		return map[string]any{"id": id, "object": "response", "created_at": time.Now().Unix(), "status": status, "error": nil, "incomplete_details": incomplete, "model": model, "output": output, "usage": map[string]any{"input_tokens": p, "output_tokens": q, "total_tokens": p + q}}
	default:
		text, thinking := "", ""
		calls := []any{}
		for _, b := range r.Blocks {
			switch b.Kind {
			case "text":
				text += b.Text
			case "thinking":
				thinking += b.Text
			case "tool":
				calls = append(calls, map[string]any{"id": b.Call.ID, "type": "function", "function": map[string]any{"name": b.Call.Name, "arguments": b.Call.Arguments}})
			}
		}
		var content any = text
		if text == "" && len(calls) > 0 {
			content = nil
		}
		msg := map[string]any{"role": "assistant", "content": content}
		if len(calls) > 0 {
			msg["tool_calls"] = calls
		}
		if thinking != "" {
			msg["reasoning_content"] = thinking
		}
		return map[string]any{"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": chatStop(r)}}, "usage": map[string]any{"prompt_tokens": p, "completion_tokens": q, "total_tokens": p + q}}
	}
}
