package main

import (
	"context"
	"github.com/gin-gonic/gin"
	"time"
)

func registerCLIProxyQuotaRoutes(group *gin.RouterGroup) {
	group.GET("/credentials/quota", func(c *gin.Context) {
		name := c.Query("name")
		if !validCoreCredentialName(name) {
			c.JSON(400, gin.H{"error": "请选择有效的账号凭据"})
			return
		}
		manager := requestCLIProxyCore(c)
		files, err := manager.ReadCredentials()
		if err != nil {
			c.JSON(502, gin.H{"error": "无法读取当前池的账号凭据"})
			return
		}
		if files[name] == nil {
			c.JSON(404, gin.H{"error": "账号不属于当前池"})
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
		defer cancel()
		view, err := manager.QueryQuota(ctx, name, c.Query("refresh") == "1")
		if err != nil {
			c.JSON(502, gin.H{"error": "额度查询暂不可用，请稍后重试"})
			return
		}
		c.JSON(200, view)
	})
}
