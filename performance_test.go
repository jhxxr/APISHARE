package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRelayHealthCooldownAndRecovery(t *testing.T) {
	modelsTestDB(t)
	gin.SetMode(gin.TestMode)
	var calls atomic.Int32
	var recovered atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !recovered.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"recovered"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer server.Close()
	up := &Upstream{Type: "openai", BaseURL: server.URL, Weight: 1, Enabled: true, Models: "review-model"}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	key := &ApiKey{Name: "review", Key: "sk-review", Enabled: true, Models: "review-model"}
	if err := dbInsertKey(key); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.POST("/v1/chat/completions", func(c *gin.Context) { c.Set("apiKey", *key) }, relayHandler(FormatOpenAIChat, false))
	request := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"review-model","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("X-Session-Id", "review-health")
		r.ServeHTTP(w, req)
		return w
	}
	for i := 0; i < 3; i++ {
		if w := request(); w.Code != http.StatusBadGateway {
			t.Fatalf("failure %d: HTTP %d", i, w.Code)
		}
	}
	if healthFails(up.ID) != 3 || healthAvailable(up.ID) {
		t.Fatalf("failed upstream did not enter cooldown: %d", healthFails(up.ID))
	}
	if w := request(); w.Code != http.StatusBadGateway || calls.Load() != 3 {
		t.Fatalf("sticky request contacted cooling upstream: status=%d calls=%d", w.Code, calls.Load())
	}
	recovered.Store(true)
	upHealth.Lock()
	upHealth.until[up.ID] = time.Now().Add(-time.Second)
	upHealth.Unlock()
	if w := request(); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "recovered") {
		t.Fatalf("upstream did not recover: %d %s", w.Code, w.Body.String())
	}
	if healthFails(up.ID) != 0 || !healthAvailable(up.ID) {
		t.Fatal("successful response did not reset health")
	}
}

func TestRelayHealthIgnoresClientErrorsAndCancellation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const id = int64(-71)
	defer healthMarkSuccess(id)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	up := &Upstream{ID: id, Type: "openai", BaseURL: server.URL}
	task := &relayTask{format: FormatOpenAIChat, endpoint: "/v1/chat/completions", model: "review-model", nativeOnly: true, body: []byte(`{"model":"review-model"}`)}
	healthMarkSuccess(id)
	healthMarkFailure(id)
	for _, canceled := range []bool{false, true} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx, cancel := context.WithCancel(context.Background())
		if canceled {
			cancel()
		}
		c.Request = httptest.NewRequest("POST", task.endpoint, nil).WithContext(ctx)
		_, wrote, err := doRelay(c, up, task)
		cancel()
		if canceled && (wrote || err == nil) {
			t.Fatal("canceled request unexpectedly succeeded")
		}
		if !canceled && (!wrote || err != nil) {
			t.Fatalf("client error was not returned: wrote=%v err=%v", wrote, err)
		}
		if healthFails(id) != 1 {
			t.Fatal("client error/cancellation changed upstream health")
		}
	}
}

func TestHealthBackoffRemainsCapped(t *testing.T) {
	const id = int64(-72)
	healthMarkSuccess(id)
	defer healthMarkSuccess(id)
	for i := 0; i < 100; i++ {
		healthMarkFailure(id)
	}
	upHealth.Lock()
	remaining := time.Until(upHealth.until[id])
	upHealth.Unlock()
	if remaining < 9*time.Minute || remaining > 10*time.Minute {
		t.Fatalf("backoff overflowed: %s", remaining)
	}
}

type failedStreamResponse struct{ discardStreamResponse }

func (w *failedStreamResponse) Write([]byte) (int, error) {
	return 0, errors.New("client disconnected")
}

func TestDownstreamWriteFailureDoesNotCoolUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const id = int64(-73)
	defer healthMarkSuccess(id)
	for _, native := range []bool{true, false} {
		healthMarkSuccess(id)
		healthMarkFailure(id)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		}))
		c, _ := gin.CreateTestContext(&failedStreamResponse{discardStreamResponse{header: make(http.Header)}})
		c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
		format := FormatOpenAIChat
		if !native {
			format = FormatOpenAIResponse
		}
		up := &Upstream{ID: id, Type: "openai", OpenAIProtocol: "chat", BaseURL: server.URL}
		task := &relayTask{format: format, endpoint: "/v1/chat/completions", model: "review-model", stream: true, canon: &canonReq{Model: "review-model", Stream: true}, body: []byte(`{"model":"review-model","stream":true}`)}
		_, _, err := doRelay(c, up, task)
		server.Close()
		if !errors.Is(err, errDownstreamWrite) || healthFails(id) != 1 {
			t.Fatalf("native=%v: downstream error counted against upstream: %v failures=%d", native, err, healthFails(id))
		}
	}
}

func TestPublicCacheCoalescesAndSeparatesResolutions(t *testing.T) {
	var cache publicOverviewCache
	database := &sql.DB{}
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	build := func() ([]byte, error) {
		calls.Add(1)
		close(started)
		<-release
		return []byte(`{"history_interval":"hour"}`), nil
	}
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			body, err := cache.get(context.Background(), database, "hour", build)
			if err != nil || string(body) != `{"history_interval":"hour"}` {
				t.Errorf("snapshot mismatch: %s %v", body, err)
			}
		}()
	}
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.get(ctx, database, "hour", build); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	close(release)
	group.Wait()
	if calls.Load() != 1 {
		t.Fatalf("simultaneous queries built %d snapshots", calls.Load())
	}
	freshBuild := func() ([]byte, error) { calls.Add(1); return []byte("fresh"), nil }
	if body, err := cache.get(context.Background(), database, "minute", freshBuild); err != nil || string(body) != "fresh" {
		t.Fatal("minute resolution reused hourly result")
	}
	cache.Lock()
	cache.entries["hour"].expires = time.Now().Add(-time.Second)
	cache.Unlock()
	if body, err := cache.get(context.Background(), database, "hour", freshBuild); err != nil || string(body) != "fresh" {
		t.Fatal("expired result was reused")
	}
	if _, err := cache.get(context.Background(), &sql.DB{}, "hour", freshBuild); err != nil || calls.Load() != 4 {
		t.Fatal("cache was shared across databases")
	}
}

func TestPublicCacheRetriesFailuresAndInvalidatesConfiguration(t *testing.T) {
	var cache publicOverviewCache
	database := &sql.DB{}
	if _, err := cache.get(context.Background(), database, "hour", func() ([]byte, error) { return nil, errors.New("failed") }); err == nil {
		t.Fatal("build failure was hidden")
	}
	if body, err := cache.get(context.Background(), database, "hour", func() ([]byte, error) { return []byte("ok"), nil }); err != nil || string(body) != "ok" {
		t.Fatal("failed snapshot was cached")
	}
	publicTestDB(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	registerPublic(r)
	request := func() PublicOverview {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/api/public/overview", nil))
		var result PublicOverview
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatalf("overview failed: %d %s", w.Code, w.Body.String())
		}
		return result
	}
	if len(request().Models) != 0 {
		t.Fatal("unexpected initial catalog")
	}
	up := &Upstream{Type: "openai", Models: "new-model", Weight: 1, Enabled: true}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	if result := request(); len(result.Models) != 1 || result.Models[0].Model != "new-model" {
		t.Fatal("new upstream did not invalidate snapshot")
	}
	up.Models = "changed-model"
	if err := dbUpdateUpstream(up); err != nil {
		t.Fatal(err)
	}
	if result := request(); len(result.Models) != 1 || result.Models[0].Model != "changed-model" {
		t.Fatal("updated upstream did not invalidate snapshot")
	}
	if err := dbDeleteUpstream(up.ID); err != nil {
		t.Fatal(err)
	}
	if len(request().Models) != 0 {
		t.Fatal("deleted upstream remained in cached catalog")
	}
}

func TestPublicTimeRangeUsesExpressionIndex(t *testing.T) {
	publicTestDB(t)
	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT COUNT(*) FROM call_logs WHERE unixepoch(created_at) BETWEEN ? AND ?`, 0, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		indexed = indexed || (strings.Contains(detail, "SEARCH") && strings.Contains(detail, "idx_logs_epoch"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("time filtering still scans the entire log index")
	}
}

func TestLongConvertedStreamsPreserveTextThinkingAndTools(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, format := range protocolFormats {
		t.Run(string(format), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			writer := newProtocolStream(c, format, "review-model", true)
			push := func(key string, block responseBlock) {
				t.Helper()
				if err := writer.push(c, streamDelta{Key: key, Block: block}, upstreamUsage{}); err != nil {
					t.Fatal(err)
				}
			}
			push("tool", responseBlock{Kind: "tool", Call: canonCall{ID: "call-long", Name: "echo", Arguments: `{"value":"`}})
			for i := 0; i < 512; i++ {
				push("text", responseBlock{Kind: "text", Text: strings.Repeat("a", 32)})
				push("thinking", responseBlock{Kind: "thinking", Text: strings.Repeat("b", 16)})
				push("tool", responseBlock{Kind: "tool", Call: canonCall{Arguments: strings.Repeat("c", 32)}})
			}
			// Force the buffer slices to grow, then continue an earlier block.
			for i := 0; i < 64; i++ {
				push(fmt.Sprintf("extra%d", i), responseBlock{Kind: "text", Text: "z"})
			}
			push("text", responseBlock{Kind: "text", Text: "end"})
			push("tool", responseBlock{Kind: "tool", Call: canonCall{Arguments: `"}`}})
			usage := upstreamUsage{Prompt: 17, Completion: 23, InKnown: true, OutKnown: true}
			if err := writer.finish(c, usage, "tool_calls", ""); err != nil {
				t.Fatal(err)
			}
			decoder := newProtocolDecoder(format)
			var text, thinking, arguments strings.Builder
			var completed json.RawMessage
			err := readSSEFrames(strings.NewReader(recorder.Body.String()), func(payload string) (bool, error) {
				deltas, _, err := decoder.decode(payload)
				for _, delta := range deltas {
					switch delta.Block.Kind {
					case "text":
						text.WriteString(delta.Block.Text)
					case "thinking":
						thinking.WriteString(delta.Block.Text)
					case "tool":
						arguments.WriteString(delta.Block.Call.Arguments)
					}
				}
				if format == FormatOpenAIResponse {
					var event struct {
						Type     string          `json:"type"`
						Response json.RawMessage `json:"response"`
					}
					if json.Unmarshal([]byte(payload), &event) == nil && event.Type == "response.completed" {
						completed = event.Response
					}
				}
				return false, err
			})
			if err != nil {
				t.Fatal(err)
			}
			wantText := strings.Repeat("a", 512*32) + strings.Repeat("z", 64) + "end"
			wantThinking := strings.Repeat("b", 512*16)
			wantArguments := `{"value":"` + strings.Repeat("c", 512*32) + `"}`
			if text.String() != wantText || thinking.String() != wantThinking || arguments.String() != wantArguments || !decoder.terminal {
				t.Fatalf("long stream changed content: text=%d thinking=%d arguments=%d terminal=%v", text.Len(), thinking.Len(), arguments.Len(), decoder.terminal)
			}
			if format == FormatOpenAIResponse {
				result, err := parseProtocolResponse(format, completed)
				if err != nil {
					t.Fatal(err)
				}
				var fullText, fullThinking, fullArguments strings.Builder
				for _, block := range result.Blocks {
					if block.Kind == "text" {
						fullText.WriteString(block.Text)
					}
					if block.Kind == "thinking" {
						fullThinking.WriteString(block.Text)
					}
					if block.Kind == "tool" {
						fullArguments.WriteString(block.Call.Arguments)
					}
				}
				if fullText.String() != strings.Repeat("a", 512*32)+"end"+strings.Repeat("z", 64) || fullThinking.String() != wantThinking || fullArguments.String() != wantArguments || result.Usage.Prompt != 17 || result.Usage.Completion != 23 {
					t.Fatal("completed Responses payload lost buffered content or usage")
				}
			}
		})
	}
}

func benchmarkPerformanceDB(b *testing.B) {
	b.Helper()
	if err := openDB(filepath.Join(b.TempDir(), "benchmark.db")); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
}

func BenchmarkPublicOverview(b *testing.B) {
	for _, size := range []int{1000, 100000} {
		for _, interval := range []string{"hour", "minute"} {
			b.Run(fmt.Sprintf("logs%d/%s", size, interval), func(b *testing.B) {
				benchmarkPerformanceDB(b)
				now := time.Now()
				models := make([]string, 20)
				for i := range models {
					models[i] = fmt.Sprintf("review-%d", i)
				}
				if err := dbInsertUpstream(&Upstream{Type: "openai", Weight: 1, Enabled: true, Models: strings.Join(models, ",")}); err != nil {
					b.Fatal(err)
				}
				_, err := db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<?)
				INSERT INTO call_logs(model,status,prompt_tokens,completion_tokens,created_at)
				SELECT 'review-'||(x%20),200,100,20,datetime(?-CAST(x*5184000/? AS INTEGER),'unixepoch') FROM n`, size, now.Unix(), size)
				if err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					result, err := buildPublicOverview(now, interval)
					if err != nil {
						b.Fatal(err)
					}
					if i == 0 {
						body, err := json.Marshal(result)
						if err != nil {
							b.Fatal(err)
						}
						b.ReportMetric(float64(len(body)), "JSONbytes")
					}
				}
			})
		}
	}
}

type discardStreamResponse struct{ header http.Header }

func (w *discardStreamResponse) Header() http.Header         { return w.header }
func (w *discardStreamResponse) WriteHeader(int)             {}
func (w *discardStreamResponse) Write(p []byte) (int, error) { return len(p), nil }
func (w *discardStreamResponse) Flush()                      {}

func BenchmarkProtocolStream(b *testing.B) {
	gin.SetMode(gin.TestMode)
	for _, chunks := range []int{1024, 4096} {
		b.Run(fmt.Sprintf("chunks%d", chunks), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				c, _ := gin.CreateTestContext(&discardStreamResponse{header: make(http.Header)})
				w := newProtocolStream(c, FormatOpenAIResponse, "review-model", false)
				for j := 0; j < chunks; j++ {
					if err := w.push(c, streamDelta{Key: "text", Block: responseBlock{Kind: "text", Text: strings.Repeat("a", 32)}}, upstreamUsage{}); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

func BenchmarkPublicOverviewCacheHit(b *testing.B) {
	var cache publicOverviewCache
	database := &sql.DB{}
	body := []byte(strings.Repeat("x", 56<<10))
	build := func() ([]byte, error) { return body, nil }
	if _, err := cache.get(context.Background(), database, "hour", build); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := cache.get(context.Background(), database, "hour", build); err != nil {
			b.Fatal(err)
		}
	}
}
