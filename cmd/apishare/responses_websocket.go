package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"apishare/internal/cliproxy"
	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

const responsesWSMaxPending = 64
const responsesWSMaxSeen = 65536
const responsesWSErrorGrace = 1200 * time.Millisecond

var responsesWSConnections = struct {
	sync.Mutex
	byKey map[int64]int
}{byKey: make(map[int64]int)}

type responsesWSMessage struct {
	kind websocket.MessageType
	body []byte
	err  error
}

type responsesWSRequest struct {
	Type       string `json:"type"`
	Model      string `json:"model"`
	StreamID   string `json:"stream_id"`
	EventID    string `json:"event_id"`
	ResponseID string `json:"response_id"`
	Generate   *bool  `json:"generate"`
}

type responsesWSProviderError struct {
	EventID string `json:"event_id"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

type responsesWSEvent struct {
	ID         string                   `json:"id"`
	Type       string                   `json:"type"`
	StreamID   *string                  `json:"stream_id"`
	ResponseID string                   `json:"response_id"`
	Status     json.RawMessage          `json:"status"`
	Usage      json.RawMessage          `json:"usage"`
	Delta      json.RawMessage          `json:"delta"`
	Error      responsesWSProviderError `json:"error"`
	Response   struct {
		ID     string                   `json:"id"`
		Status string                   `json:"status"`
		Usage  json.RawMessage          `json:"usage"`
		Error  responsesWSProviderError `json:"error"`
	} `json:"response"`
}

type responsesWSTurn struct {
	key        ApiKey
	task       relayTask
	info       relayInfo
	streamID   string
	eventID    string
	responseID string
	warmup     bool
	limit      *keyLimit
	failedAt   time.Time
	failStatus int
	failDetail string
}

type responsesWSSession struct {
	c              *gin.Context
	keyID          int64
	credential     string
	model          string
	sessionID      string
	coreTrace      string
	up             *Upstream
	pending        []*responsesWSTurn
	byResponse     map[string]*responsesWSTurn
	seen           map[string]bool
	downstreamGone bool
}

func nativeResponsesWS(up *Upstream) bool {
	return up.Type == "cliproxy" || up.Type == "openai" && supportsNativeEndpoint(up, FormatOpenAIResponse)
}

func responsesWebSocket(c *gin.Context) {
	if !strings.EqualFold(c.GetHeader("Upgrade"), "websocket") {
		errJSON(c, http.StatusUpgradeRequired, "Responses requires a WebSocket upgrade")
		return
	}
	snapshot, err := getRoutingSnapshot()
	if err != nil {
		errJSON(c, http.StatusServiceUnavailable, "unable to load upstreams")
		return
	}
	available := false
	for i := range snapshot.upstreams {
		up := &snapshot.upstreams[i]
		available = available || up.Enabled && up.Weight > 0 && nativeResponsesWS(up)
	}
	if !available {
		// Reject before upgrading so HTTP clients can retain their POST path.
		errJSON(c, http.StatusNotFound, "Responses WebSocket requires a native Responses upstream; use POST /v1/responses")
		return
	}
	ak := c.MustGet("apiKey").(ApiKey)
	maxConnections := networkEnvInt("RESPONSES_WS_MAX_CONNECTIONS_PER_KEY", 16, 1, 1024)
	responsesWSConnections.Lock()
	if responsesWSConnections.byKey[ak.ID] >= maxConnections {
		responsesWSConnections.Unlock()
		c.Header("Retry-After", "1")
		errJSON(c, http.StatusTooManyRequests, "too many Responses WebSocket connections for this key")
		return
	}
	responsesWSConnections.byKey[ak.ID]++
	responsesWSConnections.Unlock()
	defer func() {
		responsesWSConnections.Lock()
		responsesWSConnections.byKey[ak.ID]--
		if responsesWSConnections.byKey[ak.ID] == 0 {
			delete(responsesWSConnections.byKey, ak.ID)
		}
		responsesWSConnections.Unlock()
	}()
	downstream, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer downstream.CloseNow()
	downstream.SetReadLimit(maxBodyBytes)
	// The reader lifetime is owned by the relay. A disconnected HTTP peer
	// must not cancel an already accepted turn before its late usage arrives.
	ctx, cancel := context.WithCancel(context.WithoutCancel(c.Request.Context()))
	defer cancel()
	state := &responsesWSSession{
		c: c, keyID: ak.ID, credential: ak.Key, sessionID: randHex(16), coreTrace: randHex(16),
		byResponse: make(map[string]*responsesWSTurn), seen: make(map[string]bool),
	}
	state.run(ctx, downstream)
}

// Pumps own only socket reads and control pings. The handler owns admission,
// provider events, billing and all Gin context access.
func responsesWSReader(ctx context.Context, conn *websocket.Conn, group *sync.WaitGroup) <-chan responsesWSMessage {
	out := make(chan responsesWSMessage, 1)
	group.Add(1)
	go func() {
		defer group.Done()
		for {
			kind, body, err := conn.Read(ctx)
			select {
			case out <- responsesWSMessage{kind, body, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return out
}

func responsesWSPings(ctx context.Context, conn *websocket.Conn, failures chan<- error, group *sync.WaitGroup) {
	interval := time.Duration(networkEnvInt("RESPONSES_WS_PING_INTERVAL_SEC", 30, 1, 300)) * time.Second
	group.Add(1)
	go func() {
		defer group.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Ping(pingCtx)
				cancel()
				if err != nil {
					select {
					case failures <- err:
					case <-ctx.Done():
					}
					return
				}
			}
		}
	}()
}

func (s *responsesWSSession) run(ctx context.Context, downstream *websocket.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	// A lifetime timer asks the handler to send a close frame first. Expiring
	// the reader context itself would tear down the socket with a bare EOF.
	duration := time.Duration(networkEnvInt("RESPONSES_WS_MAX_DURATION_SEC", 3300, 1, 86400)) * time.Second
	lifetime := time.NewTimer(duration)
	defer lifetime.Stop()
	errorTimer := time.NewTimer(time.Hour)
	errorTimer.Stop()
	defer errorTimer.Stop()
	var errorsDue <-chan time.Time
	rearmErrors := func() {
		errorTimer.Stop()
		var deadline time.Time
		for _, turn := range s.pending {
			if !turn.failedAt.IsZero() {
				due := turn.failedAt.Add(responsesWSErrorGrace)
				if deadline.IsZero() || due.Before(deadline) {
					deadline = due
				}
			}
		}
		errorsDue = nil
		if !deadline.IsZero() {
			errorTimer.Reset(max(0, time.Until(deadline)))
			errorsDue = errorTimer.C
		}
	}
	var group sync.WaitGroup
	var upstream *websocket.Conn
	var incoming <-chan responsesWSMessage
	failures := make(chan error, 2)
	outgoing := responsesWSReader(ctx, downstream, &group)
	responsesWSPings(ctx, downstream, failures, &group)
	defer func() {
		cancel()
		_ = downstream.CloseNow()
		if upstream != nil {
			_ = upstream.CloseNow()
		}
		group.Wait()
		for len(s.pending) > 0 {
			turn := s.pending[0]
			status, detail := http.StatusBadGateway, "Responses WebSocket interrupted before terminal usage"
			if !turn.failedAt.IsZero() {
				status, detail = turn.failStatus, turn.failDetail
			}
			_ = s.finish(turn, nil, status, detail)
		}
	}()
	closeCode := websocket.StatusNormalClosure
	defer func() {
		_ = downstream.Close(closeCode, "Responses connection closed; reconnect with full input if needed")
	}()
	for {
		select {
		case <-lifetime.C:
			closeCode = websocket.StatusGoingAway
			return
		case <-ctx.Done():
			closeCode = websocket.StatusGoingAway
			return
		case <-s.c.Request.Context().Done():
			s.drain(ctx, incoming)
			closeCode = websocket.StatusGoingAway
			return
		case <-errorsDue:
			if err := s.settleErrors(time.Now(), false); err != nil {
				closeCode = websocket.StatusInternalError
				return
			}
			rearmErrors()
			continue
		default:
		}
		var message responsesWSMessage
		fromUpstream := false
		// Prefer already received usage over a concurrent downstream disconnect.
		select {
		case message = <-incoming:
			fromUpstream = true
		default:
			select {
			case message = <-incoming:
				fromUpstream = true
			case message = <-outgoing:
			case <-ctx.Done():
				closeCode = websocket.StatusGoingAway
				return
			case <-s.c.Request.Context().Done():
				s.drain(ctx, incoming)
				closeCode = websocket.StatusGoingAway
				return
			case <-errorsDue:
				if err := s.settleErrors(time.Now(), false); err != nil {
					closeCode = websocket.StatusInternalError
					return
				}
				rearmErrors()
				continue
			case <-lifetime.C:
				closeCode = websocket.StatusGoingAway
				return
			case <-failures:
				s.drain(ctx, incoming)
				closeCode = websocket.StatusGoingAway
				return
			}
		}
		if message.err != nil {
			if !fromUpstream {
				s.drain(ctx, incoming)
			} else if err := s.settleErrors(time.Now(), true); err != nil {
				closeCode = websocket.StatusInternalError
				return
			}
			code := websocket.CloseStatus(message.err)
			if code >= 1000 && code != 1005 && code != 1006 && code != 1015 {
				closeCode = code
			} else {
				closeCode = websocket.StatusInternalError
			}
			if fromUpstream && len(s.pending) > 0 && s.c.Request.Context().Err() == nil {
				// A clean transport close is still a failed inference when the
				// provider never sent the terminal protocol event.
				if code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway {
					closeCode = websocket.StatusInternalError
				}
				if code != websocket.StatusPolicyViolation && code != websocket.StatusMessageTooBig {
					s.up.markRelayFailure(0, time.Time{})
				}
			}
			return
		}
		if message.kind != websocket.MessageText {
			if fromUpstream {
				_ = writeResponsesWSError(ctx, downstream, nil, 502, "upstream_error", "upstream Responses events must be JSON text")
				closeCode = websocket.StatusUnsupportedData
				return
			}
			if err := writeResponsesWSError(ctx, downstream, nil, 400, "invalid_request_error", "Responses requires JSON text messages"); err != nil {
				return
			}
			continue
		}
		if fromUpstream {
			body, err := s.serverEvent(message.body)
			if err != nil {
				_ = writeResponsesWSError(ctx, downstream, nil, 502, "upstream_error", err.Error())
				closeCode = websocket.StatusInternalError
				return
			}
			rearmErrors()
			if err := realtimeWrite(ctx, downstream, websocket.MessageText, body); err != nil {
				s.drain(ctx, incoming)
				return
			}
			continue
		}
		var request responsesWSRequest
		if err := json.Unmarshal(message.body, &request); err != nil || request.Type == "" || len(request.StreamID) > 128 || len(request.EventID) > 256 {
			if err := writeResponsesWSError(ctx, downstream, nil, 400, "invalid_request_error", "invalid Responses WebSocket event"); err != nil {
				return
			}
			continue
		}
		if request.Type != "response.create" && request.Type != "response.append" {
			if upstream != nil && (request.Type == "response.cancel" || request.Type == "response.interrupt") {
				if request.ResponseID != "" && s.byResponse[request.ResponseID] == nil && !s.seen[request.ResponseID] {
					if err := writeResponsesWSError(ctx, downstream, &request, 400, "invalid_request_error", "response does not belong to this connection"); err != nil {
						return
					}
					continue
				}
				if err := realtimeWrite(ctx, upstream, websocket.MessageText, message.body); err != nil {
					return
				}
			} else if err := writeResponsesWSError(ctx, downstream, &request, 400, "unsupported_event", "supported events are response.create, response.append, response.cancel and response.interrupt"); err != nil {
				return
			}
			continue
		}
		model := request.Model
		if model == "" {
			model = s.model
		}
		if s.model != "" && model != s.model {
			s.reject(model, 400, "model cannot change within a Responses connection")
			if err := writeResponsesWSError(ctx, downstream, &request, 400, "invalid_request_error", "model cannot change within a Responses connection"); err != nil {
				return
			}
			continue
		}
		turn, status, detail := s.admit(model, &request)
		if status != 0 {
			s.reject(model, status, detail)
			if err := writeResponsesWSError(ctx, downstream, &request, status, "apishare_error", detail); err != nil {
				return
			}
			continue
		}
		if upstream == nil {
			var err error
			upstream, err = s.dial(ctx, model)
			if err != nil {
				if s.up != nil {
					turn.info.UpstreamID, turn.info.UpstreamName, turn.info.UpstreamModel = s.up.ID, s.up.Name, s.up.mapModel(model)
				}
				_ = s.finish(turn, nil, 502, "no available Responses WebSocket upstream")
				_ = writeResponsesWSError(ctx, downstream, &request, 502, "upstream_error", "Responses WebSocket handshake failed; use HTTP POST for HTTP-only providers")
				closeCode = websocket.StatusInternalError
				return
			}
			s.model = model
			incoming = responsesWSReader(ctx, upstream, &group)
			responsesWSPings(ctx, upstream, failures, &group)
		}
		turn.info.UpstreamID, turn.info.UpstreamName, turn.info.UpstreamModel = s.up.ID, s.up.Name, s.up.mapModel(model)
		if s.up.Type == "cliproxy" {
			// SDK attempt records share the handshake trace. Billing still
			// produces an independent receipt for every terminal response ID.
			turn.task.traceID = s.coreTrace
		}
		body := message.body
		if request.Model != turn.info.UpstreamModel {
			body = replaceModelInBody(body, turn.info.UpstreamModel)
		}
		if err := realtimeWrite(ctx, upstream, websocket.MessageText, body); err != nil {
			closeCode = websocket.StatusInternalError
			return
		}
	}
}

// Stop accepting client input, but briefly retain the upstream reader so a
// close/write failure cannot discard an already generated terminal receipt.
func (s *responsesWSSession) drain(ctx context.Context, incoming <-chan responsesWSMessage) {
	s.downstreamGone = true
	if incoming == nil || len(s.pending) == 0 {
		return
	}
	timeout := time.Duration(networkEnvInt("RESPONSES_WS_DRAIN_TIMEOUT_MS", 1500, 0, 30000)) * time.Millisecond
	if timeout <= 0 {
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for len(s.pending) > 0 {
		select {
		case message := <-incoming:
			if message.err != nil {
				return
			}
			if message.kind == websocket.MessageText {
				if _, err := s.serverEvent(message.body); err != nil {
					return
				}
			}
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (s *responsesWSSession) settleErrors(now time.Time, force bool) error {
	for i := 0; i < len(s.pending); {
		turn := s.pending[i]
		if !turn.failedAt.IsZero() && (force || !now.Before(turn.failedAt.Add(responsesWSErrorGrace))) {
			if err := s.finish(turn, nil, turn.failStatus, turn.failDetail); err != nil {
				return err
			}
			continue
		}
		i++
	}
	return nil
}

func writeResponsesWSError(ctx context.Context, conn *websocket.Conn, request *responsesWSRequest, status int, code, detail string) error {
	event := map[string]any{"type": "error", "status": status, "error": map[string]any{
		"type": "invalid_request_error", "code": code, "message": detail,
	}}
	if status >= 500 {
		event["error"].(map[string]any)["type"] = "server_error"
	}
	if request != nil {
		if request.StreamID != "" {
			event["stream_id"] = request.StreamID
		}
		if request.EventID != "" {
			event["error"].(map[string]any)["event_id"] = request.EventID
		}
	}
	body, _ := json.Marshal(event)
	return realtimeWrite(ctx, conn, websocket.MessageText, body)
}

func (s *responsesWSSession) dial(ctx context.Context, model string) (*websocket.Conn, error) {
	tried := make(map[int64]bool)
	triedKeys := make(map[string]bool)
	key := scopedSessionKey(s.keyID, sessionKeyFromCanon("/v1/responses", s.c.Request.Header, &canonReq{Model: model, SessionID: s.sessionID}))
	var lastErr error
	for attempt := 0; attempt <= getSettings().Retry; attempt++ {
		up, err := pickSessionUpstream(FormatOpenAIResponse, model, key, tried, triedKeys, true, nil)
		if err != nil {
			return nil, err
		}
		s.up = up
		client, err := audioClient(up)
		if err != nil {
			if up.selectedKeyID != "" {
				up.markRelayFailure(0, time.Time{})
			}
			lastErr = err
			continue
		}
		headers, err := s.headers(up, model)
		if err != nil {
			lastErr = err
			continue
		}
		dialCtx, cancel := context.WithTimeout(ctx, relayNetwork.DialTimeout)
		start := time.Now()
		conn, response, err := websocket.Dial(dialCtx, upstreamURL(up.BaseURL, "/v1/responses"), &websocket.DialOptions{HTTPClient: client, HTTPHeader: headers})
		cancel()
		if err == nil {
			conn.SetReadLimit(maxBodyBytes)
			affinitySet(key, up.ID)
			up.markRelaySuccess(start)
			return conn, nil
		}
		lastErr = err
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if ctx.Err() != nil {
			break
		}
		// Missing WebSocket support is a capability mismatch, not an outage.
		if response == nil || isRetryableStatus(response.StatusCode) {
			status, retryAt := 0, time.Time{}
			if response != nil {
				status, retryAt = response.StatusCode, retryAfterHeader(response.Header)
			}
			up.markRelayFailure(status, retryAt)
		} else {
			tried[up.ID] = true
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no available Responses WebSocket upstream")
	}
	return nil, lastErr
}

func (s *responsesWSSession) headers(up *Upstream, model string) (http.Header, error) {
	req := &http.Request{Header: make(http.Header)}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
	for _, name := range []string{"User-Agent", "Originator", "OpenAI-Beta", "OpenAI-Organization", "OpenAI-Project", "OpenAI-Safety-Identifier", "X-Codex-Beta-Features", "X-Codex-Turn-Metadata"} {
		if value := s.c.GetHeader(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	if err := up.applyHeaders(req, s.c.Request.Header); err != nil {
		return nil, err
	}
	if up.Type == "cliproxy" {
		setCLIProxyHeaders(req, s.c.Request.Header, &relayTask{
			apiKeyID: s.keyID, format: FormatOpenAIResponse, endpoint: "/v1/responses",
			model: model, canon: &canonReq{Model: model, SessionID: s.sessionID},
		})
		req.Header.Set(cliproxy.TraceHeader, s.coreTrace)
	}
	for name := range req.Header {
		if strings.HasPrefix(strings.ToLower(name), "sec-websocket-") || transportHeader(name) || strings.EqualFold(name, "Cookie") {
			req.Header.Del(name)
		}
	}
	return req.Header, nil
}

func (s *responsesWSSession) admit(model string, request *responsesWSRequest) (*responsesWSTurn, int, string) {
	key, err := dbGetKeyByID(s.keyID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, 503, "unable to refresh API key permissions"
		}
		return nil, 401, "API key is no longer available"
	}
	if s.credential != "" && key.Key != s.credential {
		return nil, 401, "API key is no longer available"
	}
	if status, detail := audioPermission(*key, model); status != 0 {
		return nil, status, detail
	}
	if s.up != nil && s.model != "" {
		current, err := cachedRoutingUpstream(s.up.ID)
		if err == nil && s.up.selectedKeyID != "" {
			pool := current
			current = nil
			for _, key := range pool.APIKeys {
				if key.ID == s.up.selectedKeyID && !key.Disabled {
					current = pool.withAPIKey(key)
					break
				}
			}
		}
		if err != nil || current == nil || !current.Enabled || current.Weight <= 0 || !nativeResponsesWS(current) || !current.servesModel(model) ||
			current.selectedKeyID != "" && !current.availableForRelay(model) ||
			current.mapModel(model) != s.up.mapModel(model) || current.APIKey != s.up.APIKey || current.BaseURL != s.up.BaseURL ||
			current.Type != s.up.Type || current.ProxyID != s.up.ProxyID || current.HeaderOverrides != s.up.HeaderOverrides {
			return nil, 503, "upstream configuration changed; reconnect before continuing"
		}
	}
	if len(s.pending) >= responsesWSMaxPending {
		return nil, 429, "too many pending Responses requests"
	}
	if len(s.seen) >= responsesWSMaxSeen {
		return nil, 429, "Responses connection turn limit reached; reconnect"
	}
	limit := getLimit(*key)
	if !limit.lim.Allow() {
		return nil, 429, "QPS limit exceeded for this key"
	}
	if !limit.acquire() {
		return nil, 429, "concurrency limit exceeded for this key"
	}
	now := time.Now()
	turn := &responsesWSTurn{
		key: *key, streamID: request.StreamID, eventID: request.EventID, limit: limit,
		warmup: request.Generate != nil && !*request.Generate,
		info:   relayInfo{startedAt: now},
		task: relayTask{client: requestClientFor(s.c), traceID: randHex(16), format: FormatOpenAIResponse,
			endpoint: "/v1/responses", model: model, apiKeyID: key.ID, stream: true},
	}
	requestClientFor(s.c).model = model
	s.pending = append(s.pending, turn)
	return turn, 0, ""
}

func (s *responsesWSSession) reject(model string, status int, detail string) {
	key := s.c.MustGet("apiKey").(ApiKey)
	state := requestClientFor(s.c)
	state.recorded.Store(true)
	if err := dbInsertLog(&CallLog{
		KeyID: s.keyID, KeyName: key.Name, Model: model, Endpoint: string(FormatOpenAIResponse),
		Client: state.identity, Status: status, Error: detail,
	}); err != nil {
		log.Printf("[responses-ws] key_id=%d: unable to record rejected turn: %v", s.keyID, err)
	}
}

func (s *responsesWSSession) eventTurn(event *responsesWSEvent) *responsesWSTurn {
	id := event.Response.ID
	if id == "" {
		id = event.ResponseID
	}
	if id != "" {
		if turn := s.byResponse[id]; turn != nil {
			return turn
		}
		if s.seen[id] {
			return nil
		}
	}
	for _, turn := range s.pending {
		if event.Error.EventID != "" {
			if turn.eventID != event.Error.EventID {
				continue
			}
		} else if event.StreamID != nil {
			if turn.streamID != *event.StreamID {
				continue
			}
		} else if turn.streamID != "" && s.up.Type != "cliproxy" {
			continue
		}
		if id != "" && turn.responseID != "" && turn.responseID != id {
			continue
		}
		if id != "" {
			turn.responseID = id
			s.byResponse[id] = turn
		}
		return turn
	}
	return nil
}

func (s *responsesWSSession) serverEvent(body []byte) ([]byte, error) {
	var event responsesWSEvent
	if err := json.Unmarshal(body, &event); err != nil || event.Type == "" || len(event.ID) > 1024 || len(event.Response.ID) > 1024 || len(event.ResponseID) > 1024 {
		return nil, errors.New("upstream returned an invalid Responses event")
	}
	switch event.Type {
	case "response.completed", "response.incomplete", "response.done", "response.failed", "response.cancelled", "response.canceled":
		// Only terminal snapshots may use a top-level response ID. Other
		// events can use the same field for an unrelated event identifier.
		if event.Response.ID == "" {
			event.Response.ID = event.ResponseID
			if event.Response.ID == "" {
				event.Response.ID = event.ID
			}
		}
		if len(event.Response.Usage) == 0 {
			event.Response.Usage = event.Usage
		}
		if event.Response.Status == "" {
			_ = json.Unmarshal(event.Status, &event.Response.Status)
		}
	}
	if event.Type == "response.created" {
		// A new response on a lane settles an earlier standalone error, while
		// error -> response.failed keeps the authoritative failed usage.
		for i := 0; i < len(s.pending); {
			turn := s.pending[i]
			sameLane := event.StreamID != nil && turn.streamID == *event.StreamID ||
				event.StreamID == nil && (turn.streamID == "" || s.up.Type == "cliproxy")
			if sameLane && !turn.failedAt.IsZero() && turn.responseID != event.Response.ID {
				if err := s.finish(turn, nil, turn.failStatus, turn.failDetail); err != nil {
					return nil, err
				}
				continue
			}
			i++
		}
	}
	turn := s.eventTurn(&event)
	terminal := false
	status, detail := http.StatusOK, ""
	switch event.Type {
	case "response.completed", "response.incomplete", "response.done":
		terminal = true
		if event.Response.Status == "failed" {
			status, detail = 502, "upstream response failed"
		}
	case "response.failed":
		terminal, status, detail = true, 502, "upstream response failed"
	case "response.cancelled", "response.canceled":
		terminal, status, detail = true, 499, "response cancelled"
	case "error":
		var httpStatus int
		_ = json.Unmarshal(event.Status, &httpStatus)
		terminal, status, detail = true, responsesWSProviderStatus(httpStatus, event.Error), "upstream rejected Responses request"
	}
	if terminal && event.Type != "error" && status == 502 {
		if turn != nil && turn.failStatus != 0 {
			status = turn.failStatus
		} else if event.Response.Error.Code != "" || event.Response.Error.Type != "" {
			status = responsesWSProviderStatus(0, event.Response.Error)
		}
	}
	if turn == nil {
		if event.Response.ID != "" && s.seen[event.Response.ID] || event.ResponseID != "" && s.seen[event.ResponseID] {
			return body, nil // Native duplicate events remain visible, but are not billed twice.
		}
		if event.Type == "error" {
			return body, nil // An error for a control event is not an inference receipt.
		}
		if terminal || event.Type == "response.created" {
			return nil, errors.New("upstream response does not match an admitted request")
		}
		return body, nil
	}
	if event.Type == "error" && turn.responseID != "" {
		if turn.failedAt.IsZero() {
			turn.failedAt, turn.failStatus, turn.failDetail = time.Now(), status, detail
		}
		terminal = false
	}
	if event.Type == "response.created" {
		turn.info.LatencyMS = turn.info.elapsedMS()
	}
	if strings.HasSuffix(event.Type, ".delta") && len(event.Delta) != 0 && string(event.Delta) != `""` && string(event.Delta) != "null" {
		turn.info.markFirstToken()
	}
	// The pinned SDK processes queued turns serially and may omit stream_id.
	// Restore only that routing envelope; opaque event fields remain raw JSON.
	if !s.downstreamGone && event.StreamID == nil && turn.streamID != "" && s.up.Type == "cliproxy" {
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(body, &raw)
		raw["stream_id"], _ = json.Marshal(turn.streamID)
		body, _ = json.Marshal(raw)
	}
	if terminal {
		if event.Type != "error" && event.Response.ID == "" {
			return nil, errors.New("upstream terminal response is missing its ID")
		}
		if len(s.seen) >= responsesWSMaxSeen {
			return nil, errors.New("Responses connection turn limit reached; reconnect")
		}
		if event.Response.ID != "" {
			s.seen[event.Response.ID] = true
		}
		if err := s.finish(turn, event.Response.Usage, status, detail); err != nil {
			return nil, fmt.Errorf("unable to record Responses usage")
		}
	}
	return body, nil
}

func responsesWSProviderStatus(status int, problem responsesWSProviderError) int {
	if status >= 400 && status <= 599 {
		return status
	}
	switch {
	case problem.Code == "rate_limit_exceeded" || problem.Type == "rate_limit_error":
		return 429
	case problem.Code == "invalid_api_key" || problem.Type == "authentication_error":
		return 401
	case problem.Type == "permission_error":
		return 403
	case problem.Type == "server_error" || problem.Code == "server_error":
		return 502
	default:
		return 400
	}
}

func (s *responsesWSSession) finish(turn *responsesWSTurn, rawUsage json.RawMessage, status int, detail string) error {
	for i, pending := range s.pending {
		if pending == turn {
			copy(s.pending[i:], s.pending[i+1:])
			s.pending[len(s.pending)-1] = nil
			s.pending = s.pending[:len(s.pending)-1]
			break
		}
	}
	delete(s.byResponse, turn.responseID)
	if turn.responseID != "" {
		s.seen[turn.responseID] = true
	}
	defer turn.limit.release()
	turn.task.markClientRecorded()
	turn.info.DurationMS = turn.info.elapsedMS()
	var usageObject map[string]any
	_ = json.Unmarshal(rawUsage, &usageObject)
	usage := readUsage(usageObject, FormatOpenAIResponse)
	counts := normalizeBillingUsage(usage)
	if turn.warmup && status == http.StatusOK && counts.inputTotal() == 0 && counts.outputTotal() == 0 {
		return nil
	}
	if s.downstreamGone && status == http.StatusOK {
		status, detail = 499, "client disconnected after upstream accepted the request"
	}
	if s.up != nil && s.up.selectedKeyID != "" {
		if isRetryableStatus(status) {
			s.up.markRelayFailure(status, time.Time{})
		} else if status >= 200 && status < 300 {
			s.up.markRelaySuccess(turn.info.startedAt)
		}
	}
	entry := &CallLog{
		Client: turn.task.clientIdentity(), TraceID: turn.task.traceID, KeyID: turn.key.ID, KeyName: turn.key.Name,
		UpstreamID: turn.info.UpstreamID, UpstreamName: turn.info.UpstreamName, Model: turn.task.model,
		Endpoint: string(FormatOpenAIResponse), Status: status, Error: detail,
		PromptTokens: counts.inputTotal(), CompletionTokens: counts.outputTotal(),
		FirstTokenMS: turn.info.FirstTokenMS, DurationMS: turn.info.DurationMS, LatencyMS: turn.info.LatencyMS,
	}
	if usageObject == nil {
		// Incremental WS inputs do not contain the full transcript. Unknown
		// usage must stay unknown rather than estimating a partial history.
		return dbInsertLog(entry)
	}
	price, source, pricingModel := lookupRelayPrice(turn.task.model, turn.info.UpstreamModel)
	cost := costForUsage(counts, price)
	_, independent := turn.key.ModelQuotas[turn.task.model]
	pool := "shared"
	if independent {
		pool = "independent"
	}
	entry.CostUSD = cost.Total
	entry.Billing = &billingDetails{Source: usage.Source, Reported: usage, Usage: counts, Cost: cost,
		Price: price, PriceSource: source, PricingModel: pricingModel, UpstreamModel: turn.info.UpstreamModel, QuotaPool: pool}
	return dbRecordBilledCall(entry, independent)
}
