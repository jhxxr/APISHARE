package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestKeyRPMTPMAdminValidationAndLegacyDefaults(t *testing.T) {
	modelsTestDB(t)
	if err := settingsSet("session_secret", "rpm-tpm-test-secret"); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	registerAdmin(r)
	token := makeAdminToken()
	path := currentAdminPath() + "/api/keys"
	result := deploymentJSON(t, deploymentRequest(r, "POST", path, token, `{"name":"bounded","models":"model-a","rpm":12,"tpm":900}`), http.StatusOK)
	id := int64(result["id"].(float64))
	if result["rpm"] != float64(12) || result["tpm"] != float64(900) {
		t.Fatalf("created key lost RPM/TPM values: %+v", result)
	}
	keyPath := fmt.Sprintf("%s/%d", path, id)
	deploymentJSON(t, deploymentRequest(r, "PATCH", keyPath, token, `{"rpm":0,"tpm":123}`), http.StatusOK)
	ak, err := dbGetKeyByID(id)
	if err != nil || ak.RPM != 0 || ak.TPM != 123 {
		t.Fatalf("updated key RPM/TPM = %+v, %v", ak, err)
	}
	for _, body := range []string{`{"rpm":-1}`, `{"tpm":-1}`, `{"rpm":1.5}`, `{"tpm":1.5}`} {
		if got := deploymentRequest(r, "PATCH", keyPath, token, body).Code; got != http.StatusBadRequest {
			t.Fatalf("PATCH %s = %d, want 400", body, got)
		}
		invalidCreate := `{"name":"invalid",` + strings.Trim(body, "{}") + `}`
		if got := deploymentRequest(r, "POST", path, token, invalidCreate).Code; got != http.StatusBadRequest {
			t.Fatalf("POST %s = %d, want 400", invalidCreate, got)
		}
	}

	// The legacy schema has no RPM/TPM columns; migration assigns unlimited defaults.
	currentDB := db
	legacyDB, err := sql.Open("sqlite", "file:"+filepathToSlash(filepath.Join(t.TempDir(), "legacy-rpm-tpm.db")))
	if err != nil {
		t.Fatal(err)
	}
	legacyDB.SetMaxOpenConns(1)
	db = legacyDB
	if _, err := legacyDB.Exec(`CREATE TABLE api_keys(id INTEGER PRIMARY KEY,name TEXT NOT NULL DEFAULT '',key TEXT NOT NULL UNIQUE,quota_usd REAL NOT NULL DEFAULT 0,used_usd REAL NOT NULL DEFAULT 0,qps REAL NOT NULL DEFAULT 3,concurrency INTEGER NOT NULL DEFAULT 5,enabled INTEGER NOT NULL DEFAULT 1,show_on_home INTEGER NOT NULL DEFAULT 0,note TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL DEFAULT '');
		INSERT INTO api_keys(id,name,key) VALUES(1,'legacy','sk-legacy-rpm-tpm')`); err != nil {
		db = currentDB
		legacyDB.Close()
		t.Fatal(err)
	}
	if err := migrate(); err != nil {
		db = currentDB
		legacyDB.Close()
		t.Fatal(err)
	}
	legacy, err := dbGetKeyByID(1)
	db = currentDB
	legacyDB.Close()
	if err != nil || legacy.RPM != 0 || legacy.TPM != 0 {
		t.Fatalf("legacy RPM/TPM defaults = %+v, %v", legacy, err)
	}
}

func TestKeyRPMTPMWindowLimitsAndTokenAccounting(t *testing.T) {
	limit := getLimit(ApiKey{ID: 938271, QPS: 1000, Concurrency: 10, RPM: 3, TPM: 10})
	if exceeded := limit.admit(); exceeded != "" {
		t.Fatalf("first request rejected by %s", exceeded)
	}
	if exceeded := limit.admit(); exceeded != "" {
		t.Fatalf("second request rejected by %s", exceeded)
	}
	limit.chargeTokens(11)
	if exceeded := limit.admit(); exceeded != "TPM" {
		t.Fatalf("third request limit = %q, want TPM", exceeded)
	}
	limit.usage.mu.Lock()
	limit.usage.start = time.Now().Add(-time.Minute)
	limit.usage.mu.Unlock()
	if exceeded := limit.admit(); exceeded != "" {
		t.Fatalf("request after window reset rejected by %s", exceeded)
	}
	if exceeded := limit.admit(); exceeded != "" {
		t.Fatalf("second request after window reset rejected by %s", exceeded)
	}
	if exceeded := limit.admit(); exceeded != "" {
		t.Fatalf("third request after window reset rejected by %s", exceeded)
	}
	if exceeded := limit.admit(); exceeded != "RPM" {
		t.Fatalf("over-RPM request = %q, want RPM", exceeded)
	}
}

func TestKeyRPMTPMHTTP429(t *testing.T) {
	limReg.Lock()
	delete(limReg.m, 938272)
	limReg.Unlock()
	r := gin.New()
	r.POST("/limited", func(c *gin.Context) { c.Set("apiKey", ApiKey{ID: 938272, QPS: 1000, Concurrency: 2, RPM: 1}); c.Next() }, keyRateLimit(), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	for i, want := range []int{http.StatusNoContent, http.StatusTooManyRequests} {
		req := httptest.NewRequest(http.MethodPost, "/limited", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("request %d status = %d, want %d: %s", i+1, w.Code, want, w.Body.String())
		}
	}
}
