package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestKeyModelsLegacyMigration(t *testing.T) {
	d, err := sql.Open("sqlite", "file:"+filepathToSlash(filepath.Join(t.TempDir(), "legacy.db")))
	if err != nil {
		t.Fatal(err)
	}
	db = d
	d.SetMaxOpenConns(1)
	t.Cleanup(func() { d.Close() })
	_, err = d.Exec(`CREATE TABLE api_keys(id INTEGER PRIMARY KEY,name TEXT NOT NULL DEFAULT '',key TEXT NOT NULL UNIQUE,quota_usd REAL NOT NULL DEFAULT 0,used_usd REAL NOT NULL DEFAULT 0,qps REAL NOT NULL DEFAULT 3,concurrency INTEGER NOT NULL DEFAULT 5,enabled INTEGER NOT NULL DEFAULT 1,show_on_home INTEGER NOT NULL DEFAULT 0,note TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL DEFAULT '');
		INSERT INTO api_keys(id,name,key,quota_usd,used_usd) VALUES(8,'legacy','sk-legacy',42,3)`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrate(); err != nil {
			t.Fatal(err)
		}
	}
	ak, err := dbGetKeyByKey("sk-legacy")
	if err != nil || ak.Models != "" || ak.allowsModel("any-model") || ak.QuotaUSD != 42 || ak.UsedUSD != 3 || ak.SharedUsedUSD != 3 {
		t.Fatalf("migration changed legacy access/data: %+v %v", ak, err)
	}
	// Nonblank malformed persisted data fails closed.
	if (ApiKey{Models: ",,\n"}).allowsModel("any-model") {
		t.Fatal("malformed permissions granted access")
	}
}

func TestKeyModelsAdminPersistenceAndPartialUpdates(t *testing.T) {
	modelsTestDB(t)
	if err := settingsSet("session_secret", "model-permission-test-secret"); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	registerAdmin(r)
	tok := makeAdminToken()
	path := currentAdminPath() + "/api/keys"
	w := deploymentRequest(r, "POST", path, tok, `{"name":"restricted","models":" model-a，model-b\nmodel-a \r\n alias "}`)
	result := deploymentJSON(t, w, 200)
	id := int64(result["id"].(float64))
	if result["models"] != "model-a,model-b,alias" {
		t.Fatalf("normalization: %v", result)
	}
	keyPath := fmt.Sprintf("%s/%d", path, id)
	deploymentJSON(t, deploymentRequest(r, "PATCH", keyPath, tok, `{"quota_usd":17,"enabled":false}`), 200)
	ak, err := dbGetKeyByID(id)
	if err != nil || ak.Models != "model-a,model-b,alias" || ak.QuotaUSD != 17 || ak.Enabled {
		t.Fatalf("partial update lost state: %+v %v", ak, err)
	}
	byKey, err := dbGetKeyByKey(result["key"].(string))
	if err != nil || byKey.Models != ak.Models {
		t.Fatal("authentication read lost permissions")
	}
	keys, err := dbListKeys()
	if err != nil || len(keys) != 1 || keys[0].Models != ak.Models {
		t.Fatal("listing lost permissions")
	}
	deploymentJSON(t, deploymentRequest(r, "PATCH", keyPath, tok, `{"models":",，\n"}`), 400)
	deploymentJSON(t, deploymentRequest(r, "POST", path, tok, `{"models":",，\n"}`), 400)
	deploymentJSON(t, deploymentRequest(r, "PATCH", keyPath, tok, `{"models":"model-c"}`), 200)
	ak, _ = dbGetKeyByID(id)
	if ak.allowsModel("model-a") || !ak.allowsModel("model-c") || ak.allowsModel("MODEL-C") {
		t.Fatal("updated permissions not exact")
	}
	deploymentJSON(t, deploymentRequest(r, "PATCH", keyPath, tok, `{"models":""}`), 200)
	ak, _ = dbGetKeyByID(id)
	if ak.allowsModel("model-a") || ak.QuotaUSD != 17 {
		t.Fatal("clearing models lost quota or granted model access")
	}
	if err := dbInsertUpstream(&Upstream{Type: "openai", Models: "model-b,model-a", Enabled: true, Weight: 1}); err != nil {
		t.Fatal(err)
	}
	if err := dbInsertUpstream(&Upstream{Type: "openai", Models: "disabled-model", Enabled: false, Weight: 1}); err != nil {
		t.Fatal(err)
	}
	w = deploymentRequest(r, "GET", path+"/models", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatal("model choices exposed without admin auth")
	}
	choices := deploymentJSON(t, deploymentRequest(r, "GET", path+"/models", tok, ""), 200)
	if !reflect.DeepEqual(choices["models"], []any{"model-a", "model-b"}) {
		t.Fatalf("choices: %v", choices)
	}
}

func TestKeyModelsCatalogIsolationAndPagination(t *testing.T) {
	modelsTestDB(t)
	a := &ApiKey{Key: "sk-restricted-a", Enabled: true, Models: "model-a,model-c,unavailable", QPS: 1000, Concurrency: 10}
	b := &ApiKey{Key: "sk-restricted-b", Enabled: true, Models: "model-b", QPS: 1000, Concurrency: 10}
	unlimited := &ApiKey{Key: "sk-unlimited", Enabled: true, QPS: 1000, Concurrency: 10}
	for _, ak := range []*ApiKey{a, b, unlimited} {
		if err := dbInsertKey(ak); err != nil {
			t.Fatal(err)
		}
	}
	if err := dbInsertUpstream(&Upstream{Type: "openai", Models: "model-a,model-b,model-c", Enabled: true, Weight: 1}); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	registerModelRoutes(r.Group("/v1", keyAuth(), keyRateLimit()))
	for _, tc := range []struct {
		key, path string
		claude    bool
		status    int
		ids       []string
		more      bool
	}{
		{a.Key, "/v1/models", false, 200, []string{"model-a", "model-c"}, false},
		{b.Key, "/v1/models", false, 200, []string{"model-b"}, false},
		{unlimited.Key, "/v1/models", false, 200, []string{}, false},
		{a.Key, "/v1/models?limit=1", true, 200, []string{"model-a"}, true},
		{a.Key, "/v1/models?limit=1&after_id=model-a", true, 200, []string{"model-c"}, false},
		{a.Key, "/v1/models?limit=1&before_id=model-c", true, 200, []string{"model-a"}, false},
		{a.Key, "/v1/models?after_id=model-b", true, 400, nil, false},
		{a.Key, "/v1/models/model-b", false, 404, nil, false},
		{a.Key, "/v1/models/model-b", true, 404, nil, false},
		{a.Key, "/v1/models/model-a", false, 200, nil, false},
	} {
		req := httptest.NewRequest("GET", tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+tc.key)
		if tc.claude {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		result := deploymentJSON(t, w, tc.status)
		if tc.ids != nil {
			ids := make([]string, 0)
			for _, row := range result["data"].([]any) {
				ids = append(ids, row.(map[string]any)["id"].(string))
			}
			if !reflect.DeepEqual(ids, tc.ids) {
				t.Fatalf("%s %s got %v want %v", tc.key, tc.path, ids, tc.ids)
			}
			if tc.claude && result["has_more"] != tc.more {
				t.Fatalf("pagination: %v", result)
			}
		}
	}
	// Permission changes are read afresh on the very next request.
	a.Models = "unavailable"
	if err := dbUpdateKey(a); err != nil {
		t.Fatal(err)
	}
	w := deploymentRequest(r, "GET", "/v1/models", a.Key, "")
	result := deploymentJSON(t, w, 200)
	if len(result["data"].([]any)) != 0 {
		t.Fatalf("stale or non-upstream models listed: %v", result)
	}
}

func TestKeyModelsRelayCannotBypassPermissions(t *testing.T) {
	modelsTestDB(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["model"] != "real-model" {
			t.Errorf("mapping not applied: %v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"fixture","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`)
	}))
	defer upstream.Close()
	ak := &ApiKey{Key: "sk-model-relay", Enabled: true, Models: "allowed-alias", QPS: 1000, Concurrency: 10}
	if err := dbInsertKey(ak); err != nil {
		t.Fatal(err)
	}
	u := &Upstream{Type: "openai", OpenAIProtocol: "chat", BaseURL: upstream.URL, APIKey: "fixture", Models: "allowed-alias,real-model,denied", ModelMap: `{"allowed-alias":"real-model","denied":"real-model"}`, Enabled: true, Weight: 1}
	if err := dbInsertUpstream(u); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	v1 := r.Group("/v1", keyAuth(), keyRateLimit())
	v1.POST("/chat/completions", relayHandler(FormatOpenAIChat, false))
	v1.POST("/responses", relayHandler(FormatOpenAIResponse, false))
	v1.POST("/messages", relayHandler(FormatAnthropic, false))
	v1.POST("/embeddings", relayHandler(FormatOpenAIChat, true))
	r.Group("/v1beta", keyAuth(), keyRateLimit()).POST("/models/*action", relayHandler(FormatGemini, false))
	affinitySet("sid:model-permissions", u.ID)
	for _, tc := range []struct{ path, body, errorType string }{
		{"/v1/chat/completions", `{"model":"denied","messages":[{"role":"user","content":"hi"}]}`, "apishare_error"},
		{"/v1/chat/completions", `{"model":"real-model","stream":true,"messages":[]}`, "apishare_error"},
		{"/v1/responses", `{"model":"denied","stream":true,"input":"hi"}`, ""},
		{"/v1/messages", `{"model":"denied","stream":true,"max_tokens":10,"messages":[]}`, "permission_error"},
		{"/v1/embeddings", `{"model":"denied","input":"hi"}`, "apishare_error"},
		{"/v1beta/models/denied:generateContent", `{"model":"allowed-alias","contents":[]}`, "PERMISSION_DENIED"},
		{"/v1beta/models/denied:streamGenerateContent?alt=sse", `{"contents":[]}`, "PERMISSION_DENIED"},
	} {
		req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+ak.Key)
		req.Header.Set("X-Session-Id", "model-permissions")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		deploymentJSON(t, w, http.StatusForbidden)
		if !strings.Contains(w.Body.String(), tc.errorType) {
			t.Fatalf("wrong error format: %s", w.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unauthorized requests reached upstream")
	}
	stored, _ := dbGetKeyByID(ak.ID)
	logs, _ := dbCountLogs()
	if stored.UsedUSD != 0 || logs != 0 {
		t.Fatal("rejected requests billed or treated as inference")
	}
	// The permitted alias can call the mapped model; revocation also wins over
	// an existing affinity binding and must not trigger any upstream retry.
	body := `{"model":"allowed-alias","messages":[{"role":"user","content":"hi"}]}`
	w := deploymentRequest(r, "POST", "/v1/chat/completions", ak.Key, body)
	deploymentJSON(t, w, 200)
	if calls.Load() != 1 {
		t.Fatal("allowed alias was not forwarded")
	}
	ak.Models = "other-model"
	if err := dbUpdateKey(ak); err != nil {
		t.Fatal(err)
	}
	w = deploymentRequest(r, "POST", "/v1/chat/completions", ak.Key, body)
	deploymentJSON(t, w, http.StatusForbidden)
	if calls.Load() != 1 {
		t.Fatal("revoked model reached upstream")
	}
}
