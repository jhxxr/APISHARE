package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

func deploymentTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	previousPath := currentAdminPath()
	modelsTestDB(t)
	t.Cleanup(func() {
		adminConfigMu.Lock()
		adminPath = previousPath
		adminConfigMu.Unlock()
	})
	t.Setenv("ADMIN_PASSWORD", "initial-test-password")
	t.Setenv("ADMIN_PATH", "")
	if err := bootstrapAdmin(); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	key := &ApiKey{Name: "deployment test", Key: "sk-deployment-test", Enabled: true, QPS: 100, Concurrency: 10}
	if err := dbInsertKey(key); err != nil {
		t.Fatal(err)
	}
	registerModelRoutes(r.Group("/v1", keyAuth(), keyRateLimit()))
	registerAdmin(r)
	registerPublic(r)
	r.GET("/healthz", func(c *gin.Context) { c.String(200, "ok") })
	r.NoRoute(func(c *gin.Context) { c.String(404, "404 page not found") })
	return r
}

func deploymentRequest(r http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func deploymentJSON(t *testing.T, w *httptest.ResponseRecorder, status int) map[string]any {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d: %s", w.Code, status, w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestDeploymentSetupRoutingSessionsAndRestart(t *testing.T) {
	r := deploymentTestRouter(t)
	for _, path := range []string{"/", "/admin/"} {
		w := deploymentRequest(r, "GET", path, "", "")
		if w.Code != 307 || w.Header().Get("Location") != "/setup/" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("first-run redirect %s = %d %s", path, w.Code, w.Header().Get("Location"))
		}
	}
	for _, path := range []string{"/setup/", "/setup/static/style.css", "/setup/static/deployment.js", "/setup/static/setup.js"} {
		if w := deploymentRequest(r, "GET", path, "", ""); w.Code != 200 {
			t.Fatalf("setup page/asset %s returned %d", path, w.Code)
		}
	}
	config := deploymentJSON(t, deploymentRequest(r, "GET", "/setup/api/deployment", "", ""), 200)
	if config["setup_required"] != true || config["admin_path"] != "/admin" {
		t.Fatalf("first-run configuration = %+v", config)
	}
	for _, setting := range []string{"admin_pass_hash", "admin_path"} {
		if value, err := settingsGet(setting); err != nil || value != "" {
			t.Fatalf("%s was saved before the setup form: %q %v", setting, value, err)
		}
	}
	deploymentJSON(t, deploymentRequest(r, "POST", "/admin/api/login", "", `{"password":"initial-test-password"}`), 409)
	deploymentJSON(t, deploymentRequest(r, "GET", "/admin/api/deployment", "", ""), 401)
	deploymentJSON(t, deploymentRequest(r, "PUT", "/admin/api/deployment", "", `{"admin_path":"console-7f3"}`), 401)
	oldToken := makeAdminToken()
	result := deploymentJSON(t, deploymentRequest(r, "PUT", "/setup/api/deployment", "",
		`{"new_password":"new-test-password-123","confirm_password":"new-test-password-123","admin_path":"/console-7f3/"}`), 200)
	newToken := result["token"].(string)
	if result["admin_path"] != "/console-7f3" || result["setup_required"] != false || validAdminToken(oldToken) || !validAdminToken(newToken) {
		t.Fatalf("configuration or session rotation failed: %+v", result)
	}
	for _, path := range []string{"/setup/api/deployment", "/setup/static/setup.js", "/setup/static/app.js"} {
		if w := deploymentRequest(r, "GET", path, "", ""); w.Code != 404 || strings.Contains(w.Body.String(), "/console-7f3") {
			t.Fatalf("finished setup still exposed %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if w := deploymentRequest(r, "PUT", "/setup/api/deployment", "",
		`{"new_password":"another-password-123","confirm_password":"another-password-123","admin_path":"overwrite"}`); w.Code != 404 {
		t.Fatalf("completed initialization still accepted updates: %d", w.Code)
	}
	w := deploymentRequest(r, "GET", "/setup/", "", "")
	if w.Code != 307 || w.Header().Get("Location") != "/" {
		t.Fatalf("completed setup page = %d %s", w.Code, w.Header().Get("Location"))
	}
	for _, path := range []string{"/admin", "/admin/", "/admin/static/app.js", "/admin/api/me", "/console-7f3-other/", "/api/deployment"} {
		if w := deploymentRequest(r, "GET", path, newToken, ""); w.Code != 404 {
			t.Fatalf("old/unregistered path %s returned %d", path, w.Code)
		}
	}
	for _, path := range []string{"/", "/public/static/home.css", "/healthz", "/console-7f3/", "/console-7f3/static/style.css", "/console-7f3/static/app.js", "/console-7f3/static/deployment.js"} {
		if w := deploymentRequest(r, "GET", path, "", ""); w.Code != 200 {
			t.Fatalf("active path %s returned %d", path, w.Code)
		}
	}
	w = deploymentRequest(r, "GET", "/console-7f3?next=settings", "", "")
	if w.Code != 301 || w.Header().Get("Location") != "/console-7f3/?next=settings" {
		t.Fatalf("trailing slash redirect = %d %s", w.Code, w.Header().Get("Location"))
	}
	deploymentJSON(t, deploymentRequest(r, "GET", "/console-7f3/api/me", oldToken, ""), 401)
	me := deploymentJSON(t, deploymentRequest(r, "GET", "/console-7f3/api/me", newToken, ""), 200)
	if me["setup_required"] != false || me["admin_path"] != "/console-7f3" {
		t.Fatalf("me = %+v", me)
	}
	deploymentJSON(t, deploymentRequest(r, "POST", "/console-7f3/api/login", "", `{"password":"initial-test-password"}`), 401)
	deploymentJSON(t, deploymentRequest(r, "POST", "/console-7f3/api/login", "", `{"password":"new-test-password-123"}`), 200)
	// A restarted deployment must preserve UI changes over initial environment values.
	t.Setenv("ADMIN_PATH", "other-environment-path")
	t.Setenv("ADMIN_PASSWORD", "other-environment-password")
	if err := bootstrapAdmin(); err != nil || currentAdminPath() != "/console-7f3" || adminSetupRequired() {
		t.Fatalf("restart lost configuration: %v", err)
	}
	hash, _ := settingsGet("admin_pass_hash")
	if hash == "new-test-password-123" || bcrypt.CompareHashAndPassword([]byte(hash), []byte("new-test-password-123")) != nil {
		t.Fatal("password was not stored as a valid hash")
	}
	// Path-only updates preserve the password and move the session cookie.
	w = deploymentRequest(r, "PUT", "/console-7f3/api/deployment", newToken,
		`{"current_password":"new-test-password-123","admin_path":"ops-final"}`)
	deploymentJSON(t, w, 200)
	var deletedOld, createdNew bool
	for _, cookie := range w.Result().Cookies() {
		deletedOld = deletedOld || (cookie.Path == "/console-7f3" && cookie.MaxAge < 0)
		createdNew = createdNew || (cookie.Path == "/ops-final" && cookie.Value != "" && cookie.HttpOnly && cookie.SameSite == http.SameSiteLaxMode)
	}
	if !deletedOld || !createdNew {
		t.Fatal("session cookie was not moved to the new path")
	}
	deploymentJSON(t, deploymentRequest(r, "POST", "/ops-final/api/login", "", `{"password":"new-test-password-123"}`), 200)
	models := deploymentJSON(t, deploymentRequest(r, "GET", "/v1/models", "sk-deployment-test", ""), 200)
	if models["object"] != "list" {
		t.Fatal("path changes broke the existing API endpoint/key")
	}
}

func TestDeploymentValidationDoesNotMutateConfiguration(t *testing.T) {
	r := deploymentTestRouter(t)
	token := makeAdminToken()
	initialHash, _ := settingsGet("admin_pass_hash")
	for _, tc := range []struct{ body, field string }{
		{`{"current_password":"initial-test-password","admin_path":"console"}`, "new_password"},
		{`{"current_password":"initial-test-password","new_password":"short","confirm_password":"short","admin_path":"console"}`, "new_password"},
		{`{"current_password":"initial-test-password","new_password":"new-test-password-123","confirm_password":"different-password-123","admin_path":"console"}`, "confirm_password"},
		{`{"current_password":"initial-test-password","new_password":"new-test-password-123","confirm_password":"new-test-password-123","admin_path":"api"}`, "admin_path"},
		{`{"new_password":"new-test-password-123","confirm_password":"new-test-password-123","admin_path":"setup"}`, "admin_path"},
		{`{"current_password":"initial-test-password","new_password":"new-test-password-123","confirm_password":"new-test-password-123","admin_path":"../escape"}`, "admin_path"},
		{`{"admin_path":123}`, ""},
	} {
		result := deploymentJSON(t, deploymentRequest(r, "PUT", "/setup/api/deployment", "", tc.body), 400)
		if result["field"] != tc.field {
			t.Fatalf("field = %v, want %s", result["field"], tc.field)
		}
		hash, _ := settingsGet("admin_pass_hash")
		if currentAdminPath() != "/admin" || hash != initialHash || !validAdminToken(token) || !adminSetupRequired() {
			t.Fatal("invalid update changed configuration")
		}
	}
	result := deploymentJSON(t, deploymentRequest(r, "PUT", "/setup/api/deployment", "",
		`{"new_password":"new-test-password-123","confirm_password":"new-test-password-123","admin_path":"console"}`), 200)
	token = result["token"].(string)
	hash, _ := settingsGet("admin_pass_hash")
	result = deploymentJSON(t, deploymentRequest(r, "PUT", "/console/api/deployment", token,
		`{"current_password":"wrong","new_password":"another-password-123","confirm_password":"another-password-123","admin_path":"changed"}`), 400)
	storedHash, _ := settingsGet("admin_pass_hash")
	if result["field"] != "current_password" || currentAdminPath() != "/console" || storedHash != hash || !validAdminToken(token) {
		t.Fatal("an unverified administrator update changed configuration")
	}
	deploymentJSON(t, deploymentRequest(r, "PUT", "/console/api/settings", token, `{"new_password":"unverified-password"}`), 400)
	for _, value := range []string{"/", "v1", "v1beta", "public", "healthz", "static", "setup", "SETUP", "API", "ab", "-ops", "foo/bar", "foo?bar", "foo#bar", "%2fops", strings.Repeat("a", 65)} {
		if _, err := normalizeAdminPath(value); err == nil {
			t.Errorf("reserved or malformed path accepted: %s", value)
		}
	}
}

func TestDeploymentFailedSaveIsAtomic(t *testing.T) {
	r := deploymentTestRouter(t)
	token := makeAdminToken()
	hash, _ := settingsGet("admin_pass_hash")
	_, err := db.Exec(`CREATE TRIGGER reject_security_save BEFORE UPDATE ON settings
		WHEN NEW.k = 'session_secret' BEGIN SELECT RAISE(ABORT, 'test write failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	deploymentJSON(t, deploymentRequest(r, "PUT", "/setup/api/deployment", "",
		`{"new_password":"new-test-password-123","confirm_password":"new-test-password-123","admin_path":"console-new"}`), 500)
	storedHash, _ := settingsGet("admin_pass_hash")
	storedPath, _ := settingsGet("admin_path")
	if storedHash != hash || storedPath != "" || currentAdminPath() != "/admin" || !validAdminToken(token) || !adminSetupRequired() {
		t.Fatal("failed transaction partially applied security settings")
	}
}

func TestDeploymentLegacyInstallationMigration(t *testing.T) {
	r := deploymentTestRouter(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("existing-password-123"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := settingsSet("admin_pass_hash", string(hash)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM settings WHERE k IN ('admin_setup_completed', 'admin_path')`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADMIN_PATH", "existing-console")
	if err := bootstrapAdmin(); err != nil || adminSetupRequired() || currentAdminPath() != "/existing-console" {
		t.Fatalf("legacy migration = %s, setup=%v, error=%v", currentAdminPath(), adminSetupRequired(), err)
	}
	if w := deploymentRequest(r, "GET", "/", "", ""); w.Code != 200 {
		t.Fatalf("legacy homepage returned %d", w.Code)
	}
	deploymentJSON(t, deploymentRequest(r, "POST", "/existing-console/api/login", "", `{"password":"existing-password-123"}`), 200)
}

func TestDeploymentPendingRestartAndConcurrentSetup(t *testing.T) {
	r := deploymentTestRouter(t)
	if err := bootstrapAdmin(); err != nil || !adminSetupRequired() {
		t.Fatalf("pending restart lost setup state: %v", err)
	}
	if w := deploymentRequest(r, "GET", "/", "", ""); w.Code != 307 {
		t.Fatalf("pending restart homepage returned %d", w.Code)
	}
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for range 2 {
		wg.Go(func() {
			w := deploymentRequest(r, "PUT", "/setup/api/deployment", "",
				`{"new_password":"new-test-password-123","confirm_password":"new-test-password-123","admin_path":"console"}`)
			results <- w.Code
		})
	}
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for status := range results {
		counts[status]++
	}
	if counts[200] != 1 || counts[404] != 1 || adminSetupRequired() {
		t.Fatalf("concurrent initialization did not save exactly once: %v", counts)
	}
}
