package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func testMessagesKeepaliveConfig(t *testing.T) {
	t.Helper()
	previous := relayNetwork
	relayNetwork.StreamKeepaliveInterval = 15 * time.Millisecond
	relayNetwork.BodyIdleTimeout = time.Second
	t.Cleanup(func() { relayNetwork = previous })
}

func TestMessagesKeepaliveDuringHeaderPreludeAndContentWait(t *testing.T) {
	for _, format := range protocolFormats {
		for _, phase := range []string{"headers", "prelude", "content"} {
			t.Run(string(format)+"/"+phase, func(t *testing.T) {
				modelsTestDB(t)
				testMessagesKeepaliveConfig(t)
				release := make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				frames := fixtureSSE(format)
				if format == FormatAnthropic {
					frames = strings.ReplaceAll(auditFormatFrames(
						`{"type":"message_start","message":{"id":"msg_live","type":"message","role":"assistant","model":"mapped","content":[],"usage":{"input_tokens":17,"output_tokens":0}}}`,
						`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
						`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
						`{"type":"content_block_stop","index":0}`,
						`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`,
						`{"type":"message_stop"}`,
					), "\n", "\r\n")
				}
				cut := 0
				if phase != "headers" {
					hello := strings.Index(frames, "Hello")
					cut = strings.LastIndex(frames[:hello], "data:")
					if phase == "content" {
						cut = hello + strings.Index(frames[hello:], "\r\n\r\n") + 4
					}
				}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if phase != "headers" {
						w.Header().Set("Content-Type", "text/event-stream")
						if cut == 0 {
							fmt.Fprint(w, nativePrelude(format))
						} else {
							fmt.Fprint(w, frames[:cut])
						}
						w.(http.Flusher).Flush()
					}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, frames[cut:])
				}))
				defer upstream.Close()
				defer unblock()
				up := fixtureUpstream(format, upstream.URL)
				key := ApiKey{Models: "alias", Name: "keepalive", Key: "sk-keepalive", Enabled: true}
				if err := dbInsertKey(&key); err != nil {
					t.Fatal(err)
				}
				router := gin.New()
				done := make(chan struct{})
				router.POST("/v1/messages", func(c *gin.Context) {
					defer close(done)
					task := fixtureTask(t, FormatAnthropic, true)
					info, wrote, err := doRelay(c, up, task)
					finalizeCall(c, key, task, info, wrote, err, 17)
				})
				gateway := httptest.NewServer(router)
				defer gateway.Close()
				req, _ := http.NewRequest("POST", gateway.URL+"/v1/messages", nil)
				req.Header.Set("User-Agent", "claude-cli/2.1.193 (external, cli)")
				resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				var wire strings.Builder
				heartbeats := 0
				err = readSSEFrames(resp.Body, func(payload string) (bool, error) {
					fmt.Fprintf(&wire, "data: %s\n\n", payload)
					var event map[string]any
					if err := json.Unmarshal([]byte(payload), &event); err != nil {
						return false, err
					}
					ping := str(event["type"]) == "ping"
					delta := object(event["delta"])
					empty := str(event["type"]) == "content_block_delta" && str(delta["type"]) == "text_delta" && delta["text"] == ""
					if ping || empty {
						heartbeats++
						if (phase == "content") != empty {
							return false, fmt.Errorf("wrong heartbeat for phase %s: %s", phase, payload)
						}
						unblock()
					}
					return false, nil
				})
				if err != nil || heartbeats == 0 {
					t.Fatalf("missing live keepalive: %v / %s", err, wire.String())
				}
				<-done
				body, err := collectNativeProtocolSSE(strings.NewReader(wire.String()), FormatAnthropic, &relayInfo{})
				if err != nil || !strings.Contains(string(body), "Hello") {
					t.Fatalf("keepalive damaged message: %v / %s", err, body)
				}
				logs, err := dbListLogs(10, 0)
				if err != nil || len(logs) != 1 || logs[0].Status != 200 || logs[0].PromptTokens != 17 || logs[0].CompletionTokens != 0 {
					t.Fatalf("keepalive changed usage or final status: %+v / %v", logs, err)
				}
			})
		}
	}
}

func TestMessagesKeepaliveDoesNotPreventInitialFailover(t *testing.T) {
	for _, format := range []clientFormat{FormatAnthropic, FormatOpenAIChat} {
		for _, fallback := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fallback=%v", format, fallback), func(t *testing.T) {
				modelsTestDB(t)
				testMessagesKeepaliveConfig(t)
				relayNetwork.BodyIdleTimeout = 70 * time.Millisecond
				previous := getSettings()
				settingsMu.Lock()
				settingsCache.Retry = 1
				settingsMu.Unlock()
				t.Cleanup(func() { settingsMu.Lock(); settingsCache = previous; settingsMu.Unlock() })
				bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, nativePrelude(format))
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}))
				defer bad.Close()
				var goodCalls atomic.Int32
				good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					goodCalls.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, fixtureSSE(format))
				}))
				defer good.Close()
				first := fixtureUpstream(format, bad.URL)
				first.Enabled, first.Weight, first.Models = true, 1, "alias"
				if err := dbInsertUpstream(first); err != nil {
					t.Fatal(err)
				}
				if fallback {
					second := fixtureUpstream(format, good.URL)
					second.Enabled, second.Weight, second.Models = true, 1, "alias"
					if err := dbInsertUpstream(second); err != nil {
						t.Fatal(err)
					}
				}
				key := ApiKey{Models: "alias", Name: "failover-pings", Key: "sk-failover-pings", Enabled: true}
				if err := dbInsertKey(&key); err != nil {
					t.Fatal(err)
				}
				router := gin.New()
				router.POST("/v1/messages", func(c *gin.Context) { c.Set("apiKey", key) }, relayHandler(FormatAnthropic, false))
				req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"alias","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
				req.Header.Set("X-Session-Id", "ping-failover")
				affinitySet(scopedSessionKey(key.ID, "sid:ping-failover"), first.ID)
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				body := rec.Body.String()
				if !strings.Contains(body, "event: ping") || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") || healthFails(first.ID) != 1 {
					t.Fatalf("ping masked upstream timeout: %s", body)
				}
				if fallback {
					if goodCalls.Load() != 1 || !strings.Contains(body, `"type":"message_stop"`) || strings.Contains(body, `"type":"error"`) {
						t.Fatalf("ping blocked failover: %s", body)
					}
				} else if !strings.Contains(body, "event: error") || strings.Contains(body, `"type":"message_stop"`) {
					t.Fatalf("failed ping-only stream did not end with SSE error: %s", body)
				}
				logs, err := dbListLogs(10, 0)
				if err != nil || len(logs) != 1 || (!fallback && (logs[0].Status != 502 || logs[0].UpstreamID != first.ID)) || (fallback && logs[0].Status != 200) {
					t.Fatalf("incorrect failure/failover log: %+v / %v", logs, err)
				}
			})
		}
	}
}

func TestClaudeKeepaliveVersionAndOpenBlock(t *testing.T) {
	for _, sample := range []struct {
		ua   string
		want bool
	}{
		{"claude-cli/2.1.192 (external, cli)", false}, {"claude-cli/2.1.193", true}, {"claude-cli/2.1.200 (external, cli)", true}, {"claude-cli/2.2.0", true}, {"claude-cli/3.0.0", true}, {"claude-cli/2.0.999", false}, {"claude-cli/2.1.193-beta", false}, {"other/3.0.0", false},
	} {
		if got := supportsClaudeEmptyDelta(sample.ua); got != sample.want {
			t.Errorf("%q: got %v", sample.ua, got)
		}
	}
	for _, kind := range []string{"text", "thinking", "tool_use", "redacted_thinking"} {
		for _, ua := range []string{"claude-cli/2.1.193", "claude-cli/2.1.192"} {
			t.Run(kind+ua, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
				state := &messagesKeepalive{c: c, emptyDelta: supportsClaudeEmptyDelta(ua), blockIndex: -1}
				state.observe(map[string]any{"type": "content_block_start", "index": 3, "content_block": map[string]any{"type": kind}})
				if err := state.ping(); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(rec.Body.String(), "event: ping") {
					t.Fatal("referenced a block before its headers were delivered")
				}
				rec.Body.Reset()
				state.committed = true
				if err := state.ping(); err != nil {
					t.Fatal(err)
				}
				wantDelta := supportsClaudeEmptyDelta(ua) && kind != "redacted_thinking"
				if strings.Contains(rec.Body.String(), "event: content_block_delta") != wantDelta {
					t.Fatalf("wrong block heartbeat: %s", rec.Body.String())
				}
				state.observe(map[string]any{"type": "content_block_stop", "index": 3})
				rec.Body.Reset()
				if err := state.ping(); err != nil || !strings.Contains(rec.Body.String(), "event: ping") {
					t.Fatal("sent delta for a closed block")
				}
			})
		}
	}
}

func TestMessagesKeepaliveCancellationJoinsReader(t *testing.T) {
	testMessagesKeepaliveConfig(t)
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)
	state := beginMessagesKeepalive(c, &relayTask{format: FormatAnthropic, stream: true})
	body := newMessagesKeepaliveBody(reader, ctx, state).(*messagesKeepaliveBody)
	defer body.Close()
	go cancel()
	_, err := body.Read(make([]byte, 64))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was lost: %v", err)
	}
	select {
	case <-body.done:
	default:
		t.Fatal("reader outlived cancellation")
	}
}

func TestMessagesKeepaliveNeverReplaysStartedTools(t *testing.T) {
	for _, format := range []clientFormat{FormatAnthropic, FormatOpenAIChat} {
		t.Run(string(format), func(t *testing.T) {
			modelsTestDB(t)
			testMessagesKeepaliveConfig(t)
			relayNetwork.BodyIdleTimeout = 70 * time.Millisecond
			var badCalls, goodCalls atomic.Int32
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				badCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if format == FormatAnthropic {
					fmt.Fprint(w, auditFormatFrames(
						`{"type":"message_start","message":{"id":"msg_tool","usage":{"input_tokens":17,"output_tokens":0}}}`,
						`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_write","name":"write","input":{}}}`,
						`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{"}}`,
					))
				} else {
					fmt.Fprint(w, auditFormatFrames(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_write","function":{"name":"write","arguments":"{"}}]}}]}`))
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer bad.Close()
			good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { goodCalls.Add(1) }))
			defer good.Close()
			first, second := fixtureUpstream(format, bad.URL), fixtureUpstream(format, good.URL)
			for _, up := range []*Upstream{first, second} {
				up.Enabled, up.Weight, up.Models = true, 1, "alias"
				if err := dbInsertUpstream(up); err != nil {
					t.Fatal(err)
				}
			}
			key := ApiKey{Models: "alias", Name: "tool-pings", Key: "sk-tool-pings", Enabled: true}
			if err := dbInsertKey(&key); err != nil {
				t.Fatal(err)
			}
			router := gin.New()
			router.POST("/v1/messages", func(c *gin.Context) { c.Set("apiKey", key) }, relayHandler(FormatAnthropic, false))
			req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"alias","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"edit file"}]}`))
			req.Header.Set("X-Session-Id", "tool-pings")
			req.Header.Set("User-Agent", "claude-cli/2.1.193 (external, cli)")
			affinitySet(scopedSessionKey(key.ID, "sid:tool-pings"), first.ID)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			body := rec.Body.String()
			if badCalls.Load() != 1 || goodCalls.Load() != 0 || !strings.Contains(body, `"partial_json":""`) || !strings.Contains(body, "event: error") || strings.Contains(body, `"type":"message_stop"`) || strings.Contains(body, `"type":"content_block_stop"`) {
				t.Fatalf("heartbeat replayed or completed an interrupted tool: calls=%d/%d body=%s", badCalls.Load(), goodCalls.Load(), body)
			}
			logs, err := dbListLogs(10, 0)
			if err != nil || len(logs) != 1 || logs[0].Status != 502 || healthFails(first.ID) != 1 {
				t.Fatalf("interrupted tool logged as success: %+v / %v", logs, err)
			}
		})
	}
}

func TestMessagesFailedHeartbeatDoesNotCoolOrRetryUpstream(t *testing.T) {
	modelsTestDB(t)
	testMessagesKeepaliveConfig(t)
	var fallbackCalls atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fallbackCalls.Add(1) }))
	defer good.Close()
	first, second := fixtureUpstream(FormatAnthropic, bad.URL), fixtureUpstream(FormatAnthropic, good.URL)
	for _, up := range []*Upstream{first, second} {
		up.Enabled, up.Weight, up.Models = true, 1, "alias"
		if err := dbInsertUpstream(up); err != nil {
			t.Fatal(err)
		}
	}
	key := ApiKey{Models: "alias", Name: "disconnected", Key: "sk-disconnected", Enabled: true}
	if err := dbInsertKey(&key); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.POST("/v1/messages", func(c *gin.Context) { c.Set("apiKey", key) }, relayHandler(FormatAnthropic, false))
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"alias","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Session-Id", "disconnected")
	affinitySet(scopedSessionKey(key.ID, "sid:disconnected"), first.ID)
	router.ServeHTTP(&failedStreamResponse{discardStreamResponse{header: make(http.Header)}}, req)
	if fallbackCalls.Load() != 0 || healthFails(first.ID) != 0 {
		t.Fatal("failed heartbeat retried or penalized the upstream")
	}
}
