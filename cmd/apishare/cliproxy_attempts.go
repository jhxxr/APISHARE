package main

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"apishare/internal/cliproxy"
	"github.com/gin-gonic/gin"
)

func registerCLIProxyAttemptRoutes(group *gin.RouterGroup) {
	group.GET("/attempts", func(c *gin.Context) {
		account, trace := strings.TrimSpace(c.Query("account")), strings.TrimSpace(c.Query("trace_id"))
		before, _ := strconv.ParseInt(c.Query("before"), 10, 64)
		if len(account) > 256 || len(trace) > 64 || before < 0 {
			c.JSON(400, gin.H{"error": "日志筛选无效"})
			return
		}
		poolID := requestCLIProxyPoolID(c)
		if poolID != 0 {
			page, err := requestCLIProxyCore(c).Attempts(c.Request.Context(), account, trace, before, 50)
			if err != nil {
				c.JSON(502, gin.H{"error": "无法读取账号调用记录"})
				return
			}
			c.JSON(200, page)
			return
		}
		if trace == "" {
			c.JSON(400, gin.H{"error": "请选择一条请求查看账号尝试"})
			return
		}
		upstreams, err := dbListUpstreams()
		if err != nil {
			c.JSON(500, gin.H{"error": "无法读取请求来源"})
			return
		}
		items := make([]cliproxy.AccountAttempt, 0)
		for _, up := range upstreams {
			if up.Type != "cliproxy" {
				continue
			}
			manager, err := cliProxyPool(up.ID)
			if err != nil {
				continue
			}
			page, err := manager.Attempts(c.Request.Context(), "", trace, 0, 200)
			if err != nil {
				c.JSON(502, gin.H{"error": "部分账号记录不可读取，请稍后重试"})
				return
			}
			for _, item := range page.Items {
				item.PoolID = up.ID
				item.PoolName = up.Name
				items = append(items, item)
			}
		}
		sort.SliceStable(items, func(i, j int) bool {
			a, _ := time.Parse(time.RFC3339Nano, items[i].CreatedAt)
			b, _ := time.Parse(time.RFC3339Nano, items[j].CreatedAt)
			if a.Equal(b) {
				return items[i].ID < items[j].ID
			}
			return a.Before(b)
		})
		for i := range items {
			items[i].Attempt = i + 1
		}
		c.JSON(200, gin.H{"items": items, "next_id": 0})
	})
}
