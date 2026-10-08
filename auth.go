package main

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// ---------- API Key 鉴权 ----------

func errJSON(c *gin.Context, status int, msg string) {
	if (c.FullPath() == "/v1/models" || c.FullPath() == "/v1/models/:model") && isClaudeModelRequest(c) {
		modelError(c, status, msg)
		return
	}
	// OpenAI 风格错误体，两种客户端都能正常解析
	c.JSON(status, gin.H{"error": gin.H{
		"message": msg,
		"type":    "apishare_error",
		"code":    status,
	}})
}

func keyAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := strings.TrimSpace(c.GetHeader("Authorization"))
		key = strings.TrimPrefix(key, "Bearer ")
		if key == "" {
			key = strings.TrimSpace(c.GetHeader("x-api-key"))
		}
		if key == "" {
			key = strings.TrimSpace(c.GetHeader("x-goog-api-key")) // Gemini 客户端
		}
		if key == "" {
			key = strings.TrimSpace(c.Query("key")) // Gemini REST 风格 ?key=
		}
		if key == "" {
			errJSON(c, http.StatusUnauthorized, "missing API key")
			c.Abort()
			return
		}
		ak, err := dbGetKeyByKey(key)
		if err != nil || !ak.Enabled {
			errJSON(c, http.StatusUnauthorized, "invalid or disabled API key")
			c.Abort()
			return
		}
		if ak.QuotaUSD > 0 && ak.UsedUSD >= ak.QuotaUSD {
			errJSON(c, http.StatusPaymentRequired, "quota exhausted for key "+ak.Name)
			c.Abort()
			return
		}
		c.Set("apiKey", *ak)
		c.Next()
	}
}

// ---------- 每 Key 限流：QPS 令牌桶 + 并发信号量 ----------

type keyLimit struct {
	qps    float64
	conc   int
	lim    *rate.Limiter
	sem    chan struct{}
	lastAt time.Time
}

var limReg = struct {
	sync.Mutex
	m map[int64]*keyLimit
}{m: map[int64]*keyLimit{}}

func getLimit(ak ApiKey) *keyLimit {
	limReg.Lock()
	defer limReg.Unlock()
	kl, ok := limReg.m[ak.ID]
	if ok && (kl.qps != ak.QPS || kl.conc != ak.Concurrency) {
		ok = false // 管理端改过参数，重建
	}
	if !ok {
		burst := int(ak.QPS)
		if burst < 1 {
			burst = 1
		}
		conc := ak.Concurrency
		if conc < 1 {
			conc = 1
		}
		kl = &keyLimit{
			qps:  ak.QPS,
			conc: conc,
			lim:  rate.NewLimiter(rate.Limit(ak.QPS), burst),
			sem:  make(chan struct{}, conc),
		}
		limReg.m[ak.ID] = kl
	}
	kl.lastAt = time.Now()
	return kl
}

func limiterJanitorLoop() {
	for {
		time.Sleep(10 * time.Minute)
		limReg.Lock()
		for id, kl := range limReg.m {
			if time.Since(kl.lastAt) > time.Hour {
				delete(limReg.m, id)
			}
		}
		limReg.Unlock()
	}
}

func keyRateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		ak := c.MustGet("apiKey").(ApiKey)
		kl := getLimit(ak)
		if !kl.lim.Allow() {
			errJSON(c, http.StatusTooManyRequests, "QPS limit exceeded for this key")
			c.Abort()
			return
		}
		select {
		case kl.sem <- struct{}{}:
			defer func() { <-kl.sem }()
			c.Next()
		case <-c.Request.Context().Done():
			return
		default:
			errJSON(c, http.StatusTooManyRequests, "concurrency limit exceeded for this key")
			c.Abort()
			return
		}
	}
}
