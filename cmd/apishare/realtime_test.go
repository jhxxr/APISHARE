package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

func realtimeTestGateway(t *testing.T) (*ApiKey, *gin.Engine) {
	t.Helper()
	modelsTestDB(t)
	ak := &ApiKey{Name: "audio", Key: "sk-audio-test", Enabled: true, Models: "voice-alias", QPS: 100, Concurrency: 1}
	if err := dbInsertKey(ak); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	registerAudioRoutes(router.Group("/v1", corsV1(), keyAuth(), keyRateLimit()))
	return ak, router
}

func realtimeTestUpstream(t *testing.T, endpoint string) *Upstream {
	t.Helper()
	up := &Upstream{Name: "voice", Type: "openai", OpenAIProtocol: "responses", BaseURL: endpoint + "/v1", APIKey: "sk-upstream-secret", Enabled: true, Weight: 1, Models: "voice-alias", ModelMap: `{"voice-alias":"gpt-realtime"}`, HeaderOverrides: `{"*":true}`}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	return up
}

func TestRealtimeWebSocketRoundTripBillingAndConcurrency(t *testing.T) {
	ak, router := realtimeTestGateway(t)
	upstreamDone := make(chan struct{})
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		if r.URL.Path != "/v1/realtime" || r.URL.Query().Get("model") != "gpt-realtime" || r.Header.Get("Authorization") != "Bearer sk-upstream-secret" || strings.Contains(r.URL.RawQuery, ak.Key) || strings.Contains(r.Header.Get("Sec-WebSocket-Protocol"), ak.Key) || r.Header.Get("OpenAI-Beta") != "realtime=v1" {
			t.Errorf("invalid upstream handshake: %s headers=%v", r.URL, r.Header)
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		kind, body, err := conn.Read(ctx)
		if err != nil || kind != websocket.MessageText || !bytes.Contains(body, []byte(`"model":"gpt-realtime"`)) || !bytes.Contains(body, []byte(`9007199254740993`)) {
			t.Errorf("session mapping corrupted payload: %s / %v", body, err)
			return
		}
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.updated"}`))
		kind, body, err = conn.Read(ctx)
		if err != nil || kind != websocket.MessageBinary || !bytes.Equal(body, []byte{0, 1, 128, 255}) {
			t.Errorf("binary audio changed: %v / %v", body, err)
			return
		}
		_ = conn.Write(ctx, kind, body)
		for _, id := range []string{"resp-1", "resp-1", "resp-2"} {
			_ = conn.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.done","response":{"id":%q,"status":"completed","usage":{"input_tokens":100,"output_tokens":30,"input_token_details":{"audio_tokens":60,"cached_tokens":20,"cached_tokens_details":{"audio_tokens":10}},"output_token_details":{"audio_tokens":20}}}}`, id)))
		}
		_, _, _ = conn.Read(ctx) // Drain the client's close handshake.
	}))
	defer fixture.Close()
	realtimeTestUpstream(t, fixture.URL)
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		router.ServeHTTP(w, r)
		if r.URL.Path == "/v1/realtime" && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			close(finished)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, server.URL+"/v1/realtime?model=voice-alias", &websocket.DialOptions{
		Subprotocols: []string{"realtime", "openai-insecure-api-key." + ak.Key},
		HTTPHeader:   http.Header{"Openai-Beta": []string{"realtime=v1"}},
	})
	if err != nil {
		t.Fatalf("connect: %v / %v", err, resp)
	}
	defer conn.CloseNow()
	if conn.Subprotocol() != "realtime" {
		t.Fatalf("credential subprotocol echoed: %s", conn.Subprotocol())
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.update","session":{"model":"voice-alias","metadata":{"id":9007199254740993}}}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageBinary, []byte{0, 1, 128, 255}); err != nil {
		t.Fatal(err)
	}
	kind, body, err := conn.Read(ctx)
	if err != nil || kind != websocket.MessageBinary || !bytes.Equal(body, []byte{0, 1, 128, 255}) {
		t.Fatalf("binary response changed: %v / %v", body, err)
	}
	for i := 0; i < 3; i++ {
		if _, _, err := conn.Read(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// A live session occupies the existing key concurrency slot.
	req, _ := http.NewRequest("POST", server.URL+"/v1/audio/speech", strings.NewReader(`{"model":"voice-alias","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+ak.Key)
	req.Header.Set("Content-Type", "application/json")
	busy, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	busy.Body.Close()
	if busy.StatusCode != 429 {
		t.Fatalf("live session released concurrency early: %d", busy.StatusCode)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("gateway pumps did not stop")
	}
	select {
	case <-upstreamDone:
	case <-ctx.Done():
		t.Fatal("upstream connection leaked")
	}
	logs, err := dbListLogs(10, 0)
	if err != nil || len(logs) != 2 {
		t.Fatalf("terminal response billed more than once or normal close logged as failure: %+v / %v", logs, err)
	}
	for _, entry := range logs {
		if entry.Status != 200 || entry.PromptTokens != 100 || entry.CompletionTokens != 30 || entry.Billing.Usage.InputAudio != 50 || entry.Billing.Usage.CacheReadAudio != 10 || entry.Billing.Usage.OutputAudio != 20 {
			t.Fatalf("incorrect audio receipt: %+v", entry)
		}
	}
	if len(getLimit(*ak).sem) != 0 {
		t.Fatal("key concurrency slot leaked")
	}
}

func TestRealtimeBrowserCredentialAndModelPermissions(t *testing.T) {
	ak, router := realtimeTestGateway(t)
	for _, sample := range []struct {
		path, protocol string
		status         int
	}{
		{"/v1/realtime?model=voice-alias", "realtime", 401},
		{"/v1/realtime?model=forbidden", "openai-insecure-api-key." + ak.Key, 403},
		{"/v1/realtime", "openai-insecure-api-key." + ak.Key, 400},
	} {
		req := httptest.NewRequest("GET", sample.path, nil)
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Protocol", sample.protocol)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != sample.status {
			t.Fatalf("%s: %d %s", sample.path, rec.Code, rec.Body.String())
		}
	}
	for _, body := range []string{
		`{"type":"session.update","session":{"model":"forbidden"}}`,
		`{"type":"response.create","response":{"model":"another-model"}}`,
		`{"type":"session.update","session":{"model":null}}`,
	} {
		if _, err := mapRealtimeClientEvent([]byte(body), "voice-alias", "gpt-realtime"); err == nil {
			t.Fatalf("model switch accepted: %s", body)
		}
	}
	untouched := `{"type":"input_audio_buffer.append","audio":"AAEC","metadata":{"model":"do not edit"}}`
	mapped, err := mapRealtimeClientEvent([]byte(untouched), "voice-alias", "gpt-realtime")
	if err != nil || string(mapped) != untouched {
		t.Fatal("opaque audio event changed")
	}
}

func TestRealtimeQuotaExhaustionClosesAfterBilledTurn(t *testing.T) {
	ak, router := realtimeTestGateway(t)
	ak.QuotaUSD = .000001
	if err := dbUpdateKey(ak); err != nil {
		t.Fatal(err)
	}
	settingsMu.Lock()
	old := settingsCache
	settingsCache.Pricing = map[string]Price{"voice-alias": {In: 1000000, Out: 1000000}}
	settingsMu.Unlock()
	t.Cleanup(func() { settingsMu.Lock(); settingsCache = old; settingsMu.Unlock() })
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_, _, _ = conn.Read(ctx)
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.done","response":{"id":"paid","usage":{"input_tokens":1,"output_tokens":1}}}`))
		_, _, _ = conn.Read(ctx)
	}))
	defer fixture.Close()
	realtimeTestUpstream(t, fixture.URL)
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { router.ServeHTTP(w, r); close(finished) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, server.URL+"/v1/realtime?model=voice-alias", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + ak.Key}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create"}`))
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("quota did not close session: %v", err)
	}
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("session did not exit")
	}
	updated, _ := dbGetKeyByID(ak.ID)
	if updated.UsedUSD != 2 {
		t.Fatalf("turn not charged before quota check: %+v", updated)
	}
}

func TestRealtimeUsageAudioRates(t *testing.T) {
	body := []byte(`{"type":"response.done","response":{"id":"r","usage":{"input_tokens":100,"output_tokens":30,"input_token_details":{"audio_tokens":60,"cached_tokens":20,"cached_tokens_details":{"audio_tokens":10}},"output_token_details":{"audio_tokens":20}}}}`)
	_, u, ok := realtimeEventUsage(body)
	if !ok {
		t.Fatal("usage missing")
	}
	counts := normalizeBillingUsage(u)
	if counts.Input != 30 || counts.CacheRead != 10 || counts.InputAudio != 50 || counts.CacheReadAudio != 10 || counts.Output != 10 || counts.OutputAudio != 20 || counts.inputTotal() != 100 || counts.outputTotal() != 30 {
		t.Fatalf("inclusive audio/cache counters double counted: %+v", counts)
	}
	audioIn, audioOut, cachedAudio, cachedText := 40.0, 80.0, 2.5, .5
	price := Price{In: 5, Out: 20, InAudio: &audioIn, OutAudio: &audioOut, CacheReadAudio: &cachedAudio, CacheRead: &cachedText}
	cost := costForUsage(counts, price)
	if math.Abs(cost.Total-(30*5+10*.5+50*40+10*2.5+10*20+20*80)/1e6) > 1e-12 {
		t.Fatalf("audio cost incorrect: %+v", cost)
	}
	var roundTrip Price
	wire, _ := json.Marshal(price)
	if err := json.Unmarshal(wire, &roundTrip); err != nil || roundTrip.InAudio == nil || *roundTrip.InAudio != 40 {
		t.Fatalf("audio rates not persisted: %s / %v", wire, err)
	}
}

func TestRealtimeHandshakeFailoverThroughSOCKSProxy(t *testing.T) {
	ak, router := realtimeTestGateway(t)
	var rejected atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { rejected.Add(1); w.WriteHeader(503) }))
	defer bad.Close()
	badUp := realtimeTestUpstream(t, bad.URL)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.created"}`))
		_, _, _ = conn.Read(ctx)
	}))
	defer good.Close()
	observed := make(chan string, 1)
	proxyURL := testSOCKSProxy(t, strings.TrimPrefix(good.URL, "http://"), observed)
	proxy := testSavedProxy(t, "socks5://"+proxyURL, "alice", "secret")
	goodUp := &Upstream{Name: "proxy voice", Type: "openai", BaseURL: "http://voice.proxy-only.invalid:8080/v1", APIKey: "secret", Models: "voice-alias", Weight: 1, Enabled: true, ProxyID: proxy.ID}
	if err := dbInsertUpstream(goodUp); err != nil {
		t.Fatal(err)
	}
	settingsMu.Lock()
	previous := settingsCache
	settingsCache.Retry = 1
	settingsMu.Unlock()
	t.Cleanup(func() { settingsMu.Lock(); settingsCache = previous; settingsMu.Unlock() })
	header := http.Header{"Authorization": []string{"Bearer " + ak.Key}, "X-Session-Id": []string{"failover-voice"}}
	skey := sessionKeyFromCanon("/v1/realtime", header, &canonReq{Model: fmt.Sprint(ak.ID) + ":voice-alias"})
	affinitySet(skey, badUp.ID)
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { router.ServeHTTP(w, r); close(finished) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, server.URL+"/v1/realtime?model=voice-alias", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("proxy connection did not exit")
	}
	select {
	case host := <-observed:
		if host != "voice.proxy-only.invalid:8080" {
			t.Fatalf("proxy DNS/target changed: %s", host)
		}
	case <-ctx.Done():
		t.Fatal("proxy bypassed")
	}
	if rejected.Load() != 1 || healthFails(badUp.ID) != 1 {
		t.Fatal("handshake did not retry the failed upstream exactly once")
	}
	if id, ok := affinityGet(skey); !ok || id != goodUp.ID {
		t.Fatal("successful failover did not update affinity")
	}
}

func TestRealtimeUpstreamBadRequestPreserved(t *testing.T) {
	ak, router := realtimeTestGateway(t)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"message":"invalid realtime model"}}`)
	}))
	defer fixture.Close()
	realtimeTestUpstream(t, fixture.URL)
	req := httptest.NewRequest("GET", "/v1/realtime?model=voice-alias", nil)
	req.Header.Set("Authorization", "Bearer "+ak.Key)
	req.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid realtime model") || strings.Contains(rec.Body.String(), "sk-upstream") {
		t.Fatalf("handshake error changed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRealtimeAutomaticAudioPricing(t *testing.T) {
	modelsTestDB(t)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"openai/voice":{"input_cost_per_token":0.000005,"output_cost_per_token":0.00002,"input_cost_per_audio_token":0.00004,"output_cost_per_audio_token":0.00008,"cache_read_input_audio_token_cost":0.0000025}}`)
	}))
	defer fixture.Close()
	settingsMu.Lock()
	previous := settingsCache
	settingsCache.AutoPricingURL = fixture.URL
	settingsMu.Unlock()
	autoPricing.Lock()
	oldData, oldAt := autoPricing.data, autoPricing.fetchedAt
	autoPricing.Unlock()
	t.Cleanup(func() {
		settingsMu.Lock()
		settingsCache = previous
		settingsMu.Unlock()
		autoPricing.Lock()
		autoPricing.data, autoPricing.fetchedAt = oldData, oldAt
		autoPricing.Unlock()
	})
	if _, err := refreshAutoPricing(); err != nil {
		t.Fatal(err)
	}
	autoPricing.RLock()
	price := autoPricing.data["openai/voice"]
	autoPricing.RUnlock()
	if price.InAudio == nil || *price.InAudio != 40 || price.OutAudio == nil || *price.OutAudio != 80 || price.CacheReadAudio == nil || *price.CacheReadAudio != 2.5 {
		t.Fatalf("audio price source fields lost: %+v", price)
	}
}

func TestAudioMalformedModelFieldsCannotBypassPermissions(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	writer.WriteField("model", "voice-alias")
	writer.WriteField("model", "forbidden")
	writer.Close()
	if _, err := parseAudioRequest("/v1/audio/transcriptions", writer.FormDataContentType(), "", body.Bytes()); err == nil {
		t.Fatal("duplicate multipart model accepted")
	}
	if _, err := parseAudioRequest("/v1/audio/speech", "application/json", "forbidden", []byte(`{"model":"voice-alias"}`)); err == nil {
		t.Fatal("conflicting model query accepted")
	}
	cleaned, err := url.ParseQuery(audioQuery("model=a&key=private&access_token=private&api_key=private&intent=transcription", "mapped"))
	if err != nil || cleaned.Get("model") != "mapped" || cleaned.Get("intent") != "transcription" || strings.Contains(cleaned.Encode(), "private") {
		t.Fatalf("credential query reached upstream: %v", cleaned)
	}
}

func TestAudioHTTPMultipartSDPAndBinary(t *testing.T) {
	for _, path := range []string{"/v1/audio/transcriptions", "/v1/audio/translations", "/v1/audio/speech", "/v1/realtime/calls", "/v1/realtime"} {
		t.Run(path, func(t *testing.T) {
			ak, router := realtimeTestGateway(t)
			var calls atomic.Int32
			fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != path || r.Header.Get("Authorization") != "Bearer sk-upstream-secret" {
					t.Errorf("invalid HTTP audio request: %s", r.URL)
				}
				if path == "/v1/audio/speech" {
					body, _ := io.ReadAll(r.Body)
					if !bytes.Contains(body, []byte(`"model":"gpt-realtime"`)) {
						t.Errorf("speech model not mapped: %s", body)
					}
					w.Header().Set("Content-Type", "audio/mpeg")
					w.Write([]byte{0, 255, 1, 128})
					return
				}
				if path == "/v1/realtime" {
					body, _ := io.ReadAll(r.Body)
					if string(body) != "v=0\r\ns=offer\r\n" || r.URL.Query().Get("model") != "gpt-realtime" {
						t.Errorf("SDP corrupted: %s %s", r.URL, body)
					}
				} else {
					if err := r.ParseMultipartForm(maxBodyBytes); err != nil {
						t.Error(err)
						return
					}
					defer r.MultipartForm.RemoveAll()
					if path == "/v1/realtime/calls" {
						if !strings.Contains(r.FormValue("session"), `"model":"gpt-realtime"`) || r.FormValue("sdp") != "v=0\r\ns=offer\r\n" {
							t.Error("WebRTC session or SDP changed")
						}
					} else {
						if r.FormValue("model") != "gpt-realtime" || r.FormValue("language") != "zh" {
							t.Error("transcription fields lost")
						}
						file, header, err := r.FormFile("file")
						if err != nil {
							t.Error(err)
							return
						}
						defer file.Close()
						body, _ := io.ReadAll(file)
						if header.Filename != "voice.wav" || !bytes.Equal(body, []byte{0, 255, 1, 128}) {
							t.Error("uploaded audio bytes or filename changed")
						}
					}
				}
				if strings.HasPrefix(path, "/v1/realtime") {
					w.Header().Set("Content-Type", "application/sdp")
					w.WriteHeader(201)
					fmt.Fprint(w, "v=0\r\ns=answer\r\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"text":"你好","usage":{"input_tokens":10,"output_tokens":2,"input_token_details":{"audio_tokens":8}}}`)
				}
			}))
			defer fixture.Close()
			realtimeTestUpstream(t, fixture.URL)
			var body bytes.Buffer
			contentType := "multipart/form-data"
			query := ""
			switch path {
			case "/v1/audio/speech":
				contentType = "application/json"
				body.WriteString(`{"model":"voice-alias","input":"你好","voice":"alloy"}`)
			case "/v1/realtime":
				contentType = "application/sdp"
				body.WriteString("v=0\r\ns=offer\r\n")
				query = "?model=voice-alias"
			default:
				writer := multipart.NewWriter(&body)
				if path == "/v1/realtime/calls" {
					writer.WriteField("session", `{"type":"realtime","model":"voice-alias","instructions":"hello"}`)
					writer.WriteField("sdp", "v=0\r\ns=offer\r\n")
				} else {
					writer.WriteField("model", "voice-alias")
					writer.WriteField("language", "zh")
					file, _ := writer.CreateFormFile("file", "voice.wav")
					file.Write([]byte{0, 255, 1, 128})
				}
				writer.Close()
				contentType = writer.FormDataContentType()
			}
			req := httptest.NewRequest("POST", path+query, &body)
			req.Header.Set("Authorization", "Bearer "+ak.Key)
			req.Header.Set("Content-Type", contentType)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			want := 200
			if strings.HasPrefix(path, "/v1/realtime") {
				want = 201
			}
			if rec.Code != want || calls.Load() != 1 {
				t.Fatalf("HTTP audio failed: %d %s", rec.Code, rec.Body.String())
			}
			if path == "/v1/audio/speech" && !bytes.Equal(rec.Body.Bytes(), []byte{0, 255, 1, 128}) {
				t.Fatal("speech binary changed")
			}
			logs, err := dbListLogs(10, 0)
			if err != nil || len(logs) != 1 {
				t.Fatalf("missing audio log: %+v / %v", logs, err)
			}
			if strings.HasPrefix(path, "/v1/realtime") && (logs[0].Billing != nil || logs[0].PromptTokens != 0) {
				t.Fatal("SDP was billed as audio/text")
			}
			if strings.Contains(path, "transcription") && (logs[0].PromptTokens != 10 || logs[0].Billing.Usage.InputAudio != 8) {
				t.Fatalf("transcription usage lost: %+v", logs[0])
			}
		})
	}
}
