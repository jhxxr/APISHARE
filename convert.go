package main

// 多格式转换核心：四种客户端格式（OpenAI Chat / OpenAI Responses / Anthropic / Gemini）
// 通过内部规范化表示（canonReq）互相转换。
//
// 路径分两类：
//   - 原生透传：客户端格式与其原生上游类型匹配时，请求/响应字节原样转发
//     （工具调用、图片、思考块等全功能无损）；
//   - 跨格式转换：客户端格式与上游类型不一致时，走 canon 转换，支持文本对话
//     （system、多轮、max_tokens、temperature、流式逐字转换、真实 usage 计费）。
//     带 tools 的跨格式请求会明确报错，请为该模型配置原生格式上游。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// ---------- 客户端格式 ----------

type clientFormat string

const (
	FormatOpenAIChat     clientFormat = "openai-chat"      // POST /v1/chat/completions
	FormatOpenAIResponse clientFormat = "openai-responses" // POST /v1/responses
	FormatAnthropic      clientFormat = "anthropic"        // POST /v1/messages
	FormatGemini         clientFormat = "gemini"           // POST /v1beta/models/{model}:generateContent
)

func (f clientFormat) nativeUpstreamType() string {
	switch f {
	case FormatAnthropic:
		return "anthropic"
	case FormatGemini:
		return "gemini"
	default:
		return "openai"
	}
}

// ---------- 规范化请求 ----------

type canonMsg struct {
	Role string // user | assistant（system 单独放在 System）
	Text string
}

type canonReq struct {
	Model       string
	System      string
	Messages    []canonMsg
	MaxTokens   int
	Temperature *float64
	Stream      bool
	HasTools    bool
	InputText   string // system + 首条 user 消息，用于会话指纹与 token 估算
}

// ---------- 通用 JSON 小工具 ----------

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// flattenText 从各种 content/parts 形态中提取纯文本：
// 字符串、[{"text":...}]（OpenAI parts / Anthropic blocks / Gemini parts）、
// {"parts":[{"text":...}]}（Gemini systemInstruction）、["a","b"]（embeddings input）。
func flattenText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var strArr []string
	if json.Unmarshal(raw, &strArr) == nil {
		return strings.Join(strArr, "\n")
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var sb strings.Builder
		for _, b := range blocks {
			sb.WriteString(b.Text)
		}
		return sb.String()
	}
	var obj struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		var sb strings.Builder
		for _, p := range obj.Parts {
			sb.WriteString(p.Text)
		}
		return sb.String()
	}
	return ""
}

func rawMap(body []byte) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("invalid JSON body")
	}
	return m, nil
}

func boolField(m map[string]json.RawMessage, key string) bool {
	var b bool
	json.Unmarshal(m[key], &b)
	return b
}

func intField(m map[string]json.RawMessage, keys ...string) int {
	for _, k := range keys {
		var n int
		if json.Unmarshal(m[k], &n) == nil && n > 0 {
			return n
		}
	}
	return 0
}

func floatPtrField(m map[string]json.RawMessage, key string) *float64 {
	var f float64
	if json.Unmarshal(m[key], &f) == nil {
		return &f
	}
	return nil
}

func buildInputText(system string, msgs []canonMsg) string {
	var sb strings.Builder
	if system != "" {
		sb.WriteString(truncate(system, 1000))
	}
	for _, m := range msgs {
		if m.Role == "user" {
			sb.WriteString("\n<u>")
			sb.WriteString(truncate(m.Text, 4000))
			break // 指纹只取到首条 user 消息，保证多轮请求指纹稳定
		}
		sb.WriteString("\n<s>")
		sb.WriteString(truncate(m.Text, 1000))
	}
	return sb.String()
}

// ---------- 客户端请求解析（→ canon） ----------

func parseClientRequest(format clientFormat, body []byte, pathModel string) (*canonReq, error) {
	switch format {
	case FormatOpenAIChat:
		return parseOpenAIChat(body)
	case FormatOpenAIResponse:
		return parseOpenAIResponses(body)
	case FormatAnthropic:
		return parseAnthropicMessages(body)
	case FormatGemini:
		return parseGeminiBody(body, pathModel)
	}
	return nil, fmt.Errorf("unknown client format")
}

func parseOpenAIChat(body []byte) (*canonReq, error) {
	m, err := rawMap(body)
	if err != nil {
		return nil, err
	}
	var model string
	json.Unmarshal(m["model"], &model)
	cr := &canonReq{
		Model:       model,
		Stream:      boolField(m, "stream"),
		MaxTokens:   intField(m, "max_tokens", "max_completion_tokens"),
		Temperature: floatPtrField(m, "temperature"),
	}
	_, cr.HasTools = m["tools"]
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(m["messages"], &msgs) == nil {
		for _, msg := range msgs {
			role := msg.Role
			if role == "assistant" {
				role = "assistant"
			} else if role == "user" {
				role = "user"
			} else {
				role = "assistant" // system/developer/tool 归入前置上下文
				if len(cr.Messages) == 0 {
					cr.System = truncate(cr.System+" "+flattenText(msg.Content), 4000)
					continue
				}
			}
			cr.Messages = append(cr.Messages, canonMsg{Role: role, Text: flattenText(msg.Content)})
		}
	}
	if len(cr.Messages) == 0 { // embeddings 等无 messages 的请求
		cr.InputText = truncate(flattenText(m["input"]), 4000)
	} else {
		cr.InputText = buildInputText("", cr.Messages)
	}
	return cr, nil
}

func parseOpenAIResponses(body []byte) (*canonReq, error) {
	m, err := rawMap(body)
	if err != nil {
		return nil, err
	}
	var typed struct {
		Model           string   `json:"model"`
		Instructions    string   `json:"instructions"`
		Stream          bool     `json:"stream"`
		MaxOutputTokens int      `json:"max_output_tokens"`
		Temperature     *float64 `json:"temperature"`
	}
	json.Unmarshal(body, &typed)
	cr := &canonReq{
		Model:       typed.Model,
		System:      typed.Instructions,
		Stream:      typed.Stream,
		MaxTokens:   typed.MaxOutputTokens,
		Temperature: typed.Temperature,
	}
	_, cr.HasTools = m["tools"]

	var input string
	if json.Unmarshal(m["input"], &input) == nil {
		cr.Messages = append(cr.Messages, canonMsg{Role: "user", Text: input})
	} else {
		var items []struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m["input"], &items) == nil {
			for _, it := range items {
				// function_call / function_call_output / reasoning 等无法跨格式转换
				if it.Type != "" && it.Type != "message" {
					cr.HasTools = true
					continue
				}
				role := it.Role
				if role != "assistant" {
					role = "user"
				}
				cr.Messages = append(cr.Messages, canonMsg{Role: role, Text: flattenText(it.Content)})
			}
		}
	}
	cr.InputText = buildInputText(cr.System, cr.Messages)
	return cr, nil
}

func parseAnthropicMessages(body []byte) (*canonReq, error) {
	m, err := rawMap(body)
	if err != nil {
		return nil, err
	}
	var typed struct {
		Model       string   `json:"model"`
		Stream      bool     `json:"stream"`
		MaxTokens   int      `json:"max_tokens"`
		Temperature *float64 `json:"temperature"`
	}
	json.Unmarshal(body, &typed)
	cr := &canonReq{
		Model:       typed.Model,
		Stream:      typed.Stream,
		MaxTokens:   typed.MaxTokens,
		Temperature: typed.Temperature,
	}
	_, cr.HasTools = m["tools"]
	cr.System = flattenText(m["system"])
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(m["messages"], &msgs) == nil {
		for _, msg := range msgs {
			role := "user"
			if msg.Role == "assistant" {
				role = "assistant"
			}
			cr.Messages = append(cr.Messages, canonMsg{Role: role, Text: flattenText(msg.Content)})
		}
	}
	cr.InputText = buildInputText(cr.System, cr.Messages)
	return cr, nil
}

func parseGeminiBody(body []byte, pathModel string) (*canonReq, error) {
	m, err := rawMap(body)
	if err != nil {
		return nil, err
	}
	cr := &canonReq{Model: pathModel}
	cr.System = flattenText(m["systemInstruction"])
	_, cr.HasTools = m["tools"]
	var gc struct {
		MaxOutputTokens int      `json:"maxOutputTokens"`
		Temperature     *float64 `json:"temperature"`
	}
	json.Unmarshal(m["generationConfig"], &gc)
	cr.MaxTokens = gc.MaxOutputTokens
	cr.Temperature = gc.Temperature
	var contents []struct {
		Role  string `json:"role"`
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}
	if json.Unmarshal(m["contents"], &contents) == nil {
		for _, c := range contents {
			role := "user"
			if c.Role == "model" {
				role = "assistant"
			}
			var sb strings.Builder
			for _, p := range c.Parts {
				sb.WriteString(p.Text)
			}
			cr.Messages = append(cr.Messages, canonMsg{Role: role, Text: sb.String()})
		}
	}
	cr.InputText = buildInputText(cr.System, cr.Messages)
	return cr, nil
}

// ---------- 上游请求渲染（canon → 上游格式） ----------

func renderUpstreamBody(upType string, cr *canonReq, model string) []byte {
	switch upType {
	case "anthropic":
		mt := cr.MaxTokens
		if mt <= 0 {
			mt = 4096 // Anthropic 必填
		}
		m := map[string]any{"model": model, "max_tokens": mt, "stream": cr.Stream}
		if cr.System != "" {
			m["system"] = cr.System
		}
		if cr.Temperature != nil {
			m["temperature"] = *cr.Temperature
		}
		msgs := make([]map[string]any, 0, len(cr.Messages))
		for _, msg := range cr.Messages {
			msgs = append(msgs, map[string]any{"role": msg.Role, "content": msg.Text})
		}
		m["messages"] = msgs
		b, _ := json.Marshal(m)
		return b
	case "gemini":
		m := map[string]any{}
		if cr.System != "" {
			m["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": cr.System}}}
		}
		contents := make([]map[string]any, 0, len(cr.Messages))
		for _, msg := range cr.Messages {
			role := "user"
			if msg.Role == "assistant" {
				role = "model"
			}
			contents = append(contents, map[string]any{"role": role, "parts": []any{map[string]any{"text": msg.Text}}})
		}
		m["contents"] = contents
		gc := map[string]any{}
		if cr.MaxTokens > 0 {
			gc["maxOutputTokens"] = cr.MaxTokens
		}
		if cr.Temperature != nil {
			gc["temperature"] = *cr.Temperature
		}
		if len(gc) > 0 {
			m["generationConfig"] = gc
		}
		b, _ := json.Marshal(m)
		return b
	default: // openai
		msgs := make([]map[string]any, 0, len(cr.Messages)+1)
		if cr.System != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": cr.System})
		}
		for _, msg := range cr.Messages {
			msgs = append(msgs, map[string]any{"role": msg.Role, "content": msg.Text})
		}
		m := map[string]any{"model": model, "messages": msgs, "stream": cr.Stream}
		if cr.Stream {
			m["stream_options"] = map[string]any{"include_usage": true}
		}
		if cr.MaxTokens > 0 {
			m["max_tokens"] = cr.MaxTokens
		}
		if cr.Temperature != nil {
			m["temperature"] = *cr.Temperature
		}
		b, _ := json.Marshal(m)
		return b
	}
}

// upstreamEndpoint 转换路径下各上游类型的请求路径。
func upstreamEndpoint(upType, model string, stream bool) string {
	switch upType {
	case "anthropic":
		return "/v1/messages"
	case "gemini":
		if stream {
			return "/v1beta/models/" + model + ":streamGenerateContent?alt=sse"
		}
		return "/v1beta/models/" + model + ":generateContent"
	default:
		return "/v1/chat/completions"
	}
}

// ---------- 上游响应解析 ----------

type upstreamUsage struct {
	Prompt     int
	Completion int
	InKnown    bool
	OutKnown   bool
}

func (u *upstreamUsage) merge(n upstreamUsage) {
	if n.InKnown {
		u.Prompt = n.Prompt
		u.InKnown = true
	}
	if n.OutKnown {
		u.Completion = n.Completion
		u.OutKnown = true
	}
}

// parseUpstreamNonStream 解析三种上游类型的非流式响应，返回输出文本与用量。
func parseUpstreamNonStream(upType string, body []byte) (string, upstreamUsage) {
	text := ""
	u := upstreamUsage{}
	switch upType {
	case "anthropic":
		var j struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(body, &j) == nil {
			var sb strings.Builder
			for _, blk := range j.Content {
				sb.WriteString(blk.Text)
			}
			text = sb.String()
			u.Prompt, u.InKnown = j.Usage.InputTokens, j.Usage.InputTokens > 0
			u.Completion, u.OutKnown = j.Usage.OutputTokens, j.Usage.OutputTokens > 0
		}
	case "gemini":
		var j struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
			UsageMetadata struct {
				PromptTokenCount     int `json:"promptTokenCount"`
				CandidatesTokenCount int `json:"candidatesTokenCount"`
			} `json:"usageMetadata"`
		}
		if json.Unmarshal(body, &j) == nil {
			if len(j.Candidates) > 0 {
				for _, p := range j.Candidates[0].Content.Parts {
					text += p.Text
				}
			}
			u.Prompt, u.InKnown = j.UsageMetadata.PromptTokenCount, j.UsageMetadata.PromptTokenCount > 0
			u.Completion, u.OutKnown = j.UsageMetadata.CandidatesTokenCount, j.UsageMetadata.CandidatesTokenCount > 0
		}
	default: // openai chat
		var j struct {
			Choices []struct {
				Message struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(body, &j) == nil {
			if len(j.Choices) > 0 {
				text = flattenText(j.Choices[0].Message.Content)
			}
			u.Prompt, u.InKnown = j.Usage.PromptTokens, j.Usage.PromptTokens > 0
			u.Completion, u.OutKnown = j.Usage.CompletionTokens, j.Usage.CompletionTokens > 0
		}
	}
	return text, u
}

// parseUpstreamSSEChunk 解析上游流式事件，返回增量文本与用量。
// openai: chat.completion.chunk；anthropic: message_* 事件；gemini: generateContent 块。
func parseUpstreamSSEChunk(upType, payload string) (string, upstreamUsage) {
	var j map[string]any
	if json.Unmarshal([]byte(payload), &j) != nil {
		return "", upstreamUsage{}
	}
	switch upType {
	case "anthropic":
		switch t, _ := j["type"].(string); t {
		case "message_start":
			u := upstreamUsage{}
			if msg, ok := j["message"].(map[string]any); ok {
				if usage, ok := msg["usage"].(map[string]any); ok {
					u.Prompt = intFromAny(usage["input_tokens"])
					u.InKnown = u.Prompt > 0
				}
			}
			return "", u
		case "message_delta":
			u := upstreamUsage{}
			if usage, ok := j["usage"].(map[string]any); ok {
				u.Completion = intFromAny(usage["output_tokens"])
				u.OutKnown = u.Completion > 0
			}
			return "", u
		case "content_block_delta":
			if d, ok := j["delta"].(map[string]any); ok {
				if s, ok := d["text"].(string); ok {
					return s, upstreamUsage{}
				}
			}
		}
		return "", upstreamUsage{}
	case "gemini":
		delta := ""
		u := upstreamUsage{}
		if candidates, ok := j["candidates"].([]any); ok && len(candidates) > 0 {
			if cand, ok := candidates[0].(map[string]any); ok {
				if content, ok := cand["content"].(map[string]any); ok {
					if parts, ok := content["parts"].([]any); ok {
						for _, p := range parts {
							if pm, ok := p.(map[string]any); ok {
								if s, ok := pm["text"].(string); ok {
									delta += s
								}
							}
						}
					}
				}
			}
		}
		if um, ok := j["usageMetadata"].(map[string]any); ok {
			u.Prompt = intFromAny(um["promptTokenCount"])
			u.InKnown = u.Prompt > 0
			u.Completion = intFromAny(um["candidatesTokenCount"])
			u.OutKnown = u.Completion > 0
		}
		return delta, u
	default: // openai chat chunk
		delta := ""
		u := upstreamUsage{}
		if usage, ok := j["usage"].(map[string]any); ok && usage != nil {
			u.Prompt = intFromAny(usage["prompt_tokens"])
			u.InKnown = u.Prompt > 0
			u.Completion = intFromAny(usage["completion_tokens"])
			u.OutKnown = u.Completion > 0
		}
		if choices, ok := j["choices"].([]any); ok && len(choices) > 0 {
			if ch, ok := choices[0].(map[string]any); ok {
				if d, ok := ch["delta"].(map[string]any); ok {
					if s, ok := d["content"].(string); ok {
						delta = s
					}
				}
			}
		}
		return delta, u
	}
}

// parseResponsesSSEEvent 解析 OpenAI Responses API 的原生流式事件（仅原生透传时用于计费）。
func parseResponsesSSEEvent(payload string) (string, upstreamUsage) {
	var j map[string]any
	if json.Unmarshal([]byte(payload), &j) != nil {
		return "", upstreamUsage{}
	}
	t, _ := j["type"].(string)
	if t == "response.output_text.delta" {
		if s, ok := j["delta"].(string); ok {
			return s, upstreamUsage{}
		}
	}
	if t == "response.completed" || t == "response.incomplete" {
		u := upstreamUsage{}
		if resp, ok := j["response"].(map[string]any); ok {
			if usage, ok := resp["usage"].(map[string]any); ok {
				u.Prompt = intFromAny(usage["input_tokens"])
				u.InKnown = u.Prompt > 0
				u.Completion = intFromAny(usage["output_tokens"])
				u.OutKnown = u.Completion > 0
			}
		}
		return "", u
	}
	return "", upstreamUsage{}
}

// parseNativeNonStream 解析原生透传的非流式响应（计费用），文本仅作估算兜底。
func parseNativeNonStream(format clientFormat, body []byte, info *relayInfo) {
	switch format {
	case FormatOpenAIResponse:
		var j struct {
			Output []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(body, &j) == nil {
			for _, o := range j.Output {
				for _, part := range o.Content {
					info.OutputText.WriteString(part.Text)
				}
			}
			info.PromptTok, info.InKnown = j.Usage.InputTokens, j.Usage.InputTokens > 0
			info.CompletionTok, info.OutKnown = j.Usage.OutputTokens, j.Usage.OutputTokens > 0
		}
	case FormatGemini:
		text, u := parseUpstreamNonStream("gemini", body)
		info.OutputText.WriteString(text)
		info.PromptTok, info.InKnown = u.Prompt, u.InKnown
		info.CompletionTok, info.OutKnown = u.Completion, u.OutKnown
	case FormatAnthropic:
		text, u := parseUpstreamNonStream("anthropic", body)
		info.OutputText.WriteString(text)
		info.PromptTok, info.InKnown = u.Prompt, u.InKnown
		info.CompletionTok, info.OutKnown = u.Completion, u.OutKnown
	default:
		text, u := parseUpstreamNonStream("openai", body)
		info.OutputText.WriteString(text)
		info.PromptTok, info.InKnown = u.Prompt, u.InKnown
		info.CompletionTok, info.OutKnown = u.Completion, u.OutKnown
	}
}

// ---------- 客户端格式错误渲染 ----------

func writeFormatError(c *gin.Context, format clientFormat, status int, msg string) {
	var body any
	switch format {
	case FormatAnthropic:
		body = gin.H{"type": "error", "error": gin.H{"type": "api_error", "message": msg}}
	case FormatGemini:
		body = gin.H{"error": gin.H{"code": status, "message": msg, "status": "INTERNAL"}}
	case FormatOpenAIResponse:
		body = gin.H{"error": gin.H{"message": msg, "code": status}}
	default:
		body = gin.H{"error": gin.H{"message": msg, "type": "apishare_error", "code": status}}
	}
	c.JSON(status, body)
}

// ---------- 转换路径的客户端响应渲染 ----------

func writeConvertedNonStream(c *gin.Context, format clientFormat, model, text string, u upstreamUsage) {
	p, q := u.Prompt, u.Completion
	var body any
	switch format {
	case FormatAnthropic:
		body = gin.H{
			"id": "msg_conv", "type": "message", "role": "assistant", "model": model,
			"content":     []any{gin.H{"type": "text", "text": text}},
			"stop_reason": "end_turn", "stop_sequence": nil,
			"usage": gin.H{"input_tokens": p, "output_tokens": q},
		}
	case FormatGemini:
		body = gin.H{
			"candidates": []any{gin.H{
				"content":      gin.H{"parts": []any{gin.H{"text": text}}, "role": "model"},
				"finishReason": "STOP", "index": 0,
			}},
			"usageMetadata": gin.H{"promptTokenCount": p, "candidatesTokenCount": q, "totalTokenCount": p + q},
			"modelVersion":  model,
		}
	case FormatOpenAIResponse:
		body = gin.H{
			"id": "resp_conv", "object": "response", "status": "completed", "model": model,
			"output": []any{gin.H{
				"type": "message", "id": "msg_conv", "status": "completed", "role": "assistant",
				"content": []any{gin.H{"type": "output_text", "text": text, "annotations": []any{}}},
			}},
			"usage": gin.H{"input_tokens": p, "output_tokens": q, "total_tokens": p + q},
		}
	default:
		body = gin.H{
			"id": "chatcmpl-conv", "object": "chat.completion", "model": model,
			"choices": []any{gin.H{
				"index": 0, "finish_reason": "stop",
				"message": gin.H{"role": "assistant", "content": text},
			}},
			"usage": gin.H{"prompt_tokens": p, "completion_tokens": q, "total_tokens": p + q},
		}
	}
	c.JSON(200, body)
}

// convertedStream 把上游增量转换成客户端格式的 SSE 流。
type convertedStream struct {
	format    clientFormat
	model     string
	fullText  strings.Builder
	wantUsage bool // openai chat：客户端是否要求 include_usage 尾块
	started   bool
	blkOpen   bool // anthropic content_block 是否已开启
}

func newConvertedStream(c *gin.Context, format clientFormat, model string, wantUsage bool) *convertedStream {
	w := &convertedStream{format: format, model: model, wantUsage: wantUsage}
	c.Writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	return w
}

func (w *convertedStream) sse(c *gin.Context, v any) {
	b, _ := json.Marshal(v)
	c.Writer.WriteString("data: " + string(b) + "\n\n")
	if fl, ok := c.Writer.(http.Flusher); ok {
		fl.Flush()
	}
}

func (w *convertedStream) start(c *gin.Context) {
	if w.started {
		return
	}
	w.started = true
	c.Writer.WriteHeader(200)
	switch w.format {
	case FormatAnthropic:
		w.sse(c, gin.H{"type": "message_start", "message": gin.H{
			"id": "msg_conv", "type": "message", "role": "assistant", "model": w.model,
			"content": []any{}, "usage": gin.H{"input_tokens": 0, "output_tokens": 0},
		}})
		w.sse(c, gin.H{"type": "content_block_start", "index": 0,
			"content_block": gin.H{"type": "text", "text": ""}})
		w.blkOpen = true
	case FormatOpenAIResponse:
		w.sse(c, gin.H{"type": "response.created", "response": gin.H{
			"id": "resp_conv", "object": "response", "status": "in_progress", "model": w.model,
			"output": []any{},
		}})
	}
}

func (w *convertedStream) delta(c *gin.Context, text string) {
	if text == "" {
		return
	}
	w.start(c)
	w.fullText.WriteString(text)
	switch w.format {
	case FormatAnthropic:
		w.sse(c, gin.H{"type": "content_block_delta", "index": 0,
			"delta": gin.H{"type": "text_delta", "text": text}})
	case FormatGemini:
		w.sse(c, gin.H{"candidates": []any{gin.H{
			"content": gin.H{"parts": []any{gin.H{"text": text}}, "role": "model"},
			"index":   0,
		}}})
	case FormatOpenAIResponse:
		w.sse(c, gin.H{"type": "response.output_text.delta", "delta": text})
	default:
		w.sse(c, gin.H{"id": "chatcmpl-conv", "object": "chat.completion.chunk", "model": w.model,
			"choices": []any{gin.H{"index": 0, "delta": gin.H{"content": text}}}})
	}
}

func (w *convertedStream) finish(c *gin.Context, u upstreamUsage) {
	w.start(c)
	text := w.fullText.String()
	switch w.format {
	case FormatAnthropic:
		if w.blkOpen {
			w.sse(c, gin.H{"type": "content_block_stop", "index": 0})
		}
		w.sse(c, gin.H{"type": "message_delta",
			"delta": gin.H{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": gin.H{"output_tokens": u.Completion, "input_tokens": u.Prompt}})
		w.sse(c, gin.H{"type": "message_stop"})
	case FormatGemini:
		w.sse(c, gin.H{"candidates": []any{gin.H{
			"content":      gin.H{"parts": []any{gin.H{"text": ""}}, "role": "model"},
			"finishReason": "STOP", "index": 0,
		}}, "usageMetadata": gin.H{
			"promptTokenCount": u.Prompt, "candidatesTokenCount": u.Completion,
			"totalTokenCount": u.Prompt + u.Completion,
		}})
	case FormatOpenAIResponse:
		w.sse(c, gin.H{"type": "response.completed", "response": gin.H{
			"id": "resp_conv", "object": "response", "status": "completed", "model": w.model,
			"output": []any{gin.H{
				"type": "message", "id": "msg_conv", "status": "completed", "role": "assistant",
				"content": []any{gin.H{"type": "output_text", "text": text, "annotations": []any{}}},
			}},
			"usage": gin.H{"input_tokens": u.Prompt, "output_tokens": u.Completion,
				"total_tokens": u.Prompt + u.Completion},
		}})
	default:
		if w.wantUsage {
			w.sse(c, gin.H{"id": "chatcmpl-conv", "object": "chat.completion.chunk", "model": w.model,
				"choices": []any{},
				"usage":   gin.H{"prompt_tokens": u.Prompt, "completion_tokens": u.Completion, "total_tokens": u.Prompt + u.Completion}})
		}
		c.Writer.WriteString("data: [DONE]\n\n")
		if fl, ok := c.Writer.(http.Flusher); ok {
			fl.Flush()
		}
	}
}

// ---------- Gemini 路径解析 ----------

// parseGeminiAction 解析 /v1beta/models/{model}:{method} 路径。
func parseGeminiAction(action string) (model, method string, ok bool) {
	s := strings.TrimPrefix(action, "/")
	i := strings.LastIndex(s, ":")
	if i <= 0 {
		return "", "", false
	}
	model, method = s[:i], s[i+1:]
	switch method {
	case "generateContent", "streamGenerateContent":
		return model, method, true
	}
	return "", "", false
}

func geminiEndpoint(model, method string) string {
	if method == "streamGenerateContent" {
		return "/v1beta/models/" + model + ":streamGenerateContent?alt=sse"
	}
	return "/v1beta/models/" + model + ":generateContent"
}
