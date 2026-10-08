package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const maxBodyBytes = 32 << 20 // 32MB

var upstreamHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 180 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   16,
		ForceAttemptHTTP2:     true,
	},
}

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout,
		http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout, 529:
		return true
	}
	return code >= 500
}

type relayInfo struct {
	UpstreamID    int64
	UpstreamName  string
	Status        int
	PromptTok     int
	CompletionTok int
	InKnown       bool
	OutKnown      bool
	OutputText    strings.Builder
	LatencyMS     int64
}

func (i *relayInfo) mergeUsage(u upstreamUsage) {
	i.mergeUsagePtr(&u)
}

func (i *relayInfo) mergeUsagePtr(u *upstreamUsage) {
	if u.InKnown {
		i.PromptTok = u.Prompt
		i.InKnown = true
	}
	if u.OutKnown {
		i.CompletionTok = u.Completion
		i.OutKnown = true
	}
}

// relayTask 一次转发所需的全部上下文。
type relayTask struct {
	format         clientFormat
	endpoint       string    // 原生透传时的上游路径
	body           []byte    // 客户端原始请求体（原生透传用）
	canon          *canonReq // 规范化请求（转换用）
	model          string    // 客户端请求的模型
	stream         bool
	nativeOnly     bool // 只允许原生类型的上游（如 embeddings），禁止跨格式转换
	wantUsageChunk bool // openai chat 流式：客户端是否要求 include_usage
}

// relayHandler 返回转发处理器（一种客户端格式一个实例）。
func relayHandler(format clientFormat, nativeOnly bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		ak := c.MustGet("apiKey").(ApiKey)

		body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes))
		if err != nil {
			writeFormatError(c, format, http.StatusRequestEntityTooLarge, "request body too large or unreadable")
			return
		}

		pathModel := ""
		if format == FormatGemini {
			var ok bool
			pathModel, _, ok = parseGeminiAction(c.Param("action"))
			if !ok {
				writeFormatError(c, format, http.StatusNotFound, "unsupported Gemini method, use :generateContent or :streamGenerateContent")
				return
			}
		}

		cr, perr := parseClientRequest(format, body, pathModel)
		if perr != nil || cr.Model == "" {
			writeFormatError(c, format, http.StatusBadRequest, "missing or invalid 'model' in request")
			return
		}

		endpoint := nativeEndpoint(format, cr)
		task := &relayTask{
			format:     format,
			endpoint:   endpoint,
			body:       body,
			canon:      cr,
			model:      cr.Model,
			stream:     cr.Stream,
			nativeOnly: nativeOnly,
		}

		// OpenAI chat 流式注入 include_usage，尽量拿到真实用量计费
		if format == FormatOpenAIChat && cr.Stream {
			task.wantUsageChunk = hasIncludeUsage(body)
			body = injectStreamUsage(body)
			task.body = body
		}

		skey := sessionKeyFromCanon(endpoint, c.Request.Header, cr)
		inputTokensEst := estimateTokensModel(cr.Model, cr.InputText)

		s := getSettings()
		nativeType := format.nativeUpstreamType()
		tried := map[int64]bool{}
		attempts := 1 + s.Retry
		var info relayInfo
		var attemptErr error
		wrote := false

		for i := 0; i < attempts; i++ {
			var up *Upstream

			// 第一跳：粘性优先（粘性上游必须健康并服务该模型，类型不限——
			// 上次可能就是通过跨格式转换落到该上游的）
			if i == 0 {
				if id, ok := affinityGet(skey); ok {
					if u, err := dbGetUpstream(id); err == nil && u.Enabled &&
						u.servesModel(cr.Model) && healthAvailable(u.ID) {
						up = u
					}
				}
			}
			if up == nil {
				var err error
				up, err = pickUpstream(nativeType, cr.Model, tried, task.nativeOnly)
				if err != nil {
					attemptErr = err
					break // Finalize once so unavailable models also contribute a failed request.
				}
				affinitySet(skey, up.ID)
			}
			tried[up.ID] = true

			info, wrote, attemptErr = doRelay(c, up, task)
			if wrote {
				break
			}
			if attemptErr != nil {
				log.Printf("[failover] key=%s model=%s upstream=%s attempt=%d err=%v",
					ak.Name, cr.Model, up.Name, i+1, attemptErr)
			}
		}

		finalizeCall(c, ak, task, info, wrote, attemptErr, inputTokensEst)
	}
}

// nativeEndpoint 原生透传时各客户端格式对应的上游路径。
func nativeEndpoint(format clientFormat, cr *canonReq) string {
	switch format {
	case FormatAnthropic:
		return "/v1/messages"
	case FormatOpenAIResponse:
		return "/v1/responses"
	case FormatGemini:
		method := "generateContent"
		if cr.Stream {
			method = "streamGenerateContent"
		}
		return geminiEndpoint(cr.Model, method)
	default:
		return "/v1/chat/completions"
	}
}

func finalizeCall(c *gin.Context, ak ApiKey, task *relayTask, info relayInfo, wrote bool, lastErr error, inputTokensEst int) {
	if !wrote {
		// 所有上游都失败且未给客户端写出任何内容
		msg := "all upstream attempts failed"
		if lastErr != nil {
			msg += ": " + lastErr.Error()
		}
		writeFormatError(c, task.format, http.StatusBadGateway, msg)
		insertFailureLog(ak, 0, "", task, http.StatusBadGateway, 0, msg)
		return
	}

	success := info.Status >= 200 && info.Status < 300
	prompt, completion := info.PromptTok, info.CompletionTok
	if success {
		if !info.InKnown {
			prompt = inputTokensEst
		}
		if !info.OutKnown {
			completion = estimateTokensModel(task.model, info.OutputText.String())
		}
		cost := priceFor(task.model, prompt, completion)
		if cost > 0 {
			dbAddUsedUSD(ak.ID, cost)
		}
		logStatus := info.Status
		logError := ""
		if lastErr != nil {
			// An interrupted stream is a failed call even after its HTTP 200 headers.
			logStatus = http.StatusBadGateway
			logError = "stream interrupted: " + lastErr.Error()
		}
		dbInsertLog(&CallLog{
			KeyID: ak.ID, KeyName: ak.Name,
			UpstreamID: info.UpstreamID, UpstreamName: info.UpstreamName,
			Model: task.model, Endpoint: string(task.format),
			PromptTokens: prompt, CompletionTokens: completion,
			CostUSD: cost, Status: logStatus, LatencyMS: info.LatencyMS, Error: logError,
		})
		if lastErr != nil {
			log.Printf("[relay] key=%s model=%s upstream=%s finished with error: %v",
				ak.Name, task.model, info.UpstreamName, lastErr)
		}
		return
	}

	// 非重试类上游错误（如 400），响应已透传，这里只记录
	errMsg := fmt.Sprintf("upstream status %d", info.Status)
	if info.OutputText.Len() > 0 {
		errMsg += ": " + truncate(info.OutputText.String(), 300)
	}
	insertFailureLog(ak, info.UpstreamID, info.UpstreamName, task, info.Status, info.LatencyMS, errMsg)
}

func insertFailureLog(ak ApiKey, upID int64, upName string, task *relayTask, status int, latency int64, errMsg string) {
	dbInsertLog(&CallLog{
		KeyID: ak.ID, KeyName: ak.Name,
		UpstreamID: upID, UpstreamName: upName,
		Model: task.model, Endpoint: string(task.format),
		Status: status, LatencyMS: latency, Error: errMsg,
	})
}

// ---------- 实际转发 ----------

func doRelay(c *gin.Context, up *Upstream, task *relayTask) (info relayInfo, wrote bool, err error) {
	info = relayInfo{UpstreamID: up.ID, UpstreamName: up.Name}
	upModel := up.mapModel(task.model)
	native := up.Type == task.format.nativeUpstreamType()

	var url string
	var reqBody []byte
	if native {
		url = upstreamURL(up.BaseURL, task.endpoint)
		reqBody = task.body
		if upModel != task.model {
			if task.format == FormatGemini {
				url = replaceGeminiModel(url, upModel)
			} else {
				reqBody = replaceModelInBody(reqBody, upModel)
			}
		}
	} else {
		// 跨格式转换：tools 无法无损转换，明确报错
		if task.canon.HasTools {
			msg := fmt.Sprintf("model %q on upstream %q (%s) requires cross-format conversion, which does not support tool calls; add a native %s upstream for this model",
				task.model, up.Name, up.Type, task.format.nativeUpstreamType())
			writeFormatError(c, task.format, http.StatusBadRequest, msg)
			info.Status = http.StatusBadRequest
			return info, true, nil
		}
		reqBody = renderUpstreamBody(up.Type, task.canon, upModel)
		url = upstreamURL(up.BaseURL, upstreamEndpoint(up.Type, upModel, task.stream))
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, url, bytes.NewReader(reqBody))
	if err != nil {
		return info, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if task.stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	switch up.Type {
	case "anthropic":
		req.Header.Set("x-api-key", up.APIKey)
		if v := c.Request.Header.Get("anthropic-version"); v != "" {
			req.Header.Set("anthropic-version", v)
		} else {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	case "gemini":
		req.Header.Set("x-goog-api-key", up.APIKey)
	default:
		req.Header.Set("Authorization", "Bearer "+up.APIKey)
	}

	start := time.Now()
	resp, err := upstreamHTTPClient.Do(req)
	info.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		return info, false, err
	}
	defer resp.Body.Close()
	info.Status = resp.StatusCode

	if info.Status < 200 || info.Status > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		info.OutputText.WriteString(string(b))
		if isRetryableStatus(info.Status) {
			return info, false, fmt.Errorf("upstream %s returned %d", up.Name, info.Status)
		}
		// 客户端错误（400/404/413/422 等）：不重试。原生透传原始错误体，转换路径包装成本格式错误
		if native {
			c.Data(info.Status, resp.Header.Get("Content-Type"), b)
		} else {
			writeFormatError(c, task.format, info.Status, truncate(string(b), 300))
		}
		return info, true, nil
	}

	ct := resp.Header.Get("Content-Type")
	isSSE := strings.Contains(ct, "text/event-stream")

	if native {
		if task.stream && isSSE {
			err = relaySSENative(c, resp, task.format, &info)
			return info, true, err
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			return info, false, err
		}
		parseNativeNonStream(task.format, b, &info)
		c.Data(info.Status, ct, b)
		return info, true, nil
	}

	// ---------- 跨格式转换 ----------
	if task.stream {
		if isSSE {
			err = relaySSEConverted(c, resp, up.Type, task, &info)
			return info, true, err
		}
		// 上游未按 SSE 返回，按非流式处理
		b, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if rerr != nil {
			return info, false, rerr
		}
		text, u := parseUpstreamNonStream(up.Type, b)
		info.OutputText.WriteString(text)
		info.mergeUsage(u)
		writeConvertedNonStream(c, task.format, task.model, text, u)
		return info, true, nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return info, false, err
	}
	text, u := parseUpstreamNonStream(up.Type, b)
	info.OutputText.WriteString(text)
	info.mergeUsage(u)
	writeConvertedNonStream(c, task.format, task.model, text, u)
	return info, true, nil
}

// relaySSENative 原生透传：逐行转发 SSE，同时按客户端格式解析用量与输出文本。
func relaySSENative(c *gin.Context, resp *http.Response, format clientFormat, info *relayInfo) error {
	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)

	flusher, _ := c.Writer.(http.Flusher)
	reader := bufio.NewReaderSize(resp.Body, 1<<16)

	for {
		line, rerr := reader.ReadString('\n')
		if line != "" {
			if strings.HasPrefix(line, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if payload != "" && payload != "[DONE]" {
					recordNativeEvent(format, payload, info)
				}
			}
			c.Writer.WriteString(line)
			if line == "\n" && flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				if flusher != nil {
					flusher.Flush()
				}
				return nil
			}
			return rerr
		}
	}
}

func recordNativeEvent(format clientFormat, payload string, info *relayInfo) {
	var delta string
	var u upstreamUsage
	if format == FormatOpenAIResponse {
		delta, u = parseResponsesSSEEvent(payload)
	} else {
		delta, u = parseUpstreamSSEChunk(string(format.nativeUpstreamType()), payload)
	}
	if delta != "" {
		info.OutputText.WriteString(delta)
	}
	info.mergeUsage(u)
}

// relaySSEConverted 跨格式流式转换：解析上游 SSE，逐增量渲染为客户端格式。
func relaySSEConverted(c *gin.Context, resp *http.Response, upType string, task *relayTask, info *relayInfo) error {
	writer := newConvertedStream(c, task.format, task.model, task.wantUsageChunk)
	reader := bufio.NewReaderSize(resp.Body, 1<<16)

	for {
		line, rerr := reader.ReadString('\n')
		if line != "" {
			if strings.HasPrefix(line, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if payload != "" && payload != "[DONE]" {
					delta, u := parseUpstreamSSEChunk(upType, payload)
					if delta != "" {
						info.OutputText.WriteString(delta)
						writer.delta(c, delta)
					}
					info.mergeUsage(u)
				}
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				writer.finish(c, upstreamUsage{Prompt: info.PromptTok, Completion: info.CompletionTok})
				return nil
			}
			return rerr
		}
	}
}

func intFromAny(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// ---------- 请求体小工具 ----------

func hasIncludeUsage(body []byte) bool {
	var m struct {
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	json.Unmarshal(body, &m)
	return m.StreamOptions != nil && m.StreamOptions.IncludeUsage
}

// injectStreamUsage 以 map 往返的方式注入 stream_options.include_usage。
func injectStreamUsage(body []byte) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	if _, exists := m["stream_options"]; exists {
		return body
	}
	m["stream_options"] = map[string]any{"include_usage": true}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func replaceModelInBody(body []byte, model string) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	m["model"] = model
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// replaceGeminiModel Gemini 的模型在 URL 路径里而非请求体里。
func replaceGeminiModel(url, model string) string {
	i := strings.Index(url, "/v1beta/models/")
	if i < 0 {
		return url
	}
	prefix := url[:i+len("/v1beta/models/")]
	rest := url[i+len("/v1beta/models/"):]
	if j := strings.Index(rest, ":"); j >= 0 {
		return prefix + model + rest[j:]
	}
	return url
}
