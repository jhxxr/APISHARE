package main

import (
	"context"
	"embed"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
)

//go:embed web
var webFS embed.FS

const defaultAdminPath = "/admin"

var adminPath = defaultAdminPath

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	port := envOr("PORT", "8080")
	dbPath := envOr("API_DB", "apishare.db")

	if err := openDB(dbPath); err != nil {
		fmt.Println("failed to open database:", err)
		os.Exit(1)
	}
	if err := loadSettingsCache(); err != nil {
		fmt.Println("failed to load settings:", err)
	}
	loadAutoPricingCache()
	loadClientVersionCache()
	if err := bootstrapAdmin(); err != nil {
		fmt.Println("failed to bootstrap admin:", err)
		os.Exit(1)
	}
	if err := initializeCLIProxy(dbPath); err != nil {
		fmt.Println("failed to initialize CLIProxyAPI:", err)
		os.Exit(1)
	}
	defer closeCLIProxyPools()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		if err := resumeCLIProxy(ctx); err != nil {
			fmt.Println("CLIProxyAPI:", err)
		}
		if err := resumeCLIProxyPools(ctx); err != nil {
			fmt.Println("CLIProxyAPI pools:", err)
		}
		resumePoolJobs(ctx)
	}()
	go affinityCleanupLoop()
	go limiterJanitorLoop()
	go pricingRefresherLoop()
	go clientVersionRefreshLoop()
	go logRetentionLoop()
	warmTokenEncodings()

	gin.SetMode(gin.ReleaseMode)
	r := newRouter()

	addr := ":" + port
	fmt.Println("apishare listening on", addr, "| admin:", currentAdminPath())
	if adminSetupRequired() {
		fmt.Println("首次部署：打开首页完成管理员密码和后台路径配置")
	}
	server := &http.Server{Addr: addr, Handler: r, ReadHeaderTimeout: 15 * time.Second}
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		defer close(shutdownDone)
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Println("server error:", err)
		cliProxyCore.Close()
		os.Exit(1)
	}
	if ctx.Err() != nil {
		<-shutdownDone
	}
}

func newRouter() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.SetTrustedProxies(nil)

	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	v1 := r.Group("/v1")
	v1.Use(corsV1(), keyAuth(), trackRequestClient(), keyRateLimit())
	v1.POST("/chat/completions", relayHandler(FormatOpenAIChat, false))
	v1.POST("/responses", relayHandler(FormatOpenAIResponse, false))
	v1.POST("/responses/compact", relayHandler(FormatOpenAIResponse, true))
	v1.POST("/messages", relayHandler(FormatAnthropic, false))
	v1.POST("/embeddings", relayHandler(FormatOpenAIChat, true))
	registerAudioRoutes(v1)
	registerModelRoutes(v1)

	v1beta := r.Group("/v1beta")
	v1beta.Use(corsV1(), keyAuth(), trackRequestClient(), keyRateLimit())
	v1beta.POST("/models/*action", relayHandler(FormatGemini, false))

	registerAdmin(r)
	registerPublic(r)

	// Unregistered routes remain private; the root is the explicit public dashboard.
	r.NoRoute(func(c *gin.Context) {
		// Handle generic API preflights here: a wildcard OPTIONS route would
		// conflict with the explicit model preflights and panic during startup.
		if c.Request.Method == http.MethodOptions && strings.HasPrefix(c.Request.URL.Path, "/v1/") {
			corsV1()(c)
			return
		}
		c.String(http.StatusNotFound, "404 page not found")
	})
	return r
}

// corsV1 允许浏览器端 OpenAI SDK 直接调用（Key 本身就是凭证，放开 CORS 无风险）。
func corsV1() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Headers",
			"Authorization, Content-Type, OpenAI-Beta, x-api-key, x-goog-api-key, anthropic-version, anthropic-beta, X-Session-Id, X-Conversation-Id")
		h.Set("Access-Control-Expose-Headers", "Location, X-Request-Id, Retry-After, X-APISHARE-RateLimit-Source")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if c.Request.Method == http.MethodOptions {
			if requested := c.GetHeader("Access-Control-Request-Headers"); requested != "" {
				h.Set("Access-Control-Allow-Headers", requested)
				h.Add("Vary", "Access-Control-Request-Headers")
			}
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
