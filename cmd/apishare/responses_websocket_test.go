package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

func responsesWSTestGateway(t *testing.T) (*ApiKey, *gin.Engine, *httptest.Server) {
	t.Helper()
	modelsTestDB(t)
	gin.SetMode(gin.TestMode)
	previous := getSettings()
	settingsMu.Lock()
	settingsCache.Retry = 1
	settingsCache.Pricing = map[string]Price{"ws-alias": {In: 1000000, Out: 1000000}}
	settingsMu.Unlock()
	t.Cleanup(func() { settingsMu.Lock(); settingsCache = previous; settingsMu.Unlock() })
	key := &ApiKey{Name: "ws", Key: "sk-responses-test", Models: "ws-alias", Enabled: true, QPS: 100, Concurrency: 4}
	if err := dbInsertKey(key); err != nil {
		t.Fatal(err)
	}
	router := newRouter()
	router.GET("/slot-probe", keyAuth(), keyRateLimit(), func(c *gin.Context) { c.Status(204) })
	var group sync.WaitGroup
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		group.Add(1)
		defer group.Done()
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { server.Close(); group.Wait() })
	return key, router, server
}

func responsesWSTestUpstream(t *testing.T, endpoint, kind string, priority int) *Upstream {
	t.Helper()
	up := &Upstream{Name: "ws-provider", Type: kind, OpenAIProtocol: "responses", BaseURL: endpoint + "/v1",
		APIKey: "sk-provider", Models: "ws-alias", ModelMap: `{"ws-alias":"ws-actual"}`,
		Enabled: true, Weight: 1, Priority: priority, HeaderOverrides: `{"*":true}`}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	return up
}

func responsesWSFixture(t *testing.T, handle func(context.Context, *websocket.Conn, *http.Request)) *httptest.Server {
	t.Helper()
	var group sync.WaitGroup
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		group.Add(1)
		defer group.Done()
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		handle(ctx, conn, r)
	}))
	t.Cleanup(func() { server.Close(); group.Wait() })
	return server
}

func responsesWSDial(t *testing.T, server *httptest.Server, key *ApiKey, headers http.Header) (context.Context, *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	t.Cleanup(cancel)
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Authorization", "Bearer "+key.Key)
	headers.Set("User-Agent", "codex_cli_rs/1.2.3")
	conn, response, err := websocket.Dial(ctx, server.URL+"/v1/responses?key="+key.Key, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatalf("WebSocket handshake: %v / %v", err, response)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return ctx, conn
}

func responsesWSWrite(t *testing.T, ctx context.Context, conn *websocket.Conn, body string) {
	t.Helper()
	if err := conn.Write(ctx, websocket.MessageText, []byte(body)); err != nil {
		t.Error(err)
	}
}

func responsesWSRead(t *testing.T, ctx context.Context, conn *websocket.Conn) []byte {
	t.Helper()
	kind, body, err := conn.Read(ctx)
	if err != nil || kind != websocket.MessageText {
		t.Fatalf("read Responses event: kind=%v body=%s err=%v", kind, body, err)
	}
	return body
}

func responsesWSTerminal(id, lane string, input, output int) string {
	stream := ""
	if lane != "" {
		stream = fmt.Sprintf(`"stream_id":%q,`, lane)
	}
	return fmt.Sprintf(`{"type":"response.completed",%s"response":{"id":%q,"status":"completed","output":[],"usage":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d}}}`, stream, id, input, output, input+output)
}

func TestResponsesWSReusesConnectionAndBillsEachTurnOnce(t *testing.T) {
	key, _, gateway := responsesWSTestGateway(t)
	key.Concurrency = 1
	if err := dbUpdateKey(key); err != nil {
		t.Fatal(err)
	}
	var connects atomic.Int32
	fixture := responsesWSFixture(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		connects.Add(1)
		if r.URL.Path != "/v1/responses" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer sk-provider" || r.Header.Get("Cookie") != "" || r.Header.Get("Originator") != "codex_cli_rs" || r.Header.Get("X-Tenant") != "one" {
			t.Errorf("unsafe or incomplete handshake: %s / %v", r.URL, r.Header)
		}
		for i := 1; i <= 2; i++ {
			_, body, err := conn.Read(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			if !bytes.Contains(body, []byte(`"model":"ws-actual"`)) || !bytes.Contains(body, []byte(`9007199254740993`)) {
				t.Errorf("model mapping changed opaque numeric content: %s", body)
			}
			if i == 2 && !bytes.Contains(body, []byte(`"previous_response_id":"response-1"`)) {
				t.Errorf("continuation was not forwarded: %s", body)
			}
			responsesWSWrite(t, ctx, conn, fmt.Sprintf(`{"type":"response.created","response":{"id":"response-%d","status":"in_progress"}}`, i))
			responsesWSWrite(t, ctx, conn, fmt.Sprintf(`{"type":"response.output_text.delta","response_id":"response-%d","delta":"hello"}`, i))
			input, output := 10, 3
			if i == 2 {
				input, output = 0, 0
			}
			terminal := responsesWSTerminal(fmt.Sprintf("response-%d", i), "", input, output)
			responsesWSWrite(t, ctx, conn, terminal)
			if i == 1 {
				responsesWSWrite(t, ctx, conn, terminal)
			}
		}
		_, _, _ = conn.Read(ctx)
	})
	responsesWSTestUpstream(t, fixture.URL, "openai", 0)
	ctx, conn := responsesWSDial(t, gateway, key, http.Header{"Originator": {"codex_cli_rs"}, "X-Tenant": {"one"}, "Cookie": {"client-secret"}})
	responsesWSWrite(t, ctx, conn, `{"type":"response.create","model":"ws-alias","input":"hello","metadata":{"number":9007199254740993}}`)
	for i := 0; i < 4; i++ {
		responsesWSRead(t, ctx, conn)
	}
	// An idle socket releases its turn slot to ordinary HTTP requests.
	req, _ := http.NewRequestWithContext(ctx, "GET", gateway.URL+"/slot-probe", nil)
	req.Header.Set("Authorization", "Bearer "+key.Key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("idle socket occupied an inference slot: %d", resp.StatusCode)
	}
	responsesWSWrite(t, ctx, conn, `{"type":"response.create","previous_response_id":"response-1","input":[{"type":"function_call_output","call_id":"c","output":"9007199254740993"}]}`)
	for i := 0; i < 3; i++ {
		responsesWSRead(t, ctx, conn)
	}
	if connects.Load() != 1 || len(getLimit(*key).sem) != 0 {
		t.Fatal("connection was not reused or turn slot was leaked")
	}
	logs, err := dbListLogs(10, 0)
	if err != nil || len(logs) != 2 {
		t.Fatalf("terminal event was duplicated or lost: %+v / %v", logs, err)
	}
	for _, log := range logs {
		if log.Status != 200 || log.Client.ID != "codex" || log.Client.Version != "1.2.3" || log.Billing == nil || !log.Billing.Reported.InKnown || !log.Billing.Reported.OutKnown {
			t.Fatalf("incorrect Responses receipt: %+v", log)
		}
	}
	updated, _ := dbGetKeyByID(key.ID)
	if updated.UsedUSD != 13 {
		t.Fatalf("duplicate terminal was charged: %v", updated.UsedUSD)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
}

func TestResponsesWSInterleavedStreamsAndQueuedTurns(t *testing.T) {
	key, _, gateway := responsesWSTestGateway(t)
	fixture := responsesWSFixture(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		for i := 0; i < 3; i++ {
			if _, _, err := conn.Read(ctx); err != nil {
				t.Error(err)
				return
			}
		}
		for _, event := range []string{
			`{"type":"response.created","stream_id":"b","response":{"id":"b-1"}}`,
			`{"type":"response.created","stream_id":"a","response":{"id":"a-1"}}`,
			`{"type":"response.output_text.delta","stream_id":"b","response_id":"b-1","delta":"B"}`,
			responsesWSTerminal("b-1", "b", 2, 0),
			responsesWSTerminal("a-1", "a", 3, 0),
			responsesWSTerminal("b-1", "b", 2, 0),
			`{"type":"response.created","stream_id":"a","response":{"id":"a-2"}}`,
			responsesWSTerminal("a-2", "a", 4, 0),
		} {
			responsesWSWrite(t, ctx, conn, event)
		}
		_, _, _ = conn.Read(ctx)
	})
	responsesWSTestUpstream(t, fixture.URL, "openai", 0)
	ctx, conn := responsesWSDial(t, gateway, key, nil)
	for _, lane := range []string{"a", "a", "b"} {
		responsesWSWrite(t, ctx, conn, fmt.Sprintf(`{"type":"response.create","model":"ws-alias","stream_id":%q,"input":[]}`, lane))
	}
	for i := 0; i < 8; i++ {
		responsesWSRead(t, ctx, conn)
	}
	logs, err := dbListLogs(10, 0)
	updated, _ := dbGetKeyByID(key.ID)
	if err != nil || len(logs) != 3 || updated.UsedUSD != 9 || len(getLimit(*key).sem) != 0 {
		t.Fatalf("interleaved/queued turns lost ownership: logs=%+v spend=%v err=%v", logs, updated.UsedUSD, err)
	}
}

func TestResponsesWSRechecksPermissionAndPreservesConcurrencyAcrossEdits(t *testing.T) {
	key, _, gateway := responsesWSTestGateway(t)
	key.Concurrency = 1
	if err := dbUpdateKey(key); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{}, 1)
	var calls atomic.Int32
	fixture := responsesWSFixture(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		calls.Add(1)
		responsesWSWrite(t, ctx, conn, `{"type":"response.created","response":{"id":"one"}}`)
		select {
		case <-release:
		case <-ctx.Done():
			return
		}
		responsesWSWrite(t, ctx, conn, responsesWSTerminal("one", "", 1, 1))
		if _, _, err := conn.Read(ctx); err == nil {
			calls.Add(1)
		}
	})
	responsesWSTestUpstream(t, fixture.URL, "openai", 0)
	ctx, conn := responsesWSDial(t, gateway, key, nil)
	responsesWSWrite(t, ctx, conn, `{"type":"response.create","model":"ws-alias","input":[]}`)
	responsesWSRead(t, ctx, conn)
	key.QPS = 99 // A limiter revision must retain the active turn's concurrency.
	if err := dbUpdateKey(key); err != nil {
		t.Fatal(err)
	}
	responsesWSWrite(t, ctx, conn, `{"type":"response.create","model":"ws-alias","event_id":"busy","input":[]}`)
	body := responsesWSRead(t, ctx, conn)
	if !bytes.Contains(body, []byte(`"status":429`)) || !bytes.Contains(body, []byte(`"event_id":"busy"`)) {
		t.Fatalf("in-flight limit was reset by an edit: %s", body)
	}
	release <- struct{}{}
	responsesWSRead(t, ctx, conn)
	key.Models = "another-model"
	if err := dbUpdateKey(key); err != nil {
		t.Fatal(err)
	}
	responsesWSWrite(t, ctx, conn, `{"type":"response.create","input":[]}`)
	if body := responsesWSRead(t, ctx, conn); !bytes.Contains(body, []byte(`"status":403`)) {
		t.Fatalf("model permissions were cached for the socket lifetime: %s", body)
	}
	key.Enabled = false
	if err := dbUpdateKey(key); err != nil {
		t.Fatal(err)
	}
	responsesWSWrite(t, ctx, conn, `{"type":"response.create","input":[]}`)
	if body := responsesWSRead(t, ctx, conn); !bytes.Contains(body, []byte(`"status":401`)) {
		t.Fatalf("disabled key retained access: %s", body)
	}
	key.Enabled, key.Models, key.Key = true, "ws-alias", "rotated-key"
	if err := dbUpdateKey(key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE api_keys SET key=? WHERE id=?`, key.Key, key.ID); err != nil {
		t.Fatal(err)
	}
	responsesWSWrite(t, ctx, conn, `{"type":"response.create","input":[]}`)
	if body := responsesWSRead(t, ctx, conn); !bytes.Contains(body, []byte(`"status":401`)) {
		t.Fatalf("rotated key retained access: %s", body)
	}
	if calls.Load() != 1 {
		t.Fatal("a rejected turn reached the upstream")
	}
}

func TestResponsesWSIndependentQuotaAndWarmup(t *testing.T) {
	for _, warmup := range []bool{false, true} {
		t.Run(fmt.Sprintf("warmup=%v", warmup), func(t *testing.T) {
			key, _, gateway := responsesWSTestGateway(t)
			key.ModelQuotas = map[string]ModelQuota{"ws-alias": {QuotaUSD: 0.5}}
			if err := dbUpdateKey(key); err != nil {
				t.Fatal(err)
			}
			fixture := responsesWSFixture(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
				if warmup {
					_, _, _ = conn.Read(ctx)
					responsesWSWrite(t, ctx, conn, responsesWSTerminal("warm", "", 0, 0))
				}
				_, _, _ = conn.Read(ctx)
				responsesWSWrite(t, ctx, conn, responsesWSTerminal("paid", "", 2, 1))
				_, _, _ = conn.Read(ctx)
			})
			responsesWSTestUpstream(t, fixture.URL, "openai", 0)
			ctx, conn := responsesWSDial(t, gateway, key, nil)
			if warmup {
				responsesWSWrite(t, ctx, conn, `{"type":"response.create","model":"ws-alias","generate":false,"input":[]}`)
				responsesWSRead(t, ctx, conn)
				logs, _ := dbListLogs(10, 0)
				if len(logs) != 0 {
					t.Fatal("zero-usage warmup became an inference receipt")
				}
			}
			responsesWSWrite(t, ctx, conn, `{"type":"response.create","model":"ws-alias","input":[]}`)
			responsesWSRead(t, ctx, conn)
			responsesWSWrite(t, ctx, conn, `{"type":"response.create","input":[]}`)
			if body := responsesWSRead(t, ctx, conn); !bytes.Contains(body, []byte(`"status":402`)) {
				t.Fatalf("next turn bypassed depleted quota: %s", body)
			}
			logs, _ := dbListLogs(10, 0)
			updated, _ := dbGetKeyByID(key.ID)
			if len(logs) != 2 || updated.UsedUSD != 3 || updated.SharedUsedUSD != 0 || updated.ModelQuotas["ws-alias"].UsedUSD != 3 {
				t.Fatalf("incorrect quota pool or warmup charge: %+v / %+v", logs, updated)
			}
		})
	}
}

func TestResponsesWSFailsOverOnlyBeforeSendingInput(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprintf("interrupted=%v", interrupted), func(t *testing.T) {
			key, _, gateway := responsesWSTestGateway(t)
			var primaryCalls, backupCalls atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryCalls.Add(1)
				if !interrupted {
					w.WriteHeader(503)
					return
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, _, _ = conn.Read(ctx)
				responsesWSWrite(t, ctx, conn, `{"type":"response.created","response":{"id":"partial"}}`)
				_ = conn.Close(websocket.StatusGoingAway, "upstream restart")
			}))
			t.Cleanup(primary.Close)
			first := responsesWSTestUpstream(t, primary.URL, "openai", 0)
			backup := responsesWSFixture(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
				backupCalls.Add(1)
				_, _, _ = conn.Read(ctx)
				responsesWSWrite(t, ctx, conn, responsesWSTerminal("backup", "", 1, 0))
				_, _, _ = conn.Read(ctx)
			})
			second := responsesWSTestUpstream(t, backup.URL, "openai", 1)
			ctx, conn := responsesWSDial(t, gateway, key, nil)
			responsesWSWrite(t, ctx, conn, `{"type":"response.create","model":"ws-alias","input":[]}`)
			responsesWSRead(t, ctx, conn)
			if interrupted {
				if _, _, err := conn.Read(ctx); websocket.CloseStatus(err) != websocket.StatusInternalError {
					t.Fatalf("mid-turn disconnect was not propagated: %v", err)
				}
				if backupCalls.Load() != 0 {
					t.Fatal("in-flight input was replayed to a different upstream")
				}
			} else {
				logs, _ := dbListLogs(10, 0)
				if primaryCalls.Load() != 1 || backupCalls.Load() != 1 || healthFails(first.ID) != 1 || len(logs) != 1 || logs[0].UpstreamID != second.ID {
					t.Fatalf("handshake failover failed: %+v", logs)
				}
			}
		})
	}
}

func TestResponsesWSDisconnectDrainsLateUsage(t *testing.T) {
	for _, graceful := range []bool{false, true} {
		t.Run(fmt.Sprintf("graceful=%v", graceful), func(t *testing.T) {
			key, _, gateway := responsesWSTestGateway(t)
			release, finished := make(chan struct{}), make(chan struct{})
			fixture := responsesWSFixture(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
				defer close(finished)
				_, _, _ = conn.Read(ctx)
				responsesWSWrite(t, ctx, conn, `{"type":"response.created","response":{"id":"late"}}`)
				select {
				case <-release:
				case <-ctx.Done():
					return
				}
				timer := time.NewTimer(50 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-ctx.Done():
					return
				}
				responsesWSWrite(t, ctx, conn, responsesWSTerminal("late", "", 2, 1))
				_, _, _ = conn.Read(ctx)
			})
			responsesWSTestUpstream(t, fixture.URL, "openai", 0)
			ctx, conn := responsesWSDial(t, gateway, key, nil)
			responsesWSWrite(t, ctx, conn, `{"type":"response.create","model":"ws-alias","input":[]}`)
			responsesWSRead(t, ctx, conn)
			if graceful {
				_ = conn.Close(websocket.StatusNormalClosure, "done")
			} else {
				_ = conn.CloseNow()
			}
			close(release)
			select {
			case <-finished:
			case <-ctx.Done():
				t.Fatal("relay did not drain and close the upstream")
			}
			logs, _ := dbListLogs(10, 0)
			updated, _ := dbGetKeyByID(key.ID)
			if len(logs) != 1 || logs[0].Status != 499 || updated.UsedUSD != 3 || len(getLimit(*key).sem) != 0 {
				t.Fatalf("late usage was lost or billed as a second turn: logs=%+v spend=%v", logs, updated.UsedUSD)
			}
		})
	}
}

func TestResponsesWSErrorThenFailedUsesAuthoritativeUsage(t *testing.T) {
	key, _, gateway := responsesWSTestGateway(t)
	fixture := responsesWSFixture(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		_, _, _ = conn.Read(ctx)
		for _, body := range []string{
			`{"type":"response.created","response":{"id":"failed"}}`,
			`{"type":"error","id":"event_not_a_response_id","response_id":"failed","error":{"type":"server_error","message":"busy"}}`,
			`{"type":"ping"}`,
			`{"type":"response.failed","response":{"id":"failed","status":"failed","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`,
		} {
			responsesWSWrite(t, ctx, conn, body)
		}
		_, _, _ = conn.Read(ctx)
		responsesWSWrite(t, ctx, conn, responsesWSTerminal("next", "", 0, 0))
		_, _, _ = conn.Read(ctx)
	})
	responsesWSTestUpstream(t, fixture.URL, "openai", 0)
	ctx, conn := responsesWSDial(t, gateway, key, nil)
	responsesWSWrite(t, ctx, conn, `{"type":"response.create","model":"ws-alias","input":[]}`)
	for i := 0; i < 4; i++ {
		responsesWSRead(t, ctx, conn)
	}
	responsesWSWrite(t, ctx, conn, `{"type":"response.create","input":[]}`)
	responsesWSRead(t, ctx, conn)
	logs, _ := dbListLogs(10, 0)
	updated, _ := dbGetKeyByID(key.ID)
	failures := 0
	for _, entry := range logs {
		if entry.Status >= 500 {
			failures++
			if entry.PromptTokens != 2 || entry.CompletionTokens != 1 || entry.CostUSD != 3 {
				t.Fatalf("error envelope hid authoritative failed usage: %+v", entry)
			}
		}
	}
	if len(logs) != 2 || failures != 1 || updated.UsedUSD != 3 || len(getLimit(*key).sem) != 0 {
		t.Fatalf("error/failed lifecycle produced duplicate receipts: %+v / %v", logs, updated.UsedUSD)
	}
}

func TestResponsesWSStandaloneErrorReleasesSlot(t *testing.T) {
	key, _, gateway := responsesWSTestGateway(t)
	fixture := responsesWSFixture(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		_, _, _ = conn.Read(ctx)
		responsesWSWrite(t, ctx, conn, `{"type":"response.created","response":{"id":"bare"}}`)
		responsesWSWrite(t, ctx, conn, `{"type":"error","response_id":"bare","status":500,"error":{"type":"server_error"}}`)
		_, _, _ = conn.Read(ctx)
	})
	responsesWSTestUpstream(t, fixture.URL, "openai", 0)
	ctx, conn := responsesWSDial(t, gateway, key, nil)
	responsesWSWrite(t, ctx, conn, `{"type":"response.create","model":"ws-alias","input":[]}`)
	responsesWSRead(t, ctx, conn)
	responsesWSRead(t, ctx, conn)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for getLimit(*key).active.Load() != 0 {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("standalone provider error leaked a concurrency slot")
		}
	}
	logs, _ := dbListLogs(10, 0)
	if len(logs) != 1 || logs[0].Status != 500 {
		t.Fatalf("standalone error never settled: %+v", logs)
	}
}

func TestResponsesWSFlatTerminalPreservesProviderPayload(t *testing.T) {
	key, _, gateway := responsesWSTestGateway(t)
	body := `{"type":"response.completed","id":"flat","status":"completed","opaque":{"id":9007199254740993},"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`
	fixture := responsesWSFixture(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		_, _, _ = conn.Read(ctx)
		responsesWSWrite(t, ctx, conn, body)
		responsesWSWrite(t, ctx, conn, body)
		_, _, _ = conn.Read(ctx)
	})
	responsesWSTestUpstream(t, fixture.URL, "openai", 0)
	ctx, conn := responsesWSDial(t, gateway, key, nil)
	responsesWSWrite(t, ctx, conn, `{"type":"response.create","model":"ws-alias","input":[]}`)
	for i := 0; i < 2; i++ {
		if got := responsesWSRead(t, ctx, conn); string(got) != body {
			t.Fatalf("native flat terminal changed: %s", got)
		}
	}
	logs, _ := dbListLogs(10, 0)
	if len(logs) != 1 || logs[0].PromptTokens != 2 || logs[0].CompletionTokens != 1 || logs[0].CostUSD != 3 {
		t.Fatalf("flat terminal usage was lost or duplicated: %+v", logs)
	}
}

func TestResponsesWSValidationErrorAndOverloadCloseCodes(t *testing.T) {
	for _, validation := range []bool{false, true} {
		t.Run(fmt.Sprintf("validation=%v", validation), func(t *testing.T) {
			key, _, gateway := responsesWSTestGateway(t)
			fixture := responsesWSFixture(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
				_, _, _ = conn.Read(ctx)
				responsesWSWrite(t, ctx, conn, `{"type":"response.created","response":{"id":"closing"}}`)
				code := websocket.StatusTryAgainLater
				if validation {
					responsesWSWrite(t, ctx, conn, `{"type":"error","response_id":"closing","status":400,"error":{"type":"invalid_request_error"}}`)
					code = websocket.StatusNormalClosure
				}
				_ = conn.Close(code, "closed")
			})
			up := responsesWSTestUpstream(t, fixture.URL, "openai", 0)
			ctx, conn := responsesWSDial(t, gateway, key, nil)
			responsesWSWrite(t, ctx, conn, `{"type":"response.create","model":"ws-alias","input":[]}`)
			responsesWSRead(t, ctx, conn)
			expected := websocket.StatusTryAgainLater
			if validation {
				responsesWSRead(t, ctx, conn)
				expected = websocket.StatusNormalClosure
			}
			if _, _, err := conn.Read(ctx); websocket.CloseStatus(err) != expected {
				t.Fatalf("provider close code was lost: expected=%v err=%v", expected, err)
			}
			if validation {
				logs, _ := dbListLogs(10, 0)
				if len(logs) != 1 || logs[0].Status != 400 || healthFails(up.ID) != 0 {
					t.Fatalf("provider validation error counted as an outage: %+v failures=%d", logs, healthFails(up.ID))
				}
			}
		})
	}
}

func TestResponsesWSHandshakeAuthAndIdleConnectionBound(t *testing.T) {
	key, router, gateway := responsesWSTestGateway(t)
	for _, status := range []int{401, 426} {
		req := httptest.NewRequest("GET", "/v1/responses", nil)
		if status == 426 {
			req.Header.Set("Authorization", "Bearer "+key.Key)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != status {
			t.Fatalf("unauthenticated or non-upgrade request: %d %s", rec.Code, rec.Body.String())
		}
	}
	up := responsesWSTestUpstream(t, "http://unused.invalid", "openai", 0)
	t.Setenv("RESPONSES_WS_MAX_CONNECTIONS_PER_KEY", "1")
	ctx, conn := responsesWSDial(t, gateway, key, nil)
	other, response, err := websocket.Dial(ctx, gateway.URL+"/v1/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key.Key}}})
	if other != nil {
		_ = other.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != 429 || len(getLimit(*key).sem) != 0 {
		t.Fatalf("idle connection bound occupied inference or accepted excess sockets: %v / %v", response, err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
	up.OpenAIProtocol = "chat"
	if err := dbUpdateUpstream(up); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer "+key.Key)
	req.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("HTTP-only configuration advertised WS support: %d", rec.Code)
	}
}

func TestResponsesWSUnknownUsageIsNotEstimatedFromIncrementalInput(t *testing.T) {
	key, _, gateway := responsesWSTestGateway(t)
	fixture := responsesWSFixture(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		_, _, _ = conn.Read(ctx)
		responsesWSWrite(t, ctx, conn, `{"type":"response.completed","response":{"id":"unknown","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"large conversation"}]}]}}`)
		_, _, _ = conn.Read(ctx)
	})
	responsesWSTestUpstream(t, fixture.URL, "openai", 0)
	ctx, conn := responsesWSDial(t, gateway, key, nil)
	responsesWSWrite(t, ctx, conn, `{"type":"response.create","model":"ws-alias","previous_response_id":"older","input":"small delta"}`)
	responsesWSRead(t, ctx, conn)
	logs, _ := dbListLogs(10, 0)
	if len(logs) != 1 || logs[0].Billing != nil || logs[0].PromptTokens != 0 || logs[0].CompletionTokens != 0 || logs[0].CostUSD != 0 {
		t.Fatalf("partial transcript was estimated as provider usage: %+v", logs)
	}
}

func TestKeyLimitEditsDoNotResetActiveRequests(t *testing.T) {
	key := ApiKey{ID: -20261023, QPS: 100, Concurrency: 2}
	first := getLimit(key)
	if !first.acquire() || !first.acquire() {
		t.Fatal("initial slots unavailable")
	}
	key.QPS, key.Concurrency = 200, 1
	second := getLimit(key)
	if second.acquire() {
		t.Fatal("edit reset outstanding requests")
	}
	first.release()
	if second.acquire() {
		t.Fatal("lower limit ignored the remaining active request")
	}
	first.release()
	if !second.acquire() {
		t.Fatal("old limiter leaked its active count")
	}
	second.release()
	if second.active.Load() != 0 {
		t.Fatal("active count was not released")
	}
	limReg.Lock()
	delete(limReg.m, key.ID)
	limReg.Unlock()
}

func TestResponsesWSCoreScopesSessionsAndRetainsPerTurnReceipts(t *testing.T) {
	key, _, gateway := responsesWSTestGateway(t)
	other := &ApiKey{Name: "other", Key: "sk-other-client", Models: "ws-alias", Enabled: true, QPS: 100, Concurrency: 4}
	if err := dbInsertKey(other); err != nil {
		t.Fatal(err)
	}
	captured := make(chan [2]string, 2)
	fixture := responsesWSFixture(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		session, trace := r.Header.Get("X-Session-Id"), r.Header.Get("X-Apishare-Trace")
		captured <- [2]string{session, trace}
		if !strings.HasPrefix(session, "apishare-") || len(trace) != 32 || r.Header.Get("Authorization") != "Bearer sk-provider" {
			t.Errorf("core scope or credential missing: %v", r.Header)
		}
		for i := 1; i <= 2; i++ {
			_, _, err := conn.Read(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			id := fmt.Sprintf("%s-%d", trace, i)
			responsesWSWrite(t, ctx, conn, fmt.Sprintf(`{"type":"response.created","response":{"id":%q}}`, id))
			responsesWSWrite(t, ctx, conn, fmt.Sprintf(`{"type":"response.completed","opaque":{"id":9007199254740993},"response":{"id":%q,"status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`, id))
		}
		_, _, _ = conn.Read(ctx)
	})
	responsesWSTestUpstream(t, fixture.URL, "cliproxy", 0)
	for _, client := range []*ApiKey{key, other} {
		ctx, conn := responsesWSDial(t, gateway, client, http.Header{"X-Session-Id": {"same-client-session"}})
		for i := 0; i < 2; i++ {
			responsesWSWrite(t, ctx, conn, `{"type":"response.create","model":"ws-alias","stream_id":"a","input":[]}`)
			responsesWSRead(t, ctx, conn)
			body := responsesWSRead(t, ctx, conn)
			var event responsesWSEvent
			if json.Unmarshal(body, &event) != nil || event.StreamID == nil || *event.StreamID != "a" || !bytes.Contains(body, []byte(`9007199254740993`)) {
				t.Fatalf("core envelope restoration changed provider state: %s", body)
			}
		}
		_ = conn.Close(websocket.StatusNormalClosure, "done")
	}
	first, second := <-captured, <-captured
	if first[0] == second[0] || first[1] == second[1] {
		t.Fatal("different gateway keys shared an upstream session or trace")
	}
	logs, err := dbListLogs(10, 0)
	traces := make(map[string]int)
	for _, log := range logs {
		traces[log.TraceID]++
	}
	if err != nil || len(logs) != 4 || traces[first[1]] != 2 || traces[second[1]] != 2 {
		t.Fatalf("connection trace collapsed distinct billed turns: %+v / %v", logs, err)
	}
}

func TestResponsesWSControlEventsAndAttributedErrors(t *testing.T) {
	key, _, gateway := responsesWSTestGateway(t)
	fixture := responsesWSFixture(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		_, _, _ = conn.Read(ctx)
		_, _, _ = conn.Read(ctx)
		responsesWSWrite(t, ctx, conn, `{"type":"error","status":400,"error":{"type":"invalid_request_error","event_id":"bad","message":"invalid input"}}`)
		responsesWSWrite(t, ctx, conn, `{"type":"response.created","stream_id":"b","response":{"id":"good"}}`)
		_, body, err := conn.Read(ctx)
		if err != nil || !bytes.Contains(body, []byte(`"type":"response.interrupt"`)) || !bytes.Contains(body, []byte(`"response_id":"good"`)) {
			t.Errorf("interrupt was not forwarded: %s / %v", body, err)
			return
		}
		responsesWSWrite(t, ctx, conn, `{"type":"response.incomplete","stream_id":"b","response":{"id":"good","status":"incomplete","usage":{"input_tokens":1,"output_tokens":0}}}`)
		_, _, _ = conn.Read(ctx)
	})
	responsesWSTestUpstream(t, fixture.URL, "openai", 0)
	ctx, conn := responsesWSDial(t, gateway, key, nil)
	for _, body := range []string{`{"type":"response.steer"}`, `{broken`} {
		responsesWSWrite(t, ctx, conn, body)
		if event := responsesWSRead(t, ctx, conn); !bytes.Contains(event, []byte(`"status":400`)) {
			t.Fatalf("unsupported or invalid event was accepted: %s", event)
		}
	}
	responsesWSWrite(t, ctx, conn, `{"type":"response.create","model":"ws-alias","stream_id":"a","event_id":"bad","input":[]}`)
	responsesWSWrite(t, ctx, conn, `{"type":"response.create","model":"ws-alias","stream_id":"b","event_id":"ok","input":[]}`)
	responsesWSRead(t, ctx, conn)
	responsesWSRead(t, ctx, conn)
	responsesWSWrite(t, ctx, conn, `{"type":"response.interrupt","response_id":"good","mode":"suspend"}`)
	responsesWSRead(t, ctx, conn)
	logs, err := dbListLogs(10, 0)
	if err != nil || len(logs) != 2 || len(getLimit(*key).sem) != 0 {
		t.Fatalf("error/cancellation leaked a turn: %+v / %v", logs, err)
	}
}

func TestResponsesWSKeepaliveAndConnectionRollover(t *testing.T) {
	key, _, gateway := responsesWSTestGateway(t)
	t.Setenv("RESPONSES_WS_PING_INTERVAL_SEC", "1")
	t.Setenv("RESPONSES_WS_MAX_DURATION_SEC", "3")
	var upstreamPings, downstreamPings atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OnPingReceived: func(context.Context, []byte) bool {
			upstreamPings.Add(1)
			return true
		}})
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_, _, _ = conn.Read(ctx)
		responsesWSWrite(t, ctx, conn, `{"type":"response.created","response":{"id":"waiting"}}`)
		_, _, _ = conn.Read(ctx)
	}))
	t.Cleanup(fixture.Close)
	responsesWSTestUpstream(t, fixture.URL, "openai", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, gateway.URL+"/v1/responses", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + key.Key}},
		OnPingReceived: func(context.Context, []byte) bool {
			downstreamPings.Add(1)
			return true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	responsesWSWrite(t, ctx, conn, `{"type":"response.create","model":"ws-alias","input":[]}`)
	responsesWSRead(t, ctx, conn)
	if _, _, err := conn.Read(ctx); websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("connection lifetime was not bounded: %v", err)
	}
	if upstreamPings.Load() == 0 || downstreamPings.Load() == 0 {
		t.Fatalf("quiet model turn lost ping/pong: upstream=%d downstream=%d", upstreamPings.Load(), downstreamPings.Load())
	}
}

func TestResponsesWSUsesConfiguredProxyAndRejectsStaleUpstream(t *testing.T) {
	key, _, gateway := responsesWSTestGateway(t)
	fixture := responsesWSFixture(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		_, _, _ = conn.Read(ctx)
		responsesWSWrite(t, ctx, conn, responsesWSTerminal("proxy", "", 1, 0))
		if _, _, err := conn.Read(ctx); err == nil {
			t.Error("disabled upstream accepted another turn")
		}
	})
	target, _ := url.Parse(fixture.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var proxied atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		if r.Header.Get("Proxy-Authorization") == "" {
			t.Error("proxy credentials were not used")
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(proxyServer.Close)
	up := responsesWSTestUpstream(t, fixture.URL, "openai", 0)
	up.ProxyID = testSavedProxy(t, proxyServer.URL, "proxy-user", "proxy-password").ID
	if err := dbUpdateUpstream(up); err != nil {
		t.Fatal(err)
	}
	ctx, conn := responsesWSDial(t, gateway, key, nil)
	responsesWSWrite(t, ctx, conn, `{"type":"response.create","model":"ws-alias","input":[]}`)
	responsesWSRead(t, ctx, conn)
	up.Enabled = false
	if err := dbUpdateUpstream(up); err != nil {
		t.Fatal(err)
	}
	responsesWSWrite(t, ctx, conn, `{"type":"response.create","input":[]}`)
	if body := responsesWSRead(t, ctx, conn); !bytes.Contains(body, []byte(`"status":503`)) || proxied.Load() != 1 {
		t.Fatalf("proxy was bypassed or upstream edit ignored: %s / %d", body, proxied.Load())
	}
}
