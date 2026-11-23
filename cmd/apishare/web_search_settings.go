package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/http/httpguts"
)

const webSearchSettingsKey = "responses_web_search"

// The database record is private. The admin API builds a separate, redacted
// response rather than returning the stored settings object.
type webSearchSettings struct {
	Provider string `json:"provider"`
	APIKey   string `json:"api_key,omitempty"`
}

var webSearchSettingsMu sync.Mutex

func readWebSearchSettings() (webSearchSettings, error) {
	raw, err := settingsGet(webSearchSettingsKey)
	if err != nil {
		return webSearchSettings{}, err
	}
	if raw == "" {
		return webSearchSettings{Provider: "environment"}, nil
	}
	var settings webSearchSettings
	if json.Unmarshal([]byte(raw), &settings) != nil {
		return webSearchSettings{}, fmt.Errorf("invalid stored web search settings")
	}
	return settings, nil
}

func braveSearchEnvKey() string {
	return strings.TrimSpace(os.Getenv("RESPONSES_BRAVE_SEARCH_API_KEY"))
}

func (settings webSearchSettings) effective() webSearchSettings {
	if settings.Provider == "environment" {
		return webSearchSettings{Provider: "brave", APIKey: braveSearchEnvKey()}
	}
	return settings
}

func registerWebSearchAdminRoutes(auth *gin.RouterGroup) {
	public := func(s webSearchSettings) gin.H {
		return gin.H{"provider": s.Provider, "key_saved": s.APIKey != "", "environment_key_set": braveSearchEnvKey() != ""}
	}
	auth.GET("/settings/web-search", func(c *gin.Context) {
		s, err := readWebSearchSettings()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "无法读取网页搜索设置"})
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, public(s))
	})
	auth.PUT("/settings/web-search", func(c *gin.Context) {
		var incoming webSearchSettings
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8192)
		if c.ShouldBindJSON(&incoming) != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的网页搜索设置"})
			return
		}
		incoming.APIKey = strings.TrimSpace(incoming.APIKey)
		webSearchSettingsMu.Lock()
		defer webSearchSettingsMu.Unlock()
		switch incoming.Provider {
		case "environment", "disabled":
			incoming.APIKey = ""
		case "brave", "tavily", "exa":
			if incoming.APIKey == "" {
				old, err := readWebSearchSettings()
				if err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "无法读取网页搜索设置"})
					return
				}
				if old.Provider == incoming.Provider {
					incoming.APIKey = old.APIKey
				}
			}
			if incoming.APIKey == "" || len(incoming.APIKey) > 1024 || !httpguts.ValidHeaderFieldValue(incoming.APIKey) || strings.ContainsAny(incoming.APIKey, " \t") {
				c.JSON(http.StatusBadRequest, gin.H{"error": "请输入所选搜索供应商的有效 API Key"})
				return
			}
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "不支持该搜索供应商"})
			return
		}
		encoded, _ := json.Marshal(incoming)
		if err := settingsSet(webSearchSettingsKey, string(encoded)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "无法保存网页搜索设置"})
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, public(incoming))
	})
}
