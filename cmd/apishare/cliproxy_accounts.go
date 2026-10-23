package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"apishare/internal/cliproxy"
	"github.com/gin-gonic/gin"
)

type duplicatePoolCredential struct {
	Name     string
	Provider string
}

func (e *duplicatePoolCredential) Error() string {
	return "当前池已有此账号，确认更新凭据后再导入"
}

func credentialIdentity(document map[string]any) (string, string) {
	provider, _ := document["type"].(string)
	provider = strings.ToLower(strings.TrimSpace(provider))
	identity := ""
	for _, field := range []string{"account_id", "account_uuid", "user_id", "sub", "email", "client_email"} {
		if value, ok := document[field].(string); ok && strings.TrimSpace(value) != "" {
			identity = strings.ToLower(strings.TrimSpace(value))
			break
		}
	}
	if identity == "" {
		// JWT claims are only a duplicate hint; they never authenticate a request.
		for _, field := range []string{"id_token", "access_token"} {
			token, _ := document[field].(string)
			parts := strings.Split(token, ".")
			if len(parts) != 3 || len(parts[1]) > 32768 {
				continue
			}
			raw, err := base64.RawURLEncoding.DecodeString(parts[1])
			if err != nil {
				continue
			}
			var claims map[string]any
			if json.Unmarshal(raw, &claims) != nil {
				continue
			}
			for _, key := range []string{"sub", "user_id", "email"} {
				if value, ok := claims[key].(string); ok && strings.TrimSpace(value) != "" {
					identity = strings.ToLower(strings.TrimSpace(value))
					break
				}
			}
			if identity != "" {
				break
			}
		}
	}
	if service, ok := document["service_account"].(map[string]any); ok {
		if email, ok := service["client_email"].(string); ok {
			identity = strings.ToLower(strings.TrimSpace(email))
		}
	}
	if provider == "service_account" {
		provider = "vertex"
	}
	if provider == "claude" && identity != "" {
		if organization, ok := document["organization_uuid"].(string); ok && organization != "" {
			identity += ":" + strings.ToLower(organization)
		}
	}
	if provider == "kimi.com" {
		provider = "kimi"
	}
	if provider == "kimi.ai" {
		provider = "kimi-ai"
	}
	if provider == "kimi" {
		if domain, ok := document["domain"].(string); ok && (domain == "kimi.ai" || strings.HasSuffix(domain, ".kimi.ai")) {
			provider = "kimi-ai"
		}
	}
	return provider, identity
}

// Only credential material is replaced; operator settings and the gateway's
// proxy binding belong to the existing account and survive reauthorization.
func prepareCredentialReplacement(manager *cliproxy.Manager, name string, document map[string]any, replace bool) (string, bool, error) {
	files, err := manager.ReadCredentials()
	if err != nil {
		return "", false, err
	}
	provider, identity := credentialIdentity(document)
	target := ""
	existingDocs := make(map[string]map[string]any)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, existing := range names {
		raw := files[existing]
		var old map[string]any
		if json.Unmarshal(raw, &old) != nil {
			continue
		}
		existingDocs[existing] = old
		oldProvider, oldIdentity := credentialIdentity(old)
		if existing == name || strings.EqualFold(existing, name) {
			if provider != oldProvider || identity != "" && oldIdentity != "" && identity != oldIdentity {
				return "", false, errors.New("同名文件属于另一个账号，请重命名凭据文件后导入")
			}
			if target != "" && target != existing {
				return "", false, errors.New("此账号在当前池中存在多份凭据，请先处理重复项")
			}
			target = existing
		}
		if identity != "" && provider == oldProvider && identity == oldIdentity {
			if target != "" && target != existing {
				return "", false, errors.New("此账号在当前池中存在多份凭据，请先处理重复项")
			}
			target = existing
		}
	}
	if target == "" {
		return name, false, nil
	}
	old := existingDocs[target]
	oldProvider, oldIdentity := credentialIdentity(old)
	if provider != oldProvider || identity != "" && oldIdentity != "" && identity != oldIdentity {
		return "", false, errors.New("同名文件属于另一个账号，请重命名凭据文件后导入")
	}
	if !replace {
		return "", false, &duplicatePoolCredential{Name: target, Provider: provider}
	}
	for _, field := range []string{"disabled", "label", "note", "proxy_url", "prefix", "priority", "weight", "excluded_models", "oauth_model_aliases", "request_retry", "headers", "siwc", "use_siwc", "location", "websockets"} {
		if value, exists := old[field]; exists {
			document[field] = value
		} else {
			delete(document, field)
		}
	}
	return target, true, nil
}

func credentialImportError(c *gin.Context, err error) {
	var duplicate *duplicatePoolCredential
	if errors.As(err, &duplicate) {
		c.JSON(409, gin.H{"error": duplicate.Error(), "code": "credential_exists", "existing_name": duplicate.Name, "provider": duplicate.Provider})
		return
	}
	c.JSON(400, gin.H{"error": err.Error()})
}

func registerCLIProxyAccountRoutes(group *gin.RouterGroup) {
	group.GET("/credentials/models", func(c *gin.Context) {
		name := c.Query("name")
		if !requirePoolCredential(c, name) {
			return
		}
		manager := requestCLIProxyCore(c)
		if err := manager.RefreshRuntime(c.Request.Context()); err != nil {
			c.JSON(502, gin.H{"error": "无法读取账号模型，请检查账号池运行状态"})
			return
		}
		for _, account := range manager.Runtime().Accounts {
			if account.Name == name || account.ID == name {
				c.JSON(200, gin.H{"account_id": account.ID, "models": account.Models, "details": account})
				return
			}
		}
		c.JSON(404, gin.H{"error": "账号不可用"})
	})
	group.POST("/credentials/test", func(c *gin.Context) {
		var request struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		}
		if c.ShouldBindJSON(&request) != nil || strings.TrimSpace(request.Model) == "" || len(request.Model) > 256 {
			c.JSON(400, gin.H{"error": "请选择账号和测试模型"})
			return
		}
		if !requirePoolCredential(c, request.Name) {
			return
		}
		manager := requestCLIProxyCore(c)
		if err := manager.RefreshRuntime(c.Request.Context()); err != nil {
			c.JSON(502, gin.H{"error": "账号池尚未就绪"})
			return
		}
		id := ""
		for _, account := range manager.Runtime().Accounts {
			if account.Name == request.Name || account.ID == request.Name {
				id = account.ID
				break
			}
		}
		if id == "" {
			c.JSON(404, gin.H{"error": "账号不属于当前池"})
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
		defer cancel()
		var result cliproxy.AccountTest
		if err := manager.Client().Do(ctx, http.MethodPost, "/apishare/test", gin.H{"id": id, "model": request.Model}, &result); err != nil {
			c.JSON(502, gin.H{"error": fmt.Sprintf("账号测试未完成：%s", err)})
			return
		}
		c.JSON(200, result)
	})
}
