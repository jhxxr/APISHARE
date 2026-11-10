package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	maxUserUpstreamsPerUser = 20
	siteDefaultSharedModel  = "__site_default__"
)

// allowLoopbackUserUpstreamForTest is only enabled in unit tests that use httptest.Server.
var allowLoopbackUserUpstreamForTest bool

type UserAutoRouterConfig struct {
	Enabled                 bool                `json:"enabled"`
	VirtualModel            string              `json:"virtual_model"`
	ChannelPreference       string              `json:"channel_preference"`       // "personal_first" | "gateway_first" | "personal_only"
	ClassifierSource        string              `json:"classifier_source"`        // "gateway_shared" | "user_upstream" | "user_external" | "heuristic"
	SharedClassifierModel   string              `json:"shared_classifier_model"`  // model ID from siteCfg.SharedClassifierModels or "__site_default__"
	UserClassifierUpstream  int64               `json:"user_classifier_upstream"` // positive user_upstreams.id (0 = auto)
	UserClassifierProvider  string              `json:"user_classifier_provider"` // "cloudflare" | "jev" | "systemone" | "openai_chat"
	UserClassifierModel     string              `json:"user_classifier_model"`    // model name for user_upstream / user_external
	UserCloudflareAccountID string              `json:"user_cloudflare_account_id"`
	UserClassifierEndpoint  string              `json:"user_classifier_endpoint"`
	UserClassifierAPIKey    string              `json:"user_classifier_api_key,omitempty"`
	ClassifierTimeoutMS     int                 `json:"classifier_timeout_ms"`
	ClassifierFallback      string              `json:"classifier_fallback"` // "heuristic" | "default_model"
	MinConfidence           float64             `json:"min_confidence"`
	SkipShortChars          int                 `json:"skip_short_chars"`
	LongContextThreshold    int                 `json:"long_context_threshold"`
	CascadeTiers            bool                `json:"cascade_tiers"`
	DefaultModel            string              `json:"default_model"`
	Tiers                   map[string][]string `json:"tiers"`
	TierThinking            map[string]string   `json:"tier_thinking"`
	TierCriteria            map[string]string   `json:"tier_criteria"`
}

type userRouteTarget struct {
	Model      string `json:"model"`
	SourceTier string `json:"source_tier"`
	PreferBYOK bool   `json:"prefer_byok"`
	AllowBYOK  bool   `json:"allow_byok"`
	AllowSite  bool   `json:"allow_site"`
}

func defaultUserAutoRouterConfig() UserAutoRouterConfig {
	return UserAutoRouterConfig{
		Enabled:                false,
		VirtualModel:           "my-auto",
		ChannelPreference:      "personal_first",
		ClassifierSource:       "heuristic",
		UserClassifierProvider: "cloudflare",
		ClassifierTimeoutMS:    1500,
		ClassifierFallback:     "heuristic",
		MinConfidence:          0.55,
		SkipShortChars:         60,
		LongContextThreshold:   32000,
		CascadeTiers:           true,
		DefaultModel:           "",
		Tiers: map[string][]string{
			TierSimple:    {},
			TierMedium:    {},
			TierComplex:   {},
			TierReasoning: {},
		},
		TierThinking: defaultTierThinkingMap(),
		TierCriteria: defaultAutoRouterTierCriteria(),
	}
}

func cloneUserAutoRouterConfig(src UserAutoRouterConfig) UserAutoRouterConfig {
	dst := src
	dst.Tiers = make(map[string][]string, len(src.Tiers))
	for k, v := range src.Tiers {
		dst.Tiers[k] = append([]string(nil), v...)
	}
	dst.TierThinking = make(map[string]string, len(src.TierThinking))
	for k, v := range src.TierThinking {
		dst.TierThinking[k] = v
	}
	dst.TierCriteria = make(map[string]string, len(src.TierCriteria))
	for k, v := range src.TierCriteria {
		dst.TierCriteria[k] = v
	}
	return dst
}

func (ucfg UserAutoRouterConfig) effectiveTierCriteria() map[string]string {
	out := defaultAutoRouterTierCriteria()
	for k, v := range ucfg.TierCriteria {
		if tk := normalizeTierName(k); tk != "" && strings.TrimSpace(v) != "" {
			out[tk] = strings.TrimSpace(v)
		}
	}
	return out
}

func migrateUserRouterSchema() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS user_upstreams (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			sub             TEXT NOT NULL,
			name            TEXT NOT NULL,
			type            TEXT NOT NULL DEFAULT 'openai',
			openai_protocol TEXT NOT NULL DEFAULT 'auto',
			base_url        TEXT NOT NULL,
			api_key         TEXT NOT NULL,
			models          TEXT NOT NULL DEFAULT '',
			model_map       TEXT NOT NULL DEFAULT '{}',
			priority        INTEGER NOT NULL DEFAULT 10,
			weight          INTEGER NOT NULL DEFAULT 1,
			enabled         INTEGER NOT NULL DEFAULT 1,
			proxy_mode      TEXT NOT NULL DEFAULT 'default',
			proxy_url       TEXT NOT NULL DEFAULT '',
			proxy_username  TEXT NOT NULL DEFAULT '',
			proxy_password  TEXT NOT NULL DEFAULT '',
			created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE INDEX IF NOT EXISTS idx_user_upstreams_sub ON user_upstreams(sub, enabled, priority DESC, id ASC);`,
		`CREATE TABLE IF NOT EXISTS user_auto_routers (
			sub         TEXT PRIMARY KEY,
			config_json TEXT NOT NULL DEFAULT '{}',
			updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	for _, column := range []struct{ name, definition string }{
		{"proxy_mode", "TEXT NOT NULL DEFAULT 'default'"},
		{"proxy_url", "TEXT NOT NULL DEFAULT ''"},
		{"proxy_username", "TEXT NOT NULL DEFAULT ''"},
		{"proxy_password", "TEXT NOT NULL DEFAULT ''"},
	} {
		var exists int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('user_upstreams') WHERE name=?`, column.name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := db.Exec(`ALTER TABLE user_upstreams ADD COLUMN ` + column.name + ` ` + column.definition); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateUserProxyHost(host string) error {
	return validatePublicUserHost(host)
}

func normalizeUserUpstreamProxy(mode, rawURL, username, password string) (string, string, string, string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "", "default":
		return "default", "", "", "", nil
	case "custom":
		if strings.TrimSpace(rawURL) == "" {
			return "", "", "", "", errors.New("选择自定义代理时，请填写代理地址（如 socks5://host:port 或 http://host:port）")
		}
		p := &OutboundProxy{
			URL:      strings.TrimSpace(rawURL),
			Username: strings.TrimSpace(username),
			Password: password,
			Enabled:  true,
		}
		if err := normalizeProxyEndpoint(p); err != nil {
			return "", "", "", "", err
		}
		u, err := url.Parse(p.URL)
		if err != nil {
			return "", "", "", "", errors.New("自定义代理地址无效")
		}
		if err := validateUserProxyHost(u.Hostname()); err != nil {
			return "", "", "", "", err
		}
		return "custom", p.URL, p.Username, p.Password, nil
	default:
		return "", "", "", "", errors.New("代理模式仅支持 default（系统默认）或 custom（自定义代理）")
	}
}

func siteDefaultProxyID() int64 {
	pid := getSettings().XAuthUserDefaultProxyID
	if pid <= 0 {
		return 0
	}
	p, err := dbGetOutboundProxy(pid)
	if err != nil || !p.Enabled {
		return 0
	}
	return pid
}

func siteDefaultProxyConfigured() bool {
	return siteDefaultProxyID() > 0
}

// validatePublicUpstreamURL enforces HTTP(S) and blocks loopback, private, link-local,
// and internal addresses to prevent SSRF from user-configured BYOK channels or classifiers.
func validatePublicUpstreamURL(rawURL string, allowQuery bool) error {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return errors.New("接口地址不能为空")
	}
	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("接口地址必须是合法的 HTTP(S) URL，且不能包含账号密码或锚点")
	}
	if !allowQuery && u.RawQuery != "" {
		return errors.New("渠道 Base URL 不能包含查询参数")
	}
	return validatePublicUserHost(u.Hostname())
}

func validUserClassifierProvider(provider string) bool {
	switch normalizeClassifierProvider(provider) {
	case "cloudflare", "jev", "openrouter", "systemone", "openai_chat":
		return true
	default:
		return false
	}
}

func validUserUpstreamType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "openai", "anthropic", "gemini", "openrouter", "systemone", "cloudflare":
		return true
	default:
		return false
	}
}

// ---------- In-memory per-user cache ----------

type userRouterCacheEntry struct {
	upstreams []Upstream
	router    UserAutoRouterConfig
	loadedUp  bool
	loadedRt  bool
}

var userRouterState = struct {
	sync.RWMutex
	database *sql.DB
	entries  map[string]*userRouterCacheEntry
}{entries: make(map[string]*userRouterCacheEntry)}

func invalidateUserRouterCache(sub string) {
	userRouterState.Lock()
	defer userRouterState.Unlock()
	if sub == "" || userRouterState.database != db {
		userRouterState.database = db
		userRouterState.entries = make(map[string]*userRouterCacheEntry)
		return
	}
	delete(userRouterState.entries, sub)
}

func dbLoadUserUpstreams(sub string) ([]Upstream, error) {
	if db == nil || strings.TrimSpace(sub) == "" {
		return nil, nil
	}
	defaultProxy := siteDefaultProxyID()
	rows, err := db.Query(`SELECT id, name, type, openai_protocol, base_url, api_key, models, model_map, priority, weight, enabled, proxy_mode, proxy_url, proxy_username, proxy_password, created_at
		FROM user_upstreams WHERE sub=? ORDER BY priority DESC, id ASC`, sub)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Upstream
	for rows.Next() {
		var (
			id                           int64
			u                            Upstream
			enabled                      int
			proxyMode, proxyURL          string
			proxyUsername, proxyPassword string
			createdAt                    sql.NullString
		)
		if err := rows.Scan(&id, &u.Name, &u.Type, &u.OpenAIProtocol, &u.BaseURL, &u.APIKey, &u.Models, &u.ModelMap, &u.Priority, &u.Weight, &enabled, &proxyMode, &proxyURL, &proxyUsername, &proxyPassword, &createdAt); err != nil {
			return nil, err
		}
		// Negative IDs distinguish user BYOK upstreams from positive gateway upstreams
		// in health cooldown tracking, proxy transport caching, and relay completion.
		u.ID = -id
		u.Enabled = enabled != 0
		if createdAt.Valid {
			u.CreatedAt = createdAt.String
		}
		if u.Weight <= 0 {
			u.Weight = 1
		}
		proxyMode = strings.ToLower(strings.TrimSpace(proxyMode))
		switch proxyMode {
		case "custom":
			u.userProxyMode = "custom"
			u.ProxyID = 0
			if strings.TrimSpace(proxyURL) != "" {
				u.customProxy = &OutboundProxy{
					ID:       -id,
					Name:     "user-byok-proxy",
					URL:      strings.TrimSpace(proxyURL),
					Username: strings.TrimSpace(proxyUsername),
					Password: proxyPassword,
					Enabled:  true,
				}
			}
		default:
			u.userProxyMode = "default"
			u.ProxyID = defaultProxy
		}
		list = append(list, u)
	}
	return list, rows.Err()
}

func getUserUpstreams(sub string) ([]Upstream, error) {
	sub = strings.TrimSpace(sub)
	if sub == "" {
		return nil, nil
	}
	userRouterState.RLock()
	if userRouterState.database == db {
		if entry, ok := userRouterState.entries[sub]; ok && entry.loadedUp {
			out := make([]Upstream, len(entry.upstreams))
			copy(out, entry.upstreams)
			userRouterState.RUnlock()
			return out, nil
		}
	}
	userRouterState.RUnlock()

	ups, err := dbLoadUserUpstreams(sub)
	if err != nil {
		return nil, err
	}
	userRouterState.Lock()
	if userRouterState.database != db {
		userRouterState.database = db
		userRouterState.entries = make(map[string]*userRouterCacheEntry)
	}
	entry := userRouterState.entries[sub]
	if entry == nil {
		entry = &userRouterCacheEntry{router: defaultUserAutoRouterConfig()}
		userRouterState.entries[sub] = entry
	}
	entry.upstreams = make([]Upstream, len(ups))
	copy(entry.upstreams, ups)
	entry.loadedUp = true
	userRouterState.Unlock()
	return ups, nil
}

func dbLoadUserAutoRouterConfig(sub string) (UserAutoRouterConfig, error) {
	cfg := defaultUserAutoRouterConfig()
	if db == nil || strings.TrimSpace(sub) == "" {
		return cfg, nil
	}
	var raw string
	err := db.QueryRow(`SELECT config_json FROM user_auto_routers WHERE sub=?`, sub).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if strings.TrimSpace(raw) != "" {
		parsed := defaultUserAutoRouterConfig()
		if json.Unmarshal([]byte(raw), &parsed) == nil {
			_ = normalizeUserAutoRouterConfig(sub, &parsed, false)
			cfg = parsed
		}
	}
	return cfg, nil
}

func getUserAutoRouterConfig(sub string) UserAutoRouterConfig {
	sub = strings.TrimSpace(sub)
	if sub == "" {
		return defaultUserAutoRouterConfig()
	}
	userRouterState.RLock()
	if userRouterState.database == db {
		if entry, ok := userRouterState.entries[sub]; ok && entry.loadedRt {
			cfg := cloneUserAutoRouterConfig(entry.router)
			userRouterState.RUnlock()
			return cfg
		}
	}
	userRouterState.RUnlock()

	cfg, _ := dbLoadUserAutoRouterConfig(sub)
	userRouterState.Lock()
	if userRouterState.database != db {
		userRouterState.database = db
		userRouterState.entries = make(map[string]*userRouterCacheEntry)
	}
	entry := userRouterState.entries[sub]
	if entry == nil {
		entry = &userRouterCacheEntry{}
		userRouterState.entries[sub] = entry
	}
	entry.router = cloneUserAutoRouterConfig(cfg)
	entry.loadedRt = true
	userRouterState.Unlock()
	return cfg
}

func saveUserAutoRouterConfig(sub string, cfg UserAutoRouterConfig) error {
	sub = strings.TrimSpace(sub)
	if sub == "" {
		return errors.New("缺少用户标识")
	}
	if err := normalizeUserAutoRouterConfig(sub, &cfg, true); err != nil {
		return err
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO user_auto_routers(sub, config_json, updated_at) VALUES(?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(sub) DO UPDATE SET config_json=excluded.config_json, updated_at=CURRENT_TIMESTAMP`,
		sub, string(encoded))
	if err != nil {
		return err
	}
	invalidateUserRouterCache(sub)
	return nil
}

func normalizeUserAutoRouterConfig(sub string, ucfg *UserAutoRouterConfig, strict bool) error {
	def := defaultUserAutoRouterConfig()
	siteCfg := getAutoRouterConfig()
	siteVM := strings.TrimSpace(siteCfg.VirtualModel)
	if siteVM == "" {
		siteVM = "auto"
	}

	ucfg.VirtualModel = strings.TrimSpace(ucfg.VirtualModel)
	if ucfg.VirtualModel == "" {
		ucfg.VirtualModel = def.VirtualModel
	}
	if strings.ContainsAny(ucfg.VirtualModel, " ,，\t\r\n/") {
		return errors.New("私人路由模型名称不能包含空格、逗号或斜杠")
	}
	if strings.EqualFold(ucfg.VirtualModel, siteVM) || strings.EqualFold(ucfg.VirtualModel, "auto") {
		if strict {
			return fmt.Errorf("私人路由模型名称 (%s) 不能与站点公共路由模型 (%s) 同名，请修改为其它名称（如 my-auto、smart 等）", ucfg.VirtualModel, siteVM)
		}
		ucfg.VirtualModel = "my-auto"
		if strings.EqualFold(ucfg.VirtualModel, siteVM) {
			ucfg.VirtualModel = "user-auto"
		}
	}

	switch strings.ToLower(strings.TrimSpace(ucfg.ChannelPreference)) {
	case "", "personal_first":
		ucfg.ChannelPreference = "personal_first"
	case "gateway_first", "personal_only":
		ucfg.ChannelPreference = strings.ToLower(strings.TrimSpace(ucfg.ChannelPreference))
	default:
		if strict {
			return errors.New("渠道优先级策略仅支持 personal_first、gateway_first 或 personal_only")
		}
		ucfg.ChannelPreference = "personal_first"
	}

	switch strings.ToLower(strings.TrimSpace(ucfg.ClassifierSource)) {
	case "", "heuristic":
		ucfg.ClassifierSource = "heuristic"
	case "gateway_shared", "user_upstream", "user_external":
		ucfg.ClassifierSource = strings.ToLower(strings.TrimSpace(ucfg.ClassifierSource))
	default:
		if strict {
			return errors.New("不支持的决策分类器来源")
		}
		ucfg.ClassifierSource = "heuristic"
	}

	ucfg.SharedClassifierModel = strings.TrimSpace(ucfg.SharedClassifierModel)
	ucfg.UserClassifierProvider = normalizeClassifierProvider(ucfg.UserClassifierProvider)
	if ucfg.UserClassifierProvider == "heuristic" || ucfg.UserClassifierProvider == "" {
		ucfg.UserClassifierProvider = "cloudflare"
	}
	if ucfg.ClassifierSource == "user_external" && !validUserClassifierProvider(ucfg.UserClassifierProvider) {
		return errors.New("个人外部分类器仅支持 cloudflare、jev、systemone 或 openai_chat")
	}
	ucfg.UserClassifierModel = strings.TrimSpace(ucfg.UserClassifierModel)
	ucfg.UserCloudflareAccountID = strings.TrimSpace(ucfg.UserCloudflareAccountID)
	ucfg.UserClassifierEndpoint = strings.TrimSpace(ucfg.UserClassifierEndpoint)
	if ucfg.UserClassifierEndpoint != "" && strict {
		if err := validatePublicUpstreamURL(ucfg.UserClassifierEndpoint, true); err != nil {
			return fmt.Errorf("自定义决策接口地址不合法: %w", err)
		}
	}

	if ucfg.ClassifierSource == "gateway_shared" && strict && ucfg.Enabled {
		if !isSharedClassifierAllowed(siteCfg, ucfg.SharedClassifierModel) {
			return errors.New("所选站点共享决策模型未开放给个人路由使用，请从站点开放列表中选择，或改用自带渠道模型 / 本地规则打分")
		}
	}
	if ucfg.ClassifierSource == "user_upstream" && strict && ucfg.Enabled {
		if ucfg.UserClassifierModel == "" {
			return errors.New("使用自带渠道作为决策模型时，请填写或选择决策模型名称")
		}
	}

	if ucfg.ClassifierTimeoutMS <= 0 {
		ucfg.ClassifierTimeoutMS = def.ClassifierTimeoutMS
	} else if ucfg.ClassifierTimeoutMS < 100 || ucfg.ClassifierTimeoutMS > 15000 {
		if strict {
			return errors.New("分类器超时时间需在 100 到 15000 毫秒之间")
		}
		ucfg.ClassifierTimeoutMS = def.ClassifierTimeoutMS
	}

	ucfg.ClassifierFallback = strings.ToLower(strings.TrimSpace(ucfg.ClassifierFallback))
	if ucfg.ClassifierFallback != "default_model" {
		ucfg.ClassifierFallback = "heuristic"
	}
	if ucfg.MinConfidence <= 0 || ucfg.MinConfidence > 1 {
		ucfg.MinConfidence = def.MinConfidence
	}
	if ucfg.SkipShortChars < 0 || ucfg.SkipShortChars > 2000 {
		ucfg.SkipShortChars = def.SkipShortChars
	}
	if ucfg.LongContextThreshold < 0 {
		ucfg.LongContextThreshold = 0
	} else if ucfg.LongContextThreshold > 2000000 {
		ucfg.LongContextThreshold = 2000000
	}

	ucfg.DefaultModel = strings.TrimSpace(ucfg.DefaultModel)
	if (strings.EqualFold(ucfg.DefaultModel, ucfg.VirtualModel) || strings.EqualFold(parseRouterModelSpec(ucfg.DefaultModel).BaseModel, ucfg.VirtualModel)) && ucfg.DefaultModel != "" {
		if strict {
			return errors.New("默认兜底模型不能与私人路由模型同名")
		}
		ucfg.DefaultModel = ""
	}

	normalizedTiers := make(map[string][]string, len(allRouterTiers))
	totalCandidates := 0
	for _, tier := range allRouterTiers {
		var list []string
		if ucfg.Tiers != nil {
			list = normalizeModelSlice(ucfg.Tiers[tier])
		}
		filtered := make([]string, 0, len(list))
		for _, m := range list {
			if strings.EqualFold(m, ucfg.VirtualModel) || strings.EqualFold(parseRouterModelSpec(m).BaseModel, ucfg.VirtualModel) {
				if strict {
					return fmt.Errorf("档位 %s 的候选模型不能包含私人路由模型自身 (%s)", tier, ucfg.VirtualModel)
				}
				continue
			}
			filtered = append(filtered, m)
		}
		normalizedTiers[tier] = filtered
		totalCandidates += len(filtered)
	}
	ucfg.Tiers = normalizedTiers
	ucfg.TierThinking = normalizeTierThinkingMap(ucfg.TierThinking)
	ucfg.TierCriteria = ucfg.effectiveTierCriteria()

	if ucfg.Enabled && strict {
		if !siteCfg.AllowUserAutoRouter {
			return errors.New("站点管理员当前已关闭个人自定义智能路由功能")
		}
		if ucfg.DefaultModel == "" && totalCandidates == 0 {
			return errors.New("启用私人智能路由前，请至少配置一个档位的候选模型或默认兜底模型")
		}
	}
	return nil
}

func isSharedClassifierAllowed(siteCfg AutoRouterConfig, model string) bool {
	m := strings.TrimSpace(model)
	if m == "" {
		return false
	}
	if m == siteDefaultSharedModel {
		return siteCfg.ShareSiteClassifierWithUsers
	}
	for _, allowed := range siteCfg.SharedClassifierModels {
		if strings.TrimSpace(allowed) == m {
			return true
		}
	}
	return false
}

// ---------- User BYOK & Personal Router Lookup Helpers ----------

func userSubForKey(ak ApiKey) string {
	if ak.XAuthUser == nil {
		return ""
	}
	return strings.TrimSpace(ak.XAuthUser.Sub)
}

func isUserAutoRouterModel(ak ApiKey, model string) bool {
	sub := userSubForKey(ak)
	if sub == "" {
		return false
	}
	siteCfg := getAutoRouterConfig()
	if !siteCfg.AllowUserAutoRouter {
		return false
	}
	ucfg := getUserAutoRouterConfig(sub)
	return ucfg.Enabled && strings.TrimSpace(model) != "" && strings.TrimSpace(model) == ucfg.VirtualModel
}

func userServesModel(ak ApiKey, model string) bool {
	sub := userSubForKey(ak)
	if sub == "" || strings.TrimSpace(model) == "" {
		return false
	}
	ups, err := getUserUpstreams(sub)
	if err != nil {
		return false
	}
	for i := range ups {
		u := &ups[i]
		if u.Enabled && u.Weight > 0 && servesRouterCandidate(u, model) {
			return true
		}
	}
	return false
}

func userAvailableModels(sub string) []string {
	ups, err := getUserUpstreams(sub)
	if err != nil || len(ups) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	out := make([]string, 0)
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for i := range ups {
		u := &ups[i]
		if !u.Enabled || u.Weight <= 0 {
			continue
		}
		if strings.TrimSpace(u.Models) != "" {
			for _, id := range strings.Split(u.Models, ",") {
				add(id)
			}
		} else if defaults := defaultDecisionModelsForType(u.Type); len(defaults) > 0 {
			for _, dm := range defaults {
				add(dm)
			}
		}
		for client := range u.modelMap() {
			if u.servesModel(client) {
				add(client)
			}
		}
	}
	sort.Strings(out)
	return out
}

func appendUserCatalogModels(ak ApiKey, base []catalogModel) []catalogModel {
	sub := userSubForKey(ak)
	if sub == "" {
		return base
	}
	seen := make(map[string]bool, len(base))
	out := make([]catalogModel, 0, len(base)+8)
	for _, m := range base {
		seen[m.ID] = true
		out = append(out, m)
	}
	for _, id := range userAvailableModels(sub) {
		if !seen[id] {
			seen[id] = true
			out = append(out, catalogModel{ID: id, Owner: "user-byok"})
		}
	}
	siteCfg := getAutoRouterConfig()
	if siteCfg.AllowUserAutoRouter {
		ucfg := getUserAutoRouterConfig(sub)
		if ucfg.Enabled && ucfg.VirtualModel != "" && !seen[ucfg.VirtualModel] {
			seen[ucfg.VirtualModel] = true
			out = append(out, catalogModel{ID: ucfg.VirtualModel, Owner: "user-router"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func pickUserUpstream(sub string, format clientFormat, model string, tried map[int64]bool, nativeOnly bool, cr *canonReq) (*Upstream, error) {
	ups, err := getUserUpstreams(sub)
	if err != nil || len(ups) == 0 {
		return nil, errors.New("no personal BYOK upstream configured")
	}
	elig := make([]Upstream, 0, len(ups))
	for _, u := range ups {
		if !u.Enabled || u.Weight <= 0 || tried[u.ID] {
			continue
		}
		if nativeOnly && u.Type != format.nativeUpstreamType() {
			continue
		}
		if !servesRouterCandidate(&u, model) {
			continue
		}
		elig = append(elig, u)
	}
	if len(elig) == 0 {
		return nil, fmt.Errorf("no personal BYOK upstream available for model %q", model)
	}
	sort.SliceStable(elig, func(i, j int) bool {
		if elig[i].Priority != elig[j].Priority {
			return elig[i].Priority > elig[j].Priority
		}
		return elig[i].ID > elig[j].ID
	})
	topPriority := elig[0].Priority
	var healthyTop []Upstream
	var cooledTop []Upstream
	for _, u := range elig {
		upTarget := resolveUpstreamCandidateModel(&u, model)
		if u.availableForRelay(upTarget) {
			if len(healthyTop) == 0 || u.Priority == healthyTop[0].Priority {
				healthyTop = append(healthyTop, u)
			}
		} else if u.Priority == topPriority {
			cooledTop = append(cooledTop, u)
		}
	}
	pool := healthyTop
	if len(pool) == 0 {
		pool = cooledTop
	}
	if len(pool) == 0 {
		pool = elig
	}
	totalWeight := 0
	for _, u := range pool {
		w := u.Weight
		if w <= 0 {
			w = 1
		}
		totalWeight += w
	}
	r := rand.Intn(max(1, totalWeight))
	for _, u := range pool {
		w := u.Weight
		if w <= 0 {
			w = 1
		}
		r -= w
		if r < 0 {
			selected := u
			if tried != nil {
				tried[selected.ID] = true
			}
			return &selected, nil
		}
	}
	selected := pool[0]
	if tried != nil {
		tried[selected.ID] = true
	}
	return &selected, nil
}

// ---------- Decision Classification via Upstream (Gateway or User BYOK) ----------

func classifyViaUpstream(ctx context.Context, up *Upstream, clientModel string, providerLabel string, state string, criteria map[string]string, instructions string, minConfidence float64, timeoutMS int) (*classifierDecisionResult, upstreamUsage, error) {
	if up == nil {
		return nil, upstreamUsage{}, errors.New("classifier upstream is nil")
	}
	if up.pooledKeys() {
		if selected := up.selectAPIKey("", clientModel, map[string]bool{}); selected != nil {
			up = selected
		}
	}
	if timeoutMS <= 0 {
		timeoutMS = 1500
	}
	if timeoutMS > 15000 {
		timeoutMS = 15000
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()

	if strings.TrimSpace(instructions) == "" {
		instructions = defaultClassifierInstructions
	}
	if len(criteria) == 0 {
		criteria = defaultAutoRouterTierCriteria()
	}

	upModel := up.mapModel(clientModel)
	if strings.TrimSpace(upModel) == "" {
		if defaults := defaultDecisionModelsForType(up.Type); len(defaults) > 0 {
			upModel = defaults[0]
		}
	}

	// Native Decision Model Upstream (SystemOne / Jev / Julia-1 / Cloudflare Workers AI Clef)
	if up.isDecisionUpstream(upModel) {
		reqBody, err := buildDecisionClassifierPayload(upModel, up.isCloudflareDecisionUpstream(upModel), state, instructions, criteria)
		if err != nil {
			return nil, upstreamUsage{}, err
		}
		reqURL := up.decisionEndpointURL(upModel)
		if reqURL == "" {
			return nil, upstreamUsage{}, errors.New("missing decision classifier upstream URL")
		}
		req, err := http.NewRequestWithContext(callCtx, http.MethodPost, reqURL, bytes.NewReader(reqBody))
		if err != nil {
			return nil, upstreamUsage{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		if strings.TrimSpace(up.APIKey) != "" {
			req.Header.Set("Authorization", "Bearer "+up.APIKey)
		}
		if up.isOpenRouterDecisionUpstream(upModel) {
			siteTitle := strings.TrimSpace(getSettings().SiteTitle)
			if siteTitle == "" {
				siteTitle = "APISHARE"
			}
			req.Header.Set("X-OpenRouter-Title", siteTitle)
		}
		if err := up.applyHeaders(req, nil); err != nil {
			return nil, upstreamUsage{}, err
		}
		httpClient, err := up.httpClient(autoRouterHTTPClient)
		if err != nil {
			return nil, upstreamUsage{}, err
		}
		start := time.Now()
		resp, err := httpClient.Do(req)
		latency := time.Since(start).Milliseconds()
		if err != nil {
			return nil, upstreamUsage{}, err
		}
		defer resp.Body.Close()
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
		if err != nil {
			return nil, upstreamUsage{}, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, upstreamUsage{}, fmt.Errorf("classifier upstream HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(respBody)), 200))
		}
		_, usage := parseUpstreamNonStream("openai", respBody)
		if !usage.InKnown {
			usage.Prompt = estimateTokensModel(clientModel, instructions+"\n"+state)
		}
		if !usage.OutKnown {
			usage.Completion = 4
		}
		parsed, parseErr := parseDecisionClassifierResponse(respBody, minConfidence)
		if parseErr != nil {
			return nil, usage, parseErr
		}
		parsed.LatencyMS = latency
		parsed.Provider = providerLabel
		parsed.Model = clientModel
		if parsed.Model == "" {
			parsed.Model = upModel
		}
		return parsed, usage, nil
	}

	wireFormat, err := up.resolveWireFormat(callCtx, FormatOpenAIChat, upModel)
	if err != nil {
		wireFormat = FormatOpenAIChat
	}

	sysPrompt := fmt.Sprintf(
		"%s\nRespond with ONLY one tier name: SIMPLE, MEDIUM, COMPLEX, or REASONING.\nTier definitions:\n- SIMPLE: %s\n- MEDIUM: %s\n- COMPLEX: %s\n- REASONING: %s",
		instructions,
		criteria[TierSimple],
		criteria[TierMedium],
		criteria[TierComplex],
		criteria[TierReasoning],
	)
	zero := 0.0
	cr := canonReq{
		Model:       upModel,
		System:      sysPrompt,
		Messages:    []canonMsg{{Role: "user", Text: truncateTail(state, 6000)}},
		MaxTokens:   16,
		Temperature: &zero,
		Stream:      false,
	}
	reqBody, err := renderProtocolRequest(wireFormat, &cr, upModel, up.StructuredOutput)
	if err != nil {
		return nil, upstreamUsage{}, err
	}
	var freeTools openCodeToolCompatibility
	var freeShaped bool
	reqBody, freeShaped, err = up.shapeOpenCodeFreeRequest(reqBody, wireFormat, upModel, &freeTools)
	if err != nil {
		return nil, upstreamUsage{}, err
	}
	var clineTask string
	if up.Type == "cline" {
		if shaped, taskID, shapeErr := shapeClineRequest(reqBody, fmt.Sprintf("classifier:%d", up.ID)); shapeErr == nil {
			reqBody = shaped
			clineTask = taskID
		}
	}
	endpoint := nativeEndpoint(wireFormat, &canonReq{Model: upModel, Stream: false})
	reqURL := up.endpointURL(wireFormat, endpoint)
	if freeShaped {
		reqURL = up.openCodeFreeEndpointURL(wireFormat, endpoint)
	}

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, reqURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, upstreamUsage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if clineTask != "" {
		req.Header.Set("X-Task-ID", clineTask)
	}
	switch up.Type {
	case "opencode", openCodeFreeType:
		if session, sErr := newOpenCodeSessionID(); sErr == nil {
			setOpenCodeHeaders(req, up.openCodeAPIKey(), wireFormat, session, nil, nil, reqBody)
		}
	case "anthropic":
		req.Header.Set("x-api-key", up.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	case "gemini":
		req.Header.Set("x-goog-api-key", up.APIKey)
	default:
		if strings.TrimSpace(up.APIKey) != "" {
			req.Header.Set("Authorization", "Bearer "+up.APIKey)
		}
	}
	if err := up.applyHeaders(req, nil); err != nil {
		return nil, upstreamUsage{}, err
	}

	httpClient, err := up.httpClient(autoRouterHTTPClient)
	if err != nil {
		return nil, upstreamUsage{}, err
	}

	start := time.Now()
	resp, err := httpClient.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return nil, upstreamUsage{}, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, upstreamUsage{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, upstreamUsage{}, fmt.Errorf("classifier upstream HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(respBody)), 200))
	}

	text, usage := parseUpstreamNonStream(up.Type, respBody)
	if strings.TrimSpace(text) == "" {
		if canonResp, pErr := parseProtocolResponse(wireFormat, respBody); pErr == nil && canonResp != nil {
			if t := strings.TrimSpace(canonResp.text()); t != "" {
				text = t
			}
			if !usage.InKnown && canonResp.Usage.InKnown {
				usage = canonResp.Usage
			}
		}
	}
	if !usage.InKnown {
		usage.Prompt = estimateTokensModel(clientModel, sysPrompt+"\n"+state)
	}
	if !usage.OutKnown {
		usage.Completion = max(1, estimateTokensModel(clientModel, text))
	}

	if parsed, parseErr := parseDecisionClassifierResponse(respBody, minConfidence); parseErr == nil {
		parsed.LatencyMS = latency
		parsed.Provider = providerLabel
		parsed.Model = clientModel
		return parsed, usage, nil
	}
	if strings.TrimSpace(text) != "" {
		if parsed, parseErr := parseDecisionClassifierResponse([]byte(text), minConfidence); parseErr == nil {
			parsed.LatencyMS = latency
			parsed.Provider = providerLabel
			parsed.Model = clientModel
			return parsed, usage, nil
		}
	}
	if tier, probs := extractTierFromChatText(text); tier != "" {
		return &classifierDecisionResult{
			Tier:          tier,
			RawChoice:     tier,
			Confidence:    1.0,
			Probabilities: probs,
			LatencyMS:     latency,
			Provider:      providerLabel,
			Model:         clientModel,
		}, usage, nil
	}
	return nil, usage, fmt.Errorf("unable to parse tier decision from upstream response: %s", truncate(strings.TrimSpace(string(respBody)), 180))
}

func classifyViaGatewayUpstream(ctx context.Context, model string, state string, criteria map[string]string, instructions string, minConfidence float64, timeoutMS int) (*classifierDecisionResult, *Upstream, upstreamUsage, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, nil, upstreamUsage{}, errors.New("missing gateway classifier model name")
	}
	up, err := pickUpstream(FormatOpenAIChat, model, nil, false, nil)
	if err != nil {
		return nil, nil, upstreamUsage{}, fmt.Errorf("no available gateway upstream for classifier model %q: %w", model, err)
	}
	if selected := up.selectAPIKey("", model, map[string]bool{}); selected != nil {
		up = selected
	}
	res, usage, callErr := classifyViaUpstream(ctx, up, model, "gateway_upstream", state, criteria, instructions, minConfidence, timeoutMS)
	if callErr != nil {
		return nil, up, usage, callErr
	}
	return res, up, usage, nil
}

func recordSharedClassifierCall(ak ApiKey, up *Upstream, model string, usage upstreamUsage, latencyMS int64) {
	if ak.ID <= 0 || up == nil || model == "" {
		return
	}
	upModel := up.mapModel(model)
	counts := normalizeBillingUsage(usage)
	price, priceSource, pricingModel := lookupRelayPrice(model, upModel)
	cost := costForUsage(counts, price)
	_, independent := ak.ModelQuotas[model]
	quotaPool := "shared"
	if independent {
		quotaPool = "independent"
	}
	entry := &CallLog{
		Client:           requestClientIdentity{ID: "auto-router-classifier"},
		KeyID:            ak.ID,
		KeyName:          ak.Name,
		UpstreamID:       up.ID,
		UpstreamName:     up.Name,
		Model:            model,
		Endpoint:         "router-classifier",
		PromptTokens:     counts.inputTotal(),
		CompletionTokens: counts.outputTotal(),
		CostUSD:          cost.Total,
		Status:           http.StatusOK,
		LatencyMS:        latencyMS,
		DurationMS:       latencyMS,
		Billing: &billingDetails{
			Source:        FormatOpenAIChat,
			Reported:      usage,
			Usage:         counts,
			Cost:          cost,
			Price:         price,
			PriceSource:   priceSource,
			PricingModel:  pricingModel,
			UpstreamModel: upModel,
			QuotaPool:     quotaPool,
		},
	}
	_ = dbRecordBilledCall(entry, independent)
}

// ---------- Personal Auto-Router Execution ----------

func classifyForUserAutoRouter(ctx context.Context, ak ApiKey, sub string, ucfg UserAutoRouterConfig, state string, dryRun bool) (*classifierDecisionResult, error) {
	siteCfg := getAutoRouterConfig()
	criteria := ucfg.effectiveTierCriteria()

	switch ucfg.ClassifierSource {
	case "gateway_shared":
		if !isSharedClassifierAllowed(siteCfg, ucfg.SharedClassifierModel) {
			return nil, errors.New("selected shared classifier model is not permitted by site administrator")
		}
		if ucfg.SharedClassifierModel == siteDefaultSharedModel {
			tempCfg := siteCfg
			tempCfg.TierCriteria = criteria
			tempCfg.MinConfidence = ucfg.MinConfidence
			tempCfg.ClassifierTimeoutMS = ucfg.ClassifierTimeoutMS
			return classifyWithDecisionModel(ctx, tempCfg, state, dryRun)
		}
		model := ucfg.SharedClassifierModel
		if ak.ID > 0 {
			if !ak.allowsModel(model) {
				return nil, fmt.Errorf("API key does not have permission for shared classifier model %q", model)
			}
			if ak.quotaExhausted(model) {
				return nil, fmt.Errorf("quota exhausted for shared classifier model %q", model)
			}
		}
		res, up, usage, err := classifyViaGatewayUpstream(ctx, model, state, criteria, defaultClassifierInstructions, ucfg.MinConfidence, ucfg.ClassifierTimeoutMS)
		if err != nil {
			return nil, err
		}
		res.Provider = "gateway_shared"
		if !dryRun && ak.ID > 0 {
			recordSharedClassifierCall(ak, up, model, usage, res.LatencyMS)
		}
		return res, nil

	case "user_upstream":
		model := strings.TrimSpace(ucfg.UserClassifierModel)
		if model == "" {
			return nil, errors.New("missing personal classifier model name")
		}
		ups, _ := getUserUpstreams(sub)
		var chosen *Upstream
		if ucfg.UserClassifierUpstream > 0 {
			targetID := -ucfg.UserClassifierUpstream
			for i := range ups {
				if ups[i].ID == targetID && ups[i].Enabled {
					chosen = &ups[i]
					break
				}
			}
		}
		if chosen == nil && len(ups) > 0 {
			chosen, _ = pickUserUpstream(sub, FormatOpenAIChat, model, nil, false, nil)
		}
		if chosen != nil {
			res, _, callErr := classifyViaUpstream(ctx, chosen, model, "user_upstream", state, criteria, defaultClassifierInstructions, ucfg.MinConfidence, ucfg.ClassifierTimeoutMS)
			return res, callErr
		}
		// Also support using a model available on the user's API key from gateway upstreams
		if ak.ID == 0 || (ak.allowsModel(model) && !ak.quotaExhausted(model)) {
			res, up, usage, gwErr := classifyViaGatewayUpstream(ctx, model, state, criteria, defaultClassifierInstructions, ucfg.MinConfidence, ucfg.ClassifierTimeoutMS)
			if gwErr == nil {
				res.Provider = "user_upstream"
				if !dryRun && ak.ID > 0 {
					recordSharedClassifierCall(ak, up, model, usage, res.LatencyMS)
				}
				return res, nil
			}
		}
		return nil, fmt.Errorf("未找到支持决策模型 %q 的可用渠道", model)

	case "user_external":
		if !validUserClassifierProvider(ucfg.UserClassifierProvider) {
			return nil, errors.New("unsupported personal external classifier provider")
		}
		if ucfg.UserClassifierEndpoint != "" {
			if err := validatePublicUpstreamURL(ucfg.UserClassifierEndpoint, true); err != nil {
				return nil, err
			}
		}
		tempCfg := defaultAutoRouterConfig()
		tempCfg.userExternal = true
		tempCfg.Classifier = ucfg.UserClassifierProvider
		tempCfg.ClassifierModel = ucfg.UserClassifierModel
		tempCfg.CloudflareAccountID = ucfg.UserCloudflareAccountID
		tempCfg.EndpointURL = ucfg.UserClassifierEndpoint
		tempCfg.APIKey = ucfg.UserClassifierAPIKey
		tempCfg.ClassifierTimeoutMS = ucfg.ClassifierTimeoutMS
		tempCfg.MinConfidence = ucfg.MinConfidence
		tempCfg.TierCriteria = criteria
		res, err := classifyWithDecisionModel(ctx, tempCfg, state, true)
		if err != nil {
			return nil, err
		}
		res.Provider = "user_" + res.Provider
		return res, nil

	default:
		return nil, errors.New("heuristic mode")
	}
}

func resolveUserAutoRoute(ctx context.Context, ak ApiKey, format clientFormat, nativeOnly bool, headers http.Header, cr *canonReq, dryRun bool) (*routerDecisionDetails, []userRouteTarget, error) {
	sub := userSubForKey(ak)
	ucfg := getUserAutoRouterConfig(sub)
	return resolveUserAutoRouteWithConfig(ctx, sub, ucfg, ak, format, nativeOnly, headers, cr, dryRun)
}

func resolveUserAutoRouteWithConfig(ctx context.Context, sub string, ucfg UserAutoRouterConfig, ak ApiKey, format clientFormat, nativeOnly bool, headers http.Header, cr *canonReq, dryRun bool) (*routerDecisionDetails, []userRouteTarget, error) {
	requested := ucfg.VirtualModel
	if cr != nil && strings.TrimSpace(cr.Model) != "" {
		requested = strings.TrimSpace(cr.Model)
	}
	estimatedTokens := estimateRequestContextTokens(cr)
	decision := &routerDecisionDetails{
		RequestedModel:       requested,
		ClassifierProvider:   ucfg.ClassifierSource,
		EstimatedInputTokens: estimatedTokens,
	}

	latestUser, stateText := extractClassifierState(cr, 2, 6000)
	hasToolResult := isToolResultTurn(cr)

	var chosenTier string
	var cause string

	switch {
	case isHousekeepingRequest(cr, latestUser):
		chosenTier = TierSimple
		cause = "housekeeping"
	case !dryRun && hasToolResult:
		endpointForKey := nativeEndpoint(format, cr)
		skey := "user:" + sub + ":" + scopedSessionKey(ak.ID, sessionKeyFromCanon(endpointForKey, headers, cr))
		if prevModel, prevTier, ok := lookupAutoRouterSession(skey); ok && prevModel != "" {
			ctxLimit := effectiveModelContextLimit(prevModel, nil)
			if estimatedTokens <= 0 || ctxLimit <= 0 || estimatedTokens <= ctxLimit {
				if target, okTarget, _ := evaluateUserCandidateTarget(sub, ucfg, ak, format, nativeOnly, cr, prevModel, prevTier); okTarget {
					tPlan := resolveAutoRouterThinking(nil, prevModel, prevTier, prevModel, ucfg.TierThinking[prevTier], "session_tool_followup", cr)
					decision.Tier = prevTier
					decision.Cause = "session_tool_followup"
					decision.SelectedModel = prevModel
					decision.SelectedThinking = tPlan.EffectiveLabel
					decision.Steps = []routerCandidateStep{{
						Priority:     1,
						Model:        prevModel,
						SourceTier:   prevTier,
						Status:       "selected",
						Thinking:     tPlan.EffectiveLabel,
						ContextLimit: ctxLimit,
					}}
					return decision, []userRouteTarget{target}, nil
				}
			}
		}
	}

	if chosenTier == "" {
		if shouldShortCircuitSimple(latestUser, ucfg.SkipShortChars) && estimatedTokens <= 2000 && (cr == nil || len(cr.Messages) <= 4) && !hasToolResult {
			chosenTier = TierSimple
			cause = "short_circuit"
		} else if ucfg.ClassifierSource == "heuristic" || ucfg.ClassifierSource == "" {
			tier, score := classifyHeuristic(latestUser, stateText, cr)
			chosenTier = tier
			cause = "heuristic"
			decision.ClassifierProvider = "heuristic"
			decision.ClassifierModel = "heuristic"
			decision.HeuristicScore = &score
		} else {
			clfRes, clfErr := classifyForUserAutoRouter(ctx, ak, sub, ucfg, stateText, dryRun)
			if clfErr == nil && clfRes != nil {
				chosenTier = clfRes.Tier
				cause = clfRes.Provider
				decision.ClassifierProvider = clfRes.Provider
				decision.ClassifierModel = clfRes.Model
				decision.ClassifierMS = clfRes.LatencyMS
				decision.Confidence = clfRes.Confidence
				decision.Probabilities = clfRes.Probabilities
				decision.UpgradedLowConfidence = clfRes.UpgradedLowConfidence
			} else {
				if clfErr != nil {
					decision.FallbackReason = clfErr.Error()
				}
				if ucfg.ClassifierFallback == "default_model" {
					chosenTier = TierDefault
					cause = "fallback:default_model"
				} else {
					tier, score := classifyHeuristic(latestUser, stateText, cr)
					chosenTier = tier
					cause = "fallback:heuristic"
					decision.HeuristicScore = &score
				}
			}
		}
	}

	if chosenTier == "" {
		chosenTier = TierComplex
	}
	if ucfg.LongContextThreshold > 0 && estimatedTokens >= ucfg.LongContextThreshold && cause != "housekeeping" {
		if chosenTier == TierSimple || chosenTier == TierMedium {
			chosenTier = TierComplex
			decision.ContextUpgraded = true
			if cause != "" {
				cause += "+long_context"
			} else {
				cause = "long_context"
			}
		}
	}
	decision.Tier = chosenTier
	decision.Cause = cause

	targets, steps, skippedCount, quotaErr := selectUserTierTargets(sub, ucfg, ak, format, nativeOnly, cr, chosenTier, estimatedTokens, cause)
	decision.Steps = steps
	decision.SkippedCandidates = skippedCount
	if quotaErr != nil {
		return decision, nil, quotaErr
	}
	if len(targets) > 0 {
		decision.SelectedModel = targets[0].Model
		for _, st := range steps {
			if st.Status == "selected" {
				decision.SelectedThinking = st.Thinking
				break
			}
		}
	}
	decision.syncOutcome(ucfg.DefaultModel)
	return decision, targets, nil
}

func evaluateUserCandidateTarget(sub string, ucfg UserAutoRouterConfig, ak ApiKey, format clientFormat, nativeOnly bool, cr *canonReq, model string, sourceTier string) (userRouteTarget, bool, string) {
	model = strings.TrimSpace(model)
	spec := parseRouterModelSpec(model)
	target := userRouteTarget{
		Model:      model,
		SourceTier: sourceTier,
	}
	if spec.BaseModel == "" || strings.EqualFold(spec.BaseModel, ucfg.VirtualModel) {
		return target, false, "invalid_model"
	}

	// Check user's personal BYOK upstreams
	var byokHealthy, byokConfigured bool
	if ups, err := getUserUpstreams(sub); err == nil {
		for i := range ups {
			u := &ups[i]
			if !u.Enabled || u.Weight <= 0 || !servesRouterCandidate(u, model) {
				continue
			}
			if nativeOnly && u.Type != format.nativeUpstreamType() {
				continue
			}
			byokConfigured = true
			candModel := resolveUpstreamCandidateModel(u, model)
			if u.availableForRelay(candModel) {
				byokHealthy = true
				break
			}
		}
	}

	// Check site public upstreams (unless personal_only)
	var siteHealthy bool
	var siteRejectReason string
	if ucfg.ChannelPreference != "personal_only" {
		if ak.ID > 0 && !allowsRouterCandidate(ak, model) {
			siteRejectReason = "not_in_key_whitelist"
		} else if ak.ID > 0 && routerCandidateQuotaExhausted(ak, "", model) {
			siteRejectReason = "quota_exhausted"
		} else if up, _, err := pickUpstreamForRouterCandidate(format, model, nil, nativeOnly, cr); err == nil && up != nil {
			siteHealthy = true
		} else {
			siteRejectReason = "no_healthy_upstream"
		}
	}

	switch ucfg.ChannelPreference {
	case "personal_only":
		if byokHealthy {
			target.PreferBYOK = true
			target.AllowBYOK = true
			return target, true, ""
		}
		if byokConfigured {
			return target, false, "personal_upstream_cooling"
		}
		return target, false, "no_personal_upstream"

	case "gateway_first":
		if siteHealthy {
			target.PreferBYOK = false
			target.AllowSite = true
			target.AllowBYOK = byokHealthy
			return target, true, ""
		}
		if byokHealthy {
			target.PreferBYOK = true
			target.AllowBYOK = true
			return target, true, ""
		}
		if siteRejectReason != "" {
			return target, false, siteRejectReason
		}
		return target, false, "no_healthy_upstream"

	default: // "personal_first"
		if byokHealthy {
			target.PreferBYOK = true
			target.AllowBYOK = true
			target.AllowSite = siteHealthy
			return target, true, ""
		}
		if siteHealthy {
			target.PreferBYOK = false
			target.AllowSite = true
			return target, true, ""
		}
		if siteRejectReason != "" {
			return target, false, siteRejectReason
		}
		if byokConfigured {
			return target, false, "personal_upstream_cooling"
		}
		return target, false, "no_healthy_upstream"
	}
}

func selectUserTierTargets(sub string, ucfg UserAutoRouterConfig, ak ApiKey, format clientFormat, nativeOnly bool, cr *canonReq, startTier string, estimatedTokens int, cause string) ([]userRouteTarget, []routerCandidateStep, int, error) {
	type tierCandidate struct {
		model      string
		sourceTier string
	}
	seen := make(map[string]bool)
	ordered := make([]tierCandidate, 0, 12)
	addCandidate := func(model, tier string) {
		m := strings.TrimSpace(model)
		spec := parseRouterModelSpec(m)
		if spec.BaseModel == "" || strings.EqualFold(spec.BaseModel, ucfg.VirtualModel) || seen[m] {
			return
		}
		seen[m] = true
		ordered = append(ordered, tierCandidate{model: m, sourceTier: tier})
	}

	if startTier != TierDefault {
		for _, t := range orderedCascadeTiers(startTier, ucfg.CascadeTiers) {
			for _, m := range ucfg.Tiers[t] {
				addCandidate(m, t)
			}
		}
	}
	if ucfg.DefaultModel != "" {
		addCandidate(ucfg.DefaultModel, TierDefault)
	}

	steps := make([]routerCandidateStep, 0, len(ordered))
	selected := make([]userRouteTarget, 0, len(ordered))
	skipped := 0
	sawQuotaExhausted := false
	var firstExhaustedModel string

	type contextFallbackItem struct {
		stepIdx  int
		target   userRouteTarget
		ctxLimit int
	}
	var ctxFallbacks []contextFallbackItem

	for idx, item := range ordered {
		priority := idx + 1
		ctxLimit := effectiveModelContextLimit(item.model, nil)
		tPlan := resolveAutoRouterThinking(nil, item.model, item.sourceTier, item.model, ucfg.TierThinking[item.sourceTier], cause, cr)
		target, ok, reason := evaluateUserCandidateTarget(sub, ucfg, ak, format, nativeOnly, cr, item.model, item.sourceTier)
		if !ok {
			skipped++
			if reason == "quota_exhausted" {
				sawQuotaExhausted = true
				if firstExhaustedModel == "" {
					firstExhaustedModel = item.model
				}
			}
			steps = append(steps, routerCandidateStep{
				Priority:     priority,
				Model:        item.model,
				SourceTier:   item.sourceTier,
				Status:       "skipped",
				Reason:       reason,
				Thinking:     tPlan.EffectiveLabel,
				ContextLimit: ctxLimit,
			})
			continue
		}
		if estimatedTokens > 0 && ctxLimit > 0 && estimatedTokens > ctxLimit {
			skipped++
			stepIdx := len(steps)
			steps = append(steps, routerCandidateStep{
				Priority:     priority,
				Model:        item.model,
				SourceTier:   item.sourceTier,
				Status:       "skipped",
				Reason:       "context_exceeded",
				Thinking:     tPlan.EffectiveLabel,
				ContextLimit: ctxLimit,
			})
			ctxFallbacks = append(ctxFallbacks, contextFallbackItem{
				stepIdx:  stepIdx,
				target:   target,
				ctxLimit: ctxLimit,
			})
			continue
		}
		status := "standby"
		if len(selected) == 0 {
			status = "selected"
		}
		upLabel := "站点渠道"
		if target.PreferBYOK {
			upLabel = "我的渠道 (BYOK)"
		}
		steps = append(steps, routerCandidateStep{
			Priority:     priority,
			Model:        item.model,
			SourceTier:   item.sourceTier,
			Status:       status,
			UpstreamName: upLabel,
			Thinking:     tPlan.EffectiveLabel,
			ContextLimit: ctxLimit,
		})
		selected = append(selected, target)
	}

	if len(selected) == 0 && len(ctxFallbacks) > 0 && !sawQuotaExhausted {
		best := ctxFallbacks[0]
		for _, fb := range ctxFallbacks[1:] {
			if fb.ctxLimit > best.ctxLimit {
				best = fb
			}
		}
		if best.stepIdx >= 0 && best.stepIdx < len(steps) {
			steps[best.stepIdx].Status = "selected"
			steps[best.stepIdx].Reason = "largest_available_context"
			if skipped > 0 {
				skipped--
			}
		}
		selected = append(selected, best.target)
	}

	if len(selected) == 0 && sawQuotaExhausted {
		return nil, steps, skipped, fmt.Errorf("quota exhausted for model %s (all personal router candidates exhausted)", firstExhaustedModel)
	}
	return selected, steps, skipped, nil
}

// buildDirectModelTargets constructs routing targets when a user calls a concrete model name directly.
func buildDirectModelTargets(ak ApiKey, format clientFormat, nativeOnly bool, cr *canonReq, model string) ([]userRouteTarget, error) {
	sub := userSubForKey(ak)
	siteAllowed := ak.allowsModel(model)
	siteQuotaOK := siteAllowed && !ak.quotaExhausted(model)
	userHasBYOK := userServesModel(ak, model)

	if !userHasBYOK && !siteAllowed {
		return nil, errModelNotAllowed
	}
	if !userHasBYOK && !siteQuotaOK {
		return nil, &modelQuotaExhaustedError{model: model}
	}

	if sub == "" || !userHasBYOK {
		return []userRouteTarget{{
			Model:      model,
			PreferBYOK: false,
			AllowBYOK:  false,
			AllowSite:  true,
		}}, nil
	}

	ucfg := getUserAutoRouterConfig(sub)
	preferBYOK := ucfg.ChannelPreference != "gateway_first" || !siteQuotaOK
	return []userRouteTarget{{
		Model:      model,
		PreferBYOK: preferBYOK,
		AllowBYOK:  true,
		AllowSite:  siteQuotaOK && ucfg.ChannelPreference != "personal_only",
	}}, nil
}

var errModelNotAllowed = errors.New("model is not allowed for this API key")

type modelQuotaExhaustedError struct {
	model string
}

func (e *modelQuotaExhaustedError) Error() string {
	return "quota exhausted for model " + e.model
}

// ---------- User Console HTTP Routes (/api/user/upstreams & /api/user/auto-router) ----------

type userUpstreamView struct {
	ID                 int64    `json:"id"`
	Name               string   `json:"name"`
	Type               string   `json:"type"`
	OpenAIProtocol     string   `json:"openai_protocol"`
	BaseURL            string   `json:"base_url"`
	KeyMasked          string   `json:"key_masked"`
	Models             []string `json:"models"`
	ModelsRaw          string   `json:"models_raw"`
	ModelMap           string   `json:"model_map"`
	Priority           int      `json:"priority"`
	Weight             int      `json:"weight"`
	Enabled            bool     `json:"enabled"`
	ProxyMode          string   `json:"proxy_mode"`
	ProxyURL           string   `json:"proxy_url"`
	ProxyUsername      string   `json:"proxy_username"`
	ProxyPasswordSaved bool     `json:"proxy_password_saved"`
	Healthy            bool     `json:"healthy"`
	Fails              int      `json:"fails"`
	CooldownUntil      string   `json:"cooldown_until,omitempty"`
	CreatedAt          string   `json:"created_at"`
}

func buildUserUpstreamViews(sub string) ([]userUpstreamView, error) {
	ups, err := getUserUpstreams(sub)
	if err != nil {
		return nil, err
	}
	out := make([]userUpstreamView, 0, len(ups))
	for _, u := range ups {
		var cooldownStr string
		if until := u.cooldownUntil(); !until.IsZero() {
			cooldownStr = until.UTC().Format(time.RFC3339)
		}
		modelsList := normalizeModelSlice([]string{u.Models})
		if len(modelsList) == 0 {
			if defaults := defaultDecisionModelsForType(u.Type); len(defaults) > 0 {
				modelsList = append([]string(nil), defaults...)
			}
		}
		proxyMode := u.userProxyMode
		if proxyMode == "" {
			proxyMode = "default"
		}
		var proxyURL, proxyUser string
		var proxyPassSaved bool
		if u.customProxy != nil {
			proxyURL = u.customProxy.URL
			proxyUser = u.customProxy.Username
			proxyPassSaved = u.customProxy.Password != ""
		}
		out = append(out, userUpstreamView{
			ID:                 -u.ID,
			Name:               u.Name,
			Type:               u.Type,
			OpenAIProtocol:     u.OpenAIProtocol,
			BaseURL:            u.BaseURL,
			KeyMasked:          maskKey(u.APIKey),
			Models:             modelsList,
			ModelsRaw:          u.Models,
			ModelMap:           u.ModelMap,
			Priority:           u.Priority,
			Weight:             u.Weight,
			Enabled:            u.Enabled,
			ProxyMode:          proxyMode,
			ProxyURL:           proxyURL,
			ProxyUsername:      proxyUser,
			ProxyPasswordSaved: proxyPassSaved,
			Healthy:            u.availableForRelay(),
			Fails:              u.relayFailures(),
			CooldownUntil:      cooldownStr,
			CreatedAt:          u.CreatedAt,
		})
	}
	return out, nil
}

func buildUserAutoRouterView(ctx context.Context, sub string, ak ApiKey) gin.H {
	siteCfg := getAutoRouterConfig()
	ucfg := getUserAutoRouterConfig(sub)
	upViews, _ := buildUserUpstreamViews(sub)
	personalModels := userAvailableModels(sub)
	siteModels := normalizeModelSlice([]string{ak.Models})
	siteVM := strings.TrimSpace(siteCfg.VirtualModel)
	if siteVM == "" {
		siteVM = "auto"
	}
	if len(siteModels) == 0 && strings.TrimSpace(ak.Models) == "" {
		siteModels, _ = collectGatewayUpstreamBoundModels(ctx, siteVM)
	}
	filteredSiteModels := make([]string, 0, len(siteModels))
	for _, m := range siteModels {
		if m != siteVM {
			filteredSiteModels = append(filteredSiteModels, m)
		}
	}

	sharedClassifiers := make([]gin.H, 0, len(siteCfg.SharedClassifierModels)+1)
	if siteCfg.ShareSiteClassifierWithUsers {
		sharedClassifiers = append(sharedClassifiers, gin.H{
			"id":    siteDefaultSharedModel,
			"label": fmt.Sprintf("站点默认决策模型 (%s · %s)", siteCfg.Classifier, siteCfg.ClassifierModel),
		})
	}
	for _, m := range siteCfg.SharedClassifierModels {
		m = strings.TrimSpace(m)
		if m != "" {
			sharedClassifiers = append(sharedClassifiers, gin.H{
				"id":    m,
				"label": m + " (站点网关共享)",
			})
		}
	}

	// Build candidate model health & source badges
	candidateStatus := make(map[string]gin.H)
	allRelevant := make(map[string]bool)
	for _, m := range personalModels {
		allRelevant[m] = true
	}
	for _, m := range filteredSiteModels {
		allRelevant[m] = true
	}
	for _, list := range ucfg.Tiers {
		for _, m := range list {
			allRelevant[m] = true
		}
	}
	if ucfg.DefaultModel != "" {
		allRelevant[ucfg.DefaultModel] = true
	}

	personalSet := make(map[string]bool, len(personalModels))
	for _, m := range personalModels {
		personalSet[m] = true
	}
	siteSet := make(map[string]bool, len(filteredSiteModels))
	for _, m := range filteredSiteModels {
		siteSet[m] = true
	}

	for m := range allRelevant {
		spec := parseRouterModelSpec(m)
		_, ok, reason := evaluateUserCandidateTarget(sub, ucfg, ak, FormatOpenAIChat, false, nil, m, "")
		inBYOK := personalSet[m] || personalSet[spec.BaseModel]
		inSite := siteSet[m] || siteSet[spec.BaseModel]
		sourceTag := "missing"
		switch {
		case inBYOK && inSite:
			sourceTag = "both"
		case inBYOK:
			sourceTag = "byok"
		case inSite:
			sourceTag = "site"
		}
		profile := classifyModelThinkingCapability(nil, m)
		ctxLimit, ctxSource := effectiveModelContextLimitWithSource(m, siteCfg.ModelContextLimits)
		candidateStatus[m] = gin.H{
			"available":         ok,
			"reason":            reason,
			"source":            sourceTag,
			"in_byok":           inBYOK,
			"in_site":           inSite,
			"site_exhausted":    inSite && routerCandidateQuotaExhausted(ak, "", m),
			"context_limit":     ctxLimit,
			"context_source":    ctxSource,
			"max_output":        modelMaxOutputLimit(m),
			"supports_thinking": profile.SupportsThinking,
		}
	}

	return gin.H{
		"allow_user_auto_router":           siteCfg.AllowUserAutoRouter,
		"site_virtual_model":               siteVM,
		"site_default_proxy_configured":    siteDefaultProxyConfigured(),
		"share_site_classifier_with_users": siteCfg.ShareSiteClassifierWithUsers,
		"shared_classifier_models":         sharedClassifiers,
		"enabled":                          ucfg.Enabled,
		"virtual_model":                    ucfg.VirtualModel,
		"channel_preference":               ucfg.ChannelPreference,
		"classifier_source":                ucfg.ClassifierSource,
		"shared_classifier_model":          ucfg.SharedClassifierModel,
		"user_classifier_upstream":         ucfg.UserClassifierUpstream,
		"user_classifier_provider":         ucfg.UserClassifierProvider,
		"user_classifier_model":            ucfg.UserClassifierModel,
		"user_cloudflare_account_id":       ucfg.UserCloudflareAccountID,
		"user_classifier_endpoint":         ucfg.UserClassifierEndpoint,
		"user_classifier_api_key_saved":    strings.TrimSpace(ucfg.UserClassifierAPIKey) != "",
		"user_classifier_api_key_masked":   maskKey(ucfg.UserClassifierAPIKey),
		"classifier_timeout_ms":            ucfg.ClassifierTimeoutMS,
		"classifier_fallback":              ucfg.ClassifierFallback,
		"min_confidence":                   ucfg.MinConfidence,
		"skip_short_chars":                 ucfg.SkipShortChars,
		"long_context_threshold":           ucfg.LongContextThreshold,
		"cascade_tiers":                    ucfg.CascadeTiers,
		"default_model":                    ucfg.DefaultModel,
		"tiers":                            ucfg.Tiers,
		"tier_thinking":                    normalizeTierThinkingMap(ucfg.TierThinking),
		"tier_criteria":                    ucfg.effectiveTierCriteria(),
		"default_tier_criteria":            defaultAutoRouterTierCriteria(),
		"upstreams":                        upViews,
		"personal_models":                  personalModels,
		"site_models":                      filteredSiteModels,
		"candidate_status":                 candidateStatus,
	}
}

func normalizeUserModelMapJSON(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "{}", nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		return "", errors.New("模型映射 (model_map) 必须是合法的 JSON 对象，例如 {\"gpt-4o\":\"deepseek-chat\"}")
	}
	clean := make(map[string]string, len(m))
	for k, v := range m {
		ck, cv := strings.TrimSpace(k), strings.TrimSpace(v)
		if ck != "" && cv != "" {
			clean[ck] = cv
		}
	}
	b, _ := json.Marshal(clean)
	return string(b), nil
}

func registerUserRouterRoutes(userAPI *gin.RouterGroup) {
	userAPI.GET("/upstreams", func(c *gin.Context) {
		user := c.MustGet("xauthUser").(xauthUserRecord)
		views, err := buildUserUpstreamViews(user.Sub)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "加载个人渠道失败"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": views, "site_default_proxy_configured": siteDefaultProxyConfigured()})
	})

	userAPI.POST("/upstreams", func(c *gin.Context) {
		user := c.MustGet("xauthUser").(xauthUserRecord)
		var req struct {
			Name           string `json:"name"`
			Type           string `json:"type"`
			OpenAIProtocol string `json:"openai_protocol"`
			BaseURL        string `json:"base_url"`
			APIKey         string `json:"api_key"`
			Models         string `json:"models"`
			ModelMap       string `json:"model_map"`
			Priority       int    `json:"priority"`
			Weight         int    `json:"weight"`
			Enabled        *bool  `json:"enabled"`
			ProxyMode      string `json:"proxy_mode"`
			ProxyURL       string `json:"proxy_url"`
			ProxyUsername  string `json:"proxy_username"`
			ProxyPassword  string `json:"proxy_password"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数格式不正确"})
			return
		}
		existing, err := getUserUpstreams(user.Sub)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "查询渠道数量失败"})
			return
		}
		if len(existing) >= maxUserUpstreamsPerUser {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("每个账号最多添加 %d 条个人渠道", maxUserUpstreamsPerUser)})
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请填写渠道名称"})
			return
		}
		upType := strings.ToLower(strings.TrimSpace(req.Type))
		if upType == "" {
			upType = "openai"
		}
		if !validUserUpstreamType(upType) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "渠道类型仅支持 openai、anthropic、gemini、openrouter、systemone 或 cloudflare"})
			return
		}
		proto := strings.ToLower(strings.TrimSpace(req.OpenAIProtocol))
		if proto == "" {
			proto = "auto"
		}
		if !validUpstreamProtocol(upType, proto) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "OpenAI 协议模式不合法"})
			return
		}
		if err := validatePublicUpstreamURL(req.BaseURL, false); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		apiKey := strings.TrimSpace(req.APIKey)
		if apiKey == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请填写渠道 API Key"})
			return
		}
		proxyMode, proxyURL, proxyUser, proxyPass, err := normalizeUserUpstreamProxy(req.ProxyMode, req.ProxyURL, req.ProxyUsername, req.ProxyPassword)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		modelsNorm := strings.Join(normalizeModelSlice([]string{req.Models}), ",")
		if modelsNorm == "" {
			if defaults := defaultDecisionModelsForType(upType); len(defaults) > 0 {
				modelsNorm = strings.Join(defaults, ",")
			}
		}
		modelMapJSON, err := normalizeUserModelMapJSON(req.ModelMap)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if modelsNorm == "" && modelMapJSON == "{}" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请至少填写一个可用模型名称（或点击探测模型获取）"})
			return
		}
		priority := req.Priority
		if priority == 0 {
			priority = 10
		}
		weight := req.Weight
		if weight <= 0 {
			weight = 1
		}
		enabled := 1
		if req.Enabled != nil && !*req.Enabled {
			enabled = 0
		}
		res, err := db.Exec(`INSERT INTO user_upstreams(sub, name, type, openai_protocol, base_url, api_key, models, model_map, priority, weight, enabled, proxy_mode, proxy_url, proxy_username, proxy_password)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			user.Sub, name, upType, proto, strings.TrimSpace(req.BaseURL), apiKey, modelsNorm, modelMapJSON, priority, weight, enabled, proxyMode, proxyURL, proxyUser, proxyPass)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存个人渠道失败"})
			return
		}
		id, _ := res.LastInsertId()
		invalidateUserRouterCache(user.Sub)
		views, _ := buildUserUpstreamViews(user.Sub)
		c.JSON(http.StatusOK, gin.H{"ok": true, "id": id, "items": views})
	})

	userAPI.PUT("/upstreams/:id", func(c *gin.Context) {
		user := c.MustGet("xauthUser").(xauthUserRecord)
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的渠道 ID"})
			return
		}
		var (
			curName, curType, curProto, curBase, curKey, curModels, curMap string
			curProxyMode, curProxyURL, curProxyUser, curProxyPass          string
			curPriority, curWeight, curEnabled                             int
		)
		err = db.QueryRow(`SELECT name, type, openai_protocol, base_url, api_key, models, model_map, priority, weight, enabled, proxy_mode, proxy_url, proxy_username, proxy_password
			FROM user_upstreams WHERE id=? AND sub=?`, id, user.Sub).
			Scan(&curName, &curType, &curProto, &curBase, &curKey, &curModels, &curMap, &curPriority, &curWeight, &curEnabled, &curProxyMode, &curProxyURL, &curProxyUser, &curProxyPass)
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到该个人渠道"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "查询个人渠道失败"})
			return
		}

		var req struct {
			Name               *string `json:"name"`
			Type               *string `json:"type"`
			OpenAIProtocol     *string `json:"openai_protocol"`
			BaseURL            *string `json:"base_url"`
			APIKey             *string `json:"api_key"`
			Models             *string `json:"models"`
			ModelMap           *string `json:"model_map"`
			Priority           *int    `json:"priority"`
			Weight             *int    `json:"weight"`
			Enabled            *bool   `json:"enabled"`
			ProxyMode          *string `json:"proxy_mode"`
			ProxyURL           *string `json:"proxy_url"`
			ProxyUsername      *string `json:"proxy_username"`
			ProxyPassword      *string `json:"proxy_password"`
			ClearProxyPassword bool    `json:"clear_proxy_password"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数格式不正确"})
			return
		}
		if req.Name != nil {
			curName = strings.TrimSpace(*req.Name)
			if curName == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "渠道名称不能为空"})
				return
			}
		}
		if req.Type != nil {
			curType = strings.ToLower(strings.TrimSpace(*req.Type))
			if !validUserUpstreamType(curType) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "渠道类型仅支持 openai、anthropic、gemini、openrouter、systemone 或 cloudflare"})
				return
			}
		}
		if req.OpenAIProtocol != nil {
			curProto = strings.ToLower(strings.TrimSpace(*req.OpenAIProtocol))
			if curProto == "" {
				curProto = "auto"
			}
		}
		if !validUpstreamProtocol(curType, curProto) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "OpenAI 协议模式不合法"})
			return
		}
		if req.BaseURL != nil {
			curBase = strings.TrimSpace(*req.BaseURL)
			if err := validatePublicUpstreamURL(curBase, false); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
		}
		if req.APIKey != nil && strings.TrimSpace(*req.APIKey) != "" {
			curKey = strings.TrimSpace(*req.APIKey)
		}
		if req.Models != nil {
			curModels = strings.Join(normalizeModelSlice([]string{*req.Models}), ",")
		}
		if req.ModelMap != nil {
			mJSON, mErr := normalizeUserModelMapJSON(*req.ModelMap)
			if mErr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": mErr.Error()})
				return
			}
			curMap = mJSON
		}
		if curModels == "" {
			if defaults := defaultDecisionModelsForType(curType); len(defaults) > 0 {
				curModels = strings.Join(defaults, ",")
			}
		}
		if curModels == "" && curMap == "{}" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请至少保留一个可用模型名称"})
			return
		}
		if req.Priority != nil {
			curPriority = *req.Priority
		}
		if req.Weight != nil && *req.Weight > 0 {
			curWeight = *req.Weight
		}
		if req.Enabled != nil {
			if *req.Enabled {
				curEnabled = 1
			} else {
				curEnabled = 0
			}
		}
		if req.ProxyMode != nil {
			curProxyMode = *req.ProxyMode
		}
		if req.ProxyURL != nil {
			curProxyURL = *req.ProxyURL
		}
		if req.ProxyUsername != nil {
			curProxyUser = *req.ProxyUsername
		}
		if req.ProxyPassword != nil && *req.ProxyPassword != "" {
			curProxyPass = *req.ProxyPassword
		}
		if req.ClearProxyPassword || (req.ProxyUsername != nil && strings.TrimSpace(*req.ProxyUsername) == "") {
			curProxyPass = ""
		}
		normMode, normURL, normUser, normPass, pErr := normalizeUserUpstreamProxy(curProxyMode, curProxyURL, curProxyUser, curProxyPass)
		if pErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": pErr.Error()})
			return
		}
		curProxyMode, curProxyURL, curProxyUser, curProxyPass = normMode, normURL, normUser, normPass

		_, err = db.Exec(`UPDATE user_upstreams SET name=?, type=?, openai_protocol=?, base_url=?, api_key=?, models=?, model_map=?, priority=?, weight=?, enabled=?, proxy_mode=?, proxy_url=?, proxy_username=?, proxy_password=?
			WHERE id=? AND sub=?`,
			curName, curType, curProto, curBase, curKey, curModels, curMap, curPriority, curWeight, curEnabled, curProxyMode, curProxyURL, curProxyUser, curProxyPass, id, user.Sub)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "更新个人渠道失败"})
			return
		}
		closeProxyTransport(-id)
		healthMarkSuccess(-id)
		invalidateUserRouterCache(user.Sub)
		views, _ := buildUserUpstreamViews(user.Sub)
		c.JSON(http.StatusOK, gin.H{"ok": true, "items": views})
	})

	userAPI.DELETE("/upstreams/:id", func(c *gin.Context) {
		user := c.MustGet("xauthUser").(xauthUserRecord)
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的渠道 ID"})
			return
		}
		_, err = db.Exec(`DELETE FROM user_upstreams WHERE id=? AND sub=?`, id, user.Sub)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "删除个人渠道失败"})
			return
		}
		closeProxyTransport(-id)
		healthMarkSuccess(-id)
		invalidateUserRouterCache(user.Sub)
		views, _ := buildUserUpstreamViews(user.Sub)
		c.JSON(http.StatusOK, gin.H{"ok": true, "items": views})
	})

	userAPI.POST("/upstreams/probe", func(c *gin.Context) {
		user := c.MustGet("xauthUser").(xauthUserRecord)
		var req struct {
			ID            int64  `json:"id"`
			Type          string `json:"type"`
			BaseURL       string `json:"base_url"`
			APIKey        string `json:"api_key"`
			ProxyMode     string `json:"proxy_mode"`
			ProxyURL      string `json:"proxy_url"`
			ProxyUsername string `json:"proxy_username"`
			ProxyPassword string `json:"proxy_password"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "请求参数格式不正确"})
			return
		}
		upType := strings.ToLower(strings.TrimSpace(req.Type))
		baseURL := strings.TrimSpace(req.BaseURL)
		apiKey := strings.TrimSpace(req.APIKey)
		proxyMode := strings.TrimSpace(req.ProxyMode)
		proxyURL := strings.TrimSpace(req.ProxyURL)
		proxyUser := strings.TrimSpace(req.ProxyUsername)
		proxyPass := req.ProxyPassword

		if req.ID > 0 {
			var savedType, savedBase, savedKey string
			var savedPMode, savedPURL, savedPUser, savedPPass string
			if err := db.QueryRow(`SELECT type, base_url, api_key, proxy_mode, proxy_url, proxy_username, proxy_password FROM user_upstreams WHERE id=? AND sub=?`, req.ID, user.Sub).
				Scan(&savedType, &savedBase, &savedKey, &savedPMode, &savedPURL, &savedPUser, &savedPPass); err == nil {
				if upType == "" {
					upType = savedType
				}
				if baseURL == "" {
					baseURL = savedBase
				}
				if apiKey == "" {
					apiKey = savedKey
				}
				if proxyMode == "" {
					proxyMode = savedPMode
				}
				if proxyURL == "" {
					proxyURL = savedPURL
				}
				if proxyUser == "" {
					proxyUser = savedPUser
				}
				if proxyPass == "" && proxyUser == savedPUser {
					proxyPass = savedPPass
				}
			}
		}
		if upType == "" {
			upType = "openai"
		}
		if !validUserUpstreamType(upType) {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "渠道类型仅支持 openai、anthropic、gemini、openrouter、systemone 或 cloudflare"})
			return
		}
		if err := validatePublicUpstreamURL(baseURL, false); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": err.Error()})
			return
		}
		if apiKey == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "请填写渠道 API Key"})
			return
		}
		normMode, normURL, normUser, normPass, pErr := normalizeUserUpstreamProxy(proxyMode, proxyURL, proxyUser, proxyPass)
		if pErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": pErr.Error()})
			return
		}

		probeUpstream := &Upstream{ID: -1, ProxyID: siteDefaultProxyID()}
		probeProxyID := int64(0)
		if req.ID > 0 {
			probeUpstream.ID = -req.ID
			probeProxyID = -req.ID
		}
		if normMode == "custom" {
			probeUpstream.ProxyID = 0
			probeUpstream.customProxy = &OutboundProxy{
				ID:       probeProxyID,
				Name:     "user-byok-probe-proxy",
				URL:      normURL,
				Username: normUser,
				Password: normPass,
				Enabled:  true,
			}
		}
		probeClient, cErr := probeUpstream.httpClient(modelsFetchClient)
		if cErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": cErr.Error()})
			return
		}

		start := time.Now()
		models, err := probeUpstreamModelsWithClient(c.Request.Context(), upType, baseURL, apiKey, probeClient)
		latency := time.Since(start).Milliseconds()
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"ok": false, "latency_ms": latency, "error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "latency_ms": latency, "models": models, "count": len(models)})
	})

	userAPI.GET("/auto-router", func(c *gin.Context) {
		user := c.MustGet("xauthUser").(xauthUserRecord)
		ak := c.MustGet("xauthKey").(ApiKey)
		c.JSON(http.StatusOK, buildUserAutoRouterView(c.Request.Context(), user.Sub, ak))
	})

	userAPI.PUT("/auto-router", func(c *gin.Context) {
		user := c.MustGet("xauthUser").(xauthUserRecord)
		ak := c.MustGet("xauthKey").(ApiKey)
		var req struct {
			Enabled                 *bool                `json:"enabled"`
			VirtualModel            *string              `json:"virtual_model"`
			ChannelPreference       *string              `json:"channel_preference"`
			ClassifierSource        *string              `json:"classifier_source"`
			SharedClassifierModel   *string              `json:"shared_classifier_model"`
			UserClassifierUpstream  *int64               `json:"user_classifier_upstream"`
			UserClassifierProvider  *string              `json:"user_classifier_provider"`
			UserClassifierModel     *string              `json:"user_classifier_model"`
			UserCloudflareAccountID *string              `json:"user_cloudflare_account_id"`
			UserClassifierEndpoint  *string              `json:"user_classifier_endpoint"`
			UserClassifierAPIKey    *string              `json:"user_classifier_api_key"`
			ClearClassifierAPIKey   *bool                `json:"clear_classifier_api_key"`
			ClassifierTimeoutMS     *int                 `json:"classifier_timeout_ms"`
			ClassifierFallback      *string              `json:"classifier_fallback"`
			MinConfidence           *float64             `json:"min_confidence"`
			SkipShortChars          *int                 `json:"skip_short_chars"`
			LongContextThreshold    *int                 `json:"long_context_threshold"`
			CascadeTiers            *bool                `json:"cascade_tiers"`
			DefaultModel            *string              `json:"default_model"`
			Tiers                   *map[string][]string `json:"tiers"`
			TierThinking            *map[string]string   `json:"tier_thinking"`
			TierCriteria            *map[string]string   `json:"tier_criteria"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数格式不正确"})
			return
		}
		ucfg := getUserAutoRouterConfig(user.Sub)
		if req.Enabled != nil {
			ucfg.Enabled = *req.Enabled
		}
		if req.VirtualModel != nil {
			ucfg.VirtualModel = *req.VirtualModel
		}
		if req.ChannelPreference != nil {
			ucfg.ChannelPreference = *req.ChannelPreference
		}
		if req.ClassifierSource != nil {
			ucfg.ClassifierSource = *req.ClassifierSource
		}
		if req.SharedClassifierModel != nil {
			ucfg.SharedClassifierModel = *req.SharedClassifierModel
		}
		if req.UserClassifierUpstream != nil {
			ucfg.UserClassifierUpstream = *req.UserClassifierUpstream
		}
		if req.UserClassifierProvider != nil {
			ucfg.UserClassifierProvider = *req.UserClassifierProvider
		}
		if req.UserClassifierModel != nil {
			ucfg.UserClassifierModel = *req.UserClassifierModel
		}
		if req.UserCloudflareAccountID != nil {
			ucfg.UserCloudflareAccountID = *req.UserCloudflareAccountID
		}
		if req.UserClassifierEndpoint != nil {
			ucfg.UserClassifierEndpoint = *req.UserClassifierEndpoint
		}
		if req.ClearClassifierAPIKey != nil && *req.ClearClassifierAPIKey {
			ucfg.UserClassifierAPIKey = ""
		} else if req.UserClassifierAPIKey != nil && strings.TrimSpace(*req.UserClassifierAPIKey) != "" {
			ucfg.UserClassifierAPIKey = strings.TrimSpace(*req.UserClassifierAPIKey)
		}
		if req.ClassifierTimeoutMS != nil {
			ucfg.ClassifierTimeoutMS = *req.ClassifierTimeoutMS
		}
		if req.ClassifierFallback != nil {
			ucfg.ClassifierFallback = *req.ClassifierFallback
		}
		if req.MinConfidence != nil {
			ucfg.MinConfidence = *req.MinConfidence
		}
		if req.SkipShortChars != nil {
			ucfg.SkipShortChars = *req.SkipShortChars
		}
		if req.LongContextThreshold != nil {
			ucfg.LongContextThreshold = *req.LongContextThreshold
		}
		if req.CascadeTiers != nil {
			ucfg.CascadeTiers = *req.CascadeTiers
		}
		if req.DefaultModel != nil {
			ucfg.DefaultModel = *req.DefaultModel
		}
		if req.Tiers != nil {
			ucfg.Tiers = *req.Tiers
		}
		if req.TierThinking != nil {
			ucfg.TierThinking = *req.TierThinking
		}
		if req.TierCriteria != nil {
			ucfg.TierCriteria = *req.TierCriteria
		}

		if err := saveUserAutoRouterConfig(user.Sub, ucfg); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, buildUserAutoRouterView(c.Request.Context(), user.Sub, ak))
	})

	userAPI.POST("/auto-router/dry-run", func(c *gin.Context) {
		user := c.MustGet("xauthUser").(xauthUserRecord)
		ak := c.MustGet("xauthKey").(ApiKey)
		var req struct {
			Prompt   string                `json:"prompt"`
			Override *UserAutoRouterConfig `json:"override"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数格式不正确"})
			return
		}
		prompt := strings.TrimSpace(req.Prompt)
		if prompt == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请输入要试算的测试提问内容"})
			return
		}
		ucfg := getUserAutoRouterConfig(user.Sub)
		if req.Override != nil {
			merged := *req.Override
			if strings.TrimSpace(merged.UserClassifierAPIKey) == "" {
				merged.UserClassifierAPIKey = ucfg.UserClassifierAPIKey
			}
			if err := normalizeUserAutoRouterConfig(user.Sub, &merged, false); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			ucfg = merged
		}
		cr := &canonReq{
			Model:     ucfg.VirtualModel,
			Messages:  []canonMsg{{Role: "user", Text: prompt}},
			InputText: prompt,
		}
		decision, targets, quotaErr := resolveUserAutoRouteWithConfig(c.Request.Context(), user.Sub, ucfg, ak, FormatOpenAIChat, false, http.Header{}, cr, true)
		var quotaErrStr string
		if quotaErr != nil {
			quotaErrStr = quotaErr.Error()
		}
		c.JSON(http.StatusOK, gin.H{
			"decision":    decision,
			"targets":     targets,
			"quota_error": quotaErrStr,
		})
	})
}
