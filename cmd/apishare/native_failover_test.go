package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func nativePrelude(format clientFormat) string {
	switch format {
	case FormatOpenAIChat:
		return "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"
	case FormatOpenAIResponse:
		return "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"failed-response\",\"status\":\"in_progress\"}}\n\n" +
			"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\",\"content\":[]}}\n\n" +
			"data: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[]}}\n\n"
	case FormatAnthropic:
		return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\ndata: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"
	default:
		return "data: {\"candidates\":[],\"usageMetadata\":{\"promptTokenCount\":1}}\n\n"
	}
}

func TestNativeInitialStreamFailureRetriesAnotherUpstream(t *testing.T) {
	for _, format := range protocolFormats {
		for _, failure := range []string{"eof", "error", "idle"} {
			t.Run(string(format)+"/"+failure, func(t *testing.T) {
				modelsTestDB(t)
				if failure == "idle" {
					previousIdle := relayNetwork.BodyIdleTimeout
					relayNetwork.BodyIdleTimeout = 30 * time.Millisecond
					t.Cleanup(func() { relayNetwork.BodyIdleTimeout = previousIdle })
				}
				previous := getSettings()
				settingsMu.Lock()
				settingsCache.Retry = 1
				settingsMu.Unlock()
				t.Cleanup(func() { settingsMu.Lock(); settingsCache = previous; settingsMu.Unlock() })
				var failedCalls, goodCalls atomic.Int32
				bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					failedCalls.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, ": heartbeat\n\n"+nativePrelude(format))
					if failure == "error" {
						fmt.Fprint(w, "data: {\"error\":{\"message\":\"overloaded\"}}\n\n")
					}
					if failure == "idle" {
						w.(http.Flusher).Flush()
						<-r.Context().Done()
					}
				}))
				defer bad.Close()
				good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					goodCalls.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, fixtureSSE(format))
				}))
				defer good.Close()
				first, second := fixtureUpstream(format, bad.URL), fixtureUpstream(format, good.URL)
				for _, up := range []*Upstream{first, second} {
					up.Enabled, up.Weight, up.Models = true, 1, "alias"
					if err := dbInsertUpstream(up); err != nil {
						t.Fatal(err)
					}
				}
				task := fixtureTask(t, format, true)
				key := ApiKey{Models: "alias", Name: "native-failover", Key: "sk-native-failover", Enabled: true}
				if err := dbInsertKey(&key); err != nil {
					t.Fatal(err)
				}
				router := gin.New()
				router.POST("/relay", func(c *gin.Context) { c.Set("apiKey", key) }, relayHandler(format, false))
				if format == FormatGemini {
					router = gin.New()
					router.POST("/v1beta/models/*action", func(c *gin.Context) { c.Set("apiKey", key) }, relayHandler(format, false))
				}
				path := "/relay"
				if format == FormatGemini {
					path = task.endpoint
				}
				req := httptest.NewRequest("POST", path, strings.NewReader(string(task.body)))
				req.Header.Set("X-Session-Id", "native-failover")
				affinitySet("sid:native-failover", first.ID)
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				if failedCalls.Load() != 1 || goodCalls.Load() != 1 || rec.Code != 200 || rec.Body.String() != fixtureSSE(format) {
					t.Fatalf("native failover failed: calls=%d/%d status=%d body=%s", failedCalls.Load(), goodCalls.Load(), rec.Code, rec.Body.String())
				}
				if healthFails(first.ID) != 1 || healthFails(second.ID) != 0 {
					t.Fatal("incorrect upstream health")
				}
				logs, err := dbListLogs(10, 0)
				if err != nil || len(logs) != 1 || logs[0].Status != 200 || logs[0].UpstreamID != second.ID {
					t.Fatalf("incorrect failover log: %+v / %v", logs, err)
				}
			})
		}
	}
}

func TestNativeActivityPreventsRetry(t *testing.T) {
	for _, sample := range []struct {
		format clientFormat
		event  string
	}{
		{FormatOpenAIChat, `{"choices":[{"delta":{"content":"partial"}}]}`},
		{FormatOpenAIChat, `{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`},
		{FormatOpenAIChat, `{"choices":[{"delta":{"tool_calls":[{"id":"call","function":{"name":"write"}}]}}]}`},
		{FormatOpenAIResponse, `{"type":"response.output_item.added","item":{"type":"function_call","name":"write","call_id":"call"}}`},
		{FormatOpenAIResponse, `{"type":"response.output_item.added","item":{"type":"web_search_call","status":"in_progress"}}`},
		{FormatOpenAIResponse, `{"type":"response.output_text.delta","delta":"partial"}`},
		{FormatOpenAIResponse, `{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`},
		{FormatAnthropic, `{"type":"content_block_start","content_block":{"type":"tool_use","id":"call","name":"write","input":{}}}`},
		{FormatAnthropic, `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"thinking"}}`},
		{FormatGemini, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"write","args":{}}}]}}]}`},
	} {
		t.Run(string(sample.format)+sample.event, func(t *testing.T) {
			frames := nativePrelude(sample.format) + "data: " + sample.event + "\n\n"
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			err := relaySSENative(c, &http.Response{Body: io.NopCloser(strings.NewReader(frames))}, sample.format, &relayInfo{})
			if !errors.Is(err, io.ErrUnexpectedEOF) || !c.Writer.Written() || rec.Body.String() != frames {
				t.Fatalf("activity was buffered or lost: %v / %s", err, rec.Body.String())
			}
		})
	}
}

func TestFailedRelayKeepsLastUpstreamInLog(t *testing.T) {
	modelsTestDB(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	up := &Upstream{Name: "unavailable-provider", Type: "openai", BaseURL: server.URL, Models: "alias", Enabled: true, Weight: 1}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	key := ApiKey{Models: "alias", Name: "failed-log", Key: "sk-failed-log", Enabled: true}
	if err := dbInsertKey(&key); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.POST("/relay", func(c *gin.Context) { c.Set("apiKey", key) }, relayHandler(FormatOpenAIChat, false))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("POST", "/relay", strings.NewReader(`{"model":"alias","messages":[{"role":"user","content":"hello"}]}`)))
	logs, err := dbListLogs(10, 0)
	if err != nil || len(logs) != 1 || logs[0].UpstreamID != up.ID || logs[0].UpstreamName != up.Name || logs[0].Status != 502 || !strings.Contains(logs[0].Error, "returned 503") {
		t.Fatalf("lost failed upstream or original cause: %+v / %v", logs, err)
	}
}

type nativeReadAfterTerminal struct{ io.Reader }

func (r nativeReadAfterTerminal) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		return n, errors.New("read past terminal event")
	}
	return n, err
}

func TestNativeStreamStopsAtExplicitTerminal(t *testing.T) {
	for _, format := range []clientFormat{FormatOpenAIChat, FormatOpenAIResponse, FormatAnthropic} {
		t.Run(string(format), func(t *testing.T) {
			frames := fixtureSSE(format)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			err := relaySSENative(c, &http.Response{Body: io.NopCloser(nativeReadAfterTerminal{strings.NewReader(frames)})}, format, &relayInfo{})
			if err != nil || rec.Body.String() != frames {
				t.Fatalf("read beyond completed stream: %v / %s", err, rec.Body.String())
			}
		})
	}
}
