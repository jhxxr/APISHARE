package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestUpstreamIdleDeadlineReleasesBlockedRead(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	body := newActivityDeadlineBody(reader, 30*time.Millisecond)
	defer body.Close()
	done := make(chan error, 1)
	go func() { _, err := body.Read(make([]byte, 1)); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, errUpstreamIdleTimeout) {
			t.Fatalf("wrong timeout: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle deadline left a blocked reader")
	}
}

func TestUpstreamIdleDeadlineAllowsLongActiveStreamsAndSlowClients(t *testing.T) {
	reader, writer := io.Pipe()
	body := newActivityDeadlineBody(reader, 100*time.Millisecond)
	defer body.Close()
	go func() {
		defer writer.Close()
		for i := 0; i < 8; i++ {
			time.Sleep(20 * time.Millisecond)
			if _, err := writer.Write([]byte("x")); err != nil {
				return
			}
		}
	}()
	buffer := make([]byte, 8)
	if _, err := io.ReadFull(body, buffer); err != nil {
		t.Fatalf("active stream was cut off: %v", err)
	}
	// No upstream read is in progress while a client consumes buffered data.
	time.Sleep(150 * time.Millisecond)
	if _, err := body.Read(buffer); err != io.EOF {
		t.Fatalf("slow downstream triggered upstream cooldown: %v", err)
	}
}

func TestRelayNetworkConfigurationAndConnectionReuse(t *testing.T) {
	for _, name := range []string{"UPSTREAM_MAX_IDLE_CONNS", "UPSTREAM_MAX_IDLE_CONNS_PER_HOST", "UPSTREAM_DIAL_TIMEOUT_SEC", "UPSTREAM_HEADER_TIMEOUT_SEC", "UPSTREAM_IDLE_TIMEOUT_SEC"} {
		t.Setenv(name, "")
	}
	config := loadRelayNetworkConfig()
	if config.MaxIdleConns != 256 || config.MaxIdleConnsPerHost != 64 || config.BodyIdleTimeout != 300*time.Second {
		t.Fatalf("wrong defaults: %+v", config)
	}
	t.Setenv("UPSTREAM_HEADER_TIMEOUT_SEC", "0")
	t.Setenv("UPSTREAM_IDLE_TIMEOUT_SEC", "0")
	t.Setenv("UPSTREAM_DIAL_TIMEOUT_SEC", "999999999999999999999999999")
	config = loadRelayNetworkConfig()
	if config.HeaderTimeout != 0 || config.BodyIdleTimeout != 0 || config.DialTimeout != 15*time.Second {
		t.Fatal("disable/overflow handling failed")
	}
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	client := newRelayHTTPClient(config)
	defer client.CloseIdleConnections()
	if client.Timeout != 0 {
		t.Fatal("a total timeout would cut off long streaming responses")
	}
	for i := 0; i < 3; i++ {
		response, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("connections were not reused: %d", connections.Load())
	}
}

func TestRelayHeaderDeadlineDoesNotWaitForever(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	client := newRelayHTTPClient(relayNetworkConfig{DialTimeout: time.Second, HeaderTimeout: 30 * time.Millisecond, MaxIdleConns: 1, MaxIdleConnsPerHost: 1})
	defer client.CloseIdleConnections()
	if _, err := client.Get(server.URL); err == nil {
		t.Fatal("stalled headers did not time out")
	}
}

func TestIdleUpstreamFailsOverWithoutReplacingStartedStreams(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream-%v", stream), func(t *testing.T) {
			modelsTestDB(t)
			previous := relayNetwork.BodyIdleTimeout
			relayNetwork.BodyIdleTimeout = 30 * time.Millisecond
			defer func() { relayNetwork.BodyIdleTimeout = previous }()
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
				} else {
					w.WriteHeader(200)
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer bad.Close()
			var fallbackCalls atomic.Int32
			good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fallbackCalls.Add(1)
				fmt.Fprint(w, `{"choices":[{"message":{"content":"fallback"}}],"usage":{"prompt_tokens":7,"completion_tokens":1}}`)
			}))
			defer good.Close()
			first := &Upstream{Type: "openai", BaseURL: bad.URL, Models: "review-idle", Weight: 1, Enabled: true}
			second := &Upstream{Type: "openai", BaseURL: good.URL, Models: "review-idle", Weight: 1, Enabled: true}
			for _, up := range []*Upstream{first, second} {
				if err := dbInsertUpstream(up); err != nil {
					t.Fatal(err)
				}
			}
			key := &ApiKey{Name: "idle", Key: "sk-idle", Models: "review-idle", Enabled: true}
			if err := dbInsertKey(key); err != nil {
				t.Fatal(err)
			}
			affinitySet("sid:idle-check", first.ID)
			r := gin.New()
			r.POST("/v1/chat/completions", func(c *gin.Context) { c.Set("apiKey", *key) }, relayHandler(FormatOpenAIChat, false))
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"review-idle","stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream)))
			req.Header.Set("X-Session-Id", "idle-check")
			response := httptest.NewRecorder()
			r.ServeHTTP(response, req)
			logs, err := dbListLogs(1, 0)
			if err != nil || len(logs) != 1 || healthFails(first.ID) != 1 {
				t.Fatalf("idle failure was lost: %v %v", logs, err)
			}
			if stream {
				if fallbackCalls.Load() != 0 || logs[0].Status != 502 || strings.Contains(response.Body.String(), "[DONE]") || !strings.Contains(response.Body.String(), "hello") {
					t.Fatal("started stream was retried or recorded as successful")
				}
			} else if fallbackCalls.Load() != 1 || logs[0].UpstreamID != second.ID || response.Code != 200 || !strings.Contains(response.Body.String(), "fallback") {
				t.Fatal("idle non-stream response did not fail over")
			}
		})
	}
}
