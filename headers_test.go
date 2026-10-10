package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestHeaderOverridesValidation(t *testing.T) {
	for _, raw := range []string{"", `{}`, `{"*":true,"re:^X-Trace-.*$":true,"X-Foo":"{client_header:X-Foo}","Authorization":"Bearer {api_key}"}`, `{"X-Remove":false,"X-Empty":""}`} {
		if _, err := parseHeaderOverrides(raw); err != nil {
			t.Errorf("valid configuration rejected: %v", err)
		}
	}
	for _, raw := range []string{`null`, `[]`, `true`, `{"X":1}`, `{"X":null}`, `{"X":[]}`, `{"X":{}}`, `{"X":"one","x":"two"}`, `{"X":"one","X":"two"}`, `{"Bad Header":"value"}`, `{"Host":"other"}`, `{"Content-Length":false}`, `{"Proxy-Authorization":"secret"}`, `{"*":"value"}`, `{"re:.*":"value"}`, `{"re:":true}`, `{"re:[":true}`, `{"X":"secret\r\nInjected: value"}`, `{"X":"{client_header:Bad Name}"}`, `{"X":true} {}`, `{"X":true`, strings.Repeat(" ", 65537) + `{}`} {
		if _, err := parseHeaderOverrides(raw); err == nil {
			t.Errorf("invalid configuration accepted: %.100s", raw)
		} else if strings.Contains(err.Error(), "secret") {
			t.Fatal("validation leaked a header value")
		}
	}
}

func TestHeaderOverridesRulesAndCredentialFiltering(t *testing.T) {
	incoming := http.Header{}
	for name, value := range map[string]string{"Authorization": "Bearer gateway-key", "x-api-key": "gateway-key", "x-goog-api-key": "gateway-key", "Cookie": "session=private", "X-Foo": "client-value", "X-Trace-ID": "trace", "X-Trace-Drop": "drop", "X-Delete": "delete", "Connection": "keep-alive, X-Hop", "X-Hop": "hop", "Content-Length": "999", "Proxy-Authorization": "private", "Anthropic-Beta": "feature", "User-Agent": "client-agent"} {
		incoming.Set(name, value)
	}
	incoming.Add("X-Multi", "one")
	incoming.Add("X-Multi", "two")
	raw := `{"X-Foo":"prefix {client_header:x-foo} {api_key}","re:^x-trace-.*$":true,"X-Trace-ID":"fixed","re:^X-Trace-Drop$":false,"*":true,"X-Delete":false,"Authorization":"Bearer {api_key}","X-Missing":"{client_header:Missing}","X-Exact":true}`
	incoming.Set("X-Exact", "exact")
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest("POST", "http://example.invalid", nil)
		req.Header.Set("Authorization", "Bearer default")
		if err := applyHeaderOverrides(req, raw, "upstream-key", incoming); err != nil {
			t.Fatal(err)
		}
		for name, want := range map[string]string{"Authorization": "Bearer upstream-key", "X-Foo": "prefix client-value upstream-key", "X-Trace-ID": "fixed", "Anthropic-Beta": "feature", "User-Agent": "client-agent", "X-Exact": "exact"} {
			if got := req.Header.Get(name); got != want {
				t.Errorf("%s: got %q want %q", name, got, want)
			}
		}
		for _, name := range []string{"X-Trace-Drop", "X-Delete", "X-Missing", "X-Hop", "Connection", "Content-Length", "Proxy-Authorization", "x-api-key", "x-goog-api-key", "Cookie"} {
			if req.Header.Get(name) != "" {
				t.Errorf("unexpected forwarded header %s", name)
			}
		}
		if !reflect.DeepEqual(req.Header.Values("X-Multi"), []string{"one", "two"}) {
			t.Fatal("multi-value header lost")
		}
		req.Header.Set("X-Multi", "changed")
		if incoming.Get("X-Multi") != "one" {
			t.Fatal("incoming headers were modified")
		}
	}
}

func TestHeaderOverridesMissingVariablesAndInjection(t *testing.T) {
	req := httptest.NewRequest("POST", "http://example.invalid", nil)
	req.Header.Set("Authorization", "Bearer upstream-key")
	if err := applyHeaderOverrides(req, `{"*":true,"Authorization":"Bearer {client_header:Authorization}","X-Missing":"{client_header:X-Foo}"}`, "upstream-key", nil); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Bearer upstream-key" || req.Header.Get("X-Missing") != "" {
		t.Fatal("missing client variables changed defaults")
	}
	if err := applyHeaderOverrides(req, `{"Authorization":false}`, "key", nil); err != nil || req.Header.Get("Authorization") != "" {
		t.Fatal("explicit removal failed")
	}
	incoming := http.Header{"X-Foo": []string{"{api_key}"}}
	if err := applyHeaderOverrides(req, `{"X-Foo":"{client_header:X-Foo}"}`, "secret", incoming); err != nil || req.Header.Get("X-Foo") != "{api_key}" {
		t.Fatal("substituted a variable inside a client value")
	}
	incoming.Set("X-Foo", "private\r\nInjected: value")
	for _, raw := range []string{`{"*":true}`, `{"X-Foo":true}`, `{"X-Other":"{client_header:X-Foo}"}`} {
		if err := applyHeaderOverrides(req, raw, "key", incoming); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("header injection accepted or exposed")
		}
	}
}

func TestHeaderOverridesMigrationAndAdminPersistence(t *testing.T) {
	r := testProxyAdminRouter(t)
	u := &Upstream{Name: "legacy", Type: "openai", BaseURL: "http://example.invalid", APIKey: "saved-key", Weight: 1, Enabled: true}
	if err := dbInsertUpstream(u); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE upstreams DROP COLUMN header_overrides`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := migrate(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := dbGetUpstream(u.ID)
	if err != nil || got.HeaderOverrides != "" || got.APIKey != u.APIKey {
		t.Fatal("migration changed existing upstream")
	}
	path := fmt.Sprintf("upstreams/%d", u.ID)
	patch := func(body any, status int) {
		t.Helper()
		raw, _ := json.Marshal(body)
		w := proxyAdminRequest(r, "PATCH", path, string(raw), true)
		if w.Code != status {
			t.Fatalf("patch: %d %s", w.Code, w.Body.String())
		}
	}
	raw := `{"X-Tenant":"tenant","Authorization":"Bearer {api_key}"}`
	patch(map[string]any{"header_overrides": raw}, 200)
	patch(map[string]any{"enabled": false}, 200)
	got, _ = dbGetUpstream(u.ID)
	if got.HeaderOverrides != raw {
		t.Fatal("unrelated edit lost header configuration")
	}
	for _, invalid := range []string{`[]`, `{"re:[":true}`, `{"Authorization":"secret\nvalue"}`} {
		patch(map[string]any{"header_overrides": invalid}, 400)
	}
	got, _ = dbGetUpstream(u.ID)
	if got.HeaderOverrides != raw {
		t.Fatal("invalid edit changed saved configuration")
	}
	w := proxyAdminRequest(r, "GET", "upstreams", "", true)
	var list []Upstream
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || list[0].HeaderOverrides != raw {
		t.Fatal("list did not return header configuration")
	}
	patch(map[string]any{"header_overrides": ""}, 200)
	got, _ = dbGetUpstream(u.ID)
	if got.HeaderOverrides != "" {
		t.Fatal("configuration was not cleared")
	}
	for _, value := range []struct {
		raw    string
		status int
	}{{raw, 200}, {`{"*":12}`, 400}} {
		body, _ := json.Marshal(map[string]any{"type": "openai", "base_url": u.BaseURL, "api_key": "key", "header_overrides": value.raw})
		if w := proxyAdminRequest(r, "POST", "upstreams", string(body), true); w.Code != value.status {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestHeaderOverridesRelayFormatsAndStreams(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		format        clientFormat
		typ, endpoint string
	}{
		{FormatOpenAIChat, "openai", "/v1/chat/completions"},
		{FormatOpenAIChat, "openai", "/v1/embeddings"},
		{FormatOpenAIResponse, "openai", "/v1/responses"},
		{FormatAnthropic, "anthropic", "/v1/messages"},
		{FormatGemini, "gemini", "/v1beta/models/model-a:generateContent"},
		{FormatOpenAIChat, "anthropic", "/v1/chat/completions"},
		{FormatAnthropic, "gemini", "/v1/messages"},
		{FormatGemini, "openai", "/v1beta/models/model-a:generateContent"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%s/%s/%t", tc.format, tc.typ, tc.endpoint, stream), func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if r.Header.Get("X-Custom") != "upstream-key" || r.Header.Get("X-Foo") != "client" || r.Header.Get("X-Trace-ID") != "trace" {
						t.Error("relay missed custom headers")
					}
					auth := "Authorization"
					want := "Bearer upstream-key"
					if tc.typ == "anthropic" {
						auth, want = "x-api-key", "upstream-key"
					}
					if tc.typ == "gemini" {
						auth, want = "x-goog-api-key", "upstream-key"
					}
					if r.Header.Get(auth) != want {
						t.Error("relay forwarded client credentials")
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: {}\n\n")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"content":[{"type":"text","text":"ok"}],"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`)
				}))
				defer server.Close()
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest("POST", "http://gateway.invalid", nil)
				c.Request.Header.Set("Authorization", "Bearer gateway-key")
				c.Request.Header.Set("x-api-key", "gateway-key")
				c.Request.Header.Set("x-goog-api-key", "gateway-key")
				c.Request.Header.Set("X-Foo", "client")
				c.Request.Header.Set("X-Trace-ID", "trace")
				u := &Upstream{Type: tc.typ, BaseURL: server.URL, APIKey: "upstream-key", HeaderOverrides: `{"*":true,"re:^X-Trace-.*$":true,"X-Custom":"{api_key}","X-Foo":"{client_header:X-Foo}"}`}
				task := &relayTask{format: tc.format, endpoint: tc.endpoint, model: "model-a", stream: stream, body: []byte(`{"model":"model-a"}`), canon: &canonReq{Model: "model-a", Stream: stream, Messages: []canonMsg{{Role: "user", Text: "hi"}}}}
				if _, wrote, err := doRelay(c, u, task); err != nil || !wrote || calls != 1 {
					t.Fatalf("relay: wrote=%t calls=%d err=%v", wrote, calls, err)
				}
			})
		}
	}
}

func TestHeaderOverridesProbeAndCatalogCache(t *testing.T) {
	r := testProxyAdminRouter(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls++
		if req.Header.Get("Authorization") != "Bearer channel-key" || req.Header.Get("X-Admin") != "" {
			t.Error("probe lost upstream credentials or leaked administrator token")
		}
		if req.Header.Get("X-Tenant") == "" {
			t.Error("probe did not use fixed headers")
		}
		fmt.Fprintf(w, `{"data":[{"id":%q}]}`, req.Header.Get("X-Tenant"))
	}))
	defer server.Close()
	u := &Upstream{Type: "openai", BaseURL: server.URL, APIKey: "channel-key", Enabled: true, Weight: 1, HeaderOverrides: `{"*":true,"X-Tenant":"saved","X-Admin":"{client_header:Authorization}"}`}
	if err := dbInsertUpstream(u); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("upstreams/%d", u.ID)
	for _, endpoint := range []string{"/test", "/probe"} {
		w := proxyAdminRequest(r, "POST", path+endpoint, `{}`, true)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
			t.Fatalf("probe failed: %d %s", w.Code, w.Body.String())
		}
	}
	for _, endpoint := range []string{"upstream-probe", path + "/probe"} {
		body, _ := json.Marshal(map[string]any{"type": u.Type, "base_url": u.BaseURL, "api_key": u.APIKey, "header_overrides": `{"X-Tenant":"unsaved"}`})
		w := proxyAdminRequest(r, "POST", endpoint, string(body), true)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "unsaved") {
			t.Fatalf("unsaved headers ignored: %d %s", w.Code, w.Body.String())
		}
	}
	ids, err := cachedUpstreamModels(context.Background(), u)
	if err != nil || !reflect.DeepEqual(ids, []string{"saved"}) {
		t.Fatalf("saved catalog: %v %v", ids, err)
	}
	before := calls
	if _, err := cachedUpstreamModels(context.Background(), u); err != nil || calls != before {
		t.Fatal("catalog cache not reused")
	}
	u.HeaderOverrides = `{"X-Tenant":"changed"}`
	ids, err = cachedUpstreamModels(context.Background(), u)
	if err != nil || !reflect.DeepEqual(ids, []string{"changed"}) || calls != before+1 {
		t.Fatal("header change did not invalidate catalog cache")
	}
}

func TestCustomHeadersCORSPreflight(t *testing.T) {
	r := gin.New()
	r.Use(corsV1())
	r.OPTIONS("/v1/chat/completions", func(c *gin.Context) { c.Status(204) })
	req := httptest.NewRequest("OPTIONS", "/v1/chat/completions", nil)
	req.Header.Set("Access-Control-Request-Headers", "authorization, content-type, x-custom-header")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 204 || w.Header().Get("Access-Control-Allow-Headers") != req.Header.Get("Access-Control-Request-Headers") {
		t.Fatal("browser custom headers blocked by CORS")
	}
}
