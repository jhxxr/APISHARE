package main

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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
			// Browser WebSockets cannot set Authorization. Accept the OpenAI
			// credential subprotocol only on the authenticated Realtime route.
			if c.FullPath() == "/v1/realtime" && c.Request.Method == http.MethodGet {
				for _, protocol := range websocketProtocols(c.Request.Header) {
					if strings.HasPrefix(protocol, "openai-insecure-api-key.") {
						key = strings.TrimPrefix(protocol, "openai-insecure-api-key.")
						break
					}
				}
			}
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
		c.Set("apiKey", *ak)
		c.Next()
	}
}

// ---------- 每 Key 限流：QPS 令牌桶 + 并发信号量 ----------

type keyUsageWindow struct {
	mu       sync.Mutex
	start    time.Time
	requests int
	tokens   int
}

type keyLimit struct {
	qps    float64
	conc   int
	rpm    int
	tpm    int
	lim    *rate.Limiter
	sem    chan struct{}
	active *atomic.Int64 // Shared by revisions so edits cannot reset in-flight usage.
	usage  *keyUsageWindow
	lastAt time.Time
}

var limReg = struct {
	sync.Mutex
	m map[int64]*keyLimit
}{m: map[int64]*keyLimit{}}

func getLimit(ak ApiKey) *keyLimit {
	limReg.Lock()
	defer limReg.Unlock()
	conc := max(1, ak.Concurrency)
	kl, ok := limReg.m[ak.ID]
	var active *atomic.Int64
	var usage *keyUsageWindow
	if ok {
		active = kl.active
		usage = kl.usage
	}
	if ok && (kl.qps != ak.QPS || kl.conc != conc || kl.rpm != ak.RPM || kl.tpm != ak.TPM) {
		ok = false // 管理端改过参数，重建
	}
	if !ok {
		burst := int(ak.QPS)
		if burst < 1 {
			burst = 1
		}
		if active == nil {
			active = &atomic.Int64{}
		}
		if usage == nil {
			usage = &keyUsageWindow{start: time.Now()}
		}
		kl = &keyLimit{
			qps:    ak.QPS,
			conc:   conc,
			rpm:    ak.RPM,
			tpm:    ak.TPM,
			lim:    rate.NewLimiter(rate.Limit(ak.QPS), burst),
			sem:    make(chan struct{}, conc),
			active: active,
			usage:  usage,
		}
		limReg.m[ak.ID] = kl
	}
	kl.lastAt = time.Now()
	return kl
}

func (limit *keyLimit) resetUsageWindowLocked(now time.Time) {
	if now.Sub(limit.usage.start) >= time.Minute {
		limit.usage.start = now
		limit.usage.requests = 0
		limit.usage.tokens = 0
	}
}

func (limit *keyLimit) admit() string {
	limit.usage.mu.Lock()
	defer limit.usage.mu.Unlock()
	limit.resetUsageWindowLocked(time.Now())
	if limit.rpm > 0 && limit.usage.requests >= limit.rpm {
		return "RPM"
	}
	if limit.tpm > 0 && limit.usage.tokens >= limit.tpm {
		return "TPM"
	}
	limit.usage.requests++
	return ""
}

func (limit *keyLimit) chargeTokens(tokens int) {
	if tokens <= 0 || limit.tpm <= 0 {
		return
	}
	limit.usage.mu.Lock()
	limit.resetUsageWindowLocked(time.Now())
	limit.usage.tokens += tokens
	limit.usage.mu.Unlock()
}

func (limit *keyLimit) acquire() bool {
	for {
		active := limit.active.Load()
		if active >= int64(limit.conc) {
			return false
		}
		if limit.active.CompareAndSwap(active, active+1) {
			select {
			case limit.sem <- struct{}{}:
				return true
			default:
				limit.active.Add(-1)
				return false
			}
		}
	}
}

func (limit *keyLimit) release() {
	<-limit.sem
	limit.active.Add(-1)
}

func limiterJanitorLoop() {
	for {
		time.Sleep(10 * time.Minute)
		limReg.Lock()
		for id, kl := range limReg.m {
			if time.Since(kl.lastAt) > time.Hour && kl.active.Load() == 0 {
				delete(limReg.m, id)
			}
		}
		limReg.Unlock()
	}
}

func keyRateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Responses sockets admit and release each turn individually. Idle
		// connections must not consume inference slots or a turn's QPS token.
		if c.Request.Method == http.MethodGet && c.FullPath() == "/v1/responses" {
			c.Next()
			return
		}
		ak := c.MustGet("apiKey").(ApiKey)
		kl := getLimit(ak)
		if !kl.lim.Allow() {
			c.Header("X-APISHARE-RateLimit-Source", "gateway-qps")
			c.Header("Retry-After", "1")
			errJSON(c, http.StatusTooManyRequests, "QPS limit exceeded for this key")
			c.Abort()
			return
		}
		if kl.acquire() {
			if exceeded := kl.admit(); exceeded != "" {
				kl.release()
				c.Header("X-APISHARE-RateLimit-Source", "gateway-"+strings.ToLower(exceeded))
				c.Header("Retry-After", "60")
				errJSON(c, http.StatusTooManyRequests, exceeded+" limit exceeded for this key")
				c.Abort()
				return
			}
			c.Set("keyLimit", kl)
			defer kl.release()
			c.Next()
		} else {
			c.Header("X-APISHARE-RateLimit-Source", "gateway-concurrency")
			c.Header("Retry-After", "1")
			errJSON(c, http.StatusTooManyRequests, "concurrency limit exceeded for this key")
			c.Abort()
			return
		}
	}
}
