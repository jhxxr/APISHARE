package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

const messagesKeepaliveKey = "apishare.messagesKeepalive"

var claudeKeepaliveVersion = regexp.MustCompile(`(?i)^claude-cli/(\d+)\.(\d+)\.(\d+)(?:\s|$)`)

// This compatibility threshold was verified against sub2api 698d142. Only
// known stable Claude Code versions receive empty deltas on an open block.
func supportsClaudeEmptyDelta(userAgent string) bool {
	parts := claudeKeepaliveVersion.FindStringSubmatch(userAgent)
	if parts == nil {
		return false
	}
	var version [3]int
	for i := range version {
		number, err := strconv.Atoi(parts[i+1])
		if err != nil {
			return false
		}
		version[i] = number
	}
	for i, minimum := range []int{2, 1, 193} {
		if version[i] != minimum {
			return version[i] > minimum
		}
	}
	return true
}

type messagesKeepalive struct {
	c                                    *gin.Context
	interval                             time.Duration
	lastWrite                            time.Time
	committed, emptyDelta, upstreamError bool
	blockIndex                           int
	deltaType, deltaField                string
}

func messagesState(c *gin.Context) *messagesKeepalive {
	value, _ := c.Get(messagesKeepaliveKey)
	state, _ := value.(*messagesKeepalive)
	return state
}

func beginMessagesKeepalive(c *gin.Context, task *relayTask) *messagesKeepalive {
	if task.format != FormatAnthropic || !task.stream || relayNetwork.StreamKeepaliveInterval <= 0 {
		return nil
	}
	state := &messagesKeepalive{c: c, interval: relayNetwork.StreamKeepaliveInterval, lastWrite: time.Now(), blockIndex: -1, emptyDelta: supportsClaudeEmptyDelta(c.GetHeader("User-Agent"))}
	c.Set(messagesKeepaliveKey, state)
	return state
}

// Control-only pings may precede message_start and survive a safe upstream
// retry. Actual Messages output commits the response and forbids replay.
func relayResponseCommitted(c *gin.Context) bool {
	if state := messagesState(c); state != nil {
		return state.committed
	}
	return c.Writer.Written()
}

func commitRelayResponse(c *gin.Context) {
	if state := messagesState(c); state != nil {
		state.committed = true
	}
}

func (s *messagesKeepalive) observe(event map[string]any) {
	switch str(event["type"]) {
	case "content_block_start":
		s.blockIndex = intFromAny(event["index"])
		s.deltaType, s.deltaField = "", ""
		switch str(object(event["content_block"])["type"]) {
		case "text":
			s.deltaType, s.deltaField = "text_delta", "text"
		case "thinking":
			s.deltaType, s.deltaField = "thinking_delta", "thinking"
		case "tool_use":
			s.deltaType, s.deltaField = "input_json_delta", "partial_json"
		}
	case "content_block_stop":
		if intFromAny(event["index"]) == s.blockIndex {
			s.blockIndex = -1
		}
	case "message_stop":
		s.blockIndex = -1
	case "error":
		s.upstreamError = true
	}
}

func (s *messagesKeepalive) ping() error {
	if err := s.c.Request.Context().Err(); err != nil {
		return err
	}
	if !s.c.Writer.Written() {
		s.c.Header("Content-Type", "text/event-stream; charset=utf-8")
		s.c.Header("Cache-Control", "no-cache")
		s.c.Header("X-Accel-Buffering", "no")
		s.c.Writer.WriteHeader(http.StatusOK)
	}
	frame := "event: ping\ndata: {\"type\":\"ping\"}\n\n"
	if s.emptyDelta && s.committed && s.blockIndex >= 0 && s.deltaType != "" {
		frame = fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":%q,%q:\"\"}}\n\n", s.blockIndex, s.deltaType, s.deltaField)
	}
	if _, err := s.c.Writer.WriteString(frame); err != nil {
		return fmt.Errorf("%w: %w", errDownstreamWrite, err)
	}
	s.c.Writer.Flush()
	s.lastWrite = time.Now()
	return nil
}

// Both upstream I/O and heartbeat delivery are coordinated by the handler.
// Only the handler writes headers, frames, usage, or Gin context state.
func awaitMessagesIO[T any](ctx context.Context, state *messagesKeepalive, ready <-chan T) (T, error) {
	timer := time.NewTimer(time.Until(state.lastWrite.Add(state.interval)))
	defer timer.Stop()
	for {
		// Prefer a ready result, particularly EOF/completion, over a heartbeat.
		select {
		case result := <-ready:
			return result, nil
		default:
		}
		select {
		case result := <-ready:
			return result, nil
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case <-timer.C:
			if err := state.ping(); err != nil {
				var zero T
				return zero, err
			}
			timer.Reset(state.interval)
		}
	}
}

func messagesHTTPDo(client *http.Client, req *http.Request, state *messagesKeepalive, cancel context.CancelFunc) (*http.Response, error) {
	if state == nil {
		return client.Do(req)
	}
	type result struct {
		response *http.Response
		err      error
	}
	ready := make(chan result, 1)
	go func() { response, err := client.Do(req); ready <- result{response, err} }()
	out, err := awaitMessagesIO(req.Context(), state, ready)
	if err != nil {
		cancel()
		out = <-ready
		if out.response != nil {
			out.response.Body.Close()
		}
		return nil, err
	}
	return out.response, out.err
}

type messagesReadResult struct {
	n   int
	err error
}

type messagesKeepaliveBody struct {
	source   io.ReadCloser
	ctx      context.Context
	state    *messagesKeepalive
	reads    chan []byte
	results  chan messagesReadResult
	done     chan struct{}
	closed   bool
	closeErr error
}

func newMessagesKeepaliveBody(source io.ReadCloser, ctx context.Context, state *messagesKeepalive) io.ReadCloser {
	if state == nil {
		return source
	}
	b := &messagesKeepaliveBody{source: source, ctx: ctx, state: state, reads: make(chan []byte), results: make(chan messagesReadResult, 1), done: make(chan struct{})}
	go func() {
		defer close(b.done)
		for buffer := range b.reads {
			n, err := source.Read(buffer)
			b.results <- messagesReadResult{n, err}
		}
	}()
	return b
}

func (b *messagesKeepaliveBody) Read(buffer []byte) (int, error) {
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	b.reads <- buffer
	result, err := awaitMessagesIO(b.ctx, b.state, b.results)
	if err != nil {
		// Close unblocks the read and joins the worker before the caller can
		// reuse its buffer. Demand-driven reads never time out a slow client.
		b.Close()
		return 0, err
	}
	return result.n, result.err
}

func (b *messagesKeepaliveBody) Close() error {
	if !b.closed {
		b.closed = true
		close(b.reads)
		b.closeErr = b.source.Close()
		<-b.done
	}
	return b.closeErr
}

func writeMessagesStreamError(c *gin.Context, message string) {
	frame, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": message}})
	c.Writer.WriteString("event: error\ndata: " + string(frame) + "\n\n")
	c.Writer.Flush()
}

func writeRelayError(c *gin.Context, format clientFormat, status int, message string) {
	if format == FormatAnthropic && messagesState(c) != nil && c.Writer.Written() {
		writeMessagesStreamError(c, message)
		return
	}
	writeFormatError(c, format, status, message)
}
