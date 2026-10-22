package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"apishare/internal/cliproxy"
	"github.com/gin-gonic/gin"
)

var cliProxyPools = struct {
	sync.Mutex
	root  string
	items map[int64]*cliproxy.Manager
}{items: make(map[int64]*cliproxy.Manager)}

func freeCLIProxyPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	return port, listener.Close()
}

func newCLIProxyPoolManager(prefix string) (*cliproxy.Manager, string, error) {
	if cliProxyPools.root == "" {
		return nil, "", errors.New("账号池尚未初始化")
	}
	if err := os.MkdirAll(cliProxyPools.root, 0700); err != nil {
		return nil, "", err
	}
	root, err := os.MkdirTemp(cliProxyPools.root, prefix)
	if err != nil {
		return nil, "", err
	}
	port, err := freeCLIProxyPort()
	if err != nil {
		return nil, "", err
	}
	manager, err := cliproxy.NewManager(root, port)
	return manager, root, err
}

func createCLIProxyPool(ctx context.Context, up *Upstream) error {
	if err := validateUpstreamProxy(up.PoolProxyID); err != nil {
		return err
	}
	manager, root, err := newCLIProxyPoolManager("pool-")
	if err != nil {
		return err
	}
	proxyURL, err := cliProxyProxyURL(up.PoolProxyID)
	if err != nil {
		return err
	}
	if err := manager.SetProxyURL(ctx, proxyURL); err != nil {
		return err
	}
	up.BaseURL, up.APIKey = manager.Connection()
	up.ProxyID = 0
	if err := dbInsertUpstream(up); err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT INTO cliproxy_pools(upstream_id,data_dir,proxy_id) VALUES(?,?,?)`, up.ID, root, up.PoolProxyID); err != nil {
		_ = dbDeleteUpstream(up.ID)
		return err
	}
	cliProxyPools.Lock()
	cliProxyPools.items[up.ID] = manager
	cliProxyPools.Unlock()
	if err := manager.Start(ctx); err != nil {
		manager.Close()
		_, _ = db.Exec(`DELETE FROM cliproxy_pools WHERE upstream_id=?`, up.ID)
		_ = dbDeleteUpstream(up.ID)
		cliProxyPools.Lock()
		delete(cliProxyPools.items, up.ID)
		cliProxyPools.Unlock()
		return err
	}
	return nil
}

func cliProxyPool(id int64) (*cliproxy.Manager, error) {
	up, err := dbGetUpstream(id)
	if err != nil || up.Type != "cliproxy" {
		return nil, errors.New("账号池上游不存在")
	}
	var root string
	if err := db.QueryRow(`SELECT data_dir FROM cliproxy_pools WHERE upstream_id=?`, id).Scan(&root); err != nil {
		return nil, errors.New("账号池尚未迁移，请重启服务")
	}
	cliProxyPools.Lock()
	defer cliProxyPools.Unlock()
	if manager := cliProxyPools.items[id]; manager != nil {
		return manager, nil
	}
	if cliProxyCore != nil && root == filepath.Dir(cliProxyPools.root) {
		cliProxyPools.items[id] = cliProxyCore
		return cliProxyCore, nil
	}
	port, err := freeCLIProxyPort()
	if err != nil {
		return nil, err
	}
	manager, err := cliproxy.NewManager(root, port)
	if err != nil {
		return nil, err
	}
	cliProxyPools.items[id] = manager
	return manager, nil
}

func cliProxyPoolProxyID(id int64) int64 {
	var proxyID int64
	_ = db.QueryRow(`SELECT proxy_id FROM cliproxy_pools WHERE upstream_id=?`, id).Scan(&proxyID)
	return proxyID
}

func requestCLIProxyCore(c *gin.Context) *cliproxy.Manager {
	if value, ok := c.Get("cliproxy_core"); ok {
		return value.(*cliproxy.Manager)
	}
	return cliProxyCore
}

func requestCLIProxyPoolID(c *gin.Context) int64 {
	value, _ := c.Get("cliproxy_pool_id")
	id, _ := value.(int64)
	return id
}

func requestCLIProxyProxyID(c *gin.Context) int64 {
	if id := requestCLIProxyPoolID(c); id != 0 {
		return cliProxyPoolProxyID(id)
	}
	return cliProxyProxyID()
}

func cliProxyPoolRevision(id int64) uint64 {
	cliProxyPools.Lock()
	manager := cliProxyPools.items[id]
	cliProxyPools.Unlock()
	if manager == nil {
		return 0
	}
	return manager.Revision()
}

func cliProxyRelayAvailable(id int64, models ...string) bool {
	cliProxyPools.Lock()
	manager := cliProxyPools.items[id]
	configured := cliProxyPools.root != ""
	cliProxyPools.Unlock()
	if manager == nil {
		return !configured
	}
	model := ""
	if len(models) > 0 {
		model = models[0]
	}
	return manager.Ready(model)
}

func accountProxyID(poolID int64, name string) int64 {
	var id int64
	if err := db.QueryRow(`SELECT proxy_id FROM cliproxy_account_proxies WHERE upstream_id=? AND name=?`, poolID, name).Scan(&id); err != nil {
		return -1
	}
	return id
}

func effectiveAccountProxyID(poolID, selected int64) int64 {
	if selected < 0 {
		return cliProxyPoolProxyID(poolID)
	}
	return selected
}

func saveAccountProxy(ctx context.Context, manager *cliproxy.Manager, poolID int64, name string, proxyID int64) error {
	if proxyID < -1 {
		return errors.New("请选择有效的代理")
	}
	effective := effectiveAccountProxyID(poolID, proxyID)
	proxyURL, err := cliProxyProxyURL(effective)
	if err != nil {
		return err
	}
	// "direct" explicitly bypasses both the pool default and process proxy env.
	if proxyID == 0 {
		proxyURL = "direct"
	} else if proxyID == -1 {
		proxyURL = ""
	}
	if err := manager.PatchCredential(ctx, name, gin.H{"proxy_url": proxyURL}); err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO cliproxy_account_proxies(upstream_id,name,proxy_id) VALUES(?,?,?)
		ON CONFLICT(upstream_id,name) DO UPDATE SET proxy_id=excluded.proxy_id`, poolID, name, proxyID)
	if err == nil {
		manager.Touch()
	}
	return err
}

func setPoolProxy(ctx context.Context, manager *cliproxy.Manager, poolID, proxyID int64) error {
	proxyURL, err := cliProxyProxyURL(proxyID)
	if err != nil {
		return err
	}
	previous := cliProxyPoolProxyID(poolID)
	if err = manager.SetProxyURL(ctx, proxyURL); err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE cliproxy_pools SET proxy_id=? WHERE upstream_id=?`, proxyID, poolID)
	if err != nil {
		oldURL, oldErr := cliProxyProxyURL(previous)
		if oldErr == nil {
			_ = manager.SetProxyURL(ctx, oldURL)
		}
		return err
	}
	if manager == cliProxyCore {
		err = settingsSet(cliProxyProxySetting, strconv.FormatInt(proxyID, 10))
	}
	if err == nil {
		manager.Touch()
	}
	return err
}

func closeCLIProxyPools() {
	cliProxyPools.Lock()
	items := make([]*cliproxy.Manager, 0, len(cliProxyPools.items))
	for _, manager := range cliProxyPools.items {
		items = append(items, manager)
	}
	cliProxyPools.Unlock()
	for _, manager := range items {
		manager.Close()
	}
	coreLoginSessions.Lock()
	defer coreLoginSessions.Unlock()
	for state, session := range coreLoginSessions.items {
		if session.worker != nil {
			session.worker.DiscardLogin()
		}
		delete(coreLoginSessions.items, state)
	}
	if cliProxyCore != nil {
		cliProxyCore.Close()
	}
}

func registerCLIProxyPoolRoutes(auth *gin.RouterGroup) {
	group := auth.Group("/upstreams/:id/cliproxy")
	group.Use(func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			c.AbortWithStatusJSON(400, gin.H{"error": "账号池 ID 无效"})
			return
		}
		manager, err := cliProxyPool(id)
		if err != nil {
			c.AbortWithStatusJSON(404, gin.H{"error": err.Error()})
			return
		}
		c.Set("cliproxy_core", manager)
		c.Set("cliproxy_pool_id", id)
		c.Next()
		if c.Request.Method != "GET" && c.Writer.Status() < 300 {
			manager.Touch()
			if manager.Running() {
				_ = manager.RefreshRuntime(c.Request.Context())
			}
			healthMarkSuccess(id)
			invalidatePublicOverviews()
		}
	})
	registerCLIProxyHandlers(group)
}

func requirePoolCredential(c *gin.Context, name string) bool {
	if !validCoreCredentialName(name) {
		c.JSON(400, gin.H{"error": "请选择有效的账号凭据"})
		return false
	}
	var result struct {
		Files []cliproxy.Credential `json:"files"`
	}
	if err := requestCLIProxyCore(c).Client().Do(c.Request.Context(), "GET", "/credentials", nil, &result); err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return false
	}
	for _, credential := range result.Files {
		if credential.Name == name || credential.ID == name {
			return true
		}
	}
	c.JSON(404, gin.H{"error": "此账号不属于当前账号池"})
	return false
}

func importPoolCredential(ctx context.Context, manager *cliproxy.Manager, poolID int64, name string, raw json.RawMessage, proxyID int64, replace ...bool) error {
	var document map[string]any
	if json.Unmarshal(raw, &document) != nil || len(document) == 0 {
		return errors.New("凭据必须是非空 JSON 对象")
	}
	if document["type"] == "service_account" {
		for _, field := range []string{"project_id", "client_email", "private_key"} {
			value, ok := document[field].(string)
			if !ok || strings.TrimSpace(value) == "" {
				return fmt.Errorf("服务账号 JSON 缺少 %s", field)
			}
		}
		location, _ := document["location"].(string)
		if location == "" {
			location = "us-central1"
		}
		if !vertexLocationPattern.MatchString(location) {
			return errors.New("Vertex AI 地区无效")
		}
		document = map[string]any{"type": "vertex", "service_account": document, "project_id": document["project_id"],
			"email": document["client_email"], "location": location}
	}
	if document["type"] == "vertex" {
		location, _ := document["location"].(string)
		if location == "" {
			location = "us-central1"
		}
		if !vertexLocationPattern.MatchString(location) {
			return errors.New("Vertex AI 地区无效")
		}
		document["location"] = location
	}
	confirmed := len(replace) > 0 && replace[0]
	target, replacing, err := prepareCredentialReplacement(manager, name, document, confirmed)
	if err != nil {
		return err
	}
	name = target
	// Pool membership is the private auth directory, never a caller-provided label.
	if poolID != 0 && !replacing {
		proxyURL, err := cliProxyProxyURL(effectiveAccountProxyID(poolID, proxyID))
		if err != nil {
			return err
		}
		if proxyID == 0 {
			proxyURL = "direct"
		} else if proxyID == -1 {
			proxyURL = ""
		}
		document["proxy_url"] = proxyURL
	}
	if err := manager.Client().Do(ctx, "POST", "/credentials?name="+url.QueryEscape(name), document, nil); err != nil {
		return err
	}
	if poolID != 0 && !replacing {
		_, err := db.Exec(`INSERT INTO cliproxy_account_proxies(upstream_id,name,proxy_id) VALUES(?,?,?)
			ON CONFLICT(upstream_id,name) DO UPDATE SET proxy_id=excluded.proxy_id`, poolID, name, proxyID)
		if err == nil {
			manager.Touch()
			invalidatePublicOverviews()
		}
		return err
	}
	return nil
}

func cliProxyReferences(proxyID int64) (int, error) {
	var count int
	err := db.QueryRow(`SELECT
		(SELECT count(*) FROM cliproxy_pools WHERE proxy_id=?) +
		(SELECT count(*) FROM cliproxy_account_proxies WHERE proxy_id=?)`, proxyID, proxyID).Scan(&count)
	return count, err
}

func syncCLIProxyPoolProxyReferences(ctx context.Context, proxyID int64) error {
	rows, err := db.Query(`SELECT upstream_id FROM cliproxy_pools WHERE proxy_id=?`, proxyID)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		manager, err := cliProxyPool(id)
		if err != nil {
			return err
		}
		if err := setPoolProxy(ctx, manager, id, proxyID); err != nil {
			return err
		}
	}
	rows, err = db.Query(`SELECT upstream_id,name FROM cliproxy_account_proxies WHERE proxy_id=?`, proxyID)
	if err != nil {
		return err
	}
	type binding struct {
		id   int64
		name string
	}
	var bindings []binding
	for rows.Next() {
		var item binding
		if err := rows.Scan(&item.id, &item.name); err != nil {
			rows.Close()
			return err
		}
		bindings = append(bindings, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range bindings {
		manager, err := cliProxyPool(item.id)
		if err != nil {
			return err
		}
		if !manager.Status().Running {
			files, err := manager.ReadCredentials()
			if err != nil {
				return err
			}
			raw, exists := files[item.name]
			if !exists {
				continue
			}
			var document map[string]any
			if err := json.Unmarshal(raw, &document); err != nil {
				return err
			}
			proxyURL, err := cliProxyProxyURL(proxyID)
			if err != nil {
				return err
			}
			document["proxy_url"] = proxyURL
			raw, err = json.Marshal(document)
			if err != nil {
				return err
			}
			if err := manager.SeedCredentials(map[string]json.RawMessage{item.name: raw}); err != nil {
				return err
			}
		} else if err := saveAccountProxy(ctx, manager, item.id, item.name, proxyID); err != nil {
			return err
		}
	}
	return nil
}

func migrateCLIProxyPools() error {
	ups, err := dbListUpstreams()
	if err != nil {
		return err
	}
	base, _ := cliProxyCore.Connection()
	legacyMigrated, err := settingsGet("cliproxy_legacy_pool_migrated")
	if err != nil {
		return err
	}
	if legacyMigrated != "1" && cliProxyCore.CredentialCount() > 0 {
		attached := false
		for _, up := range ups {
			attached = attached || up.Type == "cliproxy" && up.BaseURL == base
		}
		if !attached {
			var migrated int
			if err := db.QueryRow(`SELECT count(*) FROM cliproxy_pools WHERE data_dir=?`, filepath.Dir(cliProxyPools.root)).Scan(&migrated); err != nil {
				return err
			}
			if migrated == 0 {
				_, key := cliProxyCore.Connection()
				up := Upstream{Name: "CLIProxyAPI 默认账号池", Type: "cliproxy", BaseURL: base, APIKey: key, Weight: 1, Enabled: true}
				if err := dbInsertUpstream(&up); err != nil {
					return err
				}
				ups = append(ups, up)
			}
		}
	}
	for i := range ups {
		up := &ups[i]
		if up.Type != "cliproxy" || up.BaseURL != base {
			continue
		}
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM cliproxy_pools WHERE upstream_id=?`, up.ID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			continue
		}
		// Preserve the original directory and credentials for the first legacy
		// channel. Copies get independent snapshots instead of sharing the core.
		var legacy int
		if err := db.QueryRow(`SELECT count(*) FROM cliproxy_pools WHERE data_dir=?`, filepath.Dir(cliProxyPools.root)).Scan(&legacy); err != nil {
			return err
		}
		if legacy == 0 {
			_, err = db.Exec(`INSERT INTO cliproxy_pools(upstream_id,data_dir,proxy_id) VALUES(?,?,?)`,
				up.ID, filepath.Dir(cliProxyPools.root), cliProxyProxyID())
			if err != nil {
				return err
			}
			cliProxyPools.items[up.ID] = cliProxyCore
		} else {
			manager, root, err := newCLIProxyPoolManager("pool-")
			if err != nil {
				return err
			}
			files, err := cliProxyCore.ReadCredentials()
			if err != nil {
				return err
			}
			if err = manager.SeedCredentials(files); err != nil {
				return err
			}
			up.BaseURL, up.APIKey = manager.Connection()
			if err = dbUpdateUpstream(up); err != nil {
				return err
			}
			_, err = db.Exec(`INSERT INTO cliproxy_pools(upstream_id,data_dir,proxy_id) VALUES(?,?,?)`, up.ID, root, cliProxyProxyID())
			if err != nil {
				return err
			}
			cliProxyPools.items[up.ID] = manager
		}
	}
	return settingsSet("cliproxy_legacy_pool_migrated", "1")
}

func resumeCLIProxyPools(ctx context.Context) error {
	rows, err := db.Query(`SELECT upstream_id FROM cliproxy_pools`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		manager, err := cliProxyPool(id)
		if err == nil && manager.Status().AutoStart {
			if manager != cliProxyCore || cliProxyProxyConfigured() {
				err = setPoolProxy(ctx, manager, id, cliProxyPoolProxyID(id))
			}
			if err == nil {
				err = manager.Start(ctx)
			}
		}
		if err != nil {
			fmt.Printf("CLIProxyAPI pool %d: %v\n", id, err)
		}
	}
	return nil
}
