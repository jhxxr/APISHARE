package cliproxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	sdkapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/api"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	sdk "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type AccountModel struct {
	ID    string `json:"id"`
	Ready bool   `json:"ready"`
}

type QuotaMetric struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type ModelQuota struct {
	Model      string        `json:"model"`
	ObservedAt string        `json:"observed_at"`
	Metrics    []QuotaMetric `json:"metrics"`
}

type AccountRuntime struct {
	ID              string                  `json:"id"`
	Name            string                  `json:"name"`
	Ready           bool                    `json:"ready"`
	Reason          string                  `json:"reason,omitempty"`
	LastRefresh     string                  `json:"last_refresh,omitempty"`
	NextRefresh     string                  `json:"next_refresh,omitempty"`
	RetryAt         string                  `json:"retry_at,omitempty"`
	QuotaObservedAt string                  `json:"quota_observed_at,omitempty"`
	Quota           []QuotaMetric           `json:"quota"`
	ModelQuotas     []ModelQuota            `json:"model_quotas"`
	Cooldowns       []coreauth.CooldownView `json:"cooldowns"`
	Models          []AccountModel          `json:"models"`
	Priority        int                     `json:"priority"`
	Weight          int                     `json:"weight"`
	Location        string                  `json:"location,omitempty"`
	SIWCStatus      string                  `json:"siwc_status,omitempty"`
	SIWCExpiresAt   string                  `json:"siwc_expires_at,omitempty"`
}

type RuntimeSnapshot struct {
	ObservedAt time.Time        `json:"observed_at"`
	Accounts   []AccountRuntime `json:"accounts"`
}

type AccountTest struct {
	OK        bool   `json:"ok"`
	AccountID string `json:"account_id"`
	Model     string `json:"model"`
	LatencyMS int64  `json:"latency_ms"`
	Status    int    `json:"status"`
	Reason    string `json:"reason,omitempty"`
}

func timestamp(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

func publicFailureReason(status int) string {
	switch status {
	case 401:
		return "unauthorized"
	case 402:
		return "payment_required"
	case 403:
		return "forbidden"
	case 404:
		return "model_not_supported"
	case 408, 504:
		return "timeout"
	case 429:
		return "quota"
	case 499:
		return "canceled"
	case 0, 500, 502, 503, 520, 521, 522, 523, 524:
		return "upstream_unavailable"
	default:
		return "request_failed"
	}
}

func quotaMetrics(provider string, quota coreauth.QuotaState) []QuotaMetric {
	out := make([]QuotaMetric, 0)
	if !coreauth.ProviderSupportsQuotaObservation(provider) {
		return out
	}
	keys := make([]string, 0, len(quota.Signals))
	for key := range quota.Signals {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		name, value := strings.ToLower(key), strings.TrimSpace(quota.Signals[key])
		if len(out) >= 32 || len(value) > 100 {
			continue
		}
		allowed := name == "retry-after" || strings.HasPrefix(name, "x-codex-") || strings.HasPrefix(name, "x-ratelimit-") || strings.HasPrefix(name, "anthropic-ratelimit-unified-")
		if !allowed {
			continue
		}
		// Values are numbers, timestamps or a known enum, never arbitrary
		// provider text, bearer tokens or request metadata.
		_, numericErr := strconv.ParseFloat(value, 64)
		_, timeErr := time.Parse(time.RFC3339, value)
		safe := numericErr == nil || timeErr == nil
		switch strings.ToLower(value) {
		case "true", "false", "allowed", "rejected", "allowed_warning", "free", "plus", "pro", "team", "business", "enterprise", "primary", "secondary", "unlimited":
			safe = true
		}
		if safe {
			out = append(out, QuotaMetric{Name: name, Value: value})
		}
	}
	return out
}

func projectAccount(auth *coreauth.Auth, models []*sdk.ModelInfo, now time.Time) AccountRuntime {
	out := AccountRuntime{ID: auth.ID, Name: auth.FileName, Models: make([]AccountModel, 0), Quota: quotaMetrics(auth.Provider, auth.Quota),
		Cooldowns: coreauth.CooldownSnapshotForAuth(auth, now), LastRefresh: timestamp(auth.LastRefreshedAt),
		NextRefresh: timestamp(auth.NextRefreshAfter), QuotaObservedAt: timestamp(auth.Quota.ObservedAt)}
	out.Weight = 1
	if value, exists := auth.Metadata["weight"]; exists {
		if number, ok := value.(float64); ok {
			out.Weight = int(number)
		}
	}
	if value, exists := auth.Metadata["priority"]; exists {
		if number, ok := value.(float64); ok {
			out.Priority = int(number)
		}
	}
	if value, err := strconv.Atoi(auth.Attributes["weight"]); err == nil {
		out.Weight = value
	}
	if value, err := strconv.Atoi(auth.Attributes["priority"]); err == nil {
		out.Priority = value
	}
	out.Location, _ = auth.Metadata["location"].(string)
	if auth.Provider == "codex" {
		out.SIWCStatus = string(auth.CodexSIWCStatus(now))
		if expiry, ok := auth.SIWCRealExpirationTime(); ok {
			out.SIWCExpiresAt = timestamp(expiry)
		}
	}
	modelKeys := make([]string, 0, len(auth.ModelStates))
	for model := range auth.ModelStates {
		modelKeys = append(modelKeys, model)
	}
	sort.Strings(modelKeys)
	for _, model := range modelKeys {
		if len(out.ModelQuotas) >= 32 {
			break
		}
		state := auth.ModelStates[model]
		if state == nil {
			continue
		}
		metrics := quotaMetrics(auth.Provider, state.Quota)
		if len(metrics) > 0 {
			out.ModelQuotas = append(out.ModelQuotas, ModelQuota{Model: model, ObservedAt: timestamp(state.Quota.ObservedAt), Metrics: metrics})
		}
	}
	if out.Name == "" {
		out.Name = auth.ID
	}
	disabled := auth.Disabled || auth.Status == coreauth.StatusDisabled
	credentialBlocked := disabled || out.Weight <= 0
	if disabled {
		out.Reason = "disabled"
	} else if out.Weight <= 0 {
		out.Reason = "weight_disabled"
	}
	for _, cooldown := range out.Cooldowns {
		if cooldown.Scope == "credential" {
			credentialBlocked = true
			out.Reason = cooldown.Reason
		}
		if out.RetryAt == "" || timestamp(cooldown.RetryAt) < out.RetryAt {
			out.RetryAt = timestamp(cooldown.RetryAt)
		}
	}
	// An unavailable auth with no past retry window is a remote/credential
	// fault. Expired cooldowns may recover without a new request.
	if auth.Unavailable && len(out.Cooldowns) == 0 && auth.NextRetryAfter.IsZero() && auth.Quota.NextRecoverAt.IsZero() && len(auth.ModelStates) == 0 {
		credentialBlocked = true
		if !disabled {
			out.Reason = "credential_unavailable"
		}
	}
	if auth.LastError != nil && !disabled && credentialBlocked && out.Reason == "credential_unavailable" {
		out.Reason = publicFailureReason(auth.LastError.HTTPStatus)
	}
	for _, model := range models {
		if model == nil || model.ID == "" {
			continue
		}
		ready := !credentialBlocked
		for _, cooldown := range out.Cooldowns {
			if cooldown.Scope == "model" && cooldown.ModelKey == model.ID {
				ready = false
			}
		}
		if state := auth.ModelStates[model.ID]; state != nil && state.Unavailable && state.NextRetryAfter.IsZero() && state.Quota.NextRecoverAt.IsZero() {
			ready = false
		}
		out.Models = append(out.Models, AccountModel{ID: model.ID, Ready: ready})
		out.Ready = out.Ready || ready
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].ID < out.Models[j].ID })
	if !out.Ready && out.Reason == "" {
		if len(out.Models) == 0 {
			out.Reason = "no_models"
		} else {
			out.Reason = "cooldown"
		}
	}
	return out
}

func poolRuntimeOption(state diskState, recorders ...*accountUptimeRecorder) sdkapi.ServerOption {
	return sdkapi.WithRouterConfigurator(func(engine *gin.Engine, handler *handlers.BaseAPIHandler, _ *config.Config) {
		group := engine.Group(ManagementPath + "/apishare")
		group.Use(func(c *gin.Context) {
			value := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(value), []byte(state.ManagementKey)) != 1 {
				c.AbortWithStatusJSON(401, gin.H{"error": "unauthorized"})
				return
			}
			c.Header("Cache-Control", "no-store")
		})
		group.GET("/runtime", func(c *gin.Context) {
			now := time.Now().UTC()
			result := RuntimeSnapshot{ObservedAt: now, Accounts: make([]AccountRuntime, 0)}
			for _, auth := range handler.AuthManager.List() {
				result.Accounts = append(result.Accounts, projectAccount(auth, sdk.GlobalModelRegistry().GetModelsForClient(auth.ID), now))
			}
			c.JSON(200, result)
		})
		group.POST("/flush", func(c *gin.Context) {
			if len(recorders) > 0 && recorders[0] != nil {
				if err := recorders[0].flush(c.Request.Context()); err != nil {
					c.JSON(503, gin.H{"error": "usage queue busy"})
					return
				}
			}
			c.JSON(200, gin.H{"ok": true})
		})
		group.POST("/test", func(c *gin.Context) {
			var request struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			}
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
			if c.ShouldBindJSON(&request) != nil || request.ID == "" || request.Model == "" {
				c.JSON(400, gin.H{"error": "invalid account or model"})
				return
			}
			auth, ok := handler.AuthManager.GetByID(request.ID)
			if !ok {
				c.JSON(404, gin.H{"error": "account not found"})
				return
			}
			account := projectAccount(auth, sdk.GlobalModelRegistry().GetModelsForClient(auth.ID), time.Now())
			found, ready := false, false
			for _, model := range account.Models {
				if model.ID == request.Model {
					found = true
					ready = model.Ready
				}
			}
			if !found || !ready {
				reason := account.Reason
				if !found {
					reason = "model_not_supported"
				} else if reason == "" {
					reason = "cooldown"
				}
				c.JSON(200, AccountTest{AccountID: auth.ID, Model: request.Model, Status: 409, Reason: reason})
				return
			}
			body, _ := json.Marshal(gin.H{"model": request.Model, "max_tokens": 16, "messages": []gin.H{{"role": "user", "content": "Reply OK."}}})
			ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
			defer cancel()
			ctx = handlers.WithPinnedAuthID(ctx, auth.ID)
			selected := ""
			ctx = handlers.WithSelectedAuthIDCallback(ctx, func(id string) { selected = id })
			start := time.Now()
			_, _, failure := handler.ExecuteWithAuthManager(ctx, "openai", request.Model, body, "")
			result := AccountTest{AccountID: auth.ID, Model: request.Model, LatencyMS: time.Since(start).Milliseconds(), Status: 200}
			if failure != nil {
				result.Status = failure.StatusCode
				result.Reason = publicFailureReason(result.Status)
			} else if selected != auth.ID {
				result.Status = 502
				result.Reason = "account_mismatch"
			} else {
				result.OK = true
			}
			c.JSON(200, result)
		})
	})
}

func (m *Manager) RefreshRuntime(ctx context.Context) error {
	var snapshot RuntimeSnapshot
	client := m.Client()
	client.HTTP = &http.Client{Timeout: 2 * time.Second}
	if err := client.Do(ctx, "GET", "/apishare/runtime", nil, &snapshot); err != nil {
		return err
	}
	m.runtimeMu.Lock()
	m.runtimeSnapshot = snapshot
	m.runtimeFetched = time.Now()
	m.runtimeMu.Unlock()
	return nil
}

func (m *Manager) pollRuntime(done <-chan struct{}) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = m.RefreshRuntime(ctx)
			cancel()
		}
	}
}

func (m *Manager) Runtime() RuntimeSnapshot {
	m.runtimeMu.RLock()
	defer m.runtimeMu.RUnlock()
	return m.runtimeSnapshot // immutable after publication
}

func (m *Manager) Ready(model string) bool {
	if !m.Running() {
		return false
	}
	m.runtimeMu.RLock()
	defer m.runtimeMu.RUnlock()
	if time.Since(m.runtimeFetched) > 6*time.Second {
		return false
	}
	model = strings.TrimSpace(model)
	if pos := strings.LastIndex(model, "("); pos > 0 && strings.HasSuffix(model, ")") {
		model = strings.TrimSpace(model[:pos])
	}
	for _, account := range m.runtimeSnapshot.Accounts {
		if model == "" && account.Ready {
			return true
		}
		for _, candidate := range account.Models {
			if candidate.Ready && candidate.ID == model {
				return true
			}
		}
	}
	return false
}
