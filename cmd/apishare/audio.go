package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"apishare/internal/requestredact"
	"github.com/gin-gonic/gin"
)

type audioPart struct {
	header textproto.MIMEHeader
	name   string
	body   []byte
}

type audioRequest struct {
	body, session []byte
	contentType   string
	model         string
	parts         []audioPart
	signaling     bool
}

func parseAudioRequest(path, contentType, queryModel string, body []byte) (*audioRequest, error) {
	r := &audioRequest{body: body, contentType: contentType, signaling: strings.HasPrefix(path, "/v1/realtime")}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, errors.New("invalid Content-Type")
	}
	switch mediaType {
	case "application/sdp":
		if !r.signaling {
			return nil, errors.New("application/sdp is only supported for Realtime")
		}
		r.model = strings.TrimSpace(queryModel)
	case "application/json":
		if path != "/v1/audio/speech" {
			return nil, errors.New("this endpoint requires multipart/form-data or application/sdp")
		}
		j, err := decodeProtocolObject(body)
		if err != nil {
			return nil, errors.New("invalid JSON request")
		}
		r.model = strings.TrimSpace(str(j["model"]))
	case "multipart/form-data":
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		modelFields := 0
		for {
			part, err := reader.NextRawPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, errors.New("invalid multipart request")
			}
			data, err := io.ReadAll(part)
			if err != nil {
				return nil, errors.New("unreadable multipart request")
			}
			name := part.FormName()
			if (!r.signaling && name == "model") || (r.signaling && name == "session") {
				modelFields++
				if part.FileName() != "" || modelFields > 1 {
					return nil, errors.New("model/session must be a single form field")
				}
				if name == "session" {
					j, err := decodeProtocolObject(data)
					if err != nil {
						return nil, errors.New("invalid Realtime session JSON")
					}
					r.session = data
					r.model = strings.TrimSpace(str(j["model"]))
				} else {
					r.model = strings.TrimSpace(string(data))
				}
			}
			r.parts = append(r.parts, audioPart{part.Header, name, data})
		}
	default:
		return nil, errors.New("unsupported audio Content-Type")
	}
	if r.model == "" {
		return nil, errors.New("missing or invalid 'model' in request")
	}
	// A second model in the query must not bypass the model checked in the body.
	if queryModel != "" && queryModel != r.model {
		return nil, errors.New("query model and request model must match")
	}
	return r, nil
}

func (r *audioRequest) mappedBody(mapped string) ([]byte, string, error) {
	if mapped == r.model || strings.HasPrefix(r.contentType, "application/sdp") {
		return r.body, r.contentType, nil
	}
	if r.parts == nil {
		return replaceModelInBody(r.body, mapped), r.contentType, nil
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, part := range r.parts {
		data := part.body
		if part.name == "model" && !r.signaling {
			data = []byte(mapped)
		}
		if part.name == "session" && r.signaling {
			data = replaceModelInBody(r.session, mapped)
		}
		out, err := writer.CreatePart(part.header)
		if err != nil {
			return nil, "", err
		}
		if _, err := out.Write(data); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func audioHTTPHandler(c *gin.Context) {
	ak := c.MustGet("apiKey").(ApiKey)
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes))
	if err != nil {
		errJSON(c, http.StatusRequestEntityTooLarge, "request body too large or unreadable")
		return
	}
	request, err := parseAudioRequest(c.Request.URL.Path, c.GetHeader("Content-Type"), c.Query("model"), body)
	if err != nil {
		errJSON(c, http.StatusBadRequest, err.Error())
		return
	}
	requestClientFor(c).model = request.model
	if status, msg := audioPermission(ak, request.model); status != 0 {
		errJSON(c, status, msg)
		return
	}
	format := clientFormat("openai-audio")
	if request.signaling {
		format = FormatRealtime
	}
	redaction, err := resolveRequestRedaction(ak, request.model)
	if err != nil {
		handleRequestRedactionError(c, format, &requestredact.Error{Kind: "unavailable"})
		return
	}
	if redaction.Config.Mode == requestredact.ModeEnforce && redaction.Config.Coverage == requestredact.CoverageStrict {
		handleRequestRedactionError(c, format, &requestredact.Error{Kind: "unsupported"})
		return
	}
	receipt := redaction.makeReceipt()
	if redaction.Config.Mode != requestredact.ModeOff {
		receipt.Unchecked = []string{"unsupported_audio_or_realtime"}
		receipt.Status = "unchecked"
	}
	task := &relayTask{client: requestClientFor(c), format: format, endpoint: c.Request.URL.Path, model: request.model, apiKeyID: ak.ID, requestRedaction: redaction, requestRedactionReceipt: receipt}
	// These endpoints are explicitly not inspected in text coverage. Never
	// persist an unchecked audio/SDP payload as a redacted diagnostic.
	failureBody := body
	if redaction.Config.Mode == requestredact.ModeEnforce {
		failureBody = nil
	}
	task.initFailureLog(c, failureBody)
	tried := map[int64]bool{}
	triedKeys := map[string]bool{}
	skey := scopedSessionKey(ak.ID, sessionKeyFromCanon(task.endpoint, c.Request.Header, &canonReq{Model: request.model}))
	var info relayInfo
	var lastErr error
	for attempt := 0; attempt <= getSettings().Retry; attempt++ {
		up, err := pickSessionUpstream(FormatOpenAIChat, request.model, skey, tried, triedKeys, true, nil)
		if err != nil {
			break
		}
		upstreamModel := up.mapModel(request.model)
		mappedBody, contentType, err := request.mappedBody(upstreamModel)
		if err != nil {
			continue
		}
		endpoint := upstreamURL(up.BaseURL, task.endpoint)
		queryModel := ""
		if c.Request.URL.Query().Has("model") {
			queryModel = upstreamModel
		}
		if query := audioQuery(c.Request.URL.RawQuery, queryModel); query != "" {
			endpoint += "?" + query
		}
		req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, endpoint, bytes.NewReader(mappedBody))
		if err != nil {
			continue
		}
		req.Header, err = audioHeaders(c, up, contentType)
		if err != nil {
			continue
		}
		// Rebuilding a multipart body requires its new boundary, even with a
		// wildcard header passthrough rule in the upstream configuration.
		req.Header.Set("Content-Type", contentType)
		if err := checkRedactionHeaders(redaction, up, c.Request.Header, req.Header); err != nil {
			handleRequestRedactionError(c, format, err)
			return
		}
		client, err := audioClient(up)
		if err != nil {
			if up.selectedKeyID != "" {
				up.markRelayFailure(0, time.Time{})
			}
			continue
		}
		start := time.Now()
		info = relayInfo{UpstreamID: up.ID, UpstreamName: up.Name, UpstreamModel: upstreamModel, startedAt: start, requestRedaction: &receipt}
		requestCtx, cancel := context.WithCancel(req.Context())
		resp, err := client.Do(req.WithContext(requestCtx))
		lastErr = err
		info.LatencyMS = time.Since(start).Milliseconds()
		if err != nil {
			cancel()
			info.DurationMS = info.elapsedMS()
			if c.Request.Context().Err() != nil {
				recordRelayFailure(c, task, info, http.StatusBadGateway, c.Request.Context().Err(), ak.Key)
				insertFailureLog(ak, info, task, http.StatusBadGateway, "client disconnected")
				return
			}
			up.markRelayFailure(0, time.Time{})
			continue
		}
		if relayNetwork.BodyIdleTimeout > 0 {
			resp.Body = newActivityDeadlineBody(resp.Body, relayNetwork.BodyIdleTimeout)
		}
		info.Status = resp.StatusCode
		info.failureHTTPStatus = resp.StatusCode
		retryable := isRetryableStatus(resp.StatusCode) || (up.ProxyID != 0 && resp.StatusCode == http.StatusProxyAuthRequired)
		redirect := resp.StatusCode >= 300 && resp.StatusCode < 400
		if retryable || redirect {
			if failureLogsEnabled() && resp.StatusCode != http.StatusProxyAuthRequired {
				_, info.failureReply = readFailureReply(resp.Body, 0, cancel)
				info.failureSecrets = upstreamFailureSecrets(up, req)
			}
			resp.Body.Close()
			cancel()
			info.DurationMS = info.elapsedMS()
			if retryable {
				up.markRelayFailure(resp.StatusCode, retryAfterHeader(resp.Header))
			}
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			up.markRelaySuccess(start)
		}
		var capture *failureReplyReader
		if failureLogsEnabled() && (info.Status < 200 || info.Status >= 300) {
			capture = &failureReplyReader{ReadCloser: resp.Body, limit: failureCaptureLimit()}
			resp.Body = capture
		}
		err = copyAudioResponse(c, resp, &info)
		if capture != nil {
			info.failureReply = capture.reply
			info.failureReply.truncated = info.failureReply.truncated || err != nil
			info.failureSecrets = upstreamFailureSecrets(up, req)
		}
		info.DurationMS = time.Since(start).Milliseconds()
		resp.Body.Close()
		cancel()
		if err != nil && !c.Writer.Written() {
			errJSON(c, http.StatusBadGateway, "audio upstream response interrupted")
		}
		if request.signaling {
			// Media and data-channel events flow directly between the browser
			// and provider. Do not invent usage or charge the SDP as text.
			status, message := info.Status, ""
			if err != nil {
				status, message = 502, "WebRTC signaling response interrupted"
			}
			if err != nil || status < 200 || status >= 300 {
				recordRelayFailure(c, task, info, status, err, ak.Key)
			}
			var billing *billingDetails
			if redaction.Config.Mode != requestredact.ModeOff {
				billing = &billingDetails{Source: format, RequestRedaction: &receipt}
			}
			task.markClientRecorded()
			dbInsertLog(&CallLog{Client: task.clientIdentity(), TraceID: task.traceID, KeyID: ak.ID, KeyName: ak.Name, UpstreamID: up.ID, UpstreamName: up.Name, Model: task.model, Endpoint: string(format), Status: status, LatencyMS: info.LatencyMS, DurationMS: info.DurationMS, Error: message, Billing: billing})
		} else {
			finalizeCall(c, ak, task, info, true, err, 0)
		}
		return
	}
	errJSON(c, http.StatusBadGateway, "no available audio upstream or upstream request failed")
	recordRelayFailure(c, task, info, http.StatusBadGateway, lastErr, ak.Key)
	insertFailureLog(ak, info, task, http.StatusBadGateway, "audio upstream request failed")
}

func copyAudioResponse(c *gin.Context, resp *http.Response, info *relayInfo) error {
	for _, name := range []string{"Content-Type", "Content-Disposition", "X-Request-Id", "Cache-Control"} {
		if value := resp.Header.Get(name); value != "" {
			c.Header(name, value)
		}
	}
	c.Status(resp.StatusCode)
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		reader := bufio.NewReader(resp.Body)
		var event bytes.Buffer
		for {
			line, err := reader.ReadSlice('\n')
			// Never retain unbounded SSE data while preserving original bytes.
			if event.Len()+len(line) > maxBodyBytes {
				return errors.New("audio SSE event too large")
			}
			event.Write(line)
			if err != bufio.ErrBufferFull {
				if bytes.HasPrefix(event.Bytes(), []byte("data:")) {
					mergeAudioUsage(bytes.TrimSpace(bytes.TrimPrefix(event.Bytes(), []byte("data:"))), info)
				}
				if _, writeErr := c.Writer.Write(event.Bytes()); writeErr != nil {
					return errDownstreamWrite
				}
				info.markFirstToken()
				c.Writer.Flush()
				event.Reset()
			}
			if err == io.EOF {
				return nil
			}
			if err != nil && err != bufio.ErrBufferFull {
				return err
			}
		}
	}
	if strings.Contains(contentType, "application/json") {
		body, err := readProtocolBody(resp.Body, maxBodyBytes)
		if err != nil {
			return err
		}
		mergeAudioUsage(body, info)
		_, err = c.Writer.Write(body)
		return err
	}
	buffer := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buffer)
		if n > 0 {
			if _, writeErr := c.Writer.Write(buffer[:n]); writeErr != nil {
				return errDownstreamWrite
			}
			c.Writer.Flush()
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func mergeAudioUsage(body []byte, info *relayInfo) {
	root, err := decodeProtocolObject(body)
	if err != nil {
		return
	}
	usage := object(root["usage"])
	if usage == nil {
		return
	}
	info.mergeUsage(readUsage(usage, FormatOpenAIResponse))
}
