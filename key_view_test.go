package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func keyViewTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	publicTestDB(t)
	keyViewAttempts.Lock()
	keyViewAttempts.clients = make(map[string]*keyViewAttempt)
	keyViewAttempts.Unlock()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	registerPublic(r)
	registerAdmin(r)
	r.GET("/v1/test", keyAuth(), func(c *gin.Context) { c.Status(200) })
	return r
}

func keyViewRequest(r *gin.Engine, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "https://share.example"+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://share.example")
	if method != http.MethodGet {
		request.Header.Set(keyViewRequestHeader, "key-view")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, request)
	return w
}

func keyViewLogin(t *testing.T, r *gin.Engine, key string, old *http.Cookie) (*http.Cookie, KeyOverview) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"key": key})
	w := keyViewRequest(r, "POST", "/api/key/session", string(body), old)
	if w.Code != 200 {
		t.Fatalf("login %d: %s", w.Code, w.Body.String())
	}
	var got KeyOverview
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatalf("session cookies: %+v", cookies)
	}
	if cookies[0].Path != "/api/key" || cookies[0].MaxAge >= 0 {
		t.Fatal("legacy cookie was not cleared")
	}
	cookie := cookies[1]
	if cookie.Name != keyViewCookie || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.MaxAge != 7*24*3600 {
		t.Fatalf("query cookie attributes: %+v", cookie)
	}
	return cookie, got
}

func TestKeyViewSessionIsolationAndReadOnlyScope(t *testing.T) {
	r := keyViewTestRouter(t)
	a := publicTestKey(t, "owner-a", 100, 0, false)
	b := publicTestKey(t, "owner-b", 999, 0, true)
	a.Models = "a,b,alias"
	a.ModelQuotas = map[string]ModelQuota{"b": {QuotaUSD: 30}, "alias": {QuotaUSD: 10}}
	if err := dbUpdateKey(a); err != nil {
		t.Fatal(err)
	}
	for _, charge := range []struct {
		model       string
		cost        float64
		independent bool
	}{{"a", 20, false}, {"b", 35, true}, {"alias", 3, true}} {
		if err := dbChargeKey(a.ID, charge.model, charge.cost, charge.independent); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO api_key_model_quotas(key_id,model,quota_usd,used_usd) VALUES(?,?,?,?)`, a.ID, "unbound", 1000, 0); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/key/overview", "/api/key/overview?key=" + a.Key + "&key_id=1"} {
		w := keyViewRequest(r, "GET", path, "", nil)
		if w.Code != 401 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("anonymous query: %d", w.Code)
		}
	}
	if w := keyViewRequest(r, "GET", "/api/key/session", "", nil); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"authenticated":false}` {
		t.Fatalf("anonymous session status disclosed data: %d %s", w.Code, w.Body.String())
	}
	cookieA, gotA := keyViewLogin(t, r, a.Key, nil)
	_, gotB := keyViewLogin(t, r, b.Key, nil)
	if w := keyViewRequest(r, "GET", "/api/key/session", "", cookieA); w.Code != 200 || !strings.Contains(w.Body.String(), `"authenticated":true`) || !strings.Contains(w.Body.String(), `"remaining_usd":87`) {
		t.Fatalf("session status did not restore own balance: %s", w.Body.String())
	}
	if gotA.Summary.QuotaUSD != 140 || gotA.Summary.RemainingUSD != 87 || gotA.Shared == nil || gotA.Shared.RemainingUSD != 80 || gotA.Models["b"].Pool.RemainingUSD != 0 || gotA.Models["alias"].Pool.RemainingUSD != 7 || len(gotA.Models) != 3 {
		t.Fatalf("own balances and active pools: %+v", gotA)
	}
	if gotB.Summary.QuotaUSD != 999 || gotB.Models["b"].Pool != nil {
		t.Fatalf("other owner's pools: %+v", gotB)
	}
	w := keyViewRequest(r, "GET", "/api/key/overview?key_id=2", "", cookieA)
	if w.Code != 200 || strings.Contains(w.Body.String(), "999") || strings.Contains(w.Body.String(), a.Key) || strings.Contains(w.Body.String(), "owner-a") || strings.Contains(w.Body.String(), "private-note") {
		t.Fatalf("isolation or redaction: %s", w.Body.String())
	}
	var stored string
	if err := db.QueryRow(`SELECT token_hash FROM key_view_sessions WHERE key_id=?`, a.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == cookieA.Value || stored == a.Key || stored != keyViewTokenHash(cookieA.Value) {
		t.Fatal("raw query token stored")
	}
	for _, path := range []string{currentAdminPath() + "/api/keys", "/v1/test"} {
		if w := keyViewRequest(r, "GET", path, "", cookieA); w.Code != 401 {
			t.Fatalf("query cookie grants extra privileges: %s %d", path, w.Code)
		}
	}
	// Changes to a budget and billing are visible without another key verification.
	if err := dbChargeKey(a.ID, "a", 5, false); err != nil {
		t.Fatal(err)
	}
	w = keyViewRequest(r, "GET", "/api/key/overview", "", cookieA)
	if !strings.Contains(w.Body.String(), `"remaining_usd":75`) {
		t.Fatalf("balance did not refresh: %s", w.Body.String())
	}
	// Switching replaces this browser's previous session.
	newCookie, _ := keyViewLogin(t, r, b.Key, cookieA)
	if w := keyViewRequest(r, "GET", "/api/key/overview", "", cookieA); w.Code != 401 {
		t.Fatal("old session survives switching")
	}
	if w := keyViewRequest(r, "DELETE", "/api/key/session", "", newCookie); w.Code != 204 {
		t.Fatal("logout failed")
	}
	if w := keyViewRequest(r, "GET", "/api/key/overview", "", newCookie); w.Code != 401 {
		t.Fatal("logged-out session survives")
	}
}

func TestKeyViewDepletedUnlimitedAndUnboundPools(t *testing.T) {
	r := keyViewTestRouter(t)
	ak := publicTestKey(t, "depleted", 10, 0, false)
	ak.Models = "a,b,c"
	ak.ModelQuotas = map[string]ModelQuota{"b": {QuotaUSD: 0}, "c": {QuotaUSD: 20}}
	if err := dbUpdateKey(ak); err != nil {
		t.Fatal(err)
	}
	if err := dbChargeKey(ak.ID, "a", 12, false); err != nil {
		t.Fatal(err)
	}
	if err := dbChargeKey(ak.ID, "c", 7, true); err != nil {
		t.Fatal(err)
	}
	cookie, got := keyViewLogin(t, r, ak.Key, nil)
	if !got.Summary.Unlimited || got.Summary.QuotaUSD != 30 || got.Summary.RemainingUSD != 13 || !got.Models["b"].Pool.Unlimited || got.Shared.RemainingUSD != 0 {
		t.Fatalf("mixed finite/unlimited: %+v", got)
	}
	ak.Models = "c"
	if err := dbUpdateKey(ak); err != nil {
		t.Fatal(err)
	}
	var next KeyOverview
	w := keyViewRequest(r, "GET", "/api/key/overview", "", cookie)
	json.Unmarshal(w.Body.Bytes(), &next)
	if next.Summary.Unlimited || next.Summary.RemainingUSD != 13 || next.Shared != nil || next.Summary.QuotaUSD != 20 {
		t.Fatalf("unbound pools still counted: %+v", next)
	}
	ak.Models = ""
	if err := dbUpdateKey(ak); err != nil {
		t.Fatal(err)
	}
	w = keyViewRequest(r, "GET", "/api/key/overview", "", cookie)
	next = KeyOverview{}
	json.Unmarshal(w.Body.Bytes(), &next)
	if next.HasPools || next.Summary.RemainingUSD != 0 || len(next.Models) != 0 {
		t.Fatalf("unbound key pretends usable: %+v", next)
	}
}

func TestKeyViewRevocationExpiryAndRestart(t *testing.T) {
	r := keyViewTestRouter(t)
	ak := publicTestKey(t, "revoked", 10, 0, false)
	cookie, _ := keyViewLogin(t, r, ak.Key, nil)
	ak.Enabled = false
	if err := dbUpdateKey(ak); err != nil {
		t.Fatal(err)
	}
	ak.Enabled = true
	if err := dbUpdateKey(ak); err != nil {
		t.Fatal(err)
	}
	if w := keyViewRequest(r, "GET", "/api/key/overview", "", cookie); w.Code != 401 {
		t.Fatal("disabled then re-enabled key restored old session")
	}
	cookie, _ = keyViewLogin(t, r, ak.Key, nil)
	if _, err := db.Exec(`UPDATE key_view_sessions SET expires_at=?`, time.Now().Add(-time.Second).Unix()); err != nil {
		t.Fatal(err)
	}
	if w := keyViewRequest(r, "GET", "/api/key/overview", "", cookie); w.Code != 401 {
		t.Fatal("expired query session accepted")
	}
	cookie, _ = keyViewLogin(t, r, ak.Key, nil)
	if err := dbDeleteKey(ak.ID); err != nil {
		t.Fatal(err)
	}
	if w := keyViewRequest(r, "GET", "/api/key/overview", "", cookie); w.Code != 401 {
		t.Fatal("deleted key session accepted")
	}
	path := filepath.Join(t.TempDir(), "restart.db")
	db.Close()
	if err := openDB(path); err != nil {
		t.Fatal(err)
	}
	ak = publicTestKey(t, "restart", 8, 0, false)
	cookie, _ = keyViewLogin(t, r, ak.Key, nil)
	db.Close()
	if err := openDB(path); err != nil {
		t.Fatal(err)
	}
	if w := keyViewRequest(r, "GET", "/api/key/overview", "", cookie); w.Code != 200 {
		t.Fatalf("session lost after restart: %d", w.Code)
	}
}

func TestKeyViewInvalidInputOriginAndLoginRateLimit(t *testing.T) {
	r := keyViewTestRouter(t)
	for _, body := range []string{`{}`, `{"key":" "}`, `broken`} {
		if w := keyViewRequest(r, "POST", "/api/key/session", body, nil); w.Code != 400 {
			t.Fatalf("invalid body accepted: %d", w.Code)
		}
	}
	for i := 0; i < 2; i++ {
		if w := keyViewRequest(r, "POST", "/api/key/session", `{"key":"invalid"}`, nil); w.Code != 401 {
			t.Fatalf("invalid key: %d", w.Code)
		}
	}
	if w := keyViewRequest(r, "POST", "/api/key/session", `{"key":"invalid"}`, nil); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("login guessing not limited")
	}
	for _, method := range []string{"POST", "DELETE"} {
		request := httptest.NewRequest(method, "https://share.example/api/key/session", strings.NewReader(`{"key":"anything"}`))
		request.Header.Set("Origin", "https://other.example")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, request)
		if w.Code != 403 {
			t.Fatalf("cross-origin mutation allowed: %s %d", method, w.Code)
		}
	}
}

func TestKeyViewEmbeddedAndProxiedRequests(t *testing.T) {
	r := keyViewTestRouter(t)
	ak := publicTestKey(t, "origin-compatibility", 10, 0, false)
	body, _ := json.Marshal(map[string]string{"key": ak.Key})
	for _, origin := range []string{"null", "https://public-preview.example", ""} {
		request := httptest.NewRequest(http.MethodPost, "http://backend.internal/api/key/session", strings.NewReader(string(body)))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", origin)
		request.Header.Set("Sec-Fetch-Site", "cross-site")
		request.Header.Set(keyViewRequestHeader, "key-view")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, request)
		if w.Code != http.StatusOK {
			t.Fatalf("embedded/proxy metadata rejected a marked request: %q %d %s", origin, w.Code, w.Body.String())
		}
		cookies := w.Result().Cookies()
		if len(cookies) != 2 {
			t.Fatal("query session missing")
		}
		request = httptest.NewRequest(http.MethodDelete, "http://backend.internal/api/key/session", nil)
		request.Header.Set("Origin", origin)
		request.Header.Set("Sec-Fetch-Site", "cross-site")
		request.Header.Set(keyViewRequestHeader, "key-view")
		request.AddCookie(cookies[1])
		w = httptest.NewRecorder()
		r.ServeHTTP(w, request)
		if w.Code != http.StatusNoContent {
			t.Fatalf("embedded/proxy logout rejected: %q %d", origin, w.Code)
		}
	}
}

func TestKeyViewMutationsRequireHeaderAndDenyCORS(t *testing.T) {
	r := keyViewTestRouter(t)
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		for _, marker := range []string{"", "wrong"} {
			request := httptest.NewRequest(method, "https://share.example/api/key/session", strings.NewReader(`{"key":"anything"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Origin", "https://share.example")
			request.Header.Set("Sec-Fetch-Site", "same-origin")
			request.Header.Set(keyViewRequestHeader, marker)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, request)
			if w.Code != http.StatusForbidden {
				t.Fatalf("unmarked mutation accepted: %s %q %d", method, marker, w.Code)
			}
		}
		request := httptest.NewRequest(http.MethodOptions, "https://share.example/api/key/session", nil)
		request.Header.Set("Origin", "https://foreign.example")
		request.Header.Set("Access-Control-Request-Method", method)
		request.Header.Set("Access-Control-Request-Headers", "content-type,"+keyViewRequestHeader)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, request)
		if w.Code >= 200 && w.Code < 300 || w.Header().Get("Access-Control-Allow-Origin") != "" || w.Header().Get("Access-Control-Allow-Headers") != "" {
			t.Fatalf("foreign origin granted preflight permission: %d %v", w.Code, w.Header())
		}
	}
}
