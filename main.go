package main

import (
	"embed"
	"fmt"
	"net/http"
	"os"

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
	go affinityCleanupLoop()
	go limiterJanitorLoop()
	go pricingRefresherLoop()
	go clientVersionRefreshLoop()

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.SetTrustedProxies(nil)

	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	v1 := r.Group("/v1")
	v1.Use(corsV1(), keyAuth(), keyRateLimit())
	v1.POST("/chat/completions", relayHandler(FormatOpenAIChat, false))
	v1.POST("/responses", relayHandler(FormatOpenAIResponse, false))
	v1.POST("/messages", relayHandler(FormatAnthropic, false))
	v1.POST("/embeddings", relayHandler(FormatOpenAIChat, true))
	registerModelRoutes(v1)

	v1beta := r.Group("/v1beta")
	v1beta.Use(corsV1(), keyAuth(), keyRateLimit())
	v1beta.POST("/models/*action", relayHandler(FormatGemini, false))

	registerAdmin(r)
	registerPublic(r)

	// Unregistered routes remain private; the root is the explicit public dashboard.
	r.NoRoute(func(c *gin.Context) {
		c.String(http.StatusNotFound, "404 page not found")
	})

	addr := ":" + port
	fmt.Println("apishare listening on", addr, "| admin:", currentAdminPath())
	if adminSetupRequired() {
		fmt.Println("首次部署：打开首页完成管理员密码和后台路径配置")
	}
	if err := r.Run(addr); err != nil {
		fmt.Println("server error:", err)
		os.Exit(1)
	}
}

// corsV1 允许浏览器端 OpenAI SDK 直接调用（Key 本身就是凭证，放开 CORS 无风险）。
func corsV1() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Headers",
			"Authorization, Content-Type, x-api-key, x-goog-api-key, anthropic-version, anthropic-beta, X-Session-Id, X-Conversation-Id")
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
