package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"apishare/internal/cliproxy"
	"github.com/gin-gonic/gin"
)

var cliProxyCore *cliproxy.Manager

const cliProxyProxySetting = "cliproxy_proxy_id"

//go:embed third_party/cliproxy/LICENSE
var cliProxyLicense string

var coreLoginSessions = struct {
	sync.Mutex
	items map[string]coreLoginSession
}{items: make(map[string]coreLoginSession)}

type coreLoginSession struct {
	owner      [32]byte
	provider   string
	expires    time.Time
	poolID     int64
	proxyID    int64
	worker     *cliproxy.Manager
	committed  bool
	flow       string
	login      cliproxy.Login
	status     string
	message    string
	duplicates []string
	op         *sync.Mutex
}

func initializeCLIProxy(dbPath string) error {
	port, err := strconv.Atoi(envOr("CLIPROXY_PORT", "18317"))
	if err != nil {
		return fmt.Errorf("CLIPROXY_PORT 无效")
	}
	cliProxyCore, err = cliproxy.NewManager(envOr("CLIPROXY_DATA", filepath.Join(filepath.Dir(dbPath), "cliproxy-data")), port)
	if err != nil {
		return err
	}
	root, err := filepath.Abs(envOr("CLIPROXY_DATA", filepath.Join(filepath.Dir(dbPath), "cliproxy-data")))
	if err != nil {
		return err
	}
	cliProxyPools.root = filepath.Join(root, "pools")
	return migrateCLIProxyPools()
}

// cliProxyProxyID returns the saved outbound proxy bound to the embedded core.
// Zero means no explicit proxy (follow the process environment).
func cliProxyProxyID() int64 {
	raw, err := settingsGet(cliProxyProxySetting)
	if err != nil {
		return 0
	}
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id < 0 {
		return 0
	}
	return id
}

func cliProxyProxyConfigured() bool {
	raw, err := settingsGet(cliProxyProxySetting)
	return err == nil && strings.TrimSpace(raw) != ""
}

// cliProxyProxyURL converts a saved gateway proxy into the URL form understood
// by CLIProxyAPI, including credentials in the userinfo section.
func cliProxyProxyURL(id int64) (string, error) {
	if id == 0 {
		return "", nil
	}
	p, err := dbGetOutboundProxy(id)
	if err != nil {
		return "", fmt.Errorf("CLIProxyAPI 出站代理不存在，请重新选择")
	}
	config := *p
	if !config.Enabled {
		return "", fmt.Errorf("CLIProxyAPI 出站代理已停用，请启用或重新选择")
	}
	if err := normalizeProxyEndpoint(&config); err != nil {
		return "", fmt.Errorf("CLIProxyAPI 出站代理配置无效")
	}
	u, err := url.Parse(config.URL)
	if err != nil {
		return "", fmt.Errorf("CLIProxyAPI 出站代理配置无效")
	}
	if config.Username != "" {
		u.User = url.UserPassword(config.Username, config.Password)
	}
	return u.String(), nil
}

// syncCLIProxyProxy pushes the current gateway proxy binding into the core.
// An unset binding leaves config.yaml untouched so manually maintained proxy
// settings keep working for deployments that never use the admin selector.
func syncCLIProxyProxy(ctx context.Context) error {
	if cliProxyCore == nil || !cliProxyProxyConfigured() {
		return nil
	}
	proxyURL, err := cliProxyProxyURL(cliProxyProxyID())
	if err != nil {
		return err
	}
	return cliProxyCore.SetProxyURL(ctx, proxyURL)
}

func coreOperation(c *gin.Context, fn func() error) {
	if err := fn(); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func coreSession(c *gin.Context, state string) (coreLoginSession, bool) {
	coreLoginSessions.Lock()
	defer coreLoginSessions.Unlock()
	session, ok := coreLoginSessions.items[state]
	return session, ok && time.Now().Before(session.expires) && session.poolID == requestCLIProxyPoolID(c) && session.owner == sha256.Sum256([]byte(adminRequestToken(c)))
}

func registerCLIProxyRoutes(auth *gin.RouterGroup) {
	group := auth.Group("/cliproxy")
	registerCLIProxyHandlers(group)
	registerCLIProxyPoolRoutes(auth)
}

func registerCLIProxyHandlers(group *gin.RouterGroup) {
	group.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if requestCLIProxyCore(c) == nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "CLIProxyAPI 核心尚未初始化"})
			return
		}
		c.Next()
	})
	registerCLIProxyAccountRoutes(group)
	registerCLIProxyOperationRoutes(group)
	registerCLIProxyAttemptRoutes(group)
	registerCLIProxySessionRoutes(group)
	registerCLIProxyJobRoutes(group)
	registerCLIProxyQuotaRoutes(group)
	group.GET("/status", func(c *gin.Context) {
		status := requestCLIProxyCore(c).Status()
		status.ProxyID = requestCLIProxyProxyID(c)
		c.JSON(200, status)
	})
	group.GET("/license", func(c *gin.Context) { c.Data(200, "text/plain; charset=utf-8", []byte(cliProxyLicense)) })
	group.POST("/start", func(c *gin.Context) {
		proxyConfigMu.Lock()
		defer proxyConfigMu.Unlock()
		coreOperation(c, func() error {
			if id := requestCLIProxyPoolID(c); id != 0 {
				if err := setPoolProxy(c.Request.Context(), requestCLIProxyCore(c), id, cliProxyPoolProxyID(id)); err != nil {
					return err
				}
			} else {
				if err := syncCLIProxyProxy(c.Request.Context()); err != nil {
					return err
				}
			}
			return requestCLIProxyCore(c).Start(c.Request.Context())
		})
	})
	group.PUT("/proxy", func(c *gin.Context) {
		var req struct {
			ProxyID int64 `json:"proxy_id"`
		}
		if c.ShouldBindJSON(&req) != nil || req.ProxyID < 0 {
			c.JSON(400, gin.H{"error": "请选择有效的出站代理"})
			return
		}
		if req.ProxyID != 0 {
			if err := validateUpstreamProxy(req.ProxyID); err != nil {
				c.JSON(400, gin.H{"error": err.Error()})
				return
			}
		}
		if id := requestCLIProxyPoolID(c); id != 0 {
			proxyConfigMu.Lock()
			defer proxyConfigMu.Unlock()
			coreOperation(c, func() error {
				return setPoolProxy(c.Request.Context(), requestCLIProxyCore(c), id, req.ProxyID)
			})
			return
		}
		previous := cliProxyProxyID()
		previousConfigured := cliProxyProxyConfigured()
		if err := settingsSet(cliProxyProxySetting, strconv.FormatInt(req.ProxyID, 10)); err != nil {
			c.JSON(500, gin.H{"error": "无法保存核心出站代理"})
			return
		}
		if err := syncCLIProxyProxy(c.Request.Context()); err != nil {
			// Restore the exact previous state: an unset binding must stay unset so
			// a later start does not overwrite a hand-edited config.yaml.
			if previousConfigured {
				_ = settingsSet(cliProxyProxySetting, strconv.FormatInt(previous, 10))
			} else {
				_, _ = db.Exec(`DELETE FROM settings WHERE k=?`, cliProxyProxySetting)
			}
			c.JSON(502, gin.H{"error": "无法应用核心出站代理，请检查核心状态后重试"})
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})
	group.POST("/stop", func(c *gin.Context) {
		proxyConfigMu.Lock()
		defer proxyConfigMu.Unlock()
		coreOperation(c, requestCLIProxyCore(c).Stop)
	})
	group.GET("/credentials", func(c *gin.Context) {
		var result struct {
			Files       []cliproxy.Credential `json:"files"`
			UptimeError string                `json:"uptime_error,omitempty"`
		}
		manager := requestCLIProxyCore(c)
		var err error
		// Preserve legacy management clients, including their error behavior.
		if requestCLIProxyPoolID(c) == 0 {
			err = manager.Client().Do(c.Request.Context(), "GET", "/credentials", nil, &result)
		} else {
			if manager.Running() {
				_ = manager.RefreshRuntime(c.Request.Context())
			}
			result.Files, err = manager.Credentials(c.Request.Context())
		}
		if err != nil {
			c.JSON(502, gin.H{"error": err.Error()})
			return
		}
		if result.Files == nil {
			result.Files = []cliproxy.Credential{}
		}
		if err := manager.AttachAccountUptimes(c.Request.Context(), result.Files, time.Now()); err != nil {
			result.UptimeError = "账号 Uptime 统计暂不可用，请稍后刷新"
		}
		if id := requestCLIProxyPoolID(c); id != 0 {
			for i := range result.Files {
				result.Files[i].ProxyID = accountProxyID(id, result.Files[i].Name)
				result.Files[i].EffectiveProxyID = effectiveAccountProxyID(id, result.Files[i].ProxyID)
			}
		}
		c.JSON(200, result)
	})
	group.POST("/credentials/import", func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2<<20)
		var req struct {
			Name       string          `json:"name"`
			Credential json.RawMessage `json:"credential"`
			ProxyID    *int64          `json:"proxy_id"`
			Replace    bool            `json:"replace"`
		}
		if c.ShouldBindJSON(&req) != nil || !validCoreCredentialName(req.Name) || len(req.Credential) == 0 {
			c.JSON(400, gin.H{"error": "请提供有效的 JSON 凭据文件和文件名"})
			return
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(req.Credential, &object) != nil || len(object) == 0 {
			c.JSON(400, gin.H{"error": "凭据必须是非空 JSON 对象"})
			return
		}
		proxyConfigMu.Lock()
		defer proxyConfigMu.Unlock()
		proxyID := int64(-1)
		if req.ProxyID != nil {
			proxyID = *req.ProxyID
		}
		if proxyID < -1 {
			c.JSON(400, gin.H{"error": "请选择有效的代理"})
			return
		}
		if err := importPoolCredential(c.Request.Context(), requestCLIProxyCore(c), requestCLIProxyPoolID(c), req.Name, req.Credential, proxyID, req.Replace); err != nil {
			credentialImportError(c, err)
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})
	group.PATCH("/credentials/status", func(c *gin.Context) {
		proxyConfigMu.Lock()
		defer proxyConfigMu.Unlock()
		var req struct {
			Name     string `json:"name"`
			Disabled *bool  `json:"disabled"`
		}
		if c.ShouldBindJSON(&req) != nil || req.Name == "" || req.Disabled == nil {
			c.JSON(400, gin.H{"error": "缺少账号或启用状态"})
			return
		}
		if requestCLIProxyPoolID(c) != 0 && !requirePoolCredential(c, req.Name) {
			return
		}
		coreOperation(c, func() error {
			return requestCLIProxyCore(c).Client().Do(c.Request.Context(), "PATCH", "/credentials/status", req, nil)
		})
	})
	group.POST("/credentials/refresh", func(c *gin.Context) {
		proxyConfigMu.Lock()
		defer proxyConfigMu.Unlock()
		var req struct {
			Name string `json:"name"`
		}
		if c.ShouldBindJSON(&req) != nil || req.Name == "" {
			c.JSON(400, gin.H{"error": "请选择账号"})
			return
		}
		if requestCLIProxyPoolID(c) != 0 && !requirePoolCredential(c, req.Name) {
			return
		}
		coreOperation(c, func() error {
			return requestCLIProxyCore(c).Client().Do(c.Request.Context(), "POST", "/credentials/refresh", req, nil)
		})
	})
	group.DELETE("/credentials", func(c *gin.Context) {
		proxyConfigMu.Lock()
		defer proxyConfigMu.Unlock()
		name := c.Query("name")
		if !validCoreCredentialName(name) {
			c.JSON(400, gin.H{"error": "请选择单个凭据文件"})
			return
		}
		if requestCLIProxyPoolID(c) != 0 && !requirePoolCredential(c, name) {
			return
		}
		coreOperation(c, func() error {
			if err := requestCLIProxyCore(c).Client().Do(c.Request.Context(), "DELETE", "/credentials?name="+url.QueryEscape(name), nil, nil); err != nil {
				return err
			}
			_, err := db.Exec(`DELETE FROM cliproxy_account_proxies WHERE upstream_id=? AND name=?`, requestCLIProxyPoolID(c), name)
			return err
		})
	})
	group.PATCH("/credentials/proxy", func(c *gin.Context) {
		var req struct {
			Name    string `json:"name"`
			ProxyID *int64 `json:"proxy_id"`
		}
		if c.ShouldBindJSON(&req) != nil || req.ProxyID == nil || *req.ProxyID < -1 {
			c.JSON(400, gin.H{"error": "请选择有效的账号和代理"})
			return
		}
		proxyConfigMu.Lock()
		defer proxyConfigMu.Unlock()
		if !requirePoolCredential(c, req.Name) {
			return
		}
		coreOperation(c, func() error {
			return saveAccountProxy(c.Request.Context(), requestCLIProxyCore(c), requestCLIProxyPoolID(c), req.Name, *req.ProxyID)
		})
	})
	group.POST("/oauth/siwc/start", func(c *gin.Context) {
		var req struct {
			Name string `json:"name"`
		}
		if c.ShouldBindJSON(&req) != nil {
			c.JSON(400, gin.H{"error": "请选择有效的 Codex 账号"})
			return
		}
		if !requirePoolCredential(c, req.Name) {
			return
		}
		manager := requestCLIProxyCore(c)
		poolID := requestCLIProxyPoolID(c)
		files, err := manager.ReadCredentials()
		if err != nil || files[req.Name] == nil {
			c.JSON(400, gin.H{"error": "请选择当前池内已保存的 Codex OAuth 账号"})
			return
		}
		worker, _, err := newCLIProxyPoolManager("login-")
		proxyID := accountProxyID(poolID, req.Name)
		if err == nil {
			err = worker.SeedCredentials(map[string]json.RawMessage{req.Name: files[req.Name]})
		}
		if err == nil {
			var proxyURL string
			proxyURL, err = cliProxyProxyURL(effectiveAccountProxyID(poolID, proxyID))
			if proxyID == 0 {
				proxyURL = "direct"
			}
			if err == nil {
				err = worker.SetProxyURL(c.Request.Context(), proxyURL)
			}
		}
		if err == nil {
			err = worker.Start(c.Request.Context())
		}
		var result cliproxy.Login
		if err == nil {
			err = worker.Client().Do(c.Request.Context(), "POST", "/oauth/siwc/bind", gin.H{"name": req.Name}, &result)
		}
		u, parseErr := url.Parse(result.URL)
		if err != nil || parseErr != nil || u.Scheme != "https" || u.Host == "" || result.State == "" {
			if worker != nil {
				worker.DiscardLogin()
			}
			c.JSON(502, gin.H{"error": "无法发起 SIWC 绑定，请确认账号包含有效的 Codex OAuth 凭据"})
			return
		}
		coreLoginSessions.Lock()
		coreLoginSessions.items[result.State] = coreLoginSession{
			owner: sha256.Sum256([]byte(adminRequestToken(c))), provider: "codex",
			expires: time.Now().Add(5 * time.Minute), poolID: poolID, proxyID: proxyID, worker: worker, flow: "siwc_bind",
			login: result, status: "wait", op: &sync.Mutex{},
		}
		coreLoginSessions.Unlock()
		expireCoreLogin(result.State, 5*time.Minute)
		go watchCoreLogin(result.State)
		c.JSON(200, result)
	})
	group.POST("/oauth/start", func(c *gin.Context) {
		var req struct {
			Provider string `json:"provider"`
			ProxyID  *int64 `json:"proxy_id"`
		}
		if c.ShouldBindJSON(&req) != nil || !validCoreProvider(req.Provider) {
			c.JSON(400, gin.H{"error": "不支持的登录提供商"})
			return
		}
		// Prune expired records and bound pending sessions before creating a core flow.
		coreLoginSessions.Lock()
		expired := make([]string, 0)
		active := 0
		for id, s := range coreLoginSessions.items {
			if time.Now().After(s.expires) {
				expired = append(expired, id)
			} else {
				active++
			}
		}
		full := active >= 64
		coreLoginSessions.Unlock()
		for _, id := range expired {
			go discardCoreLogin(id)
		}
		if full {
			c.JSON(429, gin.H{"error": "待处理登录过多，请稍后重试"})
			return
		}
		var result cliproxy.Login
		worker := requestCLIProxyCore(c)
		poolID, proxyID := requestCLIProxyPoolID(c), int64(-1)
		if req.ProxyID != nil {
			proxyID = *req.ProxyID
		}
		if proxyID < -1 {
			c.JSON(400, gin.H{"error": "请选择有效的代理"})
			return
		}
		if poolID != 0 {
			// A short-lived isolated worker performs the entire OAuth exchange
			// through this account's chosen proxy, without changing live traffic.
			var err error
			worker, _, err = newCLIProxyPoolManager("login-")
			if err == nil {
				var proxyURL string
				proxyURL, err = cliProxyProxyURL(effectiveAccountProxyID(poolID, proxyID))
				if proxyID == 0 {
					proxyURL = "direct"
				}
				if err == nil {
					err = worker.SetProxyURL(c.Request.Context(), proxyURL)
				}
			}
			if err == nil {
				err = worker.Start(c.Request.Context())
			}
			if err != nil {
				if worker != nil {
					worker.DiscardLogin()
				}
				c.JSON(502, gin.H{"error": "无法启动授权，请检查代理和核心状态"})
				return
			}
		}
		if err := worker.Client().Do(c.Request.Context(), "GET", "/oauth/auth-url?is_webui=true&provider="+url.QueryEscape(req.Provider), nil, &result); err != nil {
			if poolID != 0 {
				worker.DiscardLogin()
			}
			c.JSON(502, gin.H{"error": err.Error()})
			return
		}
		u, err := url.Parse(result.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || result.State == "" {
			if poolID != 0 {
				worker.DiscardLogin()
			}
			c.JSON(502, gin.H{"error": "核心返回的授权链接无效"})
			return
		}
		coreLoginSessions.Lock()
		ttl := 15 * time.Minute
		if result.ExpiresIn > 0 && time.Duration(result.ExpiresIn)*time.Second < ttl {
			ttl = time.Duration(result.ExpiresIn) * time.Second
		}
		result.ExpiresIn = int(ttl / time.Second)
		session := coreLoginSession{owner: sha256.Sum256([]byte(adminRequestToken(c))), provider: req.Provider, expires: time.Now().Add(ttl), poolID: poolID, proxyID: proxyID, login: result, status: "wait", op: &sync.Mutex{}}
		if poolID != 0 {
			session.worker = worker
		}
		coreLoginSessions.items[result.State] = session
		coreLoginSessions.Unlock()
		expireCoreLogin(result.State, ttl)
		if poolID != 0 {
			go watchCoreLogin(result.State)
		}
		c.JSON(200, result)
	})
	group.GET("/oauth/status", func(c *gin.Context) {
		state := c.Query("state")
		session, ok := coreSession(c, state)
		if !ok {
			c.JSON(404, gin.H{"error": "登录会话已过期，请重新发起"})
			return
		}
		if session.worker == nil {
			var result struct {
				Status string `json:"status"`
				Error  string `json:"error,omitempty"`
			}
			if err := requestCLIProxyCore(c).Client().Do(c.Request.Context(), "GET", "/oauth/status?state="+url.QueryEscape(state), nil, &result); err != nil {
				c.JSON(502, gin.H{"error": err.Error()})
				return
			}
			if result.Error != "" {
				result.Error = "授权失败，请重新登录或导入凭据"
			}
			c.JSON(200, result)
			return
		}
		advanceCoreLogin(c.Request.Context(), state, false)
		session, ok = coreSession(c, state)
		if !ok {
			c.JSON(404, gin.H{"error": "登录会话已取消"})
			return
		}
		c.JSON(200, coreLoginView(state, session))
	})
	group.POST("/oauth/callback", func(c *gin.Context) {
		var req struct {
			State       string `json:"state"`
			RedirectURL string `json:"redirect_url"`
		}
		if c.ShouldBindJSON(&req) != nil {
			c.JSON(400, gin.H{"error": "回填参数无效"})
			return
		}
		session, ok := coreSession(c, req.State)
		if !ok {
			c.JSON(404, gin.H{"error": "登录会话已过期"})
			return
		}
		u, err := url.Parse(req.RedirectURL)
		if err != nil || u.Query().Get("state") != req.State || u.Query().Get("code") == "" {
			c.JSON(400, gin.H{"error": "请粘贴当前登录的完整回调地址，包含 code 和 state"})
			return
		}
		coreOperation(c, func() error {
			worker := requestCLIProxyCore(c)
			if session.worker != nil {
				worker = session.worker
			}
			return worker.Client().Do(c.Request.Context(), "POST", "/oauth/callback", gin.H{"provider": session.provider, "state": req.State, "code": u.Query().Get("code"), "client_id": u.Query().Get("client_id"), "redirect_url": req.RedirectURL}, nil)
		})
	})
	group.DELETE("/oauth/session", func(c *gin.Context) {
		state := c.Query("state")
		session, ok := coreSession(c, state)
		if !ok {
			c.JSON(404, gin.H{"error": "登录会话不存在"})
			return
		}
		if session.worker == nil {
			if err := requestCLIProxyCore(c).Client().Do(c.Request.Context(), "DELETE", "/oauth/session?state="+url.QueryEscape(state), nil, nil); err != nil {
				c.JSON(502, gin.H{"error": err.Error()})
				return
			}
		}
		discardCoreLogin(state)
		c.JSON(200, gin.H{"ok": true})
	})
	group.POST("/upstream", func(c *gin.Context) {
		if id := requestCLIProxyPoolID(c); id != 0 {
			c.JSON(200, gin.H{"ok": true, "id": id, "existing": true})
			return
		}
		base, key := requestCLIProxyCore(c).Connection()
		// Verify connectivity before publishing the pool to the gateway router.
		models, err := probeUpstreamModels(c.Request.Context(), "cliproxy", base, key)
		if err != nil {
			c.JSON(502, gin.H{"error": "无法读取账号池模型，请检查核心状态"})
			return
		}
		if len(models) == 0 {
			c.JSON(409, gin.H{"error": "账号池暂无可用模型，请先登录或导入账号"})
			return
		}
		proxyConfigMu.Lock()
		defer proxyConfigMu.Unlock()
		ups, err := dbListUpstreams()
		if err != nil {
			c.JSON(500, gin.H{"error": "无法读取上游配置"})
			return
		}
		for _, up := range ups {
			if up.Type == "cliproxy" && up.BaseURL == base {
				c.JSON(200, gin.H{"ok": true, "id": up.ID, "existing": true})
				return
			}
		}
		up := &Upstream{Name: "CLIProxyAPI 订阅账号池", Type: "cliproxy", BaseURL: base, APIKey: key, Weight: 1, Enabled: true}
		if err := dbInsertUpstream(up); err != nil {
			c.JSON(500, gin.H{"error": "保存账号池上游失败"})
			return
		}
		c.JSON(200, gin.H{"ok": true, "id": up.ID})
	})
}

func validCoreCredentialName(name string) bool {
	return len(name) > 5 && len(name) < 240 && strings.HasSuffix(name, ".json") && !strings.ContainsAny(name, "/\\:*?\"<>|\x00\r\n") && name != ".json" && name != "..json"
}

func validCoreProvider(provider string) bool {
	switch provider {
	case "codex", "claude", "antigravity", "kimi", "kimi-ai", "xai", "devin", "meta":
		return true
	}
	return false
}

// Normalize the pool's affinity to the authenticated gateway key. Provider-specific
// client session headers must not cause unrelated gateway users to share a binding.
func setCLIProxyHeaders(req *http.Request, incoming http.Header, task *relayTask) {
	for _, name := range []string{"OpenAI-Beta", "anthropic-beta", "anthropic-version"} {
		if value := incoming.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	if task.format == FormatAnthropic && req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	cr := task.canon
	if cr == nil {
		cr = &canonReq{Model: task.model}
	}
	key := scopedSessionKey(task.apiKeyID, sessionKeyFromCanon(task.endpoint, incoming, cr))
	id := sha256.Sum256([]byte(key))
	scoped := fmt.Sprintf("apishare-%x", id[:])
	req.Header.Set("X-Session-Id", scoped)
	// Claude metadata.user_id outranks generic session headers in the SDK.
	payload, _ := decodeProtocolObject(task.body)
	if task.format == FormatAnthropic || claudeCodeSessionID(str(object(payload["metadata"])["user_id"])) != "" {
		req.Header.Set("X-Claude-Code-Session-Id", scoped)
	}
}

// Keep context imported here so initialization can be reused by application hosts.
func resumeCLIProxy(ctx context.Context) error {
	if cliProxyCore == nil {
		return nil
	}
	if err := syncCLIProxyProxy(ctx); err != nil {
		// A stale selection must not prevent the gateway (and direct upstreams)
		// from starting. The next explicit start retries after the admin fixes it.
		fmt.Println("CLIProxyAPI 出站代理:", err)
	}
	if cliProxyCore.Status().AutoStart {
		return cliProxyCore.Start(ctx)
	}
	return nil
}
