package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"apishare/internal/cliproxy"
	"github.com/gin-gonic/gin"
)

var vertexLocationPattern = regexp.MustCompile(`^(global|[a-z]+(?:-[a-z0-9]+){1,4}[0-9])$`)

type poolAccountOperation struct {
	Action       string   `json:"action"`
	Names        []string `json:"names"`
	ProxyID      int64    `json:"proxy_id"`
	TargetPoolID int64    `json:"target_pool_id"`
}
type poolAccountResult struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

func registerCLIProxyOperationRoutes(group *gin.RouterGroup) {
	group.GET("/routing", func(c *gin.Context) {
		settings, err := requestCLIProxyCore(c).Routing()
		if err != nil {
			c.JSON(502, gin.H{"error": "无法读取池内调度设置"})
			return
		}
		c.JSON(200, settings)
	})
	group.PUT("/routing", func(c *gin.Context) {
		var settings cliproxy.PoolRouting
		if c.ShouldBindJSON(&settings) != nil {
			c.JSON(400, gin.H{"error": "调度参数无效"})
			return
		}
		coreOperation(c, func() error { return requestCLIProxyCore(c).SetRouting(c.Request.Context(), settings) })
	})
	group.PATCH("/credentials/settings", func(c *gin.Context) {
		var request struct {
			Name       string  `json:"name"`
			Priority   *int    `json:"priority"`
			Weight     *int    `json:"weight"`
			Location   *string `json:"location"`
			Label      *string `json:"label"`
			Note       *string `json:"note"`
			Websockets *bool   `json:"websockets"`
		}
		if c.ShouldBindJSON(&request) != nil || !validCoreCredentialName(request.Name) {
			c.JSON(400, gin.H{"error": "账号设置无效"})
			return
		}
		proxyConfigMu.Lock()
		defer proxyConfigMu.Unlock()
		fields := make(map[string]any)
		if request.Label != nil {
			if utf8.RuneCountInString(*request.Label) > 80 {
				c.JSON(400, gin.H{"error": "账号名称最多 80 字"})
				return
			}
			fields["label"] = strings.TrimSpace(*request.Label)
		}
		if request.Note != nil {
			if utf8.RuneCountInString(*request.Note) > 500 {
				c.JSON(400, gin.H{"error": "账号备注最多 500 字"})
				return
			}
			fields["note"] = strings.TrimSpace(*request.Note)
		}
		if request.Priority != nil {
			if *request.Priority < 0 || *request.Priority > 1000 {
				c.JSON(400, gin.H{"error": "账号优先级需在 0–1000 之间，数值越大越优先"})
				return
			}
			fields["priority"] = *request.Priority
		}
		if request.Weight != nil {
			if *request.Weight < 1 || *request.Weight > 1000000 {
				c.JSON(400, gin.H{"error": "账号权重需在 1–1000000 之间"})
				return
			}
			fields["weight"] = *request.Weight
		}
		manager := requestCLIProxyCore(c)
		files, err := manager.ReadCredentials()
		if err != nil || files[request.Name] == nil {
			c.JSON(404, gin.H{"error": "账号不属于当前池"})
			return
		}
		if request.Websockets != nil {
			var document struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(files[request.Name], &document)
			if document.Type != "codex" && document.Type != "xai" {
				c.JSON(400, gin.H{"error": "仅 Codex / xAI 账号支持此 WebSocket 设置"})
				return
			}
			fields["websockets"] = *request.Websockets
		}
		if request.Location != nil {
			var document struct {
				Type string `json:"type"`
			}
			json.Unmarshal(files[request.Name], &document)
			if document.Type != "vertex" || !vertexLocationPattern.MatchString(*request.Location) {
				c.JSON(400, gin.H{"error": "请选择有效的 Vertex AI 地区"})
				return
			}
			fields["location"] = *request.Location
		}
		coreOperation(c, func() error { return manager.PatchCredential(c.Request.Context(), request.Name, fields) })
	})
	group.POST("/credentials/batch", func(c *gin.Context) {
		var operation poolAccountOperation
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32768)
		if c.ShouldBindJSON(&operation) != nil || len(operation.Names) == 0 || len(operation.Names) > 100 {
			c.JSON(400, gin.H{"error": "每次请选择 1–100 个账号"})
			return
		}
		switch operation.Action {
		case "enable", "disable", "refresh", "proxy", "move":
		default:
			c.JSON(400, gin.H{"error": "不支持的批量操作"})
			return
		}
		proxyConfigMu.Lock()
		defer proxyConfigMu.Unlock()
		if operation.Action == "proxy" && (operation.ProxyID < -1 || operation.ProxyID > 0 && validateUpstreamProxy(operation.ProxyID) != nil) {
			c.JSON(400, gin.H{"error": "请选择有效的代理"})
			return
		}
		sourceID := requestCLIProxyPoolID(c)
		source := requestCLIProxyCore(c)
		var target *cliproxy.Manager
		if operation.Action == "move" {
			if operation.TargetPoolID == sourceID {
				c.JSON(400, gin.H{"error": "请选择其他账号池"})
				return
			}
			var err error
			target, err = cliProxyPool(operation.TargetPoolID)
			if err != nil {
				c.JSON(400, gin.H{"error": "目标账号池不存在"})
				return
			}
		}
		files, err := source.ReadCredentials()
		if err != nil {
			c.JSON(502, gin.H{"error": "无法读取账号列表"})
			return
		}
		results := make([]poolAccountResult, 0, len(operation.Names))
		seen := make(map[string]bool)
		for _, name := range operation.Names {
			if seen[name] {
				continue
			}
			seen[name] = true
			result := poolAccountResult{Name: name}
			if !validCoreCredentialName(name) || files[name] == nil {
				result.Message = "账号不属于当前池"
				results = append(results, result)
				continue
			}
			var err error
			switch operation.Action {
			case "enable", "disable":
				err = source.PatchCredential(c.Request.Context(), name, gin.H{"disabled": operation.Action == "disable"})
			case "refresh":
				err = source.Client().Do(c.Request.Context(), "POST", "/credentials/refresh", gin.H{"name": name}, nil)
			case "proxy":
				err = saveAccountProxy(c.Request.Context(), source, sourceID, name, operation.ProxyID)
			case "move":
				err = movePoolCredential(c.Request.Context(), source, sourceID, target, operation.TargetPoolID, name)
			}
			result.OK = err == nil
			if err != nil {
				result.Message = err.Error()
			}
			results = append(results, result)
		}
		if target != nil {
			_ = target.RefreshRuntime(c.Request.Context())
		}
		c.JSON(200, gin.H{"results": results})
	})
	group.GET("/pools", func(c *gin.Context) {
		upstreams, err := dbListUpstreams()
		if err != nil {
			c.JSON(500, gin.H{"error": "无法读取目标池列表"})
			return
		}
		out := make([]gin.H, 0)
		for _, up := range upstreams {
			if up.Type == "cliproxy" && up.ID != requestCLIProxyPoolID(c) {
				if manager, err := cliProxyPool(up.ID); err == nil {
					out = append(out, gin.H{"id": up.ID, "name": up.Name, "running": manager.Running()})
				}
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i]["id"].(int64) < out[j]["id"].(int64) })
		c.JSON(200, gin.H{"pools": out})
	})
}
