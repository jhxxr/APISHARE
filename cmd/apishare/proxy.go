package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"apishare/internal/cliproxy"
	"github.com/gin-gonic/gin"
)

const maxBodyBytes = 32 << 20 // 32MB

var errDownstreamWrite = errors.New("writing client response failed")

type protocolConversionError struct{ cause error }

func (e *protocolConversionError) Error() string { return e.cause.Error() }
func (e *protocolConversionError) Unwrap() error { return e.cause }

var relayNetwork = loadRelayNetworkConfig()
var upstreamHTTPClient = newRelayHTTPClient(relayNetwork)

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
	UpstreamModel string
	Status        int
	PromptTok     int
	CompletionTok int
	InKnown       bool
	OutKnown      bool
	Usage         upstreamUsage
	OutputText    strings.Builder
	LatencyMS     int64 // 上游响应头到达耗时
	FirstTokenMS  int64 // 首字延迟：请求开始到首个输出增量的耗时
	DurationMS    int64 // 总耗时：请求开始到转发结束的耗时
	startedAt     time.Time
	firstTokenSet bool
}

// markFirstToken 记录首个输出增量的到达时刻，只生效一次。
func (i *relayInfo) markFirstToken() {
	if i.firstTokenSet || i.startedAt.IsZero() {
		return
	}
	i.firstTokenSet = true
	i.FirstTokenMS = time.Since(i.startedAt).Milliseconds()
}

// elapsedMS 返回本次尝试已消耗的毫秒数；从未发起请求时返回 0。
func (i *relayInfo) elapsedMS() int64 {
	if i.startedAt.IsZero() {
		return 0
	}
	return time.Since(i.startedAt).Milliseconds()
}

func (i *relayInfo) mergeUsage(u upstreamUsage) {
	i.mergeUsagePtr(&u)
}

func (i *relayInfo) mergeUsagePtr(u *upstreamUsage) {
	i.Usage.merge(*u)
	if u.InKnown {
		i.PromptTok = u.Prompt
		i.InKnown = true
	}
	if u.OutKnown {
		i.CompletionTok = u.Completion
		i.OutKnown = true
	}
}

func (i *relayInfo) protocolUsage() upstreamUsage {
	u := i.Usage
	u.Prompt, u.Completion, u.InKnown, u.OutKnown = i.PromptTok, i.CompletionTok, i.InKnown, i.OutKnown
	return u
}

// relayTask 一次转发所需的全部上下文。
type relayTask struct {
	client                    *requestClientState
	traceID                   string
	format                    clientFormat
	endpoint                  string    // 原生透传时的上游路径
	body                      []byte    // 客户端原始请求体（原生透传用）
	canon                     *canonReq // 规范化请求（转换用）
	model                     string    // 客户端请求的模型
	stream                    bool
	nativeOnly                bool // 只允许原生类型的上游（如 embeddings），禁止跨格式转换
	apiKeyID                  int64
	wantUsageChunk            bool   // openai chat 流式：客户端是否要求 include_usage
	openCodeSession           string // Reused across retries, tiers, and protocol conversions.
	openCodeRequest           string
	openCodeReasoningRepaired bool
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
		requestClientFor(c).model = cr.Model
		// Enforce permissions before affinity, routing, retries or billing.
		if !ak.allowsModel(cr.Model) {
			writeFormatError(c, format, http.StatusForbidden, "model is not allowed for this API key")
			return
		}
		if ak.quotaExhausted(cr.Model) {
			writeFormatError(c, format, http.StatusPaymentRequired, "quota exhausted for model "+cr.Model)
			return
		}
		if format == FormatGemini {
			_, method, _ := parseGeminiAction(c.Param("action"))
			cr.Stream = method == "streamGenerateContent"
		}

		endpoint := nativeEndpoint(format, cr)
		if nativeOnly {
			endpoint = c.Request.URL.Path
		}
		task := &relayTask{
			client:     requestClientFor(c),
			format:     format,
			endpoint:   endpoint,
			body:       body,
			canon:      cr,
			model:      cr.Model,
			stream:     cr.Stream,
			nativeOnly: nativeOnly,
			apiKeyID:   ak.ID,
		}

		// OpenAI chat 流式注入 include_usage，尽量拿到真实用量计费
		if format == FormatOpenAIChat && cr.Stream {
			task.wantUsageChunk = hasIncludeUsage(body)
			body = injectStreamUsage(body)
			task.body = body
		}

		// Routing affinity is also scoped by gateway key: identical opening
		// messages from different users must not share an upstream, its key,
		// or its proxy egress.
		skey := scopedSessionKey(ak.ID, sessionKeyFromCanon(endpoint, c.Request.Header, cr))

		s := getSettings()
		tried := map[int64]bool{}
		triedKeys := map[string]bool{}
		attempts := 1 + s.Retry
		var info relayInfo
		var attemptErr error
		var conversionErr error
		upstreamAttempted := false
		wrote := false

		for i := 0; i < attempts; i++ {
			if err := c.Request.Context().Err(); err != nil {
				attemptErr = err
				break
			}
			up, err := pickSessionUpstream(format, cr.Model, skey, tried, triedKeys, task.nativeOnly, cr)
			if err != nil {
				if attemptErr != nil && upstreamAttempted {
					attemptErr = fmt.Errorf("%w; failover unavailable: %v", attemptErr, err)
				} else if conversionErr != nil && !upstreamAttempted {
					attemptErr = conversionErr
				} else {
					attemptErr = err
				}
				break // Finalize once so unavailable models also contribute a failed request.
			}

			previousInfo, previousErr := info, attemptErr
			info, wrote, attemptErr = doRelay(c, up, task)
			var conversion *protocolConversionError
			if errors.As(attemptErr, &conversion) {
				tried[up.ID] = true
				conversionErr = attemptErr
				if upstreamAttempted {
					// A later local capability rejection must not replace the
					// actual upstream failure, its usage or the last attempted ID.
					info, attemptErr = previousInfo, previousErr
				}
				// A local conversion rejection made no upstream request. Try other
				// eligible protocols without spending a network retry or breaking affinity.
				i--
				continue
			}
			upstreamAttempted = true
			if wrote || errors.Is(attemptErr, errDownstreamWrite) {
				break
			}
			if attemptErr != nil {
				log.Printf("[failover] key=%s model=%s upstream=%s attempt=%d err=%v",
					ak.Name, cr.Model, up.Name, i+1, attemptErr)
			}
		}

		finalizeCall(c, ak, task, info, wrote, attemptErr, -1)
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
	if task.client == nil {
		task.client = requestClientFor(c)
	}
	task.markClientRecorded()
	if !wrote {
		if (c.Request != nil && c.Request.Context().Err() != nil) || errors.Is(lastErr, errDownstreamWrite) {
			insertFailureLog(ak, info, task, http.StatusBadGateway, "client disconnected")
			return
		}
		var conversion *protocolConversionError
		if errors.As(lastErr, &conversion) {
			writeRelayError(c, task.format, http.StatusBadRequest, conversion.Error())
			insertFailureLog(ak, info, task, http.StatusBadRequest, conversion.Error())
			return
		}
		var rateLimit *openCodeRateLimitError
		if errors.As(lastErr, &rateLimit) {
			c.Header("Retry-After", rateLimit.retryAfter())
			c.Header("X-APISHARE-RateLimit-Source", "upstream")
			writeRelayError(c, task.format, rateLimit.statusCode(), rateLimit.Error())
			insertFailureLog(ak, info, task, rateLimit.statusCode(), rateLimit.Error())
			return
		}
		// 所有上游都失败且未给客户端写出任何内容
		msg := "all upstream attempts failed"
		if lastErr != nil {
			msg += ": " + lastErr.Error()
		}
		writeRelayError(c, task.format, http.StatusBadGateway, msg)
		insertFailureLog(ak, info, task, http.StatusBadGateway, msg)
		return
	}

	success := info.Status >= 200 && info.Status < 300
	prompt, completion := info.PromptTok, info.CompletionTok
	if success {
		if !info.InKnown {
			prompt = inputTokensEst
			if prompt < 0 {
				prompt = estimatePromptTokens(task)
			}
		}
		if !info.OutKnown {
			completion = estimateTokensModel(task.model, info.OutputText.String())
		}
		reported := info.protocolUsage()
		usage := reported
		usage.Prompt, usage.Completion = prompt, completion
		counts := normalizeBillingUsage(usage)
		price, priceSource, pricingModel := lookupRelayPrice(task.model, info.UpstreamModel)
		cost := costForUsage(counts, price)
		_, independent := ak.ModelQuotas[task.model]
		quotaPool := "shared"
		if independent {
			quotaPool = "independent"
		}
		logStatus := info.Status
		logError := ""
		if lastErr != nil {
			// An interrupted stream is a failed call even after its HTTP 200 headers.
			logStatus = http.StatusBadGateway
			logError = "stream interrupted: " + lastErr.Error()
		}
		entry := &CallLog{
			Client:  task.clientIdentity(),
			TraceID: task.traceID,
			KeyID:   ak.ID, KeyName: ak.Name,
			UpstreamID: info.UpstreamID, UpstreamName: info.UpstreamName,
			Model: task.model, Endpoint: string(task.format),
			PromptTokens: counts.inputTotal(), CompletionTokens: counts.outputTotal(),
			CostUSD: cost.Total, Status: logStatus, LatencyMS: info.LatencyMS, Error: logError,
			FirstTokenMS: info.FirstTokenMS, DurationMS: info.DurationMS,
			Billing: &billingDetails{Source: usage.Source, Reported: reported, Usage: counts, Cost: cost, Price: price, PriceSource: priceSource, PricingModel: pricingModel, UpstreamModel: info.UpstreamModel, QuotaPool: quotaPool},
		}
		if err := dbRecordBilledCall(entry, independent); err != nil {
			log.Printf("[billing] key_id=%d model=%s: receipt and debit rolled back: %v", ak.ID, task.model, err)
		}
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
	insertFailureLog(ak, info, task, info.Status, errMsg)
}

func insertFailureLog(ak ApiKey, info relayInfo, task *relayTask, status int, errMsg string) {
	task.markClientRecorded()
	dbInsertLog(&CallLog{
		Client:  task.clientIdentity(),
		TraceID: task.traceID,
		KeyID:   ak.ID, KeyName: ak.Name,
		UpstreamID: info.UpstreamID, UpstreamName: info.UpstreamName,
		Model: task.model, Endpoint: string(task.format),
		Status: status, LatencyMS: info.LatencyMS, FirstTokenMS: info.FirstTokenMS, DurationMS: info.DurationMS, Error: errMsg,
	})
}

// ---------- 实际转发 ----------

func doRelay(c *gin.Context, up *Upstream, task *relayTask) (info relayInfo, wrote bool, err error) {
	info = relayInfo{UpstreamID: up.ID, UpstreamName: up.Name}
	upModel := up.mapModel(task.model)
	info.UpstreamModel = upModel
	if limit := up.openCodeRateLimit(upModel); limit != nil {
		info.Status = limit.statusCode()
		return info, false, limit
	}
	replayScope := responseReplayScope{task.apiKeyID, up.ID, task.model, upModel}
	wireFormat, err := up.resolveWireFormat(c.Request.Context(), task.format, upModel)
	if err != nil {
		return info, false, err
	}
	info.Usage.Source = wireFormat
	native := wireFormat == task.format || task.nativeOnly
	var effortLevels []string
	// A fully parsed request without an explicit or derived effort needs neither
	// catalog lookup nor another JSON pass. Opaque native requests keep the old path.
	if !task.nativeOnly && (task.canon == nil || task.canon.ConversionError != nil || protocolReasoningEffort(wireFormat, task.canon) != "") {
		effortLevels = upstreamReasoningEfforts(up, upModel)
	}

	var url string
	var reqBody []byte
	if native {
		url = up.endpointURL(wireFormat, task.endpoint)
		reqBody = task.body
		if upModel != task.model {
			if task.format == FormatGemini {
				url = replaceGeminiModel(url, upModel)
			} else {
				reqBody = replaceModelInBody(reqBody, upModel)
			}
		}
		if !task.nativeOnly {
			reqBody = mapReasoningEffortBody(reqBody, wireFormat, effortLevels)
		}
	} else {
		// Each failover starts from the client's original effort, not the value
		// mapped for the previous upstream.
		cr := *task.canon
		if task.format == FormatOpenAIResponse {
			cr.Messages = replayResponseReasoning(cr.Messages, replayScope)
		}
		cr.ReasoningEffort = mapReasoningEffort(protocolReasoningEffort(wireFormat, &cr), effortLevels)
		reqBody, err = renderProtocolRequest(wireFormat, &cr, upModel, up.StructuredOutput)
		if err != nil {
			info.Status = http.StatusBadRequest
			return info, false, &protocolConversionError{err}
		}
		url = up.endpointURL(wireFormat, nativeEndpoint(wireFormat, &canonReq{Model: upModel, Stream: task.stream}))
	}
	var freeShaped bool
	var freeTools openCodeToolCompatibility
	reqBody, freeShaped, err = up.shapeOpenCodeFreeRequest(reqBody, wireFormat, upModel, &freeTools)
	if err != nil {
		info.Status = http.StatusBadRequest
		return info, false, &protocolConversionError{err}
	}
	if freeShaped {
		endpoint := task.endpoint
		if !native {
			endpoint = nativeEndpoint(wireFormat, &canonReq{Model: upModel, Stream: task.stream})
		}
		url = up.openCodeFreeEndpointURL(wireFormat, endpoint)
	}

	var clineTask string
	if up.Type == "cline" {
		session := c.Request.Header.Get("X-Task-ID")
		if session == "" {
			session = c.Request.Header.Get("X-Session-Id")
		}
		if session == "" && task.canon != nil {
			session = task.canon.SessionID
		}
		reqBody, clineTask, err = shapeClineRequest(reqBody, fmt.Sprintf("%d:%d", task.apiKeyID, up.ID), session)
		if err != nil {
			return info, false, &protocolConversionError{err}
		}
	}
	requestCtx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, c.Request.Method, url, bytes.NewReader(reqBody))
	if err != nil {
		return info, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if clineTask != "" {
		req.Header.Set("X-Task-ID", clineTask)
	}
	fingerprint := openCodeFreeFingerprintRequest(up, req, upModel)
	if task.stream || freeShaped {
		req.Header.Set("Accept", "text/event-stream")
		if !fingerprint {
			// Compressors that do not flush each SSE frame can hide the entire
			// reasoning phase. An upstream may explicitly override this in
			// headers. Bun-compatible traffic instead advertises its native
			// encodings and decodes them in the transport.
			req.Header.Set("Accept-Encoding", "identity")
		}
	}
	switch up.Type {
	case "opencode", openCodeFreeType:
		session, sessionErr := openCodeSessionForTask(task, c.Request.Header)
		if sessionErr != nil {
			return info, false, sessionErr
		}
		setOpenCodeHeaders(req, up.openCodeAPIKey(), wireFormat, session, c.Request.Header, task, reqBody)
		if openCodeLegacyRequest(req) {
			if task.openCodeRequest == "" {
				task.openCodeRequest = req.Header.Get("x-opencode-request")
			}
			req.Header.Set("x-opencode-request", task.openCodeRequest)
		}
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
	if err := up.applyHeaders(req, c.Request.Header); err != nil {
		return info, false, err
	}
	if up.Type == "cliproxy" {
		if task.traceID == "" {
			task.traceID = randHex(16)
		}
		req.Header.Set(cliproxy.TraceHeader, task.traceID)
		setCLIProxyHeaders(req, c.Request.Header, task)
	}

	start := time.Now()
	info.startedAt = start
	// 总耗时覆盖整个转发过程（含流式输出），与只计到响应头的 LatencyMS 区分。
	defer func() { info.DurationMS = time.Since(start).Milliseconds() }()
	var client *http.Client
	if fingerprint {
		client, err = clientForOpenCodeFree(upstreamHTTPClient, up.ProxyID)
	} else {
		client, err = clientForUpstream(upstreamHTTPClient, up.ProxyID)
	}
	if err != nil {
		if up.selectedKeyID != "" {
			up.markRelayFailure(0, time.Time{})
		}
		return info, false, err
	}
	var retryAfter time.Time
	var rateLimit *openCodeRateLimitError
	// Count actual attempts, including interrupted streams, but never penalize
	// an upstream for client cancellation or request/configuration validation.
	defer func() {
		if c.Request.Context().Err() != nil || errors.Is(err, errDownstreamWrite) {
			return
		}
		if up.isOpenCode() && rateLimit != nil && up.selectedKeyID == "" {
			return
		}
		if err != nil || isRetryableStatus(info.Status) {
			up.markRelayFailure(info.Status, retryAfter)
		} else if info.Status >= 200 && info.Status < 300 {
			up.markRelaySuccess(start)
			markOpenCodeRateSuccess(up, start)
		}
	}()
	keepalive := beginMessagesKeepalive(c, task)
	resp, err := messagesHTTPDo(client, req, keepalive, cancel)
	if err == nil && up.Type == "cline" {
		resp, err = retryClineUnauthorized(client, req, resp, up, func(retry *http.Request) (*http.Response, error) {
			return messagesHTTPDo(client, retry, keepalive, cancel)
		})
	}
	info.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		if up.ProxyID != 0 && !errors.Is(err, errDownstreamWrite) && c.Request.Context().Err() == nil {
			// Transport errors may contain proxy URLs or credentials.
			return info, false, fmt.Errorf("通过代理连接上游失败，请检查代理地址、认证和网络")
		}
		return info, false, err
	}
	if relayNetwork.BodyIdleTimeout > 0 {
		resp.Body = newActivityDeadlineBody(resp.Body, relayNetwork.BodyIdleTimeout)
	}
	if up.isOpenCode() && wireFormat == FormatOpenAIResponse && resp.StatusCode == http.StatusBadRequest && !task.openCodeReasoningRepaired {
		original := resp
		resp, err = retryOpenCodeReasoning(client, req, resp, reqBody, keepalive, cancel)
		task.openCodeReasoningRepaired = resp != original
		if err != nil {
			original.Body.Close()
			return info, false, err
		}
		if resp != original && relayNetwork.BodyIdleTimeout > 0 {
			resp.Body = newActivityDeadlineBody(resp.Body, relayNetwork.BodyIdleTimeout)
		}
	}
	resp.Body = newMessagesKeepaliveBody(resp.Body, req.Context(), keepalive)
	defer resp.Body.Close()
	info.Status = resp.StatusCode
	if up.selectedKeyID != "" && isRetryableStatus(info.Status) {
		retryAfter = retryAfterHeader(resp.Header)
	}
	if up.Type == "cline" && isRetryableStatus(info.Status) {
		retryAfter = openCodeRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	}
	if up.isOpenCode() && isRetryableStatus(info.Status) {
		retryAfter = openCodeRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		if info.Status == http.StatusTooManyRequests {
			rateLimit = recordOpenCodeRateLimit(up, retryAfter)
			current, currentErr := cachedRoutingUpstream(up.ID)
			if up.selectedKeyID == "" && (currentErr != nil || (current.isOpenCode() && current.openCodeRateKey() == up.openCodeRateKey())) {
				healthMarkRetryAfter(up.ID, rateLimit.until)
			}
			if rateLimit.until.After(retryAfter) {
				retryAfter = rateLimit.until
			}
		}
	}
	if up.ProxyID != 0 && info.Status == http.StatusProxyAuthRequired {
		return info, false, fmt.Errorf("代理认证失败，请检查代理用户名和密码")
	}

	if info.Status < 200 || info.Status > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		if fingerprint && freeShaped && openCodeFreeIPBlocked(info.Status, b) {
			rateLimit = recordOpenCodeFreeExitBlock(up, openCodeRetryAfter(resp.Header.Get("Retry-After"), time.Now()))
		}
		if rateLimit != nil {
			rememberOpenCodeRateDetail(up, rateLimit, b, req.Header)
			info.OutputText.WriteString(rateLimit.detail)
			return info, false, rateLimit
		}
		detail := up.redactAPIKeys(string(b))
		if up.isOpenCode() {
			detail = redactModelTestError(detail, up.openCodeAPIKey())
		}
		info.OutputText.WriteString(detail)
		if isRetryableStatus(info.Status) {
			if up.isOpenCode() {
				if info.Status == http.StatusForbidden {
					return info, false, fmt.Errorf("upstream %s returned %d: %s; %s", up.Name, info.Status, truncate(detail, 1000), up.openCodeRequestSummary(req, reqBody, wireFormat))
				}
				return info, false, fmt.Errorf("upstream %s returned %d (model=%s, path=%s): %s", up.Name, info.Status, upModel, req.URL.EscapedPath(), truncate(detail, 1000))
			}
			return info, false, fmt.Errorf("upstream %s returned %d", up.Name, info.Status)
		}
		// 客户端错误（400/404/413/422 等）：不重试。原生透传原始错误体，转换路径包装成本格式错误
		if native && !(task.format == FormatAnthropic && keepalive != nil && c.Writer.Written()) {
			c.Data(info.Status, resp.Header.Get("Content-Type"), b)
		} else {
			writeRelayError(c, task.format, info.Status, truncate(string(b), 300))
		}
		return info, true, nil
	}

	ct := resp.Header.Get("Content-Type")
	if up.Type == "cline" {
		normalizeClineResponse(resp)
	}
	isSSE := strings.Contains(ct, "text/event-stream")
	if !freeTools.Renames.Empty() {
		resp.Body = newOpenCodeAliasBody(resp.Body, wireFormat, freeTools, isSSE)
	}

	if native {
		if !task.stream && isSSE && (!task.nativeOnly || task.format == FormatOpenAIResponse) {
			body, err := collectNativeProtocolSSE(resp.Body, task.format, &info)
			if err != nil {
				return info, false, err
			}
			var finalInfo relayInfo
			parseNativeNonStream(task.format, body, &finalInfo)
			info.mergeUsage(finalInfo.protocolUsage())
			if finalInfo.OutputText.Len() > 0 {
				info.OutputText.Reset()
				info.OutputText.WriteString(finalInfo.OutputText.String())
			}
			c.Data(http.StatusOK, "application/json", body)
			return info, true, nil
		}
		if task.stream && isSSE {
			err = relaySSENative(c, resp, task.format, &info, !task.nativeOnly)
			if err != nil && keepalive != nil && keepalive.committed && !keepalive.upstreamError && !errors.Is(err, errDownstreamWrite) && c.Request.Context().Err() == nil {
				writeMessagesStreamError(c, "upstream stream interrupted")
			}
			return info, relayResponseCommitted(c), err
		}
		b, err := readProtocolBody(resp.Body, 64<<20)
		if err != nil {
			return info, false, err
		}
		if !task.nativeOnly || task.format == FormatOpenAIResponse {
			if err := nativeProtocolResponseError(task.format, b); err != nil {
				return info, false, err
			}
		}
		parseNativeNonStream(task.format, b, &info)
		if task.stream && !task.nativeOnly {
			// The upstream answered with a single JSON body; the client still gets
			// SSE, so the first token time is when that delivery starts.
			info.markFirstToken()
			err = relayNativeJSONStream(c, task.format, b, task.wantUsageChunk)
			return info, relayResponseCommitted(c), err
		}
		c.Data(info.Status, ct, b)
		return info, true, nil
	}

	if task.stream && isSSE && task.canon.OutputFormat == nil {
		err = relayProtocolSSE(c, resp, wireFormat, task, &info)
		return info, relayResponseCommitted(c), err
	}
	var result *canonResponse
	if isSSE {
		// Buffer non-streaming clients and converted structured output. Validate
		// the complete JSON before sending content; Messages heartbeats continue.
		result, err = collectProtocolSSE(resp.Body, wireFormat)
	} else {
		var b []byte
		b, err = readProtocolBody(resp.Body, 64<<20)
		if err == nil {
			result, err = parseProtocolResponse(wireFormat, b)
		}
	}
	if err != nil {
		return info, false, err
	}
	info.OutputText.WriteString(result.text())
	info.mergeUsage(result.Usage)
	if err := validateStructuredOutput(task.canon.OutputFormat, result); err != nil {
		return info, false, err
	}
	if task.stream {
		// A JSON-only upstream still returns an SSE envelope to a streaming client.
		info.markFirstToken()
		writer := newProtocolStream(c, task.format, task.model, task.wantUsageChunk)
		writer.responseTools = task.canon.ResponseTools
		writer.reasoningScope = replayScope
		for i, block := range result.Blocks {
			if err = writer.push(c, streamDelta{Key: fmt.Sprintf("block_%d", i), Block: block}, result.Usage); err != nil {
				writer.fail(c)
				return info, true, err
			}
		}
		err = writer.finish(c, result.Usage, result.Finish, result.StopSequence)
		if err != nil {
			writer.fail(c)
		}
		return info, true, err
	}
	if task.format == FormatOpenAIResponse {
		for i, block := range result.Blocks {
			result.Blocks[i] = restoreResponseTool(block, task.canon.ResponseTools)
			if err := validateResponseToolArguments(result.Blocks[i]); err != nil {
				return info, false, err
			}
		}
	}
	response := renderProtocolResponse(task.format, task.model, "", result)
	if task.format == FormatOpenAIResponse {
		cacheResponseOutputReasoning(replayScope, response)
	}
	c.JSON(http.StatusOK, response)
	return info, true, nil
}

// relaySSENative preserves native frames, but holds initialization events until
// output starts so an early failure can still fail over without mixing streams.
func relaySSENative(c *gin.Context, resp *http.Response, format clientFormat, info *relayInfo, checkTerminal ...bool) error {
	verify := len(checkTerminal) == 0 || checkTerminal[0]
	reader := bufio.NewReaderSize(resp.Body, 1<<16)
	terminal := false
	committed := false
	keepalive := messagesState(c)
	var data []string
	var pending, frame strings.Builder
	consume := func() (bool, error) {
		payload := strings.Join(data, "\n")
		clear(data)
		data = data[:0]
		var done, output bool
		var eventErr error
		var event map[string]any
		var parsed bool
		if strings.TrimSpace(payload) != "" {
			// Validation, prelude detection, keepalive and accounting share the
			// same decoded event instead of parsing each frame independently.
			decodeErr := json.Unmarshal([]byte(payload), &event)
			parsed = decodeErr == nil
			if verify {
				switch {
				case payload == "[DONE]":
					done = format == FormatOpenAIChat
				case decodeErr != nil:
					eventErr = fmt.Errorf("invalid upstream SSE JSON")
				default:
					done, eventErr = nativeSSETerminalObject(format, event)
				}
				terminal = terminal || done
			}
			// An error before output must leave both headers and body untouched.
			if eventErr != nil && !committed {
				return false, eventErr
			}
			output = !committed && (!parsed || !nativeSSEPreludeObject(format, event))
			if keepalive != nil && parsed {
				keepalive.observe(event)
			}
		}
		if committed || !verify || output || done {
			if !c.Writer.Written() {
				h := c.Writer.Header()
				h.Set("Content-Type", "text/event-stream; charset=utf-8")
				h.Set("Cache-Control", "no-cache")
				h.Set("X-Accel-Buffering", "no")
				c.Writer.WriteHeader(http.StatusOK)
			}
			committed = true
			commitRelayResponse(c)
			if pending.Len() > 0 {
				if _, err := c.Writer.WriteString(pending.String()); err != nil {
					return false, fmt.Errorf("%w: %w", errDownstreamWrite, err)
				}
				pending.Reset()
			}
			if _, err := c.Writer.WriteString(frame.String()); err != nil {
				return false, fmt.Errorf("%w: %w", errDownstreamWrite, err)
			}
			c.Writer.Flush()
			if keepalive != nil {
				keepalive.lastWrite = time.Now()
			}
		} else {
			if pending.Len()+frame.Len() > maxBodyBytes {
				return false, fmt.Errorf("upstream SSE prelude too large")
			}
			pending.WriteString(frame.String())
		}
		frame.Reset()
		if parsed && recordNativeEventObject(format, event, info) {
			info.markFirstToken()
		}
		// Chat/Gemini finish reasons may precede usage. Only explicit closing
		// events end the read; waiting for TCP EOF can turn success into timeout.
		stop := verify && (payload == "[DONE]" || (done && (format == FormatOpenAIResponse || format == FormatAnthropic)))
		return stop, eventErr
	}

	for {
		line, rerr := reader.ReadString('\n')
		if line != "" {
			if frame.Len()+len(line) > maxBodyBytes {
				return fmt.Errorf("upstream SSE frame too large")
			}
			frame.WriteString(line)
			if strings.HasPrefix(line, "data:") {
				payload := strings.TrimPrefix(strings.TrimRight(line, "\r\n"), "data:")
				payload = strings.TrimPrefix(payload, " ")
				data = append(data, payload)
			}
			if line == "\n" || line == "\r\n" {
				if done, err := consume(); err != nil || done {
					return err
				}
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				if _, err := consume(); err != nil {
					return err
				}
				if verify && !terminal {
					return io.ErrUnexpectedEOF
				}
				return nil
			}
			return rerr
		}
	}
}

// recordNativeEvent 解析原生事件并累积输出与用量，返回是否解析出正文增量。
func recordNativeEvent(format clientFormat, payload string, info *relayInfo) bool {
	var j map[string]any
	if json.Unmarshal([]byte(payload), &j) != nil {
		return false
	}
	return recordNativeEventObject(format, j, info)
}

func recordNativeEventObject(format clientFormat, j map[string]any, info *relayInfo) bool {
	var delta string
	if format == FormatOpenAIResponse {
		delta, _ = parseResponsesSSEObject(j)
	} else {
		delta, _ = parseUpstreamSSEObject(string(format.nativeUpstreamType()), j)
	}
	if delta != "" {
		info.OutputText.WriteString(delta)
	}
	// Presence, rather than a positive value, distinguishes real zero usage from missing usage.
	usage := object(j["usage"])
	if format == FormatGemini {
		usage = object(j["usageMetadata"])
	}
	if format == FormatAnthropic && str(j["type"]) == "message_start" {
		usage = object(object(j["message"])["usage"])
	}
	if format == FormatOpenAIResponse {
		response := object(j["response"])
		if response == nil {
			response = j
		}
		usage = object(response["usage"])
	}
	info.mergeUsage(readUsage(usage, format))
	return delta != ""
}

func intFromAny(v any) int {
	// Converted stream events use Go ints before JSON encoding; native events
	// arrive as json.Number or float64. Preserve indexes in both paths.
	if n, ok := v.(int); ok {
		return n
	}
	if n, ok := v.(json.Number); ok {
		if integer, err := n.Int64(); err == nil {
			return int(integer)
		}
		if number, err := n.Float64(); err == nil {
			return int(number)
		}
	}
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
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	if _, exists := m["stream_options"]; exists {
		return body
	}
	m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func replaceModelInBody(body []byte, model string) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	m["model"], _ = json.Marshal(model)
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
