package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
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
	auth.GET("/me", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true, "admin_path": currentAdminPath(), "setup_required": adminSetupRequired()})
	})
	registerDeploymentRoutes(auth)
	registerOutboundProxyRoutes(auth)

	// ---------- API Keys ----------
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
			Name        string  `json:"name"`
			QuotaUSD    float64 `json:"quota_usd"`
			QPS         float64 `json:"qps"`
			Concurrency int     `json:"concurrency"`
			Note        string  `json:"note"`
			ShowOnHome  bool    `json:"show_on_home"`
		}
		if c.BindJSON(&req) != nil {
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
			ShowOnHome:  req.ShowOnHome,
		}
		if err := dbInsertKey(ak); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, ak) // key 完整值仅此一次返回
	})

	auth.PATCH("/keys/:id", func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		ak, err := dbGetKeyByID(id)
		if err != nil {
			c.JSON(404, gin.H{"error": "key not found"})
			return
		}
		var req struct {
			Name        *string  `json:"name"`
			QuotaUSD    *float64 `json:"quota_usd"`
			QPS         *float64 `json:"qps"`
			Concurrency *int     `json:"concurrency"`
			Enabled     *bool    `json:"enabled"`
			Note        *string  `json:"note"`
			ShowOnHome  *bool    `json:"show_on_home"`
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
		if req.ShowOnHome != nil {
			ak.ShowOnHome = *req.ShowOnHome
		}
		if err := dbUpdateKey(ak); err != nil {
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
			APIKey    string `json:"api_key"`
			KeyMasked string `json:"key_masked"`
			Fails     int    `json:"fails"`
			Healthy   bool   `json:"healthy"`
		}
		out := make([]upView, 0, len(ups))
		for _, u := range ups {
			out = append(out, upView{
				Upstream:  u,
				KeyMasked: maskKey(u.APIKey),
				Fails:     healthFails(u.ID),
				Healthy:   healthAvailable(u.ID),
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
		if u.Type != "openai" && u.Type != "anthropic" && u.Type != "gemini" {
			c.JSON(400, gin.H{"error": "type must be openai, anthropic or gemini"})
			return
		}
		if u.BaseURL == "" || u.APIKey == "" {
			c.JSON(400, gin.H{"error": "base_url and api_key are required"})
			return
		}
		if err := validateUpstreamProxy(u.ProxyID); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if u.Weight <= 0 {
			u.Weight = 1
		}
		u.Enabled = true
		if err := dbInsertUpstream(&u); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
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
			Name     *string `json:"name"`
			Type     *string `json:"type"`
			BaseURL  *string `json:"base_url"`
			APIKey   *string `json:"api_key"`
			Weight   *int    `json:"weight"`
			Models   *string `json:"models"`
			ModelMap *string `json:"model_map"`
			Enabled  *bool   `json:"enabled"`
			ProxyID  *int64  `json:"proxy_id"`
		}
		if c.BindJSON(&req) != nil {
			return
		}
		if req.Name != nil {
			u.Name = *req.Name
		}
		if req.Type != nil {
			u.Type = *req.Type
		}
		if req.BaseURL != nil {
			u.BaseURL = *req.BaseURL
		}
		if req.APIKey != nil && strings.TrimSpace(*req.APIKey) != "" {
			u.APIKey = *req.APIKey // 只有显式传入非空值才更新
		}
		if req.Weight != nil {
			u.Weight = *req.Weight
		}
		if req.Models != nil {
			u.Models = *req.Models
		}
		if req.ModelMap != nil {
			u.ModelMap = *req.ModelMap
		}
		if req.Enabled != nil {
			u.Enabled = *req.Enabled
		}
		if req.ProxyID != nil {
			if *req.ProxyID != u.ProxyID {
				if err := validateUpstreamProxy(*req.ProxyID); err != nil {
					c.JSON(400, gin.H{"error": err.Error()})
					return
				}
			}
			u.ProxyID = *req.ProxyID
		}
		if err := dbUpdateUpstream(u); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})

	auth.DELETE("/upstreams/:id", func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if err := dbDeleteUpstream(id); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})

	auth.POST("/upstreams/:id/test", func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		u, err := dbGetUpstream(id)
		if err != nil {
			c.JSON(404, gin.H{"error": "upstream not found"})
			return
		}
		start := time.Now()
		models, err := probeUpstreamModelsWithProxy(c.Request.Context(), u.Type, u.BaseURL, u.APIKey, u.ProxyID)
		latency := time.Since(start).Milliseconds()
		if err != nil {
			c.JSON(200, gin.H{"ok": false, "latency_ms": latency, "error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "latency_ms": latency, "model_count": len(models)})
	})

	// probeUpstream 处理两种探测：未保存的上游（/upstream-probe，必须显式给全参数）
	// 与已保存的上游（/upstreams/:id/probe，参数留空则用已存值）。
	probeResult := func(c *gin.Context, typ, base, key string, proxyID int64) {
		start := time.Now()
		models, err := probeUpstreamModelsWithProxy(c.Request.Context(), typ, base, key, proxyID)
		latency := time.Since(start).Milliseconds()
		if err != nil {
			c.JSON(200, gin.H{"ok": false, "latency_ms": latency, "error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "latency_ms": latency, "models": models, "count": len(models)})
	}

	auth.POST("/upstream-probe", func(c *gin.Context) {
		var req struct {
			Type    string `json:"type"`
			BaseURL string `json:"base_url"`
			APIKey  string `json:"api_key"`
			ProxyID int64  `json:"proxy_id"`
		}
		if c.ShouldBindJSON(&req) != nil || req.Type == "" || req.BaseURL == "" || req.APIKey == "" {
			c.JSON(400, gin.H{"ok": false, "error": "type, base_url and api_key are required"})
			return
		}
		if req.Type != "openai" && req.Type != "anthropic" && req.Type != "gemini" {
			c.JSON(400, gin.H{"ok": false, "error": "type must be openai, anthropic or gemini"})
			return
		}
		probeResult(c, req.Type, req.BaseURL, req.APIKey, req.ProxyID)
	})

	auth.POST("/upstreams/:id/probe", func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		u, err := dbGetUpstream(id)
		if err != nil {
			c.JSON(404, gin.H{"ok": false, "error": "upstream not found"})
			return
		}
		var req struct {
			Type    string `json:"type"`
			BaseURL string `json:"base_url"`
			APIKey  string `json:"api_key"`
			ProxyID *int64 `json:"proxy_id"`
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
		probeResult(c, typ, base, key, proxyID)
	})

	// ---------- Logs ----------
	auth.GET("/logs", func(c *gin.Context) {
		page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
		size, _ := strconv.Atoi(c.DefaultQuery("size", "50"))
		if page < 1 {
			page = 1
		}
		if size < 1 || size > 200 {
			size = 50
		}
		logs, err := dbListLogs(size, (page-1)*size)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		total, _ := dbCountLogs()
		if logs == nil {
			logs = []CallLog{}
		}
		c.JSON(200, gin.H{"total": total, "page": page, "size": size, "items": logs})
	})

	// ---------- Stats ----------
	auth.GET("/stats", func(c *gin.Context) {
		now := beijingNow()
		todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, beijingLocation)
		var calls int
		var pt, ct int64
		var cost float64
		db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(cost_usd),0) FROM call_logs WHERE status BETWEEN 200 AND 299 AND unixepoch(created_at) >= ?`,
			todayStart.Unix()).Scan(&calls, &pt, &ct, &cost)

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
		rows, err := db.Query(`SELECT date(created_at,'+8 hours') d, COUNT(*), COALESCE(SUM(cost_usd),0)
			FROM call_logs WHERE status BETWEEN 200 AND 299 AND unixepoch(created_at) >= ? AND unixepoch(created_at) < ?
			GROUP BY d ORDER BY d`, weekStart.Unix(), todayStart.AddDate(0, 0, 1).Unix())
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var d dailyRow
				rows.Scan(&d.Date, &d.Calls, &d.CostUSD)
				daily = append(daily, d)
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
		c.JSON(200, gin.H{
			"affinity_ttl_min":     s.AffinityTTLMin,
			"retry":                s.Retry,
			"inject_stream_usage":  s.InjectStreamUsage,
			"pricing":              s.Pricing,
			"auto_pricing_enabled": s.AutoPricingEnabled,
			"auto_pricing_url":     s.AutoPricingURL,
			"auto_pricing":         autoInfo,
		})
	})

	auth.PUT("/settings", func(c *gin.Context) {
		var req struct {
			AffinityTTLMin     *int              `json:"affinity_ttl_min"`
			Retry              *int              `json:"retry"`
			InjectStreamUsage  *bool             `json:"inject_stream_usage"`
			Pricing            *map[string]Price `json:"pricing"`
			NewPassword        *string           `json:"new_password"`
			AutoPricingEnabled *bool             `json:"auto_pricing_enabled"`
			AutoPricingURL     *string           `json:"auto_pricing_url"`
		}
		if c.BindJSON(&req) != nil {
			return
		}
		if req.NewPassword != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请在部署配置中验证当前密码后更新管理员密码"})
			return
		}
		s := getSettings()
		if req.AffinityTTLMin != nil && *req.AffinityTTLMin > 0 {
			s.AffinityTTLMin = *req.AffinityTTLMin
		}
		if req.Retry != nil && *req.Retry >= 0 && *req.Retry <= 5 {
			s.Retry = *req.Retry
		}
		if req.InjectStreamUsage != nil {
			s.InjectStreamUsage = *req.InjectStreamUsage
		}
		if req.Pricing != nil && len(*req.Pricing) > 0 {
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
		p, src := lookupPrice(model)
		c.JSON(200, gin.H{"model": model, "price": p, "source": src})
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
	}
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Frame-Options", "DENY")
	c.Data(http.StatusOK, ct, b)
}
