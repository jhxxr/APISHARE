package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const openCodeFreeType = "opencode-free"
const openCodeFreeBase = "https://opencode.ai/zen"

// Keep the original paid/subscription channel readable without changing its
// credential or tier. New channels expose the separate, free-only contract.
func (u *Upstream) isOpenCode() bool {
	return u.Type == "opencode" || u.Type == openCodeFreeType
}

func (u *Upstream) openCodeFreeOnly() bool { return u.Type == openCodeFreeType }

func (u *Upstream) openCodeAPIKey() string {
	if u.openCodeFreeOnly() && strings.TrimSpace(u.APIKey) == "" {
		return "public"
	}
	return u.APIKey
}

func (u *Upstream) openCodeKeyForType(next string) string {
	if next == openCodeFreeType && u.Type != openCodeFreeType && (!u.isOpenCode() || u.openCodeTier() != "zen") {
		return "public"
	}
	return u.APIKey
}

func (u *Upstream) normalizeOpenCodeFree() error {
	if !u.openCodeFreeOnly() {
		return nil
	}
	if strings.TrimSpace(u.BaseURL) == "" {
		u.BaseURL = openCodeFreeBase
	}
	parsed, err := url.Parse(u.BaseURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("OpenCode 免费模型地址须为不含认证、查询参数或片段的 HTTP(S) 地址")
	}
	if u.OpenCodeTier == "go" || (strings.EqualFold(parsed.Hostname(), "opencode.ai") && strings.Contains("/"+strings.Trim(parsed.Path, "/")+"/", "/go/")) {
		return fmt.Errorf("OpenCode 免费模型使用 Zen 免费目录，请使用 https://opencode.ai/zen")
	}
	u.OpenCodeTier = "zen"
	u.APIKey = u.openCodeAPIKey()
	if u.OpenAIProtocol == "" {
		u.OpenAIProtocol = "auto"
	}
	return nil
}

func (u *Upstream) validateOpenCodeFreeModel(ctx context.Context, model string) error {
	if !u.openCodeFreeOnly() {
		return nil
	}
	catalog, err := u.openCodeCatalog(ctx)
	if err != nil {
		return err
	}
	// A documentation fallback can identify a known -free ID when the price
	// catalog is unavailable. Never infer free eligibility from the caller's
	// alias or from a Go model's similar name.
	if openCodeCatalogFreeModel(catalog, "zen", model) {
		return nil
	}
	return fmt.Errorf("OpenCode 免费模型渠道不支持上游模型 %s；请获取免费模型列表，或将客户端别名映射到实际的免费模型 ID", model)
}

func openCodeCatalogFreeModel(catalog openCodeCatalog, tier, model string) bool {
	return catalog.FreeModels[tier][model] || (catalog.Tiers[tier][model] != "" && strings.HasSuffix(strings.ToLower(model), "-free"))
}

// Record only the request shape needed to compare with opencode2api. Never
// include credentials, session/project IDs, messages, or tool arguments.
func (u *Upstream) openCodeRequestSummary(req *http.Request, body []byte, format clientFormat) string {
	var payload struct {
		Model  string            `json:"model"`
		Stream bool              `json:"stream"`
		Tools  []json.RawMessage `json:"tools"`
	}
	_ = json.Unmarshal(body, &payload)
	auth := "missing"
	if req.Header.Get("Authorization") == "Bearer public" || req.Header.Get("x-api-key") == "public" {
		auth = "public"
	} else if req.Header.Get("Authorization") != "" || req.Header.Get("x-api-key") != "" {
		auth = "key"
	}
	profile := "inference"
	if openCodeLegacyRequest(req) {
		profile = "legacy-zen"
	}
	pair := "missing"
	if child := req.Header.Get("x-opencode-session-id"); child != "" {
		pair = "different"
		if child == req.Header.Get("x-opencode-session") {
			pair = "same"
		}
	}
	names := make([]string, 0, min(12, len(payload.Tools)))
	for _, raw := range payload.Tools[:min(12, len(payload.Tools))] {
		name, _ := openCodeToolContract(raw, format)
		names = append(names, truncate(name, 64))
	}
	summary := struct {
		Model        string   `json:"model"`
		Path         string   `json:"path"`
		Profile      string   `json:"profile"`
		Auth         string   `json:"auth"`
		ProxyID      int64    `json:"proxy_id"`
		Stream       bool     `json:"stream"`
		ToolCount    int      `json:"tool_count"`
		Tools        []string `json:"tools"`
		SessionValid bool     `json:"session_valid"`
		SessionPair  string   `json:"session_pair"`
		RequestIDLen int      `json:"request_id_length"`
	}{payload.Model, req.URL.EscapedPath(), profile, auth, u.ProxyID, payload.Stream, len(payload.Tools), names,
		openCodeDescendingPattern.MatchString(req.Header.Get("x-opencode-session")), pair, len(req.Header.Get("x-opencode-request"))}
	encoded, _ := json.Marshal(summary)
	return "OpenCode 请求诊断：" + string(encoded)
}
