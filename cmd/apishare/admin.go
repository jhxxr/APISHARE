package main

import (
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

const sessionCookie = "apishare_session"

func makeAdminToken() string {
	secret, _ := settingsGet("session_secret")
	payload := fmt.Sprintf("%d", time.Now().Add(7*24*time.Hour).Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

func validAdminToken(tok string) bool {
	parts := strings.SplitN(tok, ".", 2)
	if len(parts) != 2 {
		return false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() >= exp {
		return false
	}
	secret, err := settingsGet("session_secret")
	if err != nil || secret == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0]))
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(parts[1]))
}

func adminRequestToken(c *gin.Context) string {
	tok := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
	if tok == "" {
		tok, _ = c.Cookie(sessionCookie)
	}
	return tok
}

func setAdminCookie(c *gin.Context, token string, age int, path string) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(sessionCookie, token, age, path, "", c.Request.TLS != nil, true)
}

func adminAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		tok := adminRequestToken(c)
		if tok == "" || !validAdminToken(tok) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}

func maskKey(s string) string {
	if len(s) <= 8 {
		return "***"
	}
	return s[:6] + "..." + s[len(s)-4:]
}

func registerAdminRoutes(r *gin.Engine) {
	api := r.Group("/api")

	api.POST("/login", func(c *gin.Context) {
		if adminSetupRequired() {
			c.JSON(http.StatusConflict, gin.H{"error": "请先完成部署配置", "setup_required": true})
			return
		}
		var req struct {
			Password string `json:"password"`
		}
		if c.BindJSON(&req) != nil {
			return
		}
		adminConfigMu.RLock()
		defer adminConfigMu.RUnlock()
		hash, _ := settingsGet("admin_pass_hash")
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
			time.Sleep(300 * time.Millisecond) // 减缓暴力尝试
			c.JSON(http.StatusUnauthorized, gin.H{"error": "wrong password"})
			return
		}
		tok := makeAdminToken()
		setAdminCookie(c, tok, 7*24*3600, requestAdminPath(c))
		c.JSON(http.StatusOK, gin.H{"token": tok, "setup_required": adminSetupRequired()})
	})

	api.POST("/logout", func(c *gin.Context) {
		setAdminCookie(c, "", -1, requestAdminPath(c))
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	auth := api.Group("", adminAuth())
	registerClientVersionRoutes(auth)
	registerClineOAuthRoutes(auth)
	registerCLIProxyRoutes(auth)
	auth.GET("/me", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true, "admin_path": currentAdminPath(), "setup_required": adminSetupRequired()})
	})
	registerDeploymentRoutes(auth)
	registerOutboundProxyRoutes(auth)
	registerAnnouncementRoutes(auth)
	registerUpstreamOverviewRoutes(auth)
	registerDashboardRoutes(auth)
	registerSiteBrandingAdmin(auth)

	// ---------- API Keys ----------
	auth.GET("/keys/models", func(c *gin.Context) {
		models, err := availableModels(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "无法获取可用模型，请检查上游连接"})
			return
		}
		ids := make([]string, 0, len(models))
		for _, model := range models {
			ids = append(ids, model.ID)
		}
		c.JSON(http.StatusOK, gin.H{"models": ids})
	})
	auth.GET("/keys", func(c *gin.Context) {
		keys, err := dbListKeys()
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, keys)
	})

	auth.POST("/keys", func(c *gin.Context) {
		var req struct {
			Name        string                `json:"name"`
			QuotaUSD    float64               `json:"quota_usd"`
			QPS         float64               `json:"qps"`
			Concurrency int                   `json:"concurrency"`
			Note        string                `json:"note"`
			Models      string                `json:"models"`
			ModelQuotas map[string]ModelQuota `json:"model_quotas"`
		}
		if c.BindJSON(&req) != nil {
			return
		}
		if strings.TrimSpace(req.Models) != "" && normalizeKeyModels(req.Models) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请填写有效模型 ID；留空表示不允许调用任何模型"})
			return
		}
		if req.Name == "" {
			req.Name = "unnamed"
		}
		if req.QPS <= 0 {
			req.QPS = 3
		}
		if req.Concurrency <= 0 {
			req.Concurrency = 5
		}
		ak := &ApiKey{
			Name:        req.Name,
			Key:         "sk-" + randHex(24),
			QuotaUSD:    req.QuotaUSD,
			QPS:         req.QPS,
			Concurrency: req.Concurrency,
			Enabled:     true,
			Note:        req.Note,
			Models:      normalizeKeyModels(req.Models),
			ModelQuotas: req.ModelQuotas,
		}
		if err := validateKeyQuotas(ak); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := dbInsertKey(ak); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		ak, err := dbGetKeyByID(ak.ID)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, ak)
	})

	auth.PATCH("/keys/:id", func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		ak, err := dbGetKeyByID(id)
		if err != nil {
			c.JSON(404, gin.H{"error": "key not found"})
			return
		}
		var req struct {
			Name        *string                `json:"name"`
			QuotaUSD    *float64               `json:"quota_usd"`
			QPS         *float64               `json:"qps"`
			Concurrency *int                   `json:"concurrency"`
			Enabled     *bool                  `json:"enabled"`
			Note        *string                `json:"note"`
			Models      *string                `json:"models"`
			ModelQuotas *map[string]ModelQuota `json:"model_quotas"`
		}
		if c.BindJSON(&req) != nil {
			return
		}
		if req.Name != nil {
			ak.Name = *req.Name
		}
		if req.QuotaUSD != nil {
			ak.QuotaUSD = *req.QuotaUSD
		}
		if req.QPS != nil {
			ak.QPS = *req.QPS
		}
		if req.Concurrency != nil {
			ak.Concurrency = *req.Concurrency
		}
		if req.Enabled != nil {
			ak.Enabled = *req.Enabled
		}
		if req.Note != nil {
			ak.Note = *req.Note
		}
		if req.Models != nil {
			if strings.TrimSpace(*req.Models) != "" && normalizeKeyModels(*req.Models) == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "请填写有效模型 ID；留空表示不允许调用任何模型"})
				return
			}
			ak.Models = normalizeKeyModels(*req.Models)
		}
		if req.ModelQuotas != nil {
			ak.ModelQuotas = *req.ModelQuotas
		} else if req.Models != nil {
			for model := range ak.ModelQuotas {
				if !ak.allowsModel(model) {
					delete(ak.ModelQuotas, model)
				}
			}
		}
		if err := validateKeyQuotas(ak); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := dbUpdateKey(ak); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		ak, err = dbGetKeyByID(id)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, ak)
	})

	auth.DELETE("/keys/:id", func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if err := dbDeleteKey(id); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})

	// ---------- Upstreams ----------
	auth.GET("/upstreams", func(c *gin.Context) {
		ups, err := dbListUpstreams()
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		type upView struct {
			Upstream
			APIKey       string `json:"api_key"`
			KeyMasked    string `json:"key_masked"`
			Fails        int    `json:"fails"`
			Healthy      bool   `json:"healthy"`
			PoolAccounts int    `json:"pool_accounts,omitempty"`
			PoolRunning  bool   `json:"pool_running,omitempty"`
			ManagedPool  bool   `json:"managed_pool,omitempty"`
		}
		out := make([]upView, 0, len(ups))
		for _, u := range ups {
			count, running, managed := 0, false, false
			if u.Type == "cliproxy" {
				if manager, err := cliProxyPool(u.ID); err == nil {
					u.PoolProxyID = cliProxyPoolProxyID(u.ID)
					count, running, managed = manager.CredentialCount(), manager.Status().Running, true
				}
			}
			out = append(out, upView{
				Upstream:     u,
				KeyMasked:    maskKey(u.APIKey),
				Fails:        u.relayFailures(),
				Healthy:      u.availableForRelay(),
				PoolAccounts: count, PoolRunning: running, ManagedPool: managed,
			})
		}
		c.JSON(200, out)
	})

	auth.POST("/upstreams", func(c *gin.Context) {
		proxyConfigMu.Lock()
		defer proxyConfigMu.Unlock()
		var u Upstream
		if c.BindJSON(&u) != nil {
			return
		}
		if u.Priority < 0 {
			c.JSON(400, gin.H{"error": "priority must be zero or a positive integer"})
			return
		}
		if !validUpstreamType(u.Type) {
			c.JSON(400, gin.H{"error": "type must be openai, cliproxy, opencode-free, opencode, cline, anthropic or gemini"})
			return
		}
		if err := u.normalizeOpenCodeFree(); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		managedPool := u.Type == "cliproxy" && cliProxyPools.root != ""
		if managedPool {
			if err := validateUpstreamProxy(u.PoolProxyID); err != nil {
				c.JSON(400, gin.H{"error": err.Error()})
				return
			}
		}
		if !managedPool && (u.BaseURL == "" || u.APIKey == "") {
			c.JSON(400, gin.H{"error": "base_url and api_key are required"})
			return
		}
		if err := normalizeUpstreamModelMap(&u); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if !validUpstreamProtocol(u.Type, u.OpenAIProtocol) {
			c.JSON(400, gin.H{"error": "invalid openai_protocol for upstream type"})
			return
		}
		if !validOpenCodeTier(u.OpenCodeTier) {
			c.JSON(400, gin.H{"error": "opencode_tier must be empty, zen or go"})
			return
		}
		if !validStructuredOutputMode(u.StructuredOutput) {
			c.JSON(400, gin.H{"error": "structured_output must be empty, json_schema, json_object or disabled"})
			return
		}
		if u.Type == "cliproxy" {
			u.HeaderOverrides = ""
		}
		if _, err := parseHeaderOverrides(u.HeaderOverrides); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if u.Type == "cliproxy" {
			if u.ProxyID != 0 {
				c.JSON(400, gin.H{"error": "请在此上游账号池的核心设置中配置默认代理，或为账号单独选择代理"})
				return
			}
		} else if err := validateUpstreamProxy(u.ProxyID); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if u.Weight <= 0 {
			u.Weight = 1
		}
		u.Enabled = true
		var saveErr error
		if managedPool {
			saveErr = createCLIProxyPool(c.Request.Context(), &u)
		} else {
			saveErr = dbInsertUpstream(&u)
		}
		if saveErr != nil {
			c.JSON(500, gin.H{"error": saveErr.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "id": u.ID})
	})

	auth.PATCH("/upstreams/:id", func(c *gin.Context) {
		proxyConfigMu.Lock()
		defer proxyConfigMu.Unlock()
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		u, err := dbGetUpstream(id)
		if err != nil {
			c.JSON(404, gin.H{"error": "upstream not found"})
			return
		}
		var req struct {
			Name             *string `json:"name"`
			Type             *string `json:"type"`
			BaseURL          *string `json:"base_url"`
			APIKey           *string `json:"api_key"`
			Priority         *int    `json:"priority"`
			Weight           *int    `json:"weight"`
			Models           *string `json:"models"`
			ModelMap         *string `json:"model_map"`
			Enabled          *bool   `json:"enabled"`
			ProxyID          *int64  `json:"proxy_id"`
			HeaderOverrides  *string `json:"header_overrides"`
			OpenAIProtocol   *string `json:"openai_protocol"`
			OpenCodeTier     *string `json:"opencode_tier"`
			StructuredOutput *string `json:"structured_output"`
			PoolProxyID      *int64  `json:"pool_proxy_id"`
		}
		if c.BindJSON(&req) != nil {
			return
		}
		if req.Name != nil {
			u.Name = *req.Name
		}
		if req.Type != nil {
			var managed int
			_ = db.QueryRow(`SELECT count(*) FROM cliproxy_pools WHERE upstream_id=?`, u.ID).Scan(&managed)
			if managed > 0 && *req.Type != u.Type {
				c.JSON(400, gin.H{"error": "账号池类型不可更改，请新建其他类型的上游"})
				return
			}
			if u.Type != "cliproxy" && *req.Type == "cliproxy" {
				c.JSON(400, gin.H{"error": "请新建 CLIProxyAPI 上游以创建独立账号池"})
				return
			}
			u.APIKey = u.openCodeKeyForType(*req.Type)
			u.Type = *req.Type
		}
		if req.OpenAIProtocol != nil {
			u.OpenAIProtocol = *req.OpenAIProtocol
		}
		if req.OpenCodeTier != nil {
			u.OpenCodeTier = *req.OpenCodeTier
		}
		if !validOpenCodeTier(u.OpenCodeTier) {
			c.JSON(400, gin.H{"error": "opencode_tier must be empty, zen or go"})
			return
		}
		if req.StructuredOutput != nil {
			u.StructuredOutput = *req.StructuredOutput
		}
		if !validStructuredOutputMode(u.StructuredOutput) {
			c.JSON(400, gin.H{"error": "structured_output must be empty, json_schema, json_object or disabled"})
			return
		}
		if !validUpstreamProtocol(u.Type, u.OpenAIProtocol) || !validUpstreamType(u.Type) {
			c.JSON(400, gin.H{"error": "invalid upstream type or openai_protocol"})
			return
		}
		if req.BaseURL != nil {
			u.BaseURL = *req.BaseURL
		}
		if req.APIKey != nil && strings.TrimSpace(*req.APIKey) != "" {
			u.APIKey = *req.APIKey // 只有显式传入非空值才更新
		}
		if req.Priority != nil {
			if *req.Priority < 0 {
				c.JSON(400, gin.H{"error": "priority must be zero or a positive integer"})
				return
			}
			u.Priority = *req.Priority
		}
		if req.Weight != nil {
			u.Weight = *req.Weight
		}
		if req.Models != nil {
			u.Models = *req.Models
		}
		if req.ModelMap != nil {
			u.ModelMap = *req.ModelMap
			if err := normalizeUpstreamModelMap(u); err != nil {
				c.JSON(400, gin.H{"error": err.Error()})
				return
			}
		}
		if u.Type == "cliproxy" {
			u.HeaderOverrides = ""
		} else if req.HeaderOverrides != nil {
			if _, err := parseHeaderOverrides(*req.HeaderOverrides); err != nil {
				c.JSON(400, gin.H{"error": err.Error()})
				return
			}
			u.HeaderOverrides = *req.HeaderOverrides
		}
		if req.Enabled != nil {
			u.Enabled = *req.Enabled
		}
		if req.ProxyID != nil {
			if u.Type == "cliproxy" {
				if *req.ProxyID != 0 {
					c.JSON(400, gin.H{"error": "请在此上游账号池的核心设置中配置默认代理，或为账号单独选择代理"})
					return
				}
			} else if *req.ProxyID != u.ProxyID {
				if err := validateUpstreamProxy(*req.ProxyID); err != nil {
					c.JSON(400, gin.H{"error": err.Error()})
					return
				}
			}
			u.ProxyID = *req.ProxyID
		}
		// The pool is a loopback upstream; its proxy must never be applied to
		// the local hop. Clear stale values left by older configurations.
		if u.Type == "cliproxy" {
			u.ProxyID = 0
			if manager, err := cliProxyPool(u.ID); err == nil {
				u.BaseURL, u.APIKey = manager.Connection()
				if req.PoolProxyID != nil {
					if err := setPoolProxy(c.Request.Context(), manager, u.ID, *req.PoolProxyID); err != nil {
						c.JSON(400, gin.H{"error": err.Error()})
						return
					}
				}
			}
		}
		if err := u.normalizeOpenCodeFree(); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if err := dbUpdateUpstream(u); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})

	auth.DELETE("/upstreams/:id", func(c *gin.Context) {
		proxyConfigMu.Lock()
		defer proxyConfigMu.Unlock()
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if err := dbDeleteUpstream(id); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		cliProxyPools.Lock()
		manager := cliProxyPools.items[id]
		delete(cliProxyPools.items, id)
		cliProxyPools.Unlock()
		if manager != nil {
			if manager == cliProxyCore {
				_ = manager.Stop()
				_ = settingsSet(cliProxyProxySetting, "0")
			} else {
				manager.Close()
			}
		}
		_, _ = db.Exec(`DELETE FROM cliproxy_pools WHERE upstream_id=?`, id)
		_, _ = db.Exec(`DELETE FROM cliproxy_account_proxies WHERE upstream_id=?`, id)
		c.JSON(200, gin.H{"ok": true})
	})

	// 复制上游：接口类型、地址、密钥、模型配置、请求头、代理、优先级、权重与启用状态
	// 全部沿用，只有名称与创建时间不同。名称留空时自动使用“原名 + 副本”。
	auth.POST("/upstreams/:id/duplicate", func(c *gin.Context) {
		proxyConfigMu.Lock()
		defer proxyConfigMu.Unlock()
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		src, err := dbGetUpstream(id)
		if err != nil {
			c.JSON(404, gin.H{"error": "upstream not found"})
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		if c.BindJSON(&req) != nil {
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			base := strings.TrimSpace(src.Name)
			if base == "" {
				base = "未命名上游"
			}
			name = base + " 副本"
		}
		dup := *src
		dup.ID = 0
		dup.Name = name
		dup.CreatedAt = ""
		dup.config = nil
		if dup.Type == "cliproxy" {
			dup.ProxyID = 0
			dup.HeaderOverrides = ""
		}
		var saveErr error
		if dup.Type == "cliproxy" && cliProxyPools.root != "" {
			dup.PoolProxyID = cliProxyPoolProxyID(src.ID)
			saveErr = createCLIProxyPool(c.Request.Context(), &dup)
		} else {
			saveErr = dbInsertUpstream(&dup)
		}
		if saveErr != nil {
			c.JSON(500, gin.H{"error": saveErr.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "id": dup.ID, "name": dup.Name})
	})

	registerModelTestRoutes(auth)
	registerModelOverviewRoutes(auth)

	auth.POST("/upstreams/:id/test", func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		u, err := dbGetUpstream(id)
		if err != nil {
			c.JSON(404, gin.H{"error": "upstream not found"})
			return
		}
		start := time.Now()
		models, err := probeConfiguredModels(c.Request.Context(), u)
		latency := time.Since(start).Milliseconds()
		if err != nil {
			c.JSON(200, gin.H{"ok": false, "latency_ms": latency, "error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "latency_ms": latency, "model_count": len(models)})
	})

	// probeUpstream 处理两种探测：未保存的上游（/upstream-probe，必须显式给全参数）
	// 与已保存的上游（/upstreams/:id/probe，参数留空则用已存值）。
	probeResult := func(c *gin.Context, up *Upstream) {
		if err := up.normalizeOpenCodeFree(); err != nil {
			c.JSON(400, gin.H{"ok": false, "error": err.Error()})
			return
		}
		if !validUpstreamType(up.Type) || !validUpstreamProtocol(up.Type, up.OpenAIProtocol) || !validOpenCodeTier(up.OpenCodeTier) {
			c.JSON(400, gin.H{"ok": false, "error": "invalid upstream protocol or OpenCode tier"})
			return
		}
		if up.BaseURL == "" || up.APIKey == "" {
			c.JSON(400, gin.H{"ok": false, "error": "base_url and api_key are required"})
			return
		}
		if up.Type == "cliproxy" {
			up.HeaderOverrides = ""
		}
		if _, err := parseHeaderOverrides(up.HeaderOverrides); err != nil {
			c.JSON(400, gin.H{"ok": false, "error": err.Error()})
			return
		}
		start := time.Now()
		models, err := probeConfiguredModels(c.Request.Context(), up)
		latency := time.Since(start).Milliseconds()
		if err != nil {
			c.JSON(200, gin.H{"ok": false, "latency_ms": latency, "error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "latency_ms": latency, "models": models, "count": len(models)})
	}

	auth.POST("/upstream-probe", func(c *gin.Context) {
		var req struct {
			Type            string `json:"type"`
			BaseURL         string `json:"base_url"`
			APIKey          string `json:"api_key"`
			ProxyID         int64  `json:"proxy_id"`
			HeaderOverrides string `json:"header_overrides"`
			OpenAIProtocol  string `json:"openai_protocol"`
			OpenCodeTier    string `json:"opencode_tier"`
		}
		if c.ShouldBindJSON(&req) != nil || req.Type == "" {
			c.JSON(400, gin.H{"ok": false, "error": "type is required"})
			return
		}
		if !validUpstreamType(req.Type) {
			c.JSON(400, gin.H{"ok": false, "error": "type must be openai, cliproxy, opencode-free, opencode, cline, anthropic or gemini"})
			return
		}
		probeResult(c, &Upstream{Type: req.Type, BaseURL: req.BaseURL, APIKey: req.APIKey, ProxyID: req.ProxyID, HeaderOverrides: req.HeaderOverrides, OpenAIProtocol: req.OpenAIProtocol, OpenCodeTier: req.OpenCodeTier})
	})

	auth.POST("/upstreams/:id/probe", func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		u, err := dbGetUpstream(id)
		if err != nil {
			c.JSON(404, gin.H{"ok": false, "error": "upstream not found"})
			return
		}
		var req struct {
			Type            string  `json:"type"`
			BaseURL         string  `json:"base_url"`
			APIKey          string  `json:"api_key"`
			ProxyID         *int64  `json:"proxy_id"`
			HeaderOverrides *string `json:"header_overrides"`
			OpenAIProtocol  *string `json:"openai_protocol"`
			OpenCodeTier    *string `json:"opencode_tier"`
		}
		// An empty body uses saved values; malformed edits must never silently
		// fall back to the stored key and report a misleading success.
		if err := c.ShouldBindJSON(&req); err != nil && err != io.EOF {
			c.JSON(400, gin.H{"ok": false, "error": "invalid probe request"})
			return
		}
		typ, base, key := u.Type, u.BaseURL, u.APIKey
		if strings.TrimSpace(req.Type) != "" {
			typ = strings.TrimSpace(req.Type)
		}
		key = u.openCodeKeyForType(typ)
		if strings.TrimSpace(req.BaseURL) != "" {
			base = strings.TrimSpace(req.BaseURL)
		}
		if strings.TrimSpace(req.APIKey) != "" {
			key = strings.TrimSpace(req.APIKey)
		}
		proxyID := u.ProxyID
		if req.ProxyID != nil {
			proxyID = *req.ProxyID
		}
		headers := u.HeaderOverrides
		if req.HeaderOverrides != nil {
			headers = *req.HeaderOverrides
		}
		u.Type, u.BaseURL, u.APIKey, u.ProxyID, u.HeaderOverrides = typ, base, key, proxyID, headers
		if req.OpenAIProtocol != nil {
			u.OpenAIProtocol = *req.OpenAIProtocol
		}
		if req.OpenCodeTier != nil {
			u.OpenCodeTier = *req.OpenCodeTier
		}
		probeResult(c, u)
	})

	// ---------- Logs ----------
	auth.GET("/logs", func(c *gin.Context) {
		serveLogQuery(c)
	})

	// ---------- Stats ----------
	auth.GET("/stats", func(c *gin.Context) {
		now := beijingNow()
		todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, beijingLocation)
		todayKey := todayStart.Format("2006-01-02")
		var calls int
		var pt, ct int64
		var cost float64
		// 当日明细永远在保留期内（保留期至少 1 天），此处合并汇总只是为了极端
		// 情况下也不丢失：汇总里只有已被删除的行，与明细天然不重叠。
		db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(cost_usd),0) FROM call_logs WHERE status BETWEEN 200 AND 299 AND unixepoch(created_at) >= ?`,
			todayStart.Unix()).Scan(&calls, &pt, &ct, &cost)
		var archivedCalls int
		var archivedPT, archivedCT int64
		var archivedCost float64
		db.QueryRow(`SELECT COALESCE(SUM(successes),0), COALESCE(SUM(prompt_tokens),0),
			COALESCE(SUM(completion_tokens),0), COALESCE(SUM(cost_usd),0)
			FROM call_log_daily WHERE day=?`, todayKey).Scan(&archivedCalls, &archivedPT, &archivedCT, &archivedCost)
		calls += archivedCalls
		pt += archivedPT
		ct += archivedCT
		cost += archivedCost

		var keysCount, upsCount int
		db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE enabled=1`).Scan(&keysCount)
		db.QueryRow(`SELECT COUNT(*) FROM upstreams WHERE enabled=1`).Scan(&upsCount)

		type dailyRow struct {
			Date    string  `json:"date"`
			Calls   int     `json:"calls"`
			CostUSD float64 `json:"cost_usd"`
		}
		var daily []dailyRow
		weekStart := todayStart.AddDate(0, 0, -6)
		weekStartKey := weekStart.Format("2006-01-02")
		weekEndKey := todayStart.AddDate(0, 0, 1).Format("2006-01-02")
		// 7 天图表同样合并汇总（保留期较短时，老日期只有汇总数据）。
		rows, err := db.Query(`SELECT date(created_at,'+8 hours') d, COUNT(*), COALESCE(SUM(cost_usd),0)
			FROM call_logs WHERE status BETWEEN 200 AND 299 AND unixepoch(created_at) >= ? AND unixepoch(created_at) < ?
			GROUP BY d
			UNION ALL
			SELECT day, SUM(successes), COALESCE(SUM(cost_usd),0)
			FROM call_log_daily WHERE day>=? AND day<? GROUP BY day ORDER BY 1`, weekStart.Unix(), todayStart.AddDate(0, 0, 1).Unix(), weekStartKey, weekEndKey)
		if err == nil {
			defer rows.Close()
			merged := map[string]*dailyRow{}
			for rows.Next() {
				var d dailyRow
				rows.Scan(&d.Date, &d.Calls, &d.CostUSD)
				if slot, ok := merged[d.Date]; ok {
					slot.Calls += d.Calls
					slot.CostUSD += d.CostUSD
				} else {
					merged[d.Date] = &dailyRow{Date: d.Date, Calls: d.Calls, CostUSD: d.CostUSD}
				}
			}
			for day := weekStart; !day.After(todayStart); day = day.AddDate(0, 0, 1) {
				key := day.Format("2006-01-02")
				if slot, ok := merged[key]; ok {
					daily = append(daily, *slot)
				}
			}
		}
		if daily == nil {
			daily = []dailyRow{}
		}

		c.JSON(200, gin.H{
			"today":        gin.H{"calls": calls, "prompt_tokens": pt, "completion_tokens": ct, "cost_usd": cost},
			"enabled_keys": keysCount, "enabled_upstreams": upsCount,
			"daily": daily,
		})
	})

	// ---------- Settings ----------
	auth.GET("/settings", func(c *gin.Context) {
		s := getSettings()
		autoPricing.RLock()
		autoInfo := gin.H{
			"enabled":     s.AutoPricingEnabled,
			"url":         s.AutoPricingURL,
			"last_update": autoPricing.fetchedAt,
			"model_count": len(autoPricing.data),
		}
		autoPricing.RUnlock()
		retention := retentionOption(s.LogRetentionDays)
		siteIconURL, siteIconType := currentSiteIcon()
		c.JSON(200, gin.H{
			"affinity_ttl_min":     s.AffinityTTLMin,
			"retry":                s.Retry,
			"inject_stream_usage":  s.InjectStreamUsage,
			"log_retention_days":   s.LogRetentionDays,
			"log_retention_label":  retention.label,
			"log_retention_detail": retention.detail,
			"pricing":              s.Pricing,
			"auto_pricing_enabled": s.AutoPricingEnabled,
			"auto_pricing_url":     s.AutoPricingURL,
			"auto_pricing":         autoInfo,
			"site_title":           s.SiteTitle,
			"site_icon_url":        siteIconURL,
			"site_icon_type":       siteIconType,
			"site_icon_custom":     siteIconURL != "",
		})
	})

	auth.PUT("/settings", func(c *gin.Context) {
		var req struct {
			AffinityTTLMin     *int              `json:"affinity_ttl_min"`
			Retry              *int              `json:"retry"`
			InjectStreamUsage  *bool             `json:"inject_stream_usage"`
			LogRetentionDays   *int              `json:"log_retention_days"`
			Pricing            *map[string]Price `json:"pricing"`
			NewPassword        *string           `json:"new_password"`
			AutoPricingEnabled *bool             `json:"auto_pricing_enabled"`
			AutoPricingURL     *string           `json:"auto_pricing_url"`
			SiteTitle          *string           `json:"site_title"`
		}
		if c.BindJSON(&req) != nil {
			return
		}
		if req.NewPassword != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请在部署配置中验证当前密码后更新管理员密码"})
			return
		}
		s := getSettings()
		if req.SiteTitle != nil {
			title, err := normalizeSiteTitle(*req.SiteTitle)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			s.SiteTitle = title
		}
		previousRetention := s.LogRetentionDays
		retentionChanged := false
		if req.AffinityTTLMin != nil && *req.AffinityTTLMin > 0 {
			s.AffinityTTLMin = *req.AffinityTTLMin
		}
		if req.Retry != nil && *req.Retry >= 0 && *req.Retry <= 5 {
			s.Retry = *req.Retry
		}
		if req.InjectStreamUsage != nil {
			s.InjectStreamUsage = *req.InjectStreamUsage
		}
		if req.LogRetentionDays != nil {
			option, ok := retentionOptionFor(*req.LogRetentionDays)
			if !ok {
				c.JSON(http.StatusBadRequest, gin.H{"error": "调用记录保留时长仅支持 7 天（默认）、30 天等预设选项"})
				return
			}
			s.LogRetentionDays = option.days
			retentionChanged = s.LogRetentionDays != previousRetention
		}
		if req.Pricing != nil && len(*req.Pricing) > 0 {
			for model, price := range *req.Pricing {
				if !validPrice(price) {
					c.JSON(http.StatusBadRequest, gin.H{"error": "模型 " + model + " 的单价必须为有限的非负数"})
					return
				}
			}
			s.Pricing = *req.Pricing
		}
		if req.AutoPricingEnabled != nil {
			s.AutoPricingEnabled = *req.AutoPricingEnabled
		}
		if req.AutoPricingURL != nil && strings.TrimSpace(*req.AutoPricingURL) != "" {
			s.AutoPricingURL = strings.TrimSpace(*req.AutoPricingURL)
		}
		if err := persistSettings(s); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		// 保留期变更后立即触发一次清理（后台循环未运行时发送会被丢弃）。
		if retentionChanged {
			kickLogRetentionSweep()
		}
		if s.AutoPricingEnabled {
			go func() {
				if _, err := refreshAutoPricing(); err != nil {
					log.Printf("[pricing] manual-trigger sync failed: %v", err)
				}
			}()
		}
		c.JSON(200, gin.H{"ok": true})
	})

	// 立即同步官方定价
	auth.POST("/settings/pricing/refresh", func(c *gin.Context) {
		n, err := refreshAutoPricing()
		if err != nil {
			c.JSON(200, gin.H{"ok": false, "error": err.Error()})
			return
		}
		autoPricing.RLock()
		at := autoPricing.fetchedAt
		autoPricing.RUnlock()
		c.JSON(200, gin.H{"ok": true, "model_count": n, "last_update": at})
	})

	// 查询某模型的有效单价与来源
	auth.GET("/settings/pricing/lookup", func(c *gin.Context) {
		model := c.Query("model")
		if model == "" {
			c.JSON(400, gin.H{"error": "missing model"})
			return
		}
		p, src, pricingModel := lookupRelayPrice(model, "")
		c.JSON(200, gin.H{"model": model, "price": p, "source": src, "pricing_model": pricingModel})
	})

	// ---------- 静态资源（嵌入二进制） ----------
	// The outer router enforces the configured prefix and trailing slash.
	sub, _ := fs.Sub(webFS, "web")
	r.GET("/", func(c *gin.Context) {
		if adminSetupRequired() {
			c.Redirect(http.StatusTemporaryRedirect, "/setup/")
			return
		}
		serveAdminFile(c, sub, "index.html")
	})
	r.GET("/static/*filepath", func(c *gin.Context) {
		serveAdminFile(c, sub, strings.TrimPrefix(c.Param("filepath"), "/"))
	})
}

func serveAdminFile(c *gin.Context, sub fs.FS, name string) {
	if name == "" || strings.Contains(name, "..") {
		c.String(http.StatusNotFound, "not found")
		return
	}
	b, err := fs.ReadFile(sub, name)
	if err != nil {
		c.String(http.StatusNotFound, "not found")
		return
	}
	ct := mime.TypeByExtension(filepath.Ext(name))
	switch filepath.Ext(name) {
	case ".js":
		ct = "text/javascript; charset=utf-8"
	case ".css":
		ct = "text/css; charset=utf-8"
	case ".html":
		ct = "text/html; charset=utf-8"
		b = injectSiteBranding(versionHTMLAssets(b, webFS))
		c.Header("Cache-Control", "no-store")
	}
	// Unversioned assets must not acquire browser/CDN heuristic freshness.
	// Only a URL matching the actual embedded content can be cached long-term.
	if c.Writer.Header().Get("Cache-Control") == "" {
		if c.Query("v") == webAssetVersion(b) {
			c.Header("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			c.Header("Cache-Control", "no-store")
		}
	}
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Frame-Options", "DENY")
	if compressibleAsset(name) && len(b) >= 1024 {
		c.Header("Vary", "Accept-Encoding")
		if strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
			c.Header("Content-Encoding", "gzip")
			b = gzipAsset(b)
		}
	}
	c.Data(http.StatusOK, ct, b)
}

func compressibleAsset(name string) bool {
	switch filepath.Ext(name) {
	case ".js", ".css", ".html", ".svg", ".json", ".txt", ".ttf":
		return true
	}
	return false
}

// Embedded assets never change at runtime, so each one is compressed once.
var gzipAssets sync.Map

func gzipAsset(b []byte) []byte {
	key := uint64(crc32.ChecksumIEEE(b))<<32 | uint64(uint32(len(b)))
	if v, ok := gzipAssets.Load(key); ok {
		return v.([]byte)
	}
	var buf bytes.Buffer
	w, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	w.Write(b)
	w.Close()
	gzipAssets.Store(key, buf.Bytes())
	return buf.Bytes()
}
