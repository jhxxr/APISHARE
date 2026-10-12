package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

const keyViewCookie = "apishare_key_view"
const keyViewLifetime = 7 * 24 * time.Hour

type KeyQuotaPool struct {
	QuotaUSD     float64 `json:"quota_usd"`
	UsedUSD      float64 `json:"used_usd"`
	RemainingUSD float64 `json:"remaining_usd"`
	Unlimited    bool    `json:"unlimited"`
}

type KeyModelQuota struct {
	Mode string        `json:"mode"`
	Pool *KeyQuotaPool `json:"pool,omitempty"`
}

type KeyOverview struct {
	KeySuffix string                   `json:"key_suffix"`
	UpdatedAt string                   `json:"updated_at"`
	HasPools  bool                     `json:"has_pools"`
	Summary   KeyQuotaPool             `json:"summary"`
	Shared    *KeyQuotaPool            `json:"shared,omitempty"`
	Models    map[string]KeyModelQuota `json:"models"`
}

func keyQuotaPool(quota, used float64) *KeyQuotaPool {
	return &KeyQuotaPool{QuotaUSD: quota, UsedUSD: used, RemainingUSD: math.Max(quota-used, 0), Unlimited: quota == 0}
}

// Only active pools count; a shared pool is counted once regardless of model count.
func buildKeyOverview(ak *ApiKey) *KeyOverview {
	suffix := ak.Key
	if len(suffix) > 4 {
		suffix = suffix[len(suffix)-4:]
	} else {
		suffix = "••••"
	}
	result := &KeyOverview{KeySuffix: suffix, UpdatedAt: nowStr(), Models: make(map[string]KeyModelQuota)}
	add := func(pool *KeyQuotaPool) {
		result.HasPools = true
		result.Summary.Unlimited = result.Summary.Unlimited || pool.Unlimited
		result.Summary.UsedUSD += pool.UsedUSD
		if !pool.Unlimited {
			result.Summary.QuotaUSD += pool.QuotaUSD
			result.Summary.RemainingUSD += pool.RemainingUSD
		}
	}
	for _, model := range keyModelIDs(ak.Models) {
		if budget, independent := ak.ModelQuotas[model]; independent {
			pool := keyQuotaPool(budget.QuotaUSD, budget.UsedUSD)
			result.Models[model] = KeyModelQuota{Mode: "independent", Pool: pool}
			add(pool)
		} else {
			result.Models[model] = KeyModelQuota{Mode: "shared"}
			if result.Shared == nil {
				result.Shared = keyQuotaPool(ak.QuotaUSD, ak.SharedUsedUSD)
				add(result.Shared)
			}
		}
	}
	return result
}

func keyViewTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func setKeyViewCookie(c *gin.Context, token string, age int) {
	secure := c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https"
	if origin, err := url.Parse(c.GetHeader("Origin")); err == nil && origin.Scheme == "https" {
		secure = true
	}
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(keyViewCookie, token, age, "/api/key", "", secure, true)
}

func sameKeyViewOrigin(c *gin.Context) bool {
	if c.GetHeader("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	if origin := c.GetHeader("Origin"); origin != "" {
		u, err := url.Parse(origin)
		return err == nil && (u.Scheme == "http" || u.Scheme == "https") && strings.EqualFold(u.Host, c.Request.Host)
	}
	return true
}

var keyViewAttempts = struct {
	sync.Mutex
	clients map[string]*keyViewAttempt
}{clients: make(map[string]*keyViewAttempt)}

type keyViewAttempt struct {
	limiter *rate.Limiter
	seen    time.Time
}

func allowKeyViewLogin(ip string) bool {
	keyViewAttempts.Lock()
	defer keyViewAttempts.Unlock()
	now := time.Now()
	for addr, attempt := range keyViewAttempts.clients {
		if now.Sub(attempt.seen) > 10*time.Minute {
			delete(keyViewAttempts.clients, addr)
		}
	}
	attempt := keyViewAttempts.clients[ip]
	if attempt == nil {
		if len(keyViewAttempts.clients) >= 10000 {
			return false
		}
		attempt = &keyViewAttempt{limiter: rate.NewLimiter(rate.Every(6*time.Second), 5)}
		keyViewAttempts.clients[ip] = attempt
	}
	attempt.seen = now
	return attempt.limiter.Allow()
}

func registerKeyView(r *gin.Engine) {
	g := r.Group("/api/key")
	g.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		if c.Request.Method != http.MethodGet && !sameKeyViewOrigin(c) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "请在本站查询密钥。"})
			return
		}
		c.Next()
	})
	g.POST("/session", func(c *gin.Context) {
		if !allowKeyViewLogin(c.ClientIP()) {
			c.Header("Retry-After", "6")
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "尝试过于频繁，请稍后再试。"})
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
		var request struct {
			Key string `json:"key"`
		}
		if c.ShouldBindJSON(&request) != nil || strings.TrimSpace(request.Key) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请输入 API 密钥。"})
			return
		}
		ak, err := dbGetKeyByKey(strings.TrimSpace(request.Key))
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !ak.Enabled) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "密钥无效或已停用，请检查后重试。"})
			return
		}
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "暂时无法查询额度，请稍后重试。"})
			return
		}
		var random [32]byte
		if _, err := rand.Read(random[:]); err != nil {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		token := hex.EncodeToString(random[:])
		tx, err := db.Begin()
		if err != nil {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		defer tx.Rollback()
		if _, err = tx.Exec(`DELETE FROM key_view_sessions WHERE expires_at<=?`, time.Now().Unix()); err == nil {
			if old, _ := c.Cookie(keyViewCookie); old != "" {
				_, err = tx.Exec(`DELETE FROM key_view_sessions WHERE token_hash=?`, keyViewTokenHash(old))
			}
		}
		if err == nil {
			// Check enabled state again in the write transaction so disabling a key
			// cannot race verification and leave a newly minted session behind.
			var inserted sql.Result
			inserted, err = tx.Exec(`INSERT INTO key_view_sessions(token_hash,key_id,expires_at)
				SELECT ?,id,? FROM api_keys WHERE id=? AND enabled=1 AND key=?`,
				keyViewTokenHash(token), time.Now().Add(keyViewLifetime).Unix(), ak.ID, ak.Key)
			if err == nil {
				var count int64
				count, err = inserted.RowsAffected()
				if err == nil && count == 0 {
					c.JSON(http.StatusUnauthorized, gin.H{"error": "密钥已停用或删除，请重新验证。"})
					return
				}
			}
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		setKeyViewCookie(c, token, int(keyViewLifetime/time.Second))
		c.JSON(http.StatusOK, buildKeyOverview(ak))
	})
	readOverview := func(c *gin.Context) {
		token, _ := c.Cookie(keyViewCookie)
		unauthenticated := func(message string) {
			if token != "" {
				setKeyViewCookie(c, "", -1)
			}
			if c.FullPath() == "/api/key/session" {
				c.JSON(http.StatusOK, gin.H{"authenticated": false})
			} else {
				c.JSON(http.StatusUnauthorized, gin.H{"error": message})
			}
		}
		if token == "" {
			unauthenticated("请输入密钥查看个人额度。")
			return
		}
		var keyID int64
		err := db.QueryRow(`SELECT key_id FROM key_view_sessions WHERE token_hash=? AND expires_at>?`, keyViewTokenHash(token), time.Now().Unix()).Scan(&keyID)
		if errors.Is(err, sql.ErrNoRows) {
			unauthenticated("请输入密钥查看个人额度。")
			return
		}
		if err != nil {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		ak, err := dbGetKeyByID(keyID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !ak.Enabled) {
			_, _ = db.Exec(`DELETE FROM key_view_sessions WHERE token_hash=?`, keyViewTokenHash(token))
			unauthenticated("密钥已停用或删除，请重新验证。")
			return
		}
		if err != nil {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		if c.FullPath() == "/api/key/session" {
			c.JSON(http.StatusOK, gin.H{"authenticated": true, "overview": buildKeyOverview(ak)})
		} else {
			c.JSON(http.StatusOK, buildKeyOverview(ak))
		}
	}
	g.GET("/overview", readOverview)
	g.GET("/session", readOverview)
	g.DELETE("/session", func(c *gin.Context) {
		if token, _ := c.Cookie(keyViewCookie); token != "" {
			if _, err := db.Exec(`DELETE FROM key_view_sessions WHERE token_hash=?`, keyViewTokenHash(token)); err != nil {
				c.Status(http.StatusServiceUnavailable)
				return
			}
		}
		setKeyViewCookie(c, "", -1)
		c.Status(http.StatusNoContent)
	})
}
