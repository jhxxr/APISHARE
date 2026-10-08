// mockupstream：本地 mock 上游（OpenAI 兼容 / Anthropic / Gemini / OpenAI Responses），
// 用于联调与验证网关的多格式转换、粘性路由、计费与故障转移。
// 回复内容中带有 -name 指定的上游名，便于确认请求落在哪个上游。
//
//	go run ./cmd/mockupstream -addr :18081 -name mockA
//	go run ./cmd/mockupstream -addr :18082 -name mockB
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
)

var upstreamName string

func main() {
	addr := flag.String("addr", ":18081", "listen address")
	name := flag.String("name", "mockA", "upstream name (echoed in reply)")
	flag.Parse()
	upstreamName = *name

	http.HandleFunc("/v1/models", handleModels)
	http.HandleFunc("/v1beta/models", handleGeminiModels)
	http.HandleFunc("/v1/chat/completions", handleOpenAIChat)
	http.HandleFunc("/v1/responses", handleOpenAIResponses)
	http.HandleFunc("/v1/messages", handleAnthropicMessages)
	http.HandleFunc("/v1beta/models/", handleGeminiGenerate)

	log.Printf("mock upstream %s listening on %s", upstreamName, *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func replyText() string {
	return fmt.Sprintf("[%s] 你好，这是来自 mock 上游的回复。", upstreamName)
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   []any{map[string]any{"id": "mock-model", "object": "model", "owned_by": upstreamName}},
	})
}

func handleGeminiModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"models": []any{map[string]any{"name": "models/mock-model", "displayName": upstreamName}},
	})
}

func needAuth(w http.ResponseWriter, r *http.Request) bool {
	if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == "" && r.Header.Get("x-api-key") == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	return true
}

// ---------- OpenAI Chat Completions ----------

func handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	if !needAuth(w, r) {
		return
	}
	var req struct {
		Model         string `json:"model"`
		Stream        bool   `json:"stream"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	last := ""
	for _, m := range req.Messages {
		if m.Role == "user" {
			last = contentString(m.Content)
		}
	}
	content := replyText() + " (你说: " + last + ")"

	w.Header().Set("Content-Type", "application/json")
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		chunk := func(v map[string]any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
		chunk(map[string]any{"id": "cmpl-mock", "object": "chat.completion.chunk", "choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}},
		}})
		for _, piece := range split(content, 6) {
			chunk(map[string]any{"id": "cmpl-mock", "object": "chat.completion.chunk", "choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"content": piece}},
			}})
		}
		if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
			chunk(map[string]any{"id": "cmpl-mock", "object": "chat.completion.chunk",
				"choices": []any{},
				"usage":   map[string]any{"prompt_tokens": 12, "completion_tokens": 8}})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"id": "cmpl-mock", "object": "chat.completion", "model": req.Model,
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": content},
		}},
		"usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 8},
	})
}

// ---------- OpenAI Responses ----------

func handleOpenAIResponses(w http.ResponseWriter, r *http.Request) {
	if !needAuth(w, r) {
		return
	}
	var req struct {
		Model  string          `json:"model"`
		Stream bool            `json:"stream"`
		Input  json.RawMessage `json:"input"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	content := replyText()

	w.Header().Set("Content-Type", "application/json")
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		ev := func(v map[string]any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
		ev(map[string]any{"type": "response.created", "response": map[string]any{
			"id": "resp_mock", "object": "response", "status": "in_progress", "model": req.Model,
		}})
		for _, piece := range split(content, 6) {
			ev(map[string]any{"type": "response.output_text.delta", "delta": piece})
		}
		ev(map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "resp_mock", "object": "response", "status": "completed", "model": req.Model,
			"usage": map[string]any{"input_tokens": 14, "output_tokens": 7, "total_tokens": 21},
		}})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"id": "resp_mock", "object": "response", "status": "completed", "model": req.Model,
		"output": []any{map[string]any{
			"type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": content}},
		}},
		"usage": map[string]any{"input_tokens": 14, "output_tokens": 7, "total_tokens": 21},
	})
}

// ---------- Anthropic Messages ----------

func handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("x-api-key") == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	content := replyText()

	w.Header().Set("Content-Type", "application/json")
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		ev := func(name string, v map[string]any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
			fl.Flush()
		}
		ev("message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": "msg-mock", "type": "message", "role": "assistant", "model": req.Model,
			"usage": map[string]any{"input_tokens": 15},
		}})
		for _, piece := range split(content, 6) {
			ev("content_block_delta", map[string]any{"type": "content_block_delta",
				"delta": map[string]any{"type": "text_delta", "text": piece}})
		}
		ev("message_delta", map[string]any{"type": "message_delta",
			"usage": map[string]any{"output_tokens": 9}})
		ev("message_stop", map[string]any{"type": "message_stop"})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"id": "msg-mock", "type": "message", "role": "assistant", "model": req.Model,
		"content":     []any{map[string]any{"type": "text", "text": content}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 15, "output_tokens": 9},
	})
}

// ---------- Gemini generateContent ----------

func geminiKeyOK(r *http.Request) bool {
	return r.Header.Get("x-goog-api-key") != "" || r.URL.Query().Get("key") != ""
}

func handleGeminiGenerate(w http.ResponseWriter, r *http.Request) {
	if !geminiKeyOK(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	// 路径形如 /v1beta/models/mock-model:generateContent
	action := strings.TrimPrefix(r.URL.Path, "/v1beta/models/")
	i := strings.LastIndex(action, ":")
	if i <= 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	model, method := action[:i], action[i+1:]
	content := replyText()
	full := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"parts": []any{map[string]any{"text": content}}, "role": "model"},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{"promptTokenCount": 20, "candidatesTokenCount": 12, "totalTokenCount": 32},
		"modelVersion":  model,
	}

	w.Header().Set("Content-Type", "application/json")
	if method == "streamGenerateContent" {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		emit := func(v map[string]any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
		for _, piece := range split(content, 6) {
			emit(map[string]any{"candidates": []any{map[string]any{
				"content": map[string]any{"parts": []any{map[string]any{"text": piece}}, "role": "model"},
			}}})
		}
		emit(full) // 末块带完整 usage
		return
	}
	json.NewEncoder(w).Encode(full)
}

// ---------- 工具 ----------

// contentString 兼容字符串与 [{type,text}] 两种 content 格式。
func contentString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
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
	return ""
}

func split(s string, n int) []string {
	r := []rune(s)
	var out []string
	for i := 0; i < len(r); i += n {
		end := i + n
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[i:end]))
	}
	return out
}
