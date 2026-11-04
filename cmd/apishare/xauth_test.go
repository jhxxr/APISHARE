package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

type mockXAuthServer struct {
	server       *httptest.Server
	clientID     string
	clientSecret string
	expectedCode string
	lastChallenge string
	userInfo     xauthUserInfo
}

func newMockXAuthServer(t *testing.T) *mockXAuthServer {
	t.Helper()
	m := &mockXAuthServer{
		clientID:     "client_apishare_test",
		clientSecret: "secret_apishare_test_123456",
		expectedCode: "xauth_code_abc123",
		userInfo: xauthUserInfo{
			Sub:               "usr_xauth_001",
			Email:             "alice@example.com",
			EmailVerified:     true,
			Name:              "Alice Chen",
			PreferredUsername: "alice",
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 m.server.URL,
			"authorization_endpoint": m.server.URL + "/authorize",
			"token_endpoint":         m.server.URL + "/oauth/token",
			"userinfo_endpoint":      m.server.URL + "/userinfo",
		})
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		cid, csec, ok := r.BasicAuth()
		if !ok {
			cid = r.FormValue("client_id")
			csec = r.FormValue("client_secret")
		} else {
			if decodedID, err := url.QueryUnescape(cid); err == nil {
				cid = decodedID
			}
			if decodedSec, err := url.QueryUnescape(csec); err == nil {
				csec = decodedSec
			}
		}
		if cid != m.clientID || csec != m.clientSecret {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
			return
		}
		if r.FormValue("code") != m.expectedCode {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		verifier := r.FormValue("code_verifier")
		if verifier == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "missing_code_verifier"})
			return
		}
		if m.lastChallenge != "" {
			sum := sha256.Sum256([]byte(verifier))
			gotChallenge := base64.RawURLEncoding.EncodeToString(sum[:])
			if gotChallenge != m.lastChallenge {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "pkce_mismatch"})
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "xauth_access_token_xyz",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer xauth_access_token_xyz" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m.userInfo)
	})
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

func performXAuthLogin(t *testing.T, r http.Handler, mock *mockXAuthServer) []*http.Cookie {
	t.Helper()
	loginReq := httptest.NewRequest(http.MethodGet, "/api/auth/login", nil)
	loginW := httptest.NewRecorder()
	r.ServeHTTP(loginW, loginReq)
	if loginW.Code != http.StatusFound {
		t.Fatalf("expected 302 from /api/auth/login, got %d (%s)", loginW.Code, loginW.Body.String())
	}
	loc, err := url.Parse(loginW.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse login redirect location: %v", err)
	}
	if !strings.HasPrefix(loc.String(), mock.server.URL+"/authorize?") {
		t.Fatalf("unexpected authorize redirect URL: %s", loc.String())
	}
	if loc.Query().Get("code_challenge_method") != "S256" || loc.Query().Get("code_challenge") == "" {
		t.Fatalf("expected S256 PKCE challenge in redirect URL: %s", loc.String())
	}
	mock.lastChallenge = loc.Query().Get("code_challenge")
	state := loc.Query().Get("state")
	flowCookies := loginW.Result().Cookies()

	cbURL := "/api/auth/callback?code=" + url.QueryEscape(mock.expectedCode) + "&state=" + url.QueryEscape(state)
	cbReq := httptest.NewRequest(http.MethodGet, cbURL, nil)
	for _, ck := range flowCookies {
		cbReq.AddCookie(ck)
	}
	cbW := httptest.NewRecorder()
	r.ServeHTTP(cbW, cbReq)
	if cbW.Code != http.StatusFound || cbW.Header().Get("Location") != "/user/" {
		t.Fatalf("expected callback redirect to /user/, got %d Location=%q", cbW.Code, cbW.Header().Get("Location"))
	}
	return cbW.Result().Cookies()
}

func TestXAuthEndToEndProvisioningDashboardRotationAndResetOnDelete(t *testing.T) {
	r := deploymentTestRouter(t)
	adminPass := "xauth-admin-password-123"
	setupRes := deploymentJSON(t, deploymentRequest(r, "PUT", "/setup/api/deployment", "",
		`{"new_password":"`+adminPass+`","confirm_password":"`+adminPass+`","admin_path":"admin"}`), http.StatusOK)
	adminToken := setupRes["token"].(string)

	mock := newMockXAuthServer(t)

	// 1. Configure XAuth and default gift provisioning in Admin Settings.
	settingsBody := map[string]any{
		"affinity_ttl_min":           30,
		"retry":                      2,
		"inject_stream_usage":        true,
		"site_title":                 "XAuth Hub",
		"log_retention_days":         7,
		"auto_pricing_enabled":       true,
		"xauth_enabled":              true,
		"xauth_issuer":               mock.server.URL,
		"xauth_client_id":            mock.clientID,
		"xauth_client_secret":        mock.clientSecret,
		"xauth_default_quota_usd":    12.5,
		"xauth_default_qps":          4,
		"xauth_default_concurrency":  8,
		"xauth_default_models":       "gpt-4o, claude-sonnet-4-5",
		"xauth_default_model_quotas": map[string]any{"claude-sonnet-4-5": map[string]any{"quota_usd": 3.0}},
	}
	rawSettings, _ := json.Marshal(settingsBody)
	deploymentJSON(t, deploymentRequest(r, "PUT", "/admin/api/settings", adminToken, string(rawSettings)), http.StatusOK)

	// Test XAuth connectivity endpoint.
	testResp := deploymentJSON(t, deploymentRequest(r, "POST", "/admin/api/settings/xauth/test", adminToken, `{}`), http.StatusOK)
	if testResp["ok"] != true {
		t.Fatalf("expected xauth connectivity test to succeed, got %v", testResp)
	}

	// 2. First-time user login via XAuth OIDC + PKCE.
	userCookies := performXAuthLogin(t, r, mock)

	// 3. Access /api/user/dashboard with session cookie.
	dashReq := httptest.NewRequest(http.MethodGet, "/api/user/dashboard", nil)
	for _, ck := range userCookies {
		dashReq.AddCookie(ck)
	}
	dashW := httptest.NewRecorder()
	r.ServeHTTP(dashW, dashReq)
	if dashW.Code != http.StatusOK {
		t.Fatalf("GET /api/user/dashboard returned %d: %s", dashW.Code, dashW.Body.String())
	}
	var dash userDashboardResponse
	if err := json.Unmarshal(dashW.Body.Bytes(), &dash); err != nil {
		t.Fatalf("decode dashboard response: %v", err)
	}
	if dash.User.Sub != "usr_xauth_001" || dash.User.Email != "alice@example.com" {
		t.Fatalf("unexpected dashboard user: %+v", dash.User)
	}
	if !strings.HasPrefix(dash.Key.Key, "sk-") || dash.Key.QuotaUSD != 12.5 || dash.Key.QPS != 4 || dash.Key.Concurrency != 8 {
		t.Fatalf("unexpected provisioned key: %+v", dash.Key)
	}
	if len(dash.Key.Models) != 2 || dash.Key.ModelQuotas["claude-sonnet-4-5"].QuotaUSD != 3.0 {
		t.Fatalf("unexpected provisioned models/quotas: models=%v quotas=%v", dash.Key.Models, dash.Key.ModelQuotas)
	}
	firstKeyID := dash.Key.ID
	firstKeySecret := dash.Key.Key

	// 4. Insert a call log for this user and another user, then verify /api/user/logs isolation & upstream redaction.
	if err := dbAddUsedUSD(firstKeyID, 1.25); err != nil {
		t.Fatalf("dbAddUsedUSD: %v", err)
	}
	if err := dbInsertLog(&CallLog{
		KeyID:            firstKeyID,
		KeyName:          dash.Key.Name,
		UpstreamID:       99,
		UpstreamName:     "internal-secret-upstream",
		Model:            "gpt-4o",
		Endpoint:         "/v1/chat/completions",
		PromptTokens:     120,
		CompletionTokens: 80,
		CostUSD:          1.25,
		Status:           200,
		LatencyMS:        450,
	}); err != nil {
		t.Fatalf("dbInsertLog user: %v", err)
	}
	if err := dbInsertLog(&CallLog{
		KeyID:            firstKeyID + 999,
		KeyName:          "other-user-key",
		UpstreamID:       88,
		UpstreamName:     "other-upstream",
		Model:            "gpt-4o",
		Endpoint:         "/v1/chat/completions",
		PromptTokens:     500,
		CompletionTokens: 500,
		CostUSD:          5.00,
		Status:           200,
	}); err != nil {
		t.Fatalf("dbInsertLog other: %v", err)
	}

	// Attempt to query /api/user/logs with forged key_id parameter -> must still only return firstKeyID's log.
	logsReq := httptest.NewRequest(http.MethodGet, "/api/user/logs?key_id=999999", nil)
	for _, ck := range userCookies {
		logsReq.AddCookie(ck)
	}
	logsW := httptest.NewRecorder()
	r.ServeHTTP(logsW, logsReq)
	if logsW.Code != http.StatusOK {
		t.Fatalf("GET /api/user/logs returned %d: %s", logsW.Code, logsW.Body.String())
	}
	var logRes struct {
		Total int       `json:"total"`
		Items []CallLog `json:"items"`
	}
	if err := json.Unmarshal(logsW.Body.Bytes(), &logRes); err != nil {
		t.Fatalf("decode user logs: %v", err)
	}
	if logRes.Total != 1 || len(logRes.Items) != 1 {
		t.Fatalf("expected exactly 1 log for user, got total=%d items=%d", logRes.Total, len(logRes.Items))
	}
	if logRes.Items[0].UpstreamID != 0 || logRes.Items[0].UpstreamName != "" {
		t.Fatalf("expected upstream details to be redacted in user logs, got %+v", logRes.Items[0])
	}

	// 5. Rotate key via POST /api/user/key/regenerate.
	rotReq := httptest.NewRequest(http.MethodPost, "/api/user/key/regenerate", nil)
	rotReq.Header.Set("X-APIShare-Request", "user-console")
	for _, ck := range userCookies {
		rotReq.AddCookie(ck)
	}
	rotW := httptest.NewRecorder()
	r.ServeHTTP(rotW, rotReq)
	if rotW.Code != http.StatusOK {
		t.Fatalf("POST /api/user/key/regenerate returned %d: %s", rotW.Code, rotW.Body.String())
	}
	var rotBody struct {
		OK  bool   `json:"ok"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rotW.Body.Bytes(), &rotBody); err != nil || !rotBody.OK {
		t.Fatalf("decode regenerate response: %v %+v", err, rotBody)
	}
	if rotBody.Key == "" || rotBody.Key == firstKeySecret || !strings.HasPrefix(rotBody.Key, "sk-") {
		t.Fatalf("expected rotated key to be a new sk- secret, got %q (old %q)", rotBody.Key, firstKeySecret)
	}
	if _, err := dbGetKeyByKey(firstKeySecret); err == nil {
		t.Fatal("old key string must not resolve after rotation")
	}
	rotatedKey, err := dbGetKeyByID(firstKeyID)
	if err != nil || rotatedKey.UsedUSD != 1.25 || rotatedKey.QuotaUSD != 12.5 {
		t.Fatalf("rotated key must preserve id, quota and used amount: %+v err=%v", rotatedKey, err)
	}

	// 6. Repeat login must preserve existing key, usage, and any admin overrides.
	repeatCookies := performXAuthLogin(t, r, mock)
	dashReq2 := httptest.NewRequest(http.MethodGet, "/api/user/dashboard", nil)
	for _, ck := range repeatCookies {
		dashReq2.AddCookie(ck)
	}
	dashW2 := httptest.NewRecorder()
	r.ServeHTTP(dashW2, dashReq2)
	var dash2 userDashboardResponse
	_ = json.Unmarshal(dashW2.Body.Bytes(), &dash2)
	if dash2.Key.ID != firstKeyID || dash2.Key.Key != rotBody.Key || dash2.Key.UsedUSD != 1.25 {
		t.Fatalf("repeat login must keep existing key & usage, got %+v", dash2.Key)
	}

	// 7. Admin deletes the XAuth user's key -> existing session is invalidated, and next login recreates key with 0 usable quota.
	delPath := "/admin/api/keys/" + strconv.FormatInt(firstKeyID, 10)
	deploymentJSON(t, deploymentRequest(r, "DELETE", delPath, adminToken, ""), http.StatusOK)

	// Old session cookie should now get 401.
	dashAfterDel := httptest.NewRequest(http.MethodGet, "/api/user/dashboard", nil)
	for _, ck := range repeatCookies {
		dashAfterDel.AddCookie(ck)
	}
	dashAfterDelW := httptest.NewRecorder()
	r.ServeHTTP(dashAfterDelW, dashAfterDel)
	if dashAfterDelW.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after admin deleted user's key, got %d", dashAfterDelW.Code)
	}

	// User logs in again via XAuth -> recreated key has 0 quota and no bound models.
	reloginCookies := performXAuthLogin(t, r, mock)
	dashReq3 := httptest.NewRequest(http.MethodGet, "/api/user/dashboard", nil)
	for _, ck := range reloginCookies {
		dashReq3.AddCookie(ck)
	}
	dashW3 := httptest.NewRecorder()
	r.ServeHTTP(dashW3, dashReq3)
	if dashW3.Code != http.StatusOK {
		t.Fatalf("expected 200 after relogin, got %d: %s", dashW3.Code, dashW3.Body.String())
	}
	var dash3 userDashboardResponse
	_ = json.Unmarshal(dashW3.Body.Bytes(), &dash3)
	if dash3.Key.ID == firstKeyID {
		t.Fatalf("expected new key ID after deletion, still got %d", dash3.Key.ID)
	}
	if dash3.Key.QuotaUSD != 0 || len(dash3.Key.Models) != 0 {
		t.Fatalf("recreated key after admin deletion must have 0 quota and 0 bound models, got quota=%v models=%v",
			dash3.Key.QuotaUSD, dash3.Key.Models)
	}
}

func TestBatchAddModelsSkipsExistingAndAddsMissing(t *testing.T) {
	r := deploymentTestRouter(t)
	adminPass := "batch-models-admin-123"
	setupRes := deploymentJSON(t, deploymentRequest(r, "PUT", "/setup/api/deployment", "",
		`{"new_password":"`+adminPass+`","confirm_password":"`+adminPass+`","admin_path":"admin"}`), http.StatusOK)
	adminToken := setupRes["token"].(string)

	// Key 1 already has gpt-4o with an independent $5 quota.
	k1 := &ApiKey{
		Name:        "Key One",
		Key:         "sk-batch-test-1",
		QuotaUSD:    10,
		QPS:         3,
		Concurrency: 5,
		Enabled:     true,
		Models:      "gpt-4o",
		ModelQuotas: map[string]ModelQuota{
			"gpt-4o": {QuotaUSD: 5},
		},
	}
	if err := dbInsertKey(k1); err != nil {
		t.Fatalf("dbInsertKey k1: %v", err)
	}

	// Key 2 has no models yet.
	k2 := &ApiKey{
		Name:        "Key Two",
		Key:         "sk-batch-test-2",
		QuotaUSD:    10,
		QPS:         3,
		Concurrency: 5,
		Enabled:     true,
		Models:      "",
	}
	if err := dbInsertKey(k2); err != nil {
		t.Fatalf("dbInsertKey k2: %v", err)
	}

	// Batch-add gpt-4o (shared) and deepseek-r1 (independent $2) to all keys.
	// For Key 1: gpt-4o already exists, so its $5 independent quota MUST be preserved; deepseek-r1 is added with $2.
	// For Key 2: both gpt-4o (shared) and deepseek-r1 ($2 independent) are added.
	batchPayload := `{
		"scope": "selected",
		"key_ids": [` + strconv.FormatInt(k1.ID, 10) + `,` + strconv.FormatInt(k2.ID, 10) + `],
		"models": "gpt-4o, deepseek-r1",
		"model_quotas": {
			"deepseek-r1": {"quota_usd": 2}
		}
	}`
	res := deploymentJSON(t, deploymentRequest(r, "POST", "/admin/api/keys/batch-models", adminToken, batchPayload), http.StatusOK)
	if res["updated_keys"] != float64(2) || res["added_models"] != float64(3) {
		t.Fatalf("unexpected batch result: %+v", res)
	}

	got1, err := dbGetKeyByID(k1.ID)
	if err != nil {
		t.Fatalf("dbGetKeyByID k1: %v", err)
	}
	if got1.Models != "gpt-4o,deepseek-r1" {
		t.Fatalf("k1 models = %q, want gpt-4o,deepseek-r1", got1.Models)
	}
	if got1.ModelQuotas["gpt-4o"].QuotaUSD != 5 {
		t.Fatalf("k1 existing gpt-4o independent quota must be preserved as 5, got %+v", got1.ModelQuotas["gpt-4o"])
	}
	if got1.ModelQuotas["deepseek-r1"].QuotaUSD != 2 {
		t.Fatalf("k1 newly added deepseek-r1 independent quota = %+v, want 2", got1.ModelQuotas["deepseek-r1"])
	}

	got2, err := dbGetKeyByID(k2.ID)
	if err != nil {
		t.Fatalf("dbGetKeyByID k2: %v", err)
	}
	if got2.Models != "gpt-4o,deepseek-r1" {
		t.Fatalf("k2 models = %q, want gpt-4o,deepseek-r1", got2.Models)
	}
	if _, hasIndependent := got2.ModelQuotas["gpt-4o"]; hasIndependent {
		t.Fatalf("k2 gpt-4o should use shared quota, got %+v", got2.ModelQuotas["gpt-4o"])
	}
	if got2.ModelQuotas["deepseek-r1"].QuotaUSD != 2 {
		t.Fatalf("k2 deepseek-r1 independent quota = %+v, want 2", got2.ModelQuotas["deepseek-r1"])
	}
}

func TestPerKeyDailyRollupPreservesUserDashboardTelemetry(t *testing.T) {
	r := deploymentTestRouter(t)
	_ = r
	ak := &ApiKey{
		Name:        "Rollup User Key",
		Key:         "sk-rollup-user-key",
		QuotaUSD:    20,
		QPS:         3,
		Concurrency: 5,
		Enabled:     true,
		Models:      "gpt-4o",
	}
	if err := dbInsertKey(ak); err != nil {
		t.Fatalf("dbInsertKey: %v", err)
	}
	rec := &xauthUserRecord{
		Sub:         "usr_rollup_1",
		Email:       "rollup@example.com",
		Name:        "Rollup User",
		KeyID:       ak.ID,
		GiftClaimed: true,
	}

	// Insert an expired log from 10 days ago (within the 30-day chart window, older than 7-day retention).
	now := beijingNow()
	tenDaysAgo := now.AddDate(0, 0, -10).In(beijingLocation).Format(time.RFC3339)
	_, err := db.Exec(`INSERT INTO call_logs(key_id,key_name,upstream_id,upstream_name,model,endpoint,
		prompt_tokens,completion_tokens,cost_usd,status,latency_ms,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		ak.ID, ak.Name, 1, "up", "gpt-4o", "/v1/chat/completions", 300, 200, 0.75, 200, 120, tenDaysAgo)
	if err != nil {
		t.Fatalf("insert expired call_log: %v", err)
	}

	// Roll up logs older than 7 days.
	cutoff, _ := logRetentionCutoff(7, now)
	if _, err := rollupExpiredLogs(cutoff); err != nil {
		t.Fatalf("rollupExpiredLogs: %v", err)
	}

	dash, err := buildUserDashboard(now, rec, ak)
	if err != nil {
		t.Fatalf("buildUserDashboard: %v", err)
	}
	if dash.Summary.Calls != 1 || dash.Summary.PromptTokens != 300 || dash.Summary.CompletionTokens != 200 || dash.Summary.CostUSD != 0.75 {
		t.Fatalf("expected rolled-up per-key telemetry to be preserved in user dashboard, got %+v", dash.Summary)
	}
}
