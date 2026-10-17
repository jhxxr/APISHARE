package main

import (
	"context"
	"fmt"
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
