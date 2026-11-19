package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type nativeFlushObservation struct {
	http.ResponseWriter
	flushes int
	onFlush func(int, string)
}

func (w *nativeFlushObservation) Flush() {
	w.flushes++
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
	if w.onFlush != nil {
		w.onFlush(w.flushes, w.ResponseWriter.(*httptest.ResponseRecorder).Body.String())
	}
}

type flushGatedNativeBody struct {
	first []byte
	last  []byte
	ready <-chan struct{}
	reads int
}

func (body *flushGatedNativeBody) Read(dst []byte) (int, error) {
	body.reads++
	switch body.reads {
	case 1:
		return copy(dst, body.first), nil
	case 2:
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case <-body.ready:
			return copy(dst, body.last), nil
		case <-timer.C:
			return 0, errors.New("upstream waited before its first buffered event was flushed")
		}
	default:
		return 0, io.EOF
	}
}

func (*flushGatedNativeBody) Close() error { return nil }

func TestNativeRelayCoalescesBufferedEventsAndFlushesBeforeNextRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	frames := nativePrelude(FormatOpenAIChat)
	for _, text := range []string{"first", "second", "third"} {
		frames += "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + text + "\"}}]}\n\n"
	}
	terminal := "data: [DONE]\n\n"
	ready := make(chan struct{})
	var releaseOnce sync.Once
	recorder := httptest.NewRecorder()
	writer := &nativeFlushObservation{ResponseWriter: recorder}
	var firstFlush string
	writer.onFlush = func(count int, body string) {
		if count == 1 {
			firstFlush = body
			releaseOnce.Do(func() { close(ready) })
		}
	}
	c, _ := gin.CreateTestContext(writer)
	upstream := &flushGatedNativeBody{first: []byte(frames), last: []byte(terminal), ready: ready}

	err := relaySSENative(c, &http.Response{Body: upstream}, FormatOpenAIChat, &relayInfo{})
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if firstFlush != frames {
		t.Fatalf("buffered output was not flushed before waiting for upstream: got %q, want %q", firstFlush, frames)
	}
	if got := writer.flushes; got != 2 {
		t.Fatalf("expected one coalesced flush and one final flush, got %d", got)
	}
	if got, want := recorder.Body.String(), frames+terminal; got != want {
		t.Fatalf("stream bytes changed: got %q, want %q", got, want)
	}
}

func TestNativeBufferedResponsesKeepDeltaAndSnapshotSemantics(t *testing.T) {
	var stream strings.Builder
	write := func(event string) {
		stream.WriteString("data: ")
		stream.WriteString(event)
		stream.WriteString("\n\n")
	}
	write(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Hel"}`)
	write(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"lo"}`)
	write(`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"Hello!"}}`)
	write(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":" world"}`)
	write(`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"path\":"}`)
	write(`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"\"main.go\"}"}`)
	write(`{"type":"response.function_call_arguments.done","output_index":1,"arguments":"{\"path\":\"main.go\",\"ok\":true}"}`)
	write(`{"type":"response.completed","response":{"status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0}}}`)
	body, err := collectNativeResponsesSSE(strings.NewReader(stream.String()), &relayInfo{})
	if err != nil {
		t.Fatalf("aggregate response: %v", err)
	}
	result, err := decodeProtocolObject(body)
	if err != nil {
		t.Fatalf("decode aggregate: %v", err)
	}
	output := array(result["output"])
	if len(output) != 2 {
		t.Fatalf("expected message and tool output, got %s", body)
	}
	message := object(array(object(output[0])["content"])[0])
	tool := object(output[1])
	if message["text"] != "Hello! world" || tool["arguments"] != `{"path":"main.go","ok":true}` {
		t.Fatalf("delta or authoritative snapshot changed: %s", body)
	}
}
