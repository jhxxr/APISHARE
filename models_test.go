package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestProbeModelsFormatsAndPagination(t *testing.T) {
	for _, typ := range []string{"openai", "anthropic", "gemini"} {
		t.Run(typ, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" {
					t.Errorf("unexpected method %s", r.Method)
				}
				switch typ {
				case "openai":
					if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer current-key" || r.Header.Get("x-api-key") != "" {
						t.Errorf("incorrect OpenAI request")
					}
					fmt.Fprint(w, `{"data":[{"id":"model-z"},{"id":"model-a"},{"id":"model-a"},{"id":" "}]}`)
				case "anthropic":
					if r.URL.Path != "/v1/models" || r.Header.Get("x-api-key") != "current-key" || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("Authorization") != "" {
						t.Errorf("incorrect Claude request")
					}
					if r.URL.Query().Get("limit") != "1000" {
						t.Errorf("missing page size")
					}
					if calls == 1 {
						fmt.Fprint(w, `{"data":[{"id":"model-z"}],"has_more":true,"last_id":"model-z"}`)
					} else {
						if r.URL.Query().Get("after_id") != "model-z" {
							t.Errorf("missing Claude cursor")
						}
						fmt.Fprint(w, `{"data":[{"id":"model-a"},{"id":"model-z"}],"has_more":false}`)
					}
				case "gemini":
					if r.URL.Path != "/v1beta/models" || r.Header.Get("x-goog-api-key") != "current-key" {
						t.Errorf("incorrect Gemini request")
					}
					if calls == 1 {
						fmt.Fprint(w, `{"models":[{"name":"models/model-z"}],"nextPageToken":"page 2&token"}`)
					} else {
						if r.URL.Query().Get("pageToken") != "page 2&token" {
							t.Errorf("missing Gemini cursor")
						}
						fmt.Fprint(w, `{"models":[{"name":"models/model-a"}]}`)
					}
				}
			}))
			defer server.Close()
			version := "/v1/"
			if typ == "gemini" {
				version = "/v1beta/"
			}
			got, err := probeUpstreamModels(context.Background(), typ, server.URL+version, " current-key ")
			if err != nil || !reflect.DeepEqual(got, []string{"model-a", "model-z"}) {
				t.Fatalf("models = %v, error = %v", got, err)
			}
			wantCalls := 2
			if typ == "openai" {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("requests = %d", calls)
			}
		})
	}
}

func TestProbeModelsRejectsInvalidAndPartialResponses(t *testing.T) {
	for _, tc := range []struct {
		name, typ, body string
		status          int
	}{
		{"missing data", "openai", `{}`, 200},
		{"null data", "openai", `{"data":null}`, 200},
		{"HTML", "openai", `<html>login</html>`, 200},
		{"error envelope", "anthropic", `{"error":{"message":"current-secret"}}`, 200},
		{"bad cursor", "anthropic", `{"data":[{"id":"a"}],"has_more":true}`, 200},
		{"repeated cursor", "anthropic", `{"data":[{"id":"a"}],"has_more":true,"last_id":"a"}`, 200},
		{"unauthorized", "openai", `current-secret`, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer server.Close()
			ids, err := probeUpstreamModels(context.Background(), tc.typ, server.URL, "current-secret")
			if err == nil || ids != nil {
				t.Fatalf("invalid/partial result accepted: %v %v", ids, err)
			}
			if strings.Contains(err.Error(), "current-secret") || strings.Contains(err.Error(), server.URL) {
				t.Fatal("upstream credentials/location leaked")
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("invalid request reached server") }))
	defer server.Close()
	for _, tc := range []struct{ typ, base, key string }{
		{"unknown", server.URL, "key"}, {"openai", server.URL, " "}, {"openai", "file:///tmp/test", "key"}, {"openai", server.URL + "?key=secret", "key"},
	} {
		if _, err := probeUpstreamModels(context.Background(), tc.typ, tc.base, tc.key); err == nil {
			t.Errorf("invalid connection accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := probeUpstreamModels(ctx, "openai", server.URL, "key"); err == nil {
		t.Error("cancellation ignored")
	}
}

func TestProbeModelsDoesNotForwardKeysOnRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed with credentials") }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer server.Close()
	if _, err := probeUpstreamModels(context.Background(), "anthropic", server.URL, "secret"); err == nil {
		t.Fatal("redirect reported as success")
	}
}

func TestModelCacheTracksCurrentCredentialsAndInflightRequests(t *testing.T) {
	oldStarted, releaseOld := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key == "old" {
			close(oldStarted)
			<-releaseOld
		}
		if key == "invalid" {
			w.WriteHeader(401)
			return
		}
		if key == "empty" {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		if r.Header.Get("x-api-key") != "" {
			key = "claude"
		}
		fmt.Fprintf(w, `{"data":[{"id":%q}]}`, key)
	}))
	defer server.Close()
	u := Upstream{ID: 999, Type: "openai", BaseURL: server.URL, APIKey: "old"}
	oldResult := make(chan []string, 1)
	go func(copy Upstream) { ids, _ := cachedUpstreamModels(context.Background(), &copy); oldResult <- ids }(u)
	<-oldStarted
	u.APIKey = "new"
	ids, err := cachedUpstreamModels(context.Background(), &u)
	close(releaseOld)
	<-oldResult
	if err != nil || !reflect.DeepEqual(ids, []string{"new"}) {
		t.Fatalf("new key inherited pending old request: %v %v", ids, err)
	}
	ids, err = cachedUpstreamModels(context.Background(), &u)
	if err != nil || !reflect.DeepEqual(ids, []string{"new"}) || calls.Load() != 2 {
		t.Fatalf("old completion overwrote new cache: %v %v calls=%d", ids, err, calls.Load())
	}
	u.Type = "anthropic"
	ids, err = cachedUpstreamModels(context.Background(), &u)
	if err != nil || !reflect.DeepEqual(ids, []string{"claude"}) {
		t.Fatalf("type change reused old cache: %v %v", ids, err)
	}
	u.Type, u.APIKey = "openai", "invalid"
	for range 2 {
		if ids, err = cachedUpstreamModels(context.Background(), &u); err == nil || ids != nil {
			t.Fatal("invalid key got cached models")
		}
	}
	if calls.Load() != 5 {
		t.Fatal("failure was cached as success")
	}
	u.APIKey = "empty"
	for range 2 {
		if ids, err = cachedUpstreamModels(context.Background(), &u); err != nil || ids == nil || len(ids) != 0 {
			t.Fatalf("empty list: %v %v", ids, err)
		}
	}
	if calls.Load() != 6 {
		t.Fatal("successful empty list was not cached")
	}
}

func modelsTestDB(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if err := openDB(filepath.Join(t.TempDir(), "models.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
}

func TestModelsEndpointOpenAIAndClaude(t *testing.T) {
	modelsTestDB(t)
	key := &ApiKey{Name: "member", Key: "sk-member", Enabled: true, QPS: 100, Concurrency: 10}
	if err := dbInsertKey(key); err != nil {
		t.Fatal(err)
	}
	for _, u := range []Upstream{
		{Name: "private connection", Type: "openai", Weight: 1, Enabled: true, Models: "model-c,model-a,model-b,model-a", ModelMap: `{"model-a":"upstream-alias","hidden-alias":"hidden"}`},
		{Name: "disabled", Type: "anthropic", Weight: 1, Enabled: false, Models: "disabled-model"},
		{Name: "zero weight", Type: "anthropic", Weight: 0, Enabled: true, Models: "zero-weight-model"},
	} {
		if err := dbInsertUpstream(&u); err != nil {
			t.Fatal(err)
		}
	}
	r := gin.New()
	v1 := r.Group("/v1", keyAuth(), keyRateLimit())
	registerModelRoutes(v1)
	request := func(path, auth, version string, status int) map[string]any {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		if strings.HasPrefix(auth, "Bearer ") {
			req.Header.Set("Authorization", auth)
		} else if auth != "" {
			req.Header.Set("x-api-key", auth)
		}
		if version != "" {
			req.Header.Set("anthropic-version", version)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != status {
			t.Fatalf("%s status %d: %s", path, w.Code, w.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(w.Body.String(), "private connection") || strings.Contains(w.Body.String(), "sk-member") {
			t.Fatal("private configuration leaked")
		}
		return result
	}
	openai := request("/v1/models", "Bearer sk-member", "", 200)
	if openai["object"] != "list" || len(openai["data"].([]any)) != 3 {
		t.Fatalf("OpenAI list: %+v", openai)
	}
	model := openai["data"].([]any)[0].(map[string]any)
	if model["id"] != "model-a" || model["object"] != "model" || model["created"] != float64(0) || model["owned_by"] != "openai" {
		t.Fatalf("OpenAI model: %+v", model)
	}
	page := request("/v1/models?limit=2", "sk-member", "2023-06-01", 200)
	if page["has_more"] != true || page["first_id"] != "model-a" || page["last_id"] != "model-b" {
		t.Fatalf("Claude first page: %+v", page)
	}
	model = page["data"].([]any)[0].(map[string]any)
	if model["type"] != "model" || model["display_name"] != "model-a" {
		t.Fatalf("Claude model: %+v", model)
	}
	if _, err := time.Parse(time.RFC3339, model["created_at"].(string)); err != nil {
		t.Fatal(err)
	}
	page = request("/v1/models?after_id=model-b&limit=2", "Bearer sk-member", "2023-06-01", 200)
	if page["has_more"] != false || page["first_id"] != "model-c" {
		t.Fatalf("Claude forward page: %+v", page)
	}
	page = request("/v1/models?before_id=model-c&limit=1", "sk-member", "", 200)
	if page["has_more"] != true || page["last_id"] != "model-b" {
		t.Fatalf("Claude previous page: %+v", page)
	}
	page = request("/v1/models?after_id=model-c", "sk-member", "", 200)
	if page["has_more"] != false || page["first_id"] != nil || page["last_id"] != nil || len(page["data"].([]any)) != 0 {
		t.Fatalf("Claude empty page: %+v", page)
	}
	for _, path := range []string{"?limit=0", "?limit=1001", "?limit=no", "?after_id=missing", "?after_id=model-a&before_id=model-c"} {
		result := request("/v1/models"+path, "sk-member", "", 400)
		if result["type"] != "error" || result["error"].(map[string]any)["type"] != "invalid_request_error" {
			t.Fatalf("Claude error: %+v", result)
		}
	}
	for _, auth := range []string{"Bearer sk-member", "sk-member"} {
		if got := request("/v1/models/model-b", auth, "", 200); got["id"] != "model-b" {
			t.Fatalf("retrieve: %+v", got)
		}
	}
	request("/v1/models/hidden-alias", "sk-member", "", 404)
	request("/v1/models", "", "", 401)
	if got := request("/v1/models", "bad-key", "2023-06-01", 401); got["type"] != "error" {
		t.Fatal("Claude auth error format")
	}
	key.Enabled = false
	if err := dbUpdateKey(key); err != nil {
		t.Fatal(err)
	}
	request("/v1/models", "Bearer sk-member", "", 401)
	key.Enabled, key.QuotaUSD = true, 1
	if err := dbUpdateKey(key); err != nil {
		t.Fatal(err)
	}
	if err := dbAddUsedUSD(key.ID, 1); err != nil {
		t.Fatal(err)
	}
	request("/v1/models", "sk-member", "", 402)
	if count, _ := dbCountLogs(); count != 0 {
		t.Fatal("model discovery billed/logged as an inference")
	}
}

func TestAdminProbeUsesCurrentKeyWithoutSaving(t *testing.T) {
	modelsTestDB(t)
	if err := settingsSet("session_secret", "test-only-session-secret"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.Header.Get("x-api-key") != "" {
			key = r.Header.Get("x-api-key")
		}
		fmt.Fprintf(w, `{"data":[{"id":%q}]}`, "model-for-"+key)
	}))
	defer server.Close()
	u := &Upstream{Name: "test", Type: "openai", BaseURL: server.URL, APIKey: "saved", Enabled: true, Weight: 1}
	if err := dbInsertUpstream(u); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	registerAdmin(r)
	for _, tc := range []struct {
		body, want string
		status     int
	}{
		{"", "model-for-saved", 200},
		{`{"api_key":"new-openai"}`, "model-for-new-openai", 200},
		{`{"type":"anthropic","api_key":"new-claude"}`, "model-for-new-claude", 200},
		{`{"api_key":" "}`, "model-for-saved", 200},
		{`{"api_key":123}`, "", 400},
	} {
		req := httptest.NewRequest("POST", fmt.Sprintf("%s/api/upstreams/%d/probe", adminPath, u.ID), strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+makeAdminToken())
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tc.status || (tc.want != "" && !strings.Contains(w.Body.String(), tc.want)) {
			t.Fatalf("probe returned %d %s", w.Code, w.Body.String())
		}
	}
	for _, typ := range []string{"openai", "anthropic"} {
		body, _ := json.Marshal(map[string]string{"type": typ, "base_url": server.URL, "api_key": "unsaved"})
		req := httptest.NewRequest("POST", adminPath+"/api/upstream-probe", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+makeAdminToken())
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "model-for-unsaved") {
			t.Fatalf("new connection: %d %s", w.Code, w.Body.String())
		}
	}
	stored, err := dbGetUpstream(u.ID)
	if err != nil || stored.APIKey != "saved" || stored.Type != "openai" {
		t.Fatalf("probe mutated saved configuration: %v", err)
	}
	req := httptest.NewRequest("POST", adminPath+"/api/upstream-probe", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatal("probe allowed unauthenticated access")
	}
}

func TestGatewayModelsRefreshWhenSavedKeyOrURLChanges(t *testing.T) {
	modelsTestDB(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key == "invalid" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprintf(w, `{"data":[{"id":%q}]}`, "model-"+key)
	}))
	defer server.Close()
	u := &Upstream{Name: "private", Type: "openai", BaseURL: server.URL, APIKey: "first", Enabled: true, Weight: 1}
	if err := dbInsertUpstream(u); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"first", "second"} {
		u.APIKey = key
		if err := dbUpdateUpstream(u); err != nil {
			t.Fatal(err)
		}
		models, err := availableModels(context.Background())
		if err != nil || len(models) != 1 || models[0].ID != "model-"+key {
			t.Fatalf("current key %s: %v %v", key, models, err)
		}
		// The public page shares only this connection's cache, including key changes.
		ids, loading := publicWildcardModels(*u)
		if loading || !reflect.DeepEqual(ids, []string{"model-" + key}) {
			t.Fatalf("public cache mismatch: %v loading=%v", ids, loading)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected discovery count")
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":[{"id":"new-location"}]}`) }))
	defer other.Close()
	u.BaseURL = other.URL
	if err := dbUpdateUpstream(u); err != nil {
		t.Fatal(err)
	}
	models, err := availableModels(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "new-location" {
		t.Fatalf("URL change: %v %v", models, err)
	}
	u.BaseURL, u.APIKey = server.URL, "invalid"
	if err := dbUpdateUpstream(u); err != nil {
		t.Fatal(err)
	}
	if _, err := availableModels(context.Background()); err == nil {
		t.Fatal("upstream auth failure reported as successful empty catalog")
	}
	u.Enabled = false
	if err := dbUpdateUpstream(u); err != nil {
		t.Fatal(err)
	}
	models, err = availableModels(context.Background())
	if err != nil || models == nil || len(models) != 0 {
		t.Fatalf("disabled catalog must be empty: %v %v", models, err)
	}
}

func TestModelRoutesAllowBrowserPreflight(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	registerModelRoutes(r.Group("/v1", corsV1(), keyAuth(), keyRateLimit()))
	for _, path := range []string{"/v1/models", "/v1/models/claude-demo"} {
		req := httptest.NewRequest(http.MethodOptions, path, nil)
		req.Header.Set("Origin", "https://client.example")
		req.Header.Set("Access-Control-Request-Method", "GET")
		req.Header.Set("Access-Control-Request-Headers", "authorization,x-api-key,anthropic-version")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("preflight %s: %d %s", path, w.Code, w.Body.String())
		}
		for _, header := range []string{"authorization", "x-api-key", "anthropic-version"} {
			if !strings.Contains(strings.ToLower(w.Header().Get("Access-Control-Allow-Headers")), header) {
				t.Fatalf("missing allowed header %s", header)
			}
		}
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("preflight bypassed GET authentication: %d", w.Code)
		}
	}
}
