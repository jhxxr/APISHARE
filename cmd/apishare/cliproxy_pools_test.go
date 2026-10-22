package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"apishare/internal/cliproxy"
)

func withTestCLIProxyPools(t *testing.T) {
	t.Helper()
	cliProxyPools.Lock()
	oldRoot, oldItems := cliProxyPools.root, cliProxyPools.items
	cliProxyPools.root, cliProxyPools.items = filepath.Join(t.TempDir(), "pools"), make(map[int64]*cliproxy.Manager)
	cliProxyPools.Unlock()
	t.Cleanup(func() {
		cliProxyPools.Lock()
		items := cliProxyPools.items
		cliProxyPools.root, cliProxyPools.items = oldRoot, oldItems
		cliProxyPools.Unlock()
		for _, manager := range items {
			manager.Close()
		}
	})
}

func TestAccountUptimeFailureKeepsCredentialManagementAvailable(t *testing.T) {
	router := testProxyAdminRouter(t)
	withTestCLIProxyPools(t)
	root := t.TempDir()
	manager, err := cliproxy.NewManager(root, 18317)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SeedCredentials(map[string]json.RawMessage{
		"safe.json": json.RawMessage(`{"type":"codex","email":"safe@example.invalid","access_token":"PRIVATE-TOKEN","disabled":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "account-uptime.db"), 0700); err != nil {
		t.Fatal(err)
	}
	base, key := manager.Connection()
	up := &Upstream{Name: "stopped", Type: "cliproxy", BaseURL: base, APIKey: key, Weight: 1, Enabled: true}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cliproxy_pools(upstream_id,data_dir,proxy_id) VALUES(?,?,0)`, up.ID, root); err != nil {
		t.Fatal(err)
	}
	cliProxyPools.Lock()
	cliProxyPools.items[up.ID] = manager
	cliProxyPools.Unlock()
	response := proxyAdminRequest(router, "GET", fmt.Sprintf("upstreams/%d/cliproxy/credentials", up.ID), "", true)
	if response.Code != 200 {
		t.Fatalf("management unavailable: %d %s", response.Code, response.Body.String())
	}
	var payload struct {
		Files       []cliproxy.Credential `json:"files"`
		UptimeError string                `json:"uptime_error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || len(payload.Files) != 1 || payload.UptimeError == "" || payload.Files[0].Uptime != nil {
		t.Fatalf("unavailable statistics looked like valid uptime: %s %v", response.Body.String(), err)
	}
	if strings.Contains(response.Body.String(), "PRIVATE-TOKEN") || strings.Contains(response.Body.String(), root) {
		t.Fatal("statistics failure exposed private credential or filesystem details")
	}
}

func TestManagedCLIProxyPoolsScopeAccountsAndProxyBindings(t *testing.T) {
	router := testProxyAdminRouter(t)
	withTestCLIProxyPools(t)
	create := func(name string) int64 {
		t.Helper()
		response := proxyAdminRequest(router, "POST", "upstreams", fmt.Sprintf(`{"name":%q,"type":"cliproxy"}`, name), true)
		if response.Code != 200 {
			t.Fatalf("create pool: %d %s", response.Code, response.Body.String())
		}
		var result struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.ID
	}
	a, b := create("pool a"), create("pool b")
	upA, _ := dbGetUpstream(a)
	upB, _ := dbGetUpstream(b)
	if upA.BaseURL == upB.BaseURL || upA.APIKey == upB.APIKey || upA.ProxyID != 0 || upB.ProxyID != 0 {
		t.Fatal("pool listeners/keys are not isolated")
	}
	scope := func(id int64, suffix string) string { return fmt.Sprintf("upstreams/%d/cliproxy/%s", id, suffix) }
	coreLoginSessions.Lock()
	coreLoginSessions.items["pool-scope-fixture"] = coreLoginSession{
		owner: sha256.Sum256([]byte(makeAdminToken())), poolID: a, expires: time.Now().Add(time.Minute), provider: "codex",
	}
	coreLoginSessions.Unlock()
	t.Cleanup(func() {
		coreLoginSessions.Lock()
		delete(coreLoginSessions.items, "pool-scope-fixture")
		coreLoginSessions.Unlock()
	})
	if response := proxyAdminRequest(router, "GET", scope(b, "oauth/status?state=pool-scope-fixture"), "", true); response.Code != 404 {
		t.Fatal("OAuth session status crossed pool boundaries")
	}
	if response := proxyAdminRequest(router, "POST", scope(b, "oauth/callback"), `{"state":"pool-scope-fixture","redirect_url":"http://localhost/callback?state=pool-scope-fixture&code=fixture"}`, true); response.Code != 404 {
		t.Fatal("OAuth callback crossed pool boundaries")
	}
	credential := `{"name":"only-a.json","credential":{"type":"codex","email":"pool-a@example.invalid","access_token":"PRIVATE-ACCESS","expired":"2099-01-01T00:00:00Z","disabled":true}}`
	if response := proxyAdminRequest(router, "POST", scope(a, "credentials/import"), credential, true); response.Code != 200 {
		t.Fatalf("import: %d %s", response.Code, response.Body.String())
	}
	var poolRoot string
	if err := db.QueryRow(`SELECT data_dir FROM cliproxy_pools WHERE upstream_id=?`, a).Scan(&poolRoot); err != nil {
		t.Fatal(err)
	}
	stats, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(poolRoot, "account-uptime.db"))+"?_pragma=busy_timeout(2000)")
	if err != nil {
		t.Fatal(err)
	}
	for _, success := range []int{1, 1, 0} {
		if _, err := stats.Exec(`INSERT INTO account_results(auth_id,observed_at,success) VALUES(?,?,?)`, "only-a.json", time.Now().Add(-time.Minute).UnixMilli(), success); err != nil {
			t.Fatal(err)
		}
	}
	stats.Close()
	for _, id := range []int64{a, b} {
		response := proxyAdminRequest(router, "GET", scope(id, "credentials"), "", true)
		if response.Code != 200 || strings.Contains(response.Body.String(), "PRIVATE") {
			t.Fatalf("credential list: %d %s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "only-a.json") != (id == a) {
			t.Fatal("credentials leaked across pools")
		}
		if id == a {
			var payload struct {
				Files []cliproxy.Credential `json:"files"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || len(payload.Files) != 1 || payload.Files[0].Uptime == nil || payload.Files[0].Uptime.Calls != 3 || payload.Files[0].Uptime.Successes != 2 {
				t.Fatalf("per-account uptime: %s %v", response.Body.String(), err)
			}
		}
	}
	for _, operation := range []struct{ method, path, body string }{
		{"PATCH", "credentials/status", `{"name":"only-a.json","disabled":false}`},
		{"PATCH", "credentials/proxy", `{"name":"only-a.json","proxy_id":0}`},
		{"POST", "credentials/refresh", `{"name":"only-a.json"}`},
		{"DELETE", "credentials?name=only-a.json", ""},
	} {
		if response := proxyAdminRequest(router, operation.method, scope(b, operation.path), operation.body, true); response.Code != 404 {
			t.Fatalf("cross-pool %s accepted: %d %s", operation.path, response.Code, response.Body.String())
		}
	}
	proxy := testSavedProxy(t, "socks5h://proxy.invalid:1080", "user", "private-password")
	response := proxyAdminRequest(router, "PATCH", scope(a, "credentials/proxy"), fmt.Sprintf(`{"name":"only-a.json","proxy_id":%d}`, proxy.ID), true)
	if response.Code != 200 {
		t.Fatalf("account proxy: %d %s", response.Code, response.Body.String())
	}
	manager, _ := cliProxyPool(a)
	files, err := manager.ReadCredentials()
	if err != nil || !strings.Contains(string(files["only-a.json"]), "socks5h://user:private-password@proxy.invalid:1080") {
		t.Fatalf("SDK credential did not receive its proxy: %v", err)
	}
	if response := proxyAdminRequest(router, "GET", scope(a, "credentials"), "", true); strings.Contains(response.Body.String(), "private-password") {
		t.Fatal("account list exposed proxy credentials")
	}
	if response := proxyAdminRequest(router, "DELETE", fmt.Sprintf("proxies/%d", proxy.ID), "", true); response.Code != 409 {
		t.Fatalf("proxy in use was deleted: %d %s", response.Code, response.Body.String())
	}
	if response := proxyAdminRequest(router, "PATCH", fmt.Sprintf("proxies/%d", proxy.ID), `{"enabled":false}`, true); response.Code != 409 {
		t.Fatal("account's proxy was disabled")
	}
	if response := proxyAdminRequest(router, "PATCH", scope(a, "credentials/proxy"), `{"name":"only-a.json","proxy_id":-1}`, true); response.Code != 200 {
		t.Fatalf("inherit pool proxy: %d %s", response.Code, response.Body.String())
	}
	files, _ = manager.ReadCredentials()
	var doc map[string]any
	json.Unmarshal(files["only-a.json"], &doc)
	if value, _ := doc["proxy_url"].(string); value != "" {
		t.Fatal("inherited proxy retained a stale override")
	}
	if response := proxyAdminRequest(router, "PUT", scope(a, "proxy"), fmt.Sprintf(`{"proxy_id":%d}`, proxy.ID), true); response.Code != 200 {
		t.Fatalf("pool default proxy: %d %s", response.Code, response.Body.String())
	}
	if response := proxyAdminRequest(router, "GET", "proxies", "", true); response.Code != 200 || !strings.Contains(response.Body.String(), `"upstream_count":1`) {
		t.Fatalf("proxy references: %d %s", response.Code, response.Body.String())
	}
	if response := proxyAdminRequest(router, "POST", scope(a, "stop"), "{}", true); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if response := proxyAdminRequest(router, "GET", scope(b, "status"), "", true); !strings.Contains(response.Body.String(), `"running":true`) {
		t.Fatal("stopping pool A stopped pool B")
	}
	if response := proxyAdminRequest(router, "GET", scope(a, "credentials"), "", true); response.Code != 200 || !strings.Contains(response.Body.String(), `"calls":3`) || strings.Contains(response.Body.String(), "PRIVATE-ACCESS") {
		t.Fatalf("stopped pool lost its safe account history: %d %s", response.Code, response.Body.String())
	}
	chosen, err := pickUpstream(FormatOpenAIChat, "a-model", nil, false)
	if err != nil || chosen.ID != b {
		t.Fatalf("stopped pool still received new traffic: %+v %v", chosen, err)
	}
}

func TestCLIProxyLegacyMigrationPreservesAndSeparatesAccounts(t *testing.T) {
	modelsTestDB(t)
	withTestCLIProxyPools(t)
	root := t.TempDir()
	manager, err := cliproxy.NewManager(root, 18317)
	if err != nil {
		t.Fatal(err)
	}
	original := cliProxyCore
	cliProxyCore = manager
	t.Cleanup(func() { cliProxyCore = original })
	cliProxyPools.root = filepath.Join(root, "pools")
	if err := manager.SeedCredentials(map[string]json.RawMessage{
		"legacy.json": json.RawMessage(`{"type":"codex","access_token":"preserved","disabled":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	base, key := manager.Connection()
	for _, name := range []string{"first", "copied legacy pool"} {
		if err := dbInsertUpstream(&Upstream{Name: name, Type: "cliproxy", BaseURL: base, APIKey: key, Weight: 1, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateCLIProxyPools(); err != nil {
		t.Fatal(err)
	}
	ups, _ := dbListUpstreams()
	if len(ups) != 2 || ups[0].BaseURL == ups[1].BaseURL || ups[0].APIKey == ups[1].APIKey {
		t.Fatal("legacy channels still share the same pool")
	}
	for _, up := range ups {
		pool, err := cliProxyPool(up.ID)
		if err != nil {
			t.Fatal(err)
		}
		files, err := pool.ReadCredentials()
		if err != nil || !strings.Contains(string(files["legacy.json"]), "preserved") {
			t.Fatal("migration lost the legacy credential")
		}
	}
	// A reconstructed registry after restart must reuse the original core
	// instead of trying to start a second process on the same legacy port.
	cliProxyPools.items = make(map[int64]*cliproxy.Manager)
	if got, err := cliProxyPool(ups[0].ID); err != nil || got != manager {
		t.Fatal("legacy pool was not rebound to its existing manager")
	}
	if err := migrateCLIProxyPools(); err != nil {
		t.Fatal(err)
	}
	if repeated, _ := dbListUpstreams(); len(repeated) != 2 {
		t.Fatal("migration is not idempotent")
	}
	// Deleting the migrated channel must not resurrect it on the next restart.
	_ = dbDeleteUpstream(ups[0].ID)
	_, _ = db.Exec(`DELETE FROM cliproxy_pools WHERE upstream_id=?`, ups[0].ID)
	if err := migrateCLIProxyPools(); err != nil {
		t.Fatal(err)
	}
	if remaining, _ := dbListUpstreams(); len(remaining) != 1 {
		t.Fatal("migration recreated a deleted legacy pool")
	}
}
