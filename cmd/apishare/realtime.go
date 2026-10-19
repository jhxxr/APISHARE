package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

const FormatRealtime clientFormat = "openai-realtime"

func registerAudioRoutes(v1 *gin.RouterGroup) {
	v1.GET("/realtime", realtimeWebSocket)
	v1.POST("/realtime", audioHTTPHandler)       // beta WebRTC: application/sdp + ?model=
	v1.POST("/realtime/calls", audioHTTPHandler) // GA WebRTC: multipart sdp + session
	v1.POST("/audio/speech", audioHTTPHandler)
	v1.POST("/audio/transcriptions", audioHTTPHandler)
	v1.POST("/audio/translations", audioHTTPHandler)
}

func websocketProtocols(header http.Header) []string {
	var protocols []string
	for _, value := range header.Values("Sec-WebSocket-Protocol") {
		for _, protocol := range strings.Split(value, ",") {
			if protocol = strings.TrimSpace(protocol); protocol != "" {
				protocols = append(protocols, protocol)
			}
		}
	}
	return protocols
}

func audioPermission(ak ApiKey, model string) (int, string) {
	if model == "" {
		return http.StatusBadRequest, "missing or invalid 'model' in request"
	}
	if !ak.Enabled {
		return http.StatusUnauthorized, "invalid or disabled API key"
	}
	if !ak.allowsModel(model) {
		return http.StatusForbidden, "model is not allowed for this API key"
	}
	if ak.quotaExhausted(model) {
		return http.StatusPaymentRequired, "quota exhausted for model " + model
	}
	return 0, ""
}

func refreshAudioPermission(keyID int64, model string) (ApiKey, error) {
	ak, err := dbGetKeyByID(keyID)
	if err != nil {
		return ApiKey{}, errors.New("API key is no longer available")
	}
	if _, msg := audioPermission(*ak, model); msg != "" {
		return ApiKey{}, errors.New(msg)
	}
	return *ak, nil
}

// Reuse the established HTTP transport, including environment and per-upstream
// HTTP(S)/SOCKS proxies. Redirects must not move an authenticated handshake.
func audioClient(up *Upstream) (*http.Client, error) {
	base, err := clientForUpstream(upstreamHTTPClient, up.ProxyID)
	if err != nil {
		return nil, err
	}
	client := *base
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client, nil
}

func audioHeaders(c *gin.Context, up *Upstream, contentType string) (http.Header, error) {
	req := &http.Request{Header: make(http.Header)}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for _, name := range []string{"OpenAI-Beta", "OpenAI-Organization", "OpenAI-Project", "OpenAI-Safety-Identifier", "Accept"} {
		if value := c.GetHeader(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	if err := up.applyHeaders(req, c.Request.Header); err != nil {
		return nil, err
	}
	// Header wildcard rules must not forward a client credential subprotocol or
	// handshake state. The WebSocket library creates its own fresh handshake.
	for name := range req.Header {
		if strings.HasPrefix(strings.ToLower(name), "sec-websocket-") || transportHeader(name) {
			req.Header.Del(name)
		}
	}
	return req.Header, nil
}

func audioQuery(raw string, model string) string {
	q, _ := url.ParseQuery(raw)
	for name := range q {
		switch strings.ToLower(name) {
		case "key", "api_key", "access_token", "authorization":
			q.Del(name)
		}
	}
	if q.Has("model") || model != "" {
		q.Set("model", model)
	}
	return q.Encode()
}

func realtimeWebSocket(c *gin.Context) {
	if !strings.EqualFold(c.GetHeader("Upgrade"), "websocket") {
		errJSON(c, http.StatusUpgradeRequired, "Realtime requires a WebSocket upgrade")
		return
	}
	ak := c.MustGet("apiKey").(ApiKey)
	model := strings.TrimSpace(c.Query("model"))
	if status, msg := audioPermission(ak, model); status != 0 {
		errJSON(c, status, msg)
		return
	}
	task := &relayTask{format: FormatRealtime, endpoint: "/v1/realtime", model: model, apiKeyID: ak.ID, stream: true}
	tried := map[int64]bool{}
	skey := scopedSessionKey(ak.ID, sessionKeyFromCanon(task.endpoint, c.Request.Header, &canonReq{Model: fmt.Sprint(ak.ID) + ":" + model}))
	var upstream *websocket.Conn
	var up *Upstream
	var info relayInfo
	for attempt := 0; attempt <= getSettings().Retry; attempt++ {
		var err error
		if attempt == 0 {
			if id, ok := affinityGet(skey); ok {
				if candidate, e := cachedRoutingUpstream(id); e == nil && candidate.Type == "openai" && candidate.Enabled && candidate.Weight > 0 && candidate.servesModel(model) && healthAvailable(id) {
					up = candidate
				}
			}
		}
		if up == nil || tried[up.ID] {
			up, err = pickUpstream(FormatOpenAIChat, model, tried, true)
			if err != nil {
				break
			}
		}
		tried[up.ID] = true
		info = relayInfo{UpstreamID: up.ID, UpstreamName: up.Name, UpstreamModel: up.mapModel(model)}
		client, err := audioClient(up)
		if err != nil {
			continue
		}
		headers, err := audioHeaders(c, up, "")
		if err != nil {
			continue
		}
		endpoint := upstreamURL(up.BaseURL, task.endpoint) + "?" + audioQuery(c.Request.URL.RawQuery, info.UpstreamModel)
		dialCtx, cancel := context.WithTimeout(c.Request.Context(), relayNetwork.DialTimeout)
		start := time.Now()
		info.startedAt = start
		var resp *http.Response
		upstream, resp, err = websocket.Dial(dialCtx, endpoint, &websocket.DialOptions{HTTPClient: client, HTTPHeader: headers})
		cancel()
		info.LatencyMS = time.Since(start).Milliseconds()
		info.DurationMS = info.LatencyMS
		if err == nil {
			healthMarkSuccess(up.ID)
			affinitySet(skey, up.ID)
			break
		}
		if c.Request.Context().Err() != nil {
			return
		}
		if resp != nil {
			info.Status = resp.StatusCode
			if !isRetryableStatus(resp.StatusCode) && resp.StatusCode >= 400 && !(up.ProxyID != 0 && resp.StatusCode == http.StatusProxyAuthRequired) {
				var body []byte
				if resp.Body != nil {
					body, _ = io.ReadAll(io.LimitReader(resp.Body, 16<<10))
					resp.Body.Close()
				}
				c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
				insertFailureLog(ak, info, task, info.Status, "upstream rejected Realtime handshake")
				return
			}
			if resp.Body != nil {
				resp.Body.Close()
			}
		}
		healthMarkFailure(up.ID)
	}
	if upstream == nil {
		errJSON(c, http.StatusBadGateway, "no available Realtime upstream or WebSocket handshake failed")
		insertFailureLog(ak, info, task, 502, "Realtime handshake failed")
		return
	}
	defer upstream.CloseNow()
	downstream, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
		// Authentication uses an explicit API key, never cookies, matching corsV1.
		InsecureSkipVerify: true, Subprotocols: []string{"realtime", "openai-beta.realtime-v1"},
	})
	if err != nil {
		return
	}
	defer downstream.CloseNow()
	upstream.SetReadLimit(maxBodyBytes)
	downstream.SetReadLimit(maxBodyBytes)
	duration := time.Duration(networkEnvInt("REALTIME_MAX_DURATION_SEC", 3600, 1, 86400)) * time.Second
	ctx, cancel := context.WithTimeout(c.Request.Context(), duration)
	defer cancel()
	bridgeRealtime(ctx, c, ak, task, info, downstream, upstream)
}

type realtimeExit struct {
	fromUpstream bool
	err          error
}

func realtimeRead(ctx context.Context, conn *websocket.Conn) (websocket.MessageType, []byte, error) {
	if relayNetwork.BodyIdleTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, relayNetwork.BodyIdleTimeout)
		defer cancel()
	}
	return conn.Read(ctx)
}

func realtimeWrite(ctx context.Context, conn *websocket.Conn, kind websocket.MessageType, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return conn.Write(ctx, kind, body)
}

func bridgeRealtime(ctx context.Context, c *gin.Context, ak ApiKey, task *relayTask, info relayInfo, downstream, upstream *websocket.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	exits := make(chan realtimeExit, 2)
	go func() {
		for {
			kind, body, err := realtimeRead(ctx, downstream)
			if err == nil && kind == websocket.MessageText {
				body, err = mapRealtimeClientEvent(body, task.model, info.UpstreamModel)
				var event struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal(body, &event)
				if err == nil && (event.Type == "response.create" || event.Type == "session.update" || event.Type == "input_audio_buffer.commit") {
					_, err = refreshAudioPermission(ak.ID, task.model)
				}
				if err != nil {
					errorBody, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": err.Error()}})
					_ = realtimeWrite(ctx, downstream, websocket.MessageText, errorBody)
					exits <- realtimeExit{err: websocket.CloseError{Code: websocket.StatusPolicyViolation}}
					return
				}
			}
			if err == nil {
				err = realtimeWrite(ctx, upstream, kind, body)
			}
			if err != nil {
				exits <- realtimeExit{err: err}
				return
			}
		}
	}()
	go func() {
		seen := map[string]bool{}
		// 每轮 response.done 记一次日志，耗时按上一轮结束到本轮结束计算。
		turnStart := info.startedAt
		if turnStart.IsZero() {
			turnStart = time.Now()
		}
		for {
			kind, body, err := realtimeRead(ctx, upstream)
			var permissionErr error
			if err == nil && kind == websocket.MessageText {
				if id, usage, ok := realtimeEventUsage(body); ok && !seen[id] {
					if len(seen) >= 65536 {
						exits <- realtimeExit{fromUpstream: true, err: websocket.CloseError{Code: websocket.StatusPolicyViolation}}
						return
					}
					seen[id] = true
					turn := relayInfo{UpstreamID: info.UpstreamID, UpstreamName: info.UpstreamName, UpstreamModel: info.UpstreamModel, Status: 200, LatencyMS: info.LatencyMS, DurationMS: time.Since(turnStart).Milliseconds()}
					turnStart = time.Now()
					turn.mergeUsage(usage)
					// Bill terminal provider usage before delivering it, even when the
					// client has already disconnected. Never estimate audio as text.
					finalizeCall(c, ak, task, turn, true, nil, 0)
					_, permissionErr = refreshAudioPermission(ak.ID, task.model)
				}
			}
			if err == nil {
				err = realtimeWrite(ctx, downstream, kind, body)
			}
			if err == nil && permissionErr != nil {
				err = websocket.CloseError{Code: websocket.StatusPolicyViolation}
			}
			if err != nil {
				exits <- realtimeExit{fromUpstream: true, err: err}
				return
			}
		}
	}()
	first := <-exits
	code := websocket.CloseStatus(first.err)
	if code < 1000 || code == 1005 || code == 1006 || code == 1015 {
		code = websocket.StatusInternalError
	}
	// Send the close to the other peer before cancelling blocked reads. CloseNow
	// below guarantees that both pumps exit and release the key concurrency slot.
	peer := upstream
	if first.fromUpstream {
		peer = downstream
	}
	_ = peer.Close(code, "Realtime connection closed")
	cancel()
	_ = upstream.CloseNow()
	_ = downstream.CloseNow()
	<-exits
	if code != websocket.StatusNormalClosure && code != websocket.StatusGoingAway && code != websocket.StatusPolicyViolation {
		info.DurationMS = info.elapsedMS()
		insertFailureLog(ak, info, task, 502, "Realtime connection interrupted")
	}
}

// Only rewrite protocol model fields; audio, tool arguments and unknown event
// payloads remain byte-for-byte intact. A connection cannot switch routing model.
func mapRealtimeClientEvent(body []byte, model, mapped string) ([]byte, error) {
	var event map[string]json.RawMessage
	if json.Unmarshal(body, &event) != nil {
		return body, nil // Let the provider return its native invalid-event error.
	}
	changed := false
	var eventType string
	_ = json.Unmarshal(event["type"], &eventType)
	field := map[string]string{"session.update": "session", "response.create": "response"}[eventType]
	if field != "" {
		var value map[string]json.RawMessage
		if json.Unmarshal(event[field], &value) != nil || value == nil {
			return body, nil
		}
		if raw, exists := value["model"]; exists {
			var requested string
			if json.Unmarshal(raw, &requested) != nil || (requested != model && requested != mapped) {
				return nil, errors.New("Realtime model cannot be changed within a connection")
			}
			if model != mapped {
				value["model"], _ = json.Marshal(mapped)
				event[field], _ = json.Marshal(value)
				changed = true
			}
		}
	}
	if !changed {
		return body, nil
	}
	return json.Marshal(event)
}

func realtimeEventUsage(body []byte) (string, upstreamUsage, bool) {
	event, err := decodeProtocolObject(body)
	if err != nil || str(event["type"]) != "response.done" {
		return "", upstreamUsage{}, false
	}
	response := object(event["response"])
	id := str(response["id"])
	if id == "" {
		return "", upstreamUsage{}, false
	}
	usage := readUsage(object(response["usage"]), FormatRealtime)
	return id, usage, usage.InKnown || usage.OutKnown
}
