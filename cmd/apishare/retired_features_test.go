package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRemovedFeatureSchemaAndUpgrade(t *testing.T) {
	gatewayTestDB(t)
	for _, table := range []string{"user_upstreams", "user_auto_routers"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("fresh database contains retired table %s: count=%d err=%v", table, count, err)
		}
		if _, err := db.Exec(`CREATE TABLE ` + table + ` (sub TEXT PRIMARY KEY, config_json TEXT)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO ` + table + ` VALUES ('legacy-user','{"enabled":true,"virtual_model":"legacy-personal"}')`); err != nil {
			t.Fatal(err)
		}
	}
	const config = `{"enabled":true,"mode":"live","virtual_model":"legacy-auto","default_model":"real"}`
	if err := settingsSet("auto_router", config); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"openrouter", "systemone", "cloudflare", "openai"} {
		up := &Upstream{Name: kind, Type: kind, Models: "legacy-" + kind, APIKey: "test", BaseURL: "https://example.invalid", Weight: 1, Enabled: true}
		if err := dbInsertUpstream(up); err != nil {
			t.Fatal(err)
		}
	}
	const receipt = `{"quota_pool":"byok","cost":{"total":0},"router":{"requested_model":"legacy-auto","arena":{"mode":"live"}}}`
	if _, err := db.Exec(`INSERT INTO call_logs (model,cost_usd,status,billing_json) VALUES ('real',0,200,?)`, receipt); err != nil {
		t.Fatal(err)
	}
	// Reopen the same database twice to exercise the actual startup migration.
	var sequence int
	var name, path string
	if err := db.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if err := openDB(path); err != nil {
			t.Fatal(err)
		}
	}
	ups, err := dbListUpstreams()
	if err != nil || len(ups) != 4 {
		t.Fatalf("upgrade removed channel records: %+v %v", ups, err)
	}
	for _, up := range ups {
		if up.Enabled != (up.Type == "openai") {
			t.Errorf("unexpected enabled state after upgrade: %+v", up)
		}
	}
	for _, table := range []string{"user_upstreams", "user_auto_routers"} {
		var raw string
		if err := db.QueryRow(`SELECT config_json FROM ` + table + ` WHERE sub='legacy-user'`).Scan(&raw); err != nil || !strings.Contains(raw, "legacy-personal") {
			t.Fatalf("legacy data was removed: %s %q %v", table, raw, err)
		}
	}
	if raw, err := settingsGet("auto_router"); err != nil || raw != config {
		t.Fatalf("legacy configuration was overwritten: %q %v", raw, err)
	}
	var saved string
	if err := db.QueryRow(`SELECT billing_json FROM call_logs LIMIT 1`).Scan(&saved); err != nil || saved != receipt {
		t.Fatalf("historical billing was rewritten: %q %v", saved, err)
	}
	logs, err := dbListLogs(10, 0)
	if err != nil || len(logs) != 1 || logs[0].Billing == nil || logs[0].Status != 200 || logs[0].CostUSD != 0 {
		t.Fatalf("legacy billing cannot be read: %+v %v", logs, err)
	}
	var roleTable int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='user_agent_purge_configs'`).Scan(&roleTable); err != nil || roleTable != 1 {
		t.Fatalf("personal prompt schema was lost: %d %v", roleTable, err)
	}
}

func TestRemovedFeatureAPIsAndChannelTypes(t *testing.T) {
	r := deploymentTestRouter(t)
	deploymentJSON(t, deploymentRequest(r, "PUT", "/setup/api/deployment", "", `{"new_password":"removed-test-password-123","confirm_password":"removed-test-password-123","admin_path":"removed-console"}`), 200)
	r = newRouter()
	token := makeAdminToken()
	for _, path := range []string{
		"/removed-console/api/auto-router/config", "/removed-console/api/auto-router/dry-run", "/removed-console/api/auto-router/bandit/reset",
		"/api/user/auto-router", "/api/user/auto-router/dry-run", "/api/user/upstreams", "/api/user/upstreams/1", "/api/user/upstreams/probe",
		"/v1/decisions", "/v1/alpha/decisions", "/v1/systemone", "/api/alpha/decisions",
	} {
		for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
			if w := deploymentRequest(r, method, path, token, `{}`); w.Code != http.StatusNotFound {
				t.Errorf("removed endpoint %s %s: HTTP %d %s", method, path, w.Code, w.Body)
			}
		}
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1) }))
	defer server.Close()
	for _, kind := range []string{"openrouter", "systemone", "cloudflare"} {
		payload := fmt.Sprintf(`{"name":"removed","type":%q,"base_url":%q,"api_key":"test","models":"legacy","enabled":true,"weight":1}`, kind, server.URL)
		for _, path := range []string{"/removed-console/api/upstreams", "/removed-console/api/upstream-probe"} {
			if w := deploymentRequest(r, "POST", path, token, payload); w.Code != 400 {
				t.Errorf("retired type %s accepted at %s: %d %s", kind, path, w.Code, w.Body)
			}
		}
		up := &Upstream{Name: kind, Type: kind, BaseURL: server.URL, APIKey: "test", Models: "legacy", Enabled: true, Weight: 1}
		if err := dbInsertUpstream(up); err != nil {
			t.Fatal(err)
		}
		path := fmt.Sprintf("/removed-console/api/upstreams/%d", up.ID)
		if w := deploymentRequest(r, "PATCH", path, token, `{"enabled":true}`); w.Code != 400 {
			t.Errorf("retired type can be re-enabled: %d %s", w.Code, w.Body)
		}
		if w := deploymentRequest(r, "POST", path+"/duplicate", token, `{}`); w.Code != 400 {
			t.Errorf("retired type can be duplicated: %d %s", w.Code, w.Body)
		}
		if _, _, err := testUpstreamModel(context.Background(), up, "legacy", "hello", 1024, false); err == nil {
			t.Error("retired type can be model-tested")
		}
		if _, err := probeConfiguredModels(context.Background(), up); err == nil {
			t.Error("retired type can be probed")
		}
	}
	snapshot, err := getRoutingSnapshot()
	if err != nil || len(snapshot.upstreams) != 0 {
		t.Fatalf("retired channels entered the routing snapshot: %+v %v", snapshot, err)
	}
	models, err := availableModels(context.Background())
	if err != nil || len(models) != 0 || calls.Load() != 0 {
		t.Fatalf("retired channels discovered or contacted: %+v %v calls=%d", models, err, calls.Load())
	}
}

func TestLegacyVirtualNamesAreOnlyOrdinaryModelIDs(t *testing.T) {
	gatewayTestDB(t)
	if err := settingsSet("auto_router", `{"enabled":true,"mode":"live","virtual_model":"legacy-auto","default_model":"real"}`); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		model := str(body["model"])
		if model != "auto" && model != "real" && model != "gpt-5.2" {
			t.Errorf("request unexpectedly changed model: %s", model)
		}
		if model == "gpt-5.2" && (body["reasoning_effort"] != "high" || object(body["metadata"])["keep"] != "yes") {
			t.Errorf("explicit model options or native extensions lost: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer server.Close()
	up := &Upstream{Name: "ordinary", Type: "openai", OpenAIProtocol: "chat", BaseURL: server.URL, Models: "auto,real,gpt-5.2", Enabled: true, Weight: 1}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	key := &ApiKey{Name: "ordinary", Key: "sk-ordinary", Models: "auto,real,gpt-5.2(high),legacy-auto,legacy-personal", Enabled: true}
	if err := dbInsertKey(key); err != nil {
		t.Fatal(err)
	}
	key.XAuthUser = &XAuthKeyUser{Sub: "legacy-user"}
	models, err := availableModels(context.Background())
	if err != nil || len(models) != 3 {
		t.Fatalf("unexpected catalog: %+v %v", models, err)
	}
	r := gin.New()
	r.POST("/relay", func(c *gin.Context) { c.Set("apiKey", *key) }, relayHandler(FormatOpenAIChat, false))
	for _, tc := range []struct {
		model string
		want  int
	}{{"auto", 200}, {"real", 200}, {"gpt-5.2(high)", 200}, {"legacy-auto", 502}, {"legacy-personal", 502}, {"denied", 403}} {
		body := fmt.Sprintf(`{"model":%q,"metadata":{"keep":"yes"},"messages":[{"role":"user","content":"Design a complex architecture"}]}`, tc.model)
		w := deploymentRequest(r, "POST", "/relay", "", body)
		if w.Code != tc.want {
			t.Errorf("model %s: HTTP %d, want %d: %s", tc.model, w.Code, tc.want, w.Body)
		}
		for name := range w.Header() {
			if strings.HasPrefix(strings.ToLower(name), "x-apishare-router-") {
				t.Errorf("request still emits router header %s", name)
			}
		}
	}
	key.QuotaUSD, key.SharedUsedUSD = 1, 1
	if w := deploymentRequest(r, "POST", "/relay", "", `{"model":"real","messages":[{"role":"user","content":"hello"}]}`); w.Code != 402 {
		t.Errorf("site quota was bypassed: %d %s", w.Code, w.Body)
	}
	if calls.Load() != 3 {
		t.Errorf("unexpected upstream requests: %d", calls.Load())
	}
}
