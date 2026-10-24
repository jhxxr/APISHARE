package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

func TestUpstreamKeysAdminCRUDAndProxyReferences(t *testing.T) {
	router := testProxyAdminRouter(t)
	proxy := testSavedProxy(t, "http://127.0.0.1:1080", "", "")
	request := func(method, path, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		result := proxyAdminRequest(router, method, path, body, true)
		if result.Code != status {
			t.Fatalf("%s %s: %d %s", method, path, result.Code, result.Body.String())
		}
		return result
	}
	body := fmt.Sprintf(`{"name":"supplier","type":"openai","base_url":"https://example.invalid","key_proxy_mode":"individual","api_keys":[
		{"id":"a","name":"first","api_key":"secret-first-key-123","proxy_id":%d},
		{"id":"b","name":"second","api_key":"secret-second-key-456","proxy_id":0}]}`, proxy.ID)
	created := request("POST", "upstreams", body, 200)
	var result struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(created.Body.Bytes(), &result)
	path := fmt.Sprintf("upstreams/%d", result.ID)
	up, err := dbGetUpstream(result.ID)
	if err != nil || len(up.APIKeys) != 2 || up.APIKey != "secret-first-key-123" || up.KeyProxyMode != "individual" {
		t.Fatalf("credentials not persisted: %+v / %v", up, err)
	}
	list := request("GET", "upstreams", "", 200).Body.String()
	if strings.Contains(list, "secret-first-key-123") || strings.Contains(list, "secret-second-key-456") || !strings.Contains(list, `"key_masked"`) {
		t.Fatal("upstream list leaked credentials or lost masked metadata")
	}
	proxies := request("GET", "proxies", "", 200).Body.String()
	if !strings.Contains(proxies, `"upstream_count":1`) {
		t.Fatalf("per-key proxy not counted: %s", proxies)
	}
	request("DELETE", fmt.Sprintf("proxies/%d", proxy.ID), "", 409)
	request("PATCH", path, `{"api_keys":[{"id":"b","name":"second renamed","api_key":"","disabled":true},{"id":"a","api_key":""}]}`, 200)
	up, _ = dbGetUpstream(result.ID)
	if up.APIKeys[0].APIKey != "secret-second-key-456" || !up.APIKeys[0].Disabled || up.APIKeys[1].APIKey != "secret-first-key-123" {
		t.Fatal("blank masked edits failed to retain secrets by ID")
	}
	request("PATCH", path, `{"api_keys":[{"id":"unknown","api_key":""}]}`, 400)
	request("PATCH", path, `{"api_keys":[{"id":"a","api_key":"same"},{"id":"a","api_key":"other"}]}`, 400)
	request("PATCH", path, `{"api_keys":[{"api_key":"same"},{"api_key":"same"}]}`, 400)
	request("PATCH", path, `{"key_proxy_mode":"random"}`, 400)
	request("PATCH", path, `{"api_keys":[{"api_key":"new","proxy_id":99999}]}`, 400)
	request("PATCH", path, `{"api_keys":[{"api_key":"new","proxy_id":-1}]}`, 400)
	request("POST", path+"/duplicate", `{}`, 200)
	all, _ := dbListUpstreams()
	if len(all) != 2 || len(all[1].APIKeys) != 2 || all[1].APIKeys[0].APIKey != up.APIKeys[0].APIKey {
		t.Fatal("duplicate lost supplier credentials")
	}
	request("PATCH", path, `{"api_keys":[]}`, 200)
	up, _ = dbGetUpstream(result.ID)
	if len(up.APIKeys) != 0 || up.APIKey != "secret-second-key-456" {
		t.Fatal("returning to a single key lost the first stored credential")
	}
}

func TestUpstreamKeysLegacyPromotionAndMigration(t *testing.T) {
	router := testProxyAdminRouter(t)
	up := &Upstream{Type: "openai", APIKey: "legacy-secret", BaseURL: "https://example.invalid", Enabled: true, Weight: 1}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	w := proxyAdminRequest(router, "PATCH", fmt.Sprintf("upstreams/%d", up.ID),
		`{"api_keys":[{"id":"legacy","api_key":""},{"id":"new","api_key":"new-secret"}]}`, true)
	if w.Code != 200 {
		t.Fatalf("promotion failed: %d %s", w.Code, w.Body.String())
	}
	stored, _ := dbGetUpstream(up.ID)
	if stored.APIKeys[0].APIKey != "legacy-secret" {
		t.Fatal("promotion replaced the saved single credential")
	}
	// Reapply migrations, as happens on each application startup.
	if err := migrate(); err != nil {
		t.Fatal(err)
	}
	stored, _ = dbGetUpstream(up.ID)
	if len(stored.APIKeys) != 2 {
		t.Fatal("migration erased pooled credentials")
	}
}

func TestUpstreamKeysStickyRelayAndIndependentCooldown(t *testing.T) {
	for _, format := range protocolFormats {
		t.Run(string(format), func(t *testing.T) {
			modelsTestDB(t)
			previous := getSettings()
			settingsMu.Lock()
			settingsCache.Retry = 1
			settingsMu.Unlock()
			t.Cleanup(func() { settingsMu.Lock(); settingsCache = previous; settingsMu.Unlock() })
			var mu sync.Mutex
			calls := map[string]int{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				if format == FormatAnthropic {
					secret = r.Header.Get("x-api-key")
				} else if format == FormatGemini {
					secret = r.Header.Get("x-goog-api-key")
				}
				mu.Lock()
				calls[secret]++
				mu.Unlock()
				if secret == "first-secret" {
					w.Header().Set("Retry-After", "120")
					http.Error(w, "rate limited", http.StatusTooManyRequests)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, fixtureSSE(format))
			}))
			defer server.Close()
			up := fixtureUpstream(format, server.URL)
			up.Enabled, up.Weight, up.Models = true, 1, "alias"
			up.APIKeys = []UpstreamCredential{{ID: "a", APIKey: "first-secret"}, {ID: "b", APIKey: "second-secret"}}
			if err := dbInsertUpstream(up); err != nil {
				t.Fatal(err)
			}
			key := ApiKey{Name: "member", Models: "alias", Enabled: true, Key: "gateway-key"}
			if err := dbInsertKey(&key); err != nil {
				t.Fatal(err)
			}
			skey := scopedSessionKey(key.ID, "sid:sticky-pool")
			affinitySet(skey, up.ID)
			keyAffinitySet(skey, up.withAPIKey(up.APIKeys[0]))
			task := fixtureTask(t, format, true)
			router := gin.New()
			path := "/relay"
			route := path
			if format == FormatGemini {
				path, route = task.endpoint, "/v1beta/models/*action"
			}
			router.POST(route, func(c *gin.Context) { c.Set("apiKey", key) }, relayHandler(format, false))
			for range 3 {
				req := httptest.NewRequest("POST", path, strings.NewReader(string(task.body)))
				req.Header.Set("X-Session-Id", "sticky-pool")
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				if w.Code != 200 || w.Body.String() != fixtureSSE(format) {
					t.Fatalf("relay failed: %d %s", w.Code, w.Body.String())
				}
			}
			mu.Lock()
			first, second := calls["first-secret"], calls["second-secret"]
			mu.Unlock()
			if first != 1 || second != 3 {
				t.Fatalf("sticky key failover was not stable: %d / %d", first, second)
			}
			failed, good := up.withAPIKey(up.APIKeys[0]), up.withAPIKey(up.APIKeys[1])
			if failed.availableForRelay("alias") || !good.availableForRelay("alias") || !up.availableForRelay("alias") {
				t.Fatal("one cooling key affected other credentials")
			}
			if time.Until(failed.keyHealth().until) < 110*time.Second || healthFails(up.ID) != 0 {
				t.Fatal("key Retry-After was ignored or supplier was penalized")
			}
			if got := keyAffinityGet(skey, up.ID); got != "b" {
				t.Fatalf("session did not rebind to the new key: %s", got)
			}
			good.markRelayFailure(401, time.Time{})
			if up.availableForRelay("alias") {
				t.Fatal("supplier with all credentials cooling remained available")
			}
			backup := fixtureUpstream(format, server.URL)
			backup.Priority, backup.Weight, backup.Enabled, backup.Models = 1, 1, true, "alias"
			if err := dbInsertUpstream(backup); err != nil {
				t.Fatal(err)
			}
			selected, err := pickUpstream(format, "alias", nil, false)
			if err != nil || selected.ID != backup.ID {
				t.Fatal("all cooling credentials did not fail over to another supplier")
			}
			if err := resetUpstreamCooldown(context.Background(), up.ID, up); err != nil || !up.availableForRelay("alias") {
				t.Fatal("manual cooldown recovery failed")
			}
		})
	}
}

func TestUpstreamKeysResponsesWSKeepsSelectedCredentialAcrossTurns(t *testing.T) {
	key, _, gateway := responsesWSTestGateway(t)
	fixture := responsesWSFixture(t, func(ctx context.Context, conn *websocket.Conn, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer secret-b" {
			t.Errorf("WebSocket used the wrong credential")
		}
		for i := 1; i <= 3; i++ {
			if _, _, err := conn.Read(ctx); err != nil {
				t.Error(err)
				return
			}
			if i == 3 {
				responsesWSWrite(t, ctx, conn, `{"type":"error","status":429,"error":{"code":"rate_limit_exceeded","message":"rate limited"}}`)
			} else {
				responsesWSWrite(t, ctx, conn, responsesWSTerminal(fmt.Sprintf("pooled-%d", i), "", 1, 1))
			}
		}
		_, _, _ = conn.Read(ctx)
	})
	up := responsesWSTestUpstream(t, fixture.URL, "openai", 0)
	up.APIKeys = []UpstreamCredential{{ID: "a", APIKey: "secret-a"}, {ID: "b", APIKey: "secret-b"}}
	if err := dbUpdateUpstream(up); err != nil {
		t.Fatal(err)
	}
	skey := scopedSessionKey(key.ID, "sid:pooled-ws")
	affinitySet(skey, up.ID)
	keyAffinitySet(skey, up.withAPIKey(up.APIKeys[1]))
	ctx, conn := responsesWSDial(t, gateway, key, http.Header{"X-Session-Id": {"pooled-ws"}})
	for i := 1; i <= 3; i++ {
		responsesWSWrite(t, ctx, conn, `{"type":"response.create","model":"ws-alias","input":"hello"}`)
		body := responsesWSRead(t, ctx, conn)
		if i < 3 && !strings.Contains(string(body), `"response.completed"`) || i == 3 && !strings.Contains(string(body), `"status":429`) {
			t.Fatalf("pooled WebSocket turn %d failed: %s", i, body)
		}
	}
	responsesWSWrite(t, ctx, conn, `{"type":"response.create","model":"ws-alias","input":"next"}`)
	body := responsesWSRead(t, ctx, conn)
	if !strings.Contains(string(body), `"status":503`) {
		t.Fatalf("cooling WebSocket credential accepted another turn: %s", body)
	}
	if up.withAPIKey(up.APIKeys[1]).availableForRelay("ws-alias") || !up.withAPIKey(up.APIKeys[0]).availableForRelay("ws-alias") {
		t.Fatal("WebSocket cooldown affected the wrong credentials")
	}
}

func TestUpstreamKeysLateSuccessDoesNotEraseNewCooldown(t *testing.T) {
	modelsTestDB(t)
	up := &Upstream{ID: 1, Type: "openai", BaseURL: "https://example.invalid", APIKeys: []UpstreamCredential{{ID: "a", APIKey: "secret-a"}}}
	selected := up.withAPIKey(up.APIKeys[0])
	start := time.Now().Add(-time.Second)
	selected.markRelayFailure(429, time.Now().Add(time.Minute))
	selected.markRelaySuccess(start)
	if selected.availableForRelay("") {
		t.Fatal("an older in-flight success cleared a newer key cooldown")
	}
	up.APIKeys[0].APIKey = "replacement-secret"
	if !up.withAPIKey(up.APIKeys[0]).availableForRelay("") {
		t.Fatal("a replacement credential inherited the old key cooldown")
	}
}

func TestUpstreamKeysProxyModesAndUnsavedProbe(t *testing.T) {
	router := testProxyAdminRouter(t)
	var mu sync.Mutex
	var routes []string
	proxyServer := func(label string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			routes = append(routes, label+":"+r.Header.Get("Authorization"))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":[{"id":"model-a"}]}`)
		}))
	}
	serverA, serverB := proxyServer("A"), proxyServer("B")
	defer serverA.Close()
	defer serverB.Close()
	proxyA, proxyB := testSavedProxy(t, serverA.URL, "", ""), testSavedProxy(t, serverB.URL, "", "")
	up := &Upstream{Type: "openai", BaseURL: "http://provider.invalid", ProxyID: proxyA.ID, KeyProxyMode: "individual",
		Enabled: true, Weight: 1, APIKeys: []UpstreamCredential{
			{ID: "a", APIKey: "secret-a", ProxyID: &proxyB.ID},
			{ID: "b", APIKey: "secret-b"},
		}}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ mode, key, want string }{
		{"individual", "a", "B:Bearer edited-a"}, {"shared", "a", "A:Bearer edited-a"}, {"individual", "b", "A:Bearer secret-b"},
	} {
		body := fmt.Sprintf(`{"key_proxy_mode":%q,"probe_key_id":%q,"api_keys":[{"id":"a","api_key":"edited-a","proxy_id":%d},{"id":"b","api_key":""}]}`, tc.mode, tc.key, proxyB.ID)
		w := proxyAdminRequest(router, "POST", fmt.Sprintf("upstreams/%d/probe", up.ID), body, true)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
			t.Fatalf("probe failed: %d %s", w.Code, w.Body.String())
		}
		mu.Lock()
		got := routes[len(routes)-1]
		mu.Unlock()
		if got != tc.want {
			t.Fatalf("probe used wrong key/proxy pair: %s, want %s", got, tc.want)
		}
	}
	saved, _ := dbGetUpstream(up.ID)
	if saved.APIKeys[0].APIKey != "secret-a" || saved.KeyProxyMode != "individual" {
		t.Fatal("unsaved probes mutated saved credentials")
	}
	w := proxyAdminRequest(router, "POST", "upstream-probe", fmt.Sprintf(`{"type":"openai","base_url":"http://provider.invalid","key_proxy_mode":"individual","probe_key_id":"new-b","api_keys":[{"id":"new-a","api_key":"unsaved-a","proxy_id":%d},{"id":"new-b","api_key":"unsaved-b","proxy_id":%d}]}`, proxyA.ID, proxyB.ID), true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("new unsaved pool could not be probed: %d %s", w.Code, w.Body.String())
	}
	mu.Lock()
	got := routes[len(routes)-1]
	mu.Unlock()
	if got != "B:Bearer unsaved-b" {
		t.Fatalf("new unsaved pool selected wrong credentials: %s", got)
	}
}

func TestUpstreamKeysAffinityPersistsAndScopesByGatewayKey(t *testing.T) {
	modelsTestDB(t)
	up := &Upstream{Type: "openai", BaseURL: "https://example.invalid", Models: "alias", Weight: 1, Enabled: true,
		APIKeys: []UpstreamCredential{{ID: "a", APIKey: "secret-a"}, {ID: "b", APIKey: "secret-b"}}}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	for index, id := range []int64{100, 200} {
		keyAffinitySet(scopedSessionKey(id, "sid:same"), up.withAPIKey(up.APIKeys[index]))
	}
	// Use the same schema on a fresh connection, as after restarting the server.
	var seq int
	var name, path string
	if err := db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := openDB(path); err != nil {
		t.Fatal(err)
	}
	stored, _ := dbGetUpstream(up.ID)
	for index, id := range []int64{100, 200} {
		selected := stored.selectAPIKey(scopedSessionKey(id, "sid:same"), "alias", nil)
		if selected == nil || selected.selectedKeyID != up.APIKeys[index].ID {
			t.Fatalf("persisted/scoped key affinity lost for caller %d", id)
		}
	}
	expired := scopedSessionKey(100, "sid:same")
	_, _ = db.Exec(`UPDATE upstream_key_affinity SET expires_at=0 WHERE skey=?`, expired)
	if got := keyAffinityGet(expired, up.ID); got != "" {
		t.Fatal("expired key binding was reused")
	}
}
