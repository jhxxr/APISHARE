package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

var modelTestClient = &http.Client{
	Timeout:       90 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func registerModelTestRoutes(auth *gin.RouterGroup) {
	auth.POST("/upstreams/:id/model-test", func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		up, err := dbGetUpstream(id)
		if err != nil {
			c.JSON(404, gin.H{"error": "upstream not found"})
			return
		}
		var input struct {
			Model     string `json:"model"`
			Message   string `json:"message"`
			MaxTokens int    `json:"max_tokens"`
			Stream    *bool  `json:"stream"`
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128<<10)
		if c.ShouldBindJSON(&input) != nil {
			c.JSON(400, gin.H{"error": "invalid model test request"})
			return
		}
		input.Model = strings.TrimSpace(input.Model)
		if input.Model == "" || len(input.Model) > 512 {
			c.JSON(400, gin.H{"error": "请填写有效的模型名称（最多 512 字节）"})
			return
		}
		if len(input.Message) > 64<<10 || input.MaxTokens < 0 || input.MaxTokens > 32768 {
			c.JSON(400, gin.H{"error": "测试消息最多 64 KiB，最大输出 Token 须在 1–32768 之间"})
			return
		}
		if strings.TrimSpace(input.Message) == "" {
			input.Message = "hi"
		}
		if input.MaxTokens == 0 {
			input.MaxTokens = 4096
		}
		start := time.Now()
		stream := input.Stream == nil || *input.Stream
		result, status, err := testUpstreamModel(c.Request.Context(), up, input.Model, input.Message, input.MaxTokens, stream)
		response := gin.H{"ok": err == nil, "latency_ms": time.Since(start).Milliseconds(), "model": input.Model, "upstream_model": up.mapModel(input.Model), "status": status, "stream": stream}
		if err != nil {
			response["error"] = truncate(redactModelTestError(err.Error(), up.APIKey), 2000)
		} else {
			response["reply"] = result.text()
			response["finish_reason"] = result.Finish
			response["input_tokens"], response["output_tokens"] = nil, nil
			if result.Usage.InKnown {
				response["input_tokens"] = result.Usage.Prompt
			}
			if result.Usage.OutKnown {
				response["output_tokens"] = result.Usage.Completion
			}
			var thinking strings.Builder
			for _, block := range result.Blocks {
				if block.Kind == "thinking" {
					thinking.WriteString(block.Text)
				}
			}
			response["thinking"] = thinking.String()
		}
		c.JSON(200, response)
	})
}

// Test only the selected upstream, even when disabled or cooling down. Reuse
// protocol rendering/parsing without routing, billing or health-state changes.
func testUpstreamModel(ctx context.Context, up *Upstream, model, message string, maxTokens int, stream bool) (*canonResponse, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	model = up.mapModel(model)
	format := up.wireFormat(FormatOpenAIChat)
	cr := &canonReq{Model: model, Messages: []canonMsg{{Role: "user", Text: message}}, MaxTokens: maxTokens, Stream: stream}
	body, err := renderProtocolRequest(format, cr, model)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL(up.BaseURL, nativeEndpoint(format, cr)), bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("无法创建模型测试请求")
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Accept-Encoding", "identity")
	}
	switch up.Type {
	case "anthropic":
		req.Header.Set("x-api-key", up.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	case "gemini":
		req.Header.Set("x-goog-api-key", up.APIKey)
	default:
		req.Header.Set("Authorization", "Bearer "+up.APIKey)
	}
	// Administrator request headers must never be forwarded to the upstream.
	if err := up.applyHeaders(req, nil); err != nil {
		return nil, 0, err
	}
	client, err := clientForUpstream(modelTestClient, up.ProxyID)
	if err != nil {
		return nil, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, fmt.Errorf("模型测试已取消或超过 90 秒，请重试")
		}
		return nil, 0, fmt.Errorf("连接上游失败，请检查上游地址、代理和网络")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		detail := redactModelTestError(string(body), up.APIKey)
		return nil, resp.StatusCode, fmt.Errorf("上游返回 HTTP %d：%s", resp.StatusCode, truncate(detail, 2000))
	}
	result, err := readModelTestResponse(resp.Body, resp.Header.Get("Content-Type"), format)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("模型测试响应异常（HTTP %d）：%s", resp.StatusCode, truncate(redactModelTestError(err.Error(), up.APIKey), 2000))
	}
	return result, resp.StatusCode, nil
}

func redactModelTestError(message, apiKey string) string {
	if apiKey != "" {
		message = strings.ReplaceAll(message, apiKey, "[REDACTED]")
	}
	return message
}

// Some gateways ignore stream or mislabel their Content-Type. Inspect the body
// before choosing a decoder, while retaining the normal terminal-event checks.
func readModelTestResponse(reader io.Reader, contentType string, format clientFormat) (*canonResponse, error) {
	const limit = 4 << 20
	wire := &io.LimitedReader{R: reader, N: limit + 1}
	buffer := bufio.NewReader(wire)
	for {
		prefix, err := buffer.Peek(1)
		if err != nil {
			return nil, fmt.Errorf("上游没有返回有效的模型响应：%w", err)
		}
		if bytes.ContainsAny(prefix, " \t\r\n") {
			buffer.Discard(1)
			continue
		}
		if bom, _ := buffer.Peek(3); bytes.Equal(bom, []byte{0xef, 0xbb, 0xbf}) {
			buffer.Discard(3)
			continue
		}
		break
	}
	prefix, _ := buffer.Peek(6)
	isJSON := len(prefix) > 0 && (prefix[0] == '{' || prefix[0] == '[')
	isSSE := bytes.HasPrefix(prefix, []byte("data:")) || bytes.HasPrefix(prefix, []byte("event:")) || bytes.HasPrefix(prefix, []byte(":")) || bytes.HasPrefix(prefix, []byte("id:")) || bytes.HasPrefix(prefix, []byte("retry:"))
	if !isJSON && (isSSE || strings.Contains(strings.ToLower(contentType), "text/event-stream")) {
		result, err := collectProtocolSSE(buffer, format)
		if wire.N == 0 {
			return nil, fmt.Errorf("上游测试响应超过 4 MiB")
		}
		return result, err
	}
	body, err := readProtocolBody(buffer, limit)
	if err != nil {
		return nil, err
	}
	if wire.N == 0 {
		return nil, fmt.Errorf("上游测试响应超过 4 MiB")
	}
	result, err := parseProtocolResponse(format, body)
	if err == nil {
		return result, nil
	}
	// Expose the upstream's error message and response shape instead of making
	// a 200 error envelope look like a missing-choices problem. Include error
	// fields only, not the complete response or echoed request.
	if object, decodeErr := decodeProtocolObject(body); decodeErr == nil {
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		detail := ""
		for _, key := range []string{"error", "message", "msg", "detail"} {
			if message := str(object[key]); message != "" {
				detail = message
				break
			}
			if nested := object[key]; key == "error" {
				if fields, ok := nested.(map[string]any); ok {
					detail = str(fields["message"])
				}
			}
			if detail != "" {
				break
			}
		}
		if detail != "" {
			return nil, fmt.Errorf("%s；上游提示：%s", err, detail)
		}
		return nil, fmt.Errorf("%s；返回字段：%s", err, strings.Join(keys, ", "))
	}
	return nil, err
}
