package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
)

func coreLoginView(state string, session coreLoginSession) gin.H {
	status := session.status
	if session.committed {
		status = "ok"
	}
	if status == "" {
		status = "wait"
	}
	return gin.H{"state": state, "provider": session.provider, "status": status, "error": session.message,
		"url": session.login.URL, "flow": session.login.Flow, "user_code": session.login.UserCode,
		"expires_in": max(0, int(time.Until(session.expires).Seconds())), "duplicates": session.duplicates}
}

func updateCoreLogin(state string, fn func(*coreLoginSession)) {
	coreLoginSessions.Lock()
	defer coreLoginSessions.Unlock()
	session, exists := coreLoginSessions.items[state]
	if exists {
		fn(&session)
		coreLoginSessions.items[state] = session
	}
}

func advanceCoreLogin(ctx context.Context, state string, confirm bool) {
	coreLoginSessions.Lock()
	session, exists := coreLoginSessions.items[state]
	coreLoginSessions.Unlock()
	if !exists || session.worker == nil || session.op == nil {
		return
	}
	session.op.Lock()
	defer session.op.Unlock()
	coreLoginSessions.Lock()
	session, exists = coreLoginSessions.items[state]
	coreLoginSessions.Unlock()
	if !exists || session.committed || session.status == "error" || time.Now().After(session.expires) {
		return
	}
	if session.status == "needs_confirmation" && !confirm {
		return
	}
	var result struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := session.worker.Client().Do(ctx, "GET", "/oauth/status?state="+url.QueryEscape(state), nil, &result); err != nil {
		updateCoreLogin(state, func(s *coreLoginSession) { s.message = "暂时无法读取授权结果，后台会继续重试" })
		return
	}
	if result.Status == "error" {
		updateCoreLogin(state, func(s *coreLoginSession) {
			s.status = "error"
			s.message = "授权失败或已过期，请重新发起授权"
		})
		go session.worker.DiscardLogin()
		return
	}
	if result.Status != "ok" {
		return
	}
	manager, err := cliProxyPool(session.poolID)
	if err != nil {
		return
	}
	files, err := session.worker.ReadCredentials()
	if err != nil || len(files) == 0 {
		return
	}
	proxyConfigMu.Lock()
	defer proxyConfigMu.Unlock()
	// Preflight the whole result before updating any credential.
	duplicates := make([]string, 0)
	if session.flow != "siwc_bind" {
		for name, raw := range files {
			var document map[string]any
			if json.Unmarshal(raw, &document) != nil {
				return
			}
			_, _, err := prepareCredentialReplacement(manager, name, document, false)
			var duplicate *duplicatePoolCredential
			if errors.As(err, &duplicate) {
				duplicates = append(duplicates, duplicate.Name)
			} else if err != nil {
				updateCoreLogin(state, func(s *coreLoginSession) { s.status = "error"; s.message = err.Error() })
				return
			}
		}
	}
	if len(duplicates) > 0 && !confirm {
		sort.Strings(duplicates)
		updateCoreLogin(state, func(s *coreLoginSession) {
			s.status = "needs_confirmation"
			s.message = "授权已完成，当前池已有此账号，请确认更新凭据"
			s.duplicates = duplicates
		})
		return
	}
	for name, raw := range files {
		if session.flow == "siwc_bind" {
			var document map[string]json.RawMessage
			err = json.Unmarshal(raw, &document)
			if err == nil && len(document["siwc"]) > 0 {
				err = manager.Client().Do(ctx, "PATCH", "/credentials/fields", gin.H{"name": name, "siwc": document["siwc"]}, nil)
			}
		} else {
			err = importPoolCredential(ctx, manager, session.poolID, name, raw, session.proxyID, confirm)
		}
		if err != nil {
			updateCoreLogin(state, func(s *coreLoginSession) {
				s.message = "授权已完成，写入当前池失败；启动账号池后后台会重试"
			})
			return
		}
	}
	manager.Touch()
	_ = manager.RefreshRuntime(ctx)
	healthMarkSuccess(session.poolID)
	updateCoreLogin(state, func(s *coreLoginSession) { s.committed = true; s.status = "ok"; s.message = ""; s.duplicates = nil })
	go session.worker.DiscardLogin()
}

func watchCoreLogin(state string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		coreLoginSessions.Lock()
		session, exists := coreLoginSessions.items[state]
		coreLoginSessions.Unlock()
		if !exists || session.committed || session.status == "error" || time.Now().After(session.expires) {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		advanceCoreLogin(ctx, state, false)
		cancel()
		<-ticker.C
	}
}

func discardCoreLogin(state string) {
	coreLoginSessions.Lock()
	session, exists := coreLoginSessions.items[state]
	coreLoginSessions.Unlock()
	if !exists {
		return
	}
	if session.op != nil {
		session.op.Lock()
		defer session.op.Unlock()
	}
	coreLoginSessions.Lock()
	delete(coreLoginSessions.items, state)
	coreLoginSessions.Unlock()
	if session.worker != nil {
		session.worker.DiscardLogin()
	}
}

func expireCoreLogin(state string, ttl time.Duration) {
	time.AfterFunc(ttl, func() { discardCoreLogin(state) })
}

func registerCLIProxySessionRoutes(group *gin.RouterGroup) {
	group.GET("/oauth/sessions", func(c *gin.Context) {
		owner := sha256.Sum256([]byte(adminRequestToken(c)))
		poolID := requestCLIProxyPoolID(c)
		coreLoginSessions.Lock()
		out := make([]gin.H, 0)
		for state, session := range coreLoginSessions.items {
			if session.owner == owner && session.poolID == poolID && time.Now().Before(session.expires) {
				out = append(out, coreLoginView(state, session))
			}
		}
		coreLoginSessions.Unlock()
		sort.Slice(out, func(i, j int) bool { return out[i]["expires_in"].(int) > out[j]["expires_in"].(int) })
		c.JSON(200, gin.H{"sessions": out})
	})
	group.POST("/oauth/confirm", func(c *gin.Context) {
		var request struct {
			State string `json:"state"`
		}
		if c.ShouldBindJSON(&request) != nil {
			c.JSON(400, gin.H{"error": "授权会话无效"})
			return
		}
		if _, ok := coreSession(c, request.State); !ok {
			c.JSON(404, gin.H{"error": "授权会话不存在"})
			return
		}
		advanceCoreLogin(c.Request.Context(), request.State, true)
		session, ok := coreSession(c, request.State)
		if !ok {
			c.JSON(404, gin.H{"error": "授权会话已过期"})
			return
		}
		c.JSON(200, coreLoginView(request.State, session))
	})
}
