package main

import (
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

type relayNetworkConfig struct {
	MaxIdleConns, MaxIdleConnsPerHost   int
	DialTimeout, HeaderTimeout          time.Duration
	BodyIdleTimeout, FirstOutputTimeout time.Duration
	StreamKeepaliveInterval             time.Duration
}

func networkEnvInt(name string, fallback, minimum, maximum int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < minimum || n > maximum {
		log.Printf("[network] invalid %s; using default %d", name, fallback)
		return fallback
	}
	return n
}

func loadRelayNetworkConfig() relayNetworkConfig {
	return relayNetworkConfig{
		MaxIdleConns:            networkEnvInt("UPSTREAM_MAX_IDLE_CONNS", 256, 1, 65536),
		MaxIdleConnsPerHost:     networkEnvInt("UPSTREAM_MAX_IDLE_CONNS_PER_HOST", 64, 1, 65536),
		DialTimeout:             time.Duration(networkEnvInt("UPSTREAM_DIAL_TIMEOUT_SEC", 15, 1, 86400)) * time.Second,
		HeaderTimeout:           time.Duration(networkEnvInt("UPSTREAM_HEADER_TIMEOUT_SEC", 180, 0, 86400)) * time.Second,
		BodyIdleTimeout:         time.Duration(networkEnvInt("UPSTREAM_IDLE_TIMEOUT_SEC", 300, 0, 86400)) * time.Second,
		FirstOutputTimeout:      time.Duration(networkEnvInt("UPSTREAM_FIRST_OUTPUT_TIMEOUT_SEC", 300, 0, 86400)) * time.Second,
		StreamKeepaliveInterval: time.Duration(networkEnvInt("STREAM_KEEPALIVE_INTERVAL_SEC", 10, 0, 300)) * time.Second,
	}
}

func newRelayHTTPClient(config relayNetworkConfig) *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: config.DialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   config.DialTimeout,
		ResponseHeaderTimeout: config.HeaderTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          config.MaxIdleConns,
		MaxIdleConnsPerHost:   config.MaxIdleConnsPerHost,
		ForceAttemptHTTP2:     true,
		ExpectContinueTimeout: time.Second,
	}}
}

var errUpstreamIdleTimeout = errors.New("upstream response body idle timeout")
var errUpstreamFirstOutputTimeout = errors.New("upstream first-output timeout")

// flushBeforeUpstreamRead lets streaming relays coalesce data already buffered
// from the upstream without adding a timer or delaying output until an event
// that has not arrived yet.
type flushBeforeUpstreamRead struct {
	source io.Reader
	flush  func()
}

func (reader *flushBeforeUpstreamRead) Read(buffer []byte) (int, error) {
	if reader.flush != nil {
		reader.flush()
	}
	return reader.source.Read(buffer)
}

// firstOutputDeadlineBody bounds the entire wait for meaningful stream
// progress. Unlike the idle deadline, comments and keepalive frames do not
// reset it. MarkOutput disarms the one-shot deadline once a stream advances.
type firstOutputDeadlineBody struct {
	source    io.ReadCloser
	mu        sync.Mutex
	timer     *time.Timer
	expired   bool
	output    bool
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

func newFirstOutputDeadlineBody(source io.ReadCloser, deadline time.Time) *firstOutputDeadlineBody {
	body := &firstOutputDeadlineBody{source: source}
	delay := time.Until(deadline)
	if delay < 0 {
		delay = 0
	}
	if delay == 0 {
		body.expire()
	} else {
		body.timer = time.AfterFunc(delay, body.expire)
	}
	return body
}

func (body *firstOutputDeadlineBody) expire() {
	body.mu.Lock()
	if body.closed || body.output || body.expired {
		body.mu.Unlock()
		return
	}
	body.expired = true
	body.mu.Unlock()
	body.closeOnce.Do(func() { body.closeErr = body.source.Close() })
}

func (body *firstOutputDeadlineBody) MarkOutput() {
	body.mu.Lock()
	if !body.expired && !body.closed {
		body.output = true
		if body.timer != nil {
			body.timer.Stop()
		}
	}
	body.mu.Unlock()
}

func (body *firstOutputDeadlineBody) outputObserved() bool {
	body.mu.Lock()
	defer body.mu.Unlock()
	return body.output
}

func (body *firstOutputDeadlineBody) Read(buffer []byte) (int, error) {
	body.mu.Lock()
	if body.expired {
		body.mu.Unlock()
		return 0, errUpstreamFirstOutputTimeout
	}
	if body.closed {
		body.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	body.mu.Unlock()

	n, err := body.source.Read(buffer)
	body.mu.Lock()
	expired := body.expired
	body.mu.Unlock()
	if expired {
		return n, errUpstreamFirstOutputTimeout
	}
	return n, err
}

func (body *firstOutputDeadlineBody) Close() error {
	body.mu.Lock()
	body.closed = true
	if body.timer != nil {
		body.timer.Stop()
	}
	body.mu.Unlock()
	body.closeOnce.Do(func() { body.closeErr = body.source.Close() })
	return body.closeErr
}

// Bound only reads that are waiting for upstream bytes, not the total stream
// duration or time spent delivering buffered data to a slow downstream client.
type activityDeadlineBody struct {
	source                   io.ReadCloser
	limit                    time.Duration
	mu                       sync.Mutex
	timer                    *time.Timer
	reading, expired, closed bool
	deadline                 time.Time
	closeOnce                sync.Once
	closeErr                 error
}

func newActivityDeadlineBody(source io.ReadCloser, limit time.Duration) *activityDeadlineBody {
	body := &activityDeadlineBody{source: source, limit: limit}
	body.timer = time.AfterFunc(limit, body.expire)
	body.timer.Stop()
	return body
}

func (body *activityDeadlineBody) expire() {
	body.mu.Lock()
	if body.closed || !body.reading || body.expired {
		body.mu.Unlock()
		return
	}
	if remaining := time.Until(body.deadline); remaining > 0 {
		body.timer.Reset(remaining)
		body.mu.Unlock()
		return
	}
	body.expired = true
	body.mu.Unlock()
	body.closeOnce.Do(func() { body.closeErr = body.source.Close() })
}

func (body *activityDeadlineBody) Read(buffer []byte) (int, error) {
	body.mu.Lock()
	if body.expired {
		body.mu.Unlock()
		return 0, errUpstreamIdleTimeout
	}
	if body.closed {
		body.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	body.reading = true
	body.deadline = time.Now().Add(body.limit)
	body.timer.Reset(body.limit)
	body.mu.Unlock()
	n, err := body.source.Read(buffer)
	body.mu.Lock()
	body.reading = false
	body.timer.Stop()
	expired := body.expired
	body.mu.Unlock()
	if expired {
		return n, errUpstreamIdleTimeout
	}
	return n, err
}

func (body *activityDeadlineBody) Close() error {
	body.mu.Lock()
	body.closed = true
	body.timer.Stop()
	body.mu.Unlock()
	body.closeOnce.Do(func() { body.closeErr = body.source.Close() })
	return body.closeErr
}
