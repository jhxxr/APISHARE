package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type protocolStreamFlushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
	onFlush func(int, string)
}

func (w *protocolStreamFlushRecorder) Flush() {
	w.flushes++
	w.ResponseRecorder.Flush()
	if w.onFlush != nil {
		w.onFlush(w.flushes, w.Body.String())
	}
}

type protocolStreamGatedBody struct {
	first, second []byte
	allowSecond   <-chan struct{}
	reads         int
}

func (body *protocolStreamGatedBody) Read(dst []byte) (int, error) {
	body.reads++
	switch body.reads {
	case 1:
		return copy(dst, body.first), nil
	case 2:
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-body.allowSecond:
			return copy(dst, body.second), nil
		case <-timer.C:
			return 0, errors.New("upstream waited before the first converted frame was flushed")
		}
	default:
		return 0, io.EOF
	}
}

func (*protocolStreamGatedBody) Close() error { return nil }

func newProtocolStreamFlushContext() (*gin.Context, *protocolStreamFlushRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := &protocolStreamFlushRecorder{ResponseRecorder: httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(recorder)
	return c, recorder
}

func protocolStreamTestTask() *relayTask {
	return &relayTask{format: FormatOpenAIChat, model: "alias", canon: &canonReq{}, stream: true}
}

func TestRelayProtocolSSECoalescesSameReadDeltas(t *testing.T) {
	c, writer := newProtocolStreamFlushContext()
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"second\"}}]}\n\n" +
		"data: [DONE]\n\n"
	err := relayProtocolSSE(c, &http.Response{Body: io.NopCloser(strings.NewReader(stream))}, FormatOpenAIChat, protocolStreamTestTask(), &relayInfo{})
	if err != nil {
		t.Fatalf("relay failed: %v", err)
	}
	if writer.flushes != 1 {
		t.Fatalf("expected one flush for same-read deltas and terminal frame, got %d", writer.flushes)
	}
	if body := writer.Body.String(); !strings.Contains(body, `"content":"first"`) || !strings.Contains(body, `"content":"second"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("converted stream output incomplete: %s", body)
	}
}

func TestRelayProtocolSSEFlushesBeforeWaitingForNextRead(t *testing.T) {
	c, writer := newProtocolStreamFlushContext()
	released := make(chan struct{})
	var firstFlush string
	writer.onFlush = func(count int, body string) {
		if count == 1 {
			firstFlush = body
			close(released)
		}
	}
	body := &protocolStreamGatedBody{
		first:       []byte("data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n"),
		second:      []byte("data: {\"choices\":[{\"delta\":{\"content\":\"second\"}}]}\n\ndata: [DONE]\n\n"),
		allowSecond: released,
	}
	err := relayProtocolSSE(c, &http.Response{Body: body}, FormatOpenAIChat, protocolStreamTestTask(), &relayInfo{})
	if err != nil {
		t.Fatalf("relay failed: %v", err)
	}
	if !strings.Contains(firstFlush, `"content":"first"`) || strings.Contains(firstFlush, `"content":"second"`) {
		t.Fatalf("first upstream batch was not delivered before the next read: %s", firstFlush)
	}
	if !strings.Contains(writer.Body.String(), `"content":"second"`) {
		t.Fatalf("second upstream batch was not relayed: %s", writer.Body.String())
	}
}

func TestRelayProtocolSSEFlushesPendingOutputOnTerminalAndError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tail      string
		wantError bool
		wantBody  string
	}{
		{name: "terminal", tail: "data: [DONE]\n\n", wantBody: "data: [DONE]"},
		{name: "decode error", tail: "data: broken\n\n", wantError: true, wantBody: "upstream stream interrupted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, writer := newProtocolStreamFlushContext()
			stream := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n" + tc.tail
			err := relayProtocolSSE(c, &http.Response{Body: io.NopCloser(strings.NewReader(stream))}, FormatOpenAIChat, protocolStreamTestTask(), &relayInfo{})
			if (err != nil) != tc.wantError {
				t.Fatalf("relay error = %v, want error %t", err, tc.wantError)
			}
			if writer.flushes != 1 {
				t.Fatalf("expected pending output to flush once on return, got %d flushes", writer.flushes)
			}
			if body := writer.Body.String(); !strings.Contains(body, `"content":"partial"`) || !strings.Contains(body, tc.wantBody) {
				t.Fatalf("terminal/error output was not flushed: %s", body)
			}
		})
	}
}
