package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestSpeedMetricsRoundTripThroughCallLogs(t *testing.T) {
	publicTestDB(t)
	if err := dbInsertLog(&CallLog{KeyName: "speed", Model: "gpt-speed", Status: 200, LatencyMS: 120, FirstTokenMS: 340, DurationMS: 9000}); err != nil {
		t.Fatal(err)
	}
	logs, err := dbListLogs(1, 0)
	if err != nil || len(logs) != 1 {
		t.Fatalf("speed log missing: %+v %v", logs, err)
	}
	if logs[0].FirstTokenMS != 340 || logs[0].DurationMS != 9000 || logs[0].LatencyMS != 120 {
		t.Fatalf("speed metrics lost: %+v", logs[0])
	}
}

func TestSpeedLegacyLogsKeepZeroMetrics(t *testing.T) {
	publicTestDB(t)
	if _, err := db.Exec(`ALTER TABLE call_logs DROP COLUMN first_token_ms`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE call_logs DROP COLUMN duration_ms`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO call_logs(model,status,latency_ms) VALUES('legacy',200,250)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := migrateSpeedLogSchema(); err != nil {
			t.Fatal(err)
		}
	}
	logs, err := dbListLogs(1, 0)
	if err != nil || len(logs) != 1 {
		t.Fatalf("legacy log lost: %+v %v", logs, err)
	}
	// Historical rows have no measured speed; the console renders them as "—".
	if logs[0].FirstTokenMS != 0 || logs[0].DurationMS != 0 || logs[0].LatencyMS != 250 {
		t.Fatalf("legacy row invented speed data: %+v", logs[0])
	}
}

func TestSpeedFirstTokenIsRecordedOnce(t *testing.T) {
	info := relayInfo{startedAt: time.Now().Add(-2 * time.Second)}
	info.markFirstToken()
	first := info.FirstTokenMS
	if first < 1900 {
		t.Fatalf("first token time not measured from request start: %d", first)
	}
	time.Sleep(20 * time.Millisecond)
	info.markFirstToken()
	if info.FirstTokenMS != first {
		t.Fatal("first token time was overwritten by a later delta")
	}
	if info.elapsedMS() < first {
		t.Fatalf("elapsed time precedes the first token: %d / %d", info.elapsedMS(), first)
	}
	if (&relayInfo{}).elapsedMS() != 0 {
		t.Fatal("a request that never started reported elapsed time")
	}
}

func TestSpeedNativeStreamingMarksFirstToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	info := relayInfo{startedAt: time.Now().Add(-30 * time.Millisecond)}
	if got := recordNativeEvent(FormatOpenAIChat, `{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`, &info); got || info.FirstTokenMS != 0 {
		t.Fatal("a role-only chunk counted as the first token")
	}
	consume := func(payload string) {
		if recordNativeEvent(FormatOpenAIChat, payload, &info) {
			info.markFirstToken()
		}
	}
	consume(`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`)
	if info.FirstTokenMS <= 0 || info.OutputText.String() != "hi" {
		t.Fatalf("first content delta was not recorded: %+v", info)
	}
	time.Sleep(15 * time.Millisecond)
	first := info.FirstTokenMS
	consume(`{"choices":[{"index":0,"delta":{"content":"!"}}]}`)
	if info.FirstTokenMS != first || info.OutputText.String() != "hi!" {
		t.Fatalf("later delta moved the first token time: %+v", info)
	}
}

// The native streaming relay is the path that decides when a first token has
// actually reached the client, so drive it end to end.
func TestSpeedRelaySSENativeRecordsFirstTokenAndDuration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	frames := "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\n" +
		"data: [DONE]\n\n"
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(frames)), Header: http.Header{"Content-Type": []string{"text/event-stream"}}}
	info := relayInfo{startedAt: time.Now().Add(-25 * time.Millisecond)}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if err := relaySSENative(c, resp, FormatOpenAIChat, &info); err != nil {
		t.Fatal(err)
	}
	if info.FirstTokenMS <= 0 || info.OutputText.String() != "hi" {
		t.Fatalf("streamed relay lost the first token: %+v", info)
	}
	info.DurationMS = info.elapsedMS()
	if info.DurationMS < info.FirstTokenMS {
		t.Fatalf("duration precedes the first token: %+v", info)
	}
}

func TestSpeedFinalizeCallPersistsMetrics(t *testing.T) {
	publicTestDB(t)
	gin.SetMode(gin.TestMode)
	key := publicTestKey(t, "speed-finalize", 0, 0, false)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	task := &relayTask{format: FormatOpenAIChat, model: "gpt-alpha"}
	finalizeCall(c, *key, task, relayInfo{Status: 200, PromptTok: 5, CompletionTok: 40, InKnown: true, OutKnown: true, LatencyMS: 200, FirstTokenMS: 450, DurationMS: 5000}, true, nil, -1)
	logs, err := dbListLogs(1, 0)
	if err != nil || len(logs) != 1 || logs[0].FirstTokenMS != 450 || logs[0].DurationMS != 5000 {
		t.Fatalf("finalize dropped speed metrics: %+v %v", logs, err)
	}
}

func TestSpeedFailureLogsCarryMetrics(t *testing.T) {
	publicTestDB(t)
	gin.SetMode(gin.TestMode)
	key := publicTestKey(t, "speed-failure", 0, 0, false)
	task := &relayTask{format: FormatOpenAIChat, model: "gpt-alpha"}
	insertFailureLog(*key, relayInfo{UpstreamID: 7, UpstreamName: "up", LatencyMS: 30, FirstTokenMS: 90, DurationMS: 150}, task, 502, "upstream failed")
	logs, err := dbListLogs(1, 0)
	if err != nil || len(logs) != 1 || logs[0].FirstTokenMS != 90 || logs[0].DurationMS != 150 {
		t.Fatalf("failure log dropped speed metrics: %+v %v", logs, err)
	}
}

func TestSpeedDoRelayMeasuresDuration(t *testing.T) {
	publicTestDB(t)
	gin.SetMode(gin.TestMode)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		time.Sleep(40 * time.Millisecond)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	defer server.Close()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "http://gateway.invalid", strings.NewReader("{}"))
	u := &Upstream{Type: "openai", BaseURL: server.URL, APIKey: "k", Weight: 1, Enabled: true}
	task := &relayTask{format: FormatOpenAIChat, endpoint: "/v1/chat/completions", model: "model-a", body: []byte(`{"model":"model-a"}`), canon: &canonReq{Model: "model-a", Messages: []canonMsg{{Role: "user", Text: "hi"}}}}
	info, wrote, err := doRelay(c, u, task)
	if err != nil || !wrote {
		t.Fatalf("relay failed: wrote=%t err=%v", wrote, err)
	}
	if info.DurationMS < 40 || info.LatencyMS < 40 || info.DurationMS < info.LatencyMS {
		t.Fatalf("duration did not cover the whole exchange: %+v", info)
	}
	// A non-streaming reply has no incremental first token, so TPS falls back to
	// the total duration rather than reporting a fabricated first-token time.
	if info.FirstTokenMS != 0 {
		t.Fatalf("non-streaming reply reported a first token time: %+v", info)
	}
}

func TestSpeedLogsEndpointExposesMetrics(t *testing.T) {
	r := testProxyAdminRouter(t)
	if err := dbInsertLog(&CallLog{KeyName: "speed-api", Model: "gpt-alpha", Status: 200, LatencyMS: 100, FirstTokenMS: 250, DurationMS: 4000, CompletionTokens: 80}); err != nil {
		t.Fatal(err)
	}
	w := proxyAdminRequest(r, "GET", "logs", "", true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"first_token_ms":250`) || !strings.Contains(w.Body.String(), `"duration_ms":4000`) {
		t.Fatalf("logs API did not expose speed metrics: %d %s", w.Code, w.Body.String())
	}
}
