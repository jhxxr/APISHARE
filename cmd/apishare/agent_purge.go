package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"apishare/internal/agentpurge"
	"github.com/gin-gonic/gin"
)

const agentPurgePolicySetting = "agent_purge_policy"

var agentPurgeConfigMu sync.Mutex

type agentPurgePair struct {
	Model      string `json:"model"`
	UpstreamID int64  `json:"upstream_id"`
}

type agentPurgePolicy struct {
	Enabled  bool             `json:"enabled"`
	Revision int64            `json:"revision"`
	Pairs    []agentPurgePair `json:"pairs"`
}

type userAgentPurgeConfig struct {
	Enabled   bool             `json:"enabled"`
	Revision  int64            `json:"revision"`
	Mode      string           `json:"mode"`
	PersonaID string           `json:"persona_id"`
	Pairs     []agentPurgePair `json:"pairs"`
	Clients   []string         `json:"clients"`
}

type agentPurgePairOption struct {
	Model        string `json:"model"`
	UpstreamID   int64  `json:"upstream_id"`
	UpstreamName string `json:"upstream_name"`
	UpstreamType string `json:"upstream_type"`
	Enabled      bool   `json:"enabled,omitempty"`
}

type agentPurgeClientOption struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SDK       bool   `json:"sdk,omitempty"`
	Available bool   `json:"available"`
	Note      string `json:"note,omitempty"`
}

type agentPurgeRequestState struct {
	policy agentPurgePolicy
	user   userAgentPurgeConfig
}

type agentPurgeReceipt struct {
	Persona string `json:"persona"`
	Mode    string `json:"mode"`
	Name    string `json:"name,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

func newAgentPurgeReceipt(id, mode string) *agentPurgeReceipt {
	persona, ok := agentpurge.Lookup(id)
	if !ok {
		return nil
	}
	return &agentPurgeReceipt{Persona: persona.ID, Mode: mode, Name: persona.Name, SHA256: persona.SHA256}
}

type agentPurgeBudgetError struct {
	Model string
	Input int
	Limit int
}

func (e *agentPurgeBudgetError) Error() string {
	return fmt.Sprintf("使用所选提示词后，模型 %s 的输入预估 %d tokens，无法在 %d tokens 窗口内保留输出余量；请缩短上下文或选择较短模板", e.Model, e.Input, e.Limit)
}

// Account for the final template after provider shaping. The original canonical
// request remains untouched for affinity, routing and retries.
func boundAgentPurgeBody(body []byte, format clientFormat, model string, task *relayTask) ([]byte, error) {
	limit := effectiveModelContextLimit(model, task.modelContextLimits)
	if limit <= 0 {
		return body, nil
	}
	cr, err := parseClientRequest(format, body, model)
	if err != nil || cr.ConversionError != nil {
		// Opaque native extensions cannot be estimated as a complete transcript.
		return body, nil
	}
	input := estimateRequestContextTokens(cr)
	if input+256 >= limit {
		return nil, &agentPurgeBudgetError{Model: model, Input: input, Limit: limit}
	}
	raw, err := decodeProtocolObject(body)
	if err != nil {
		return body, nil
	}
	if !clampNativeMaxTokensFields(raw, format, model, input, task.modelContextLimits) {
		if cr.MaxTokens > 0 && input+cr.MaxTokens+256 > limit {
			return nil, &agentPurgeBudgetError{Model: model, Input: input, Limit: limit}
		}
		return body, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	if clamped, err := parseClientRequest(format, encoded, model); err == nil && clamped.MaxTokens > 0 && input+clamped.MaxTokens+256 > limit {
		return nil, &agentPurgeBudgetError{Model: model, Input: input, Limit: limit}
	}
	return encoded, nil
}

func loadAgentPurgePolicy() agentPurgePolicy {
	policy := agentPurgePolicy{Pairs: []agentPurgePair{}}
	raw, err := settingsGet(agentPurgePolicySetting)
	if err != nil || strings.TrimSpace(raw) == "" {
		return policy
	}
	if json.Unmarshal([]byte(raw), &policy) != nil {
		return agentPurgePolicy{Pairs: []agentPurgePair{}}
	}
	policy.Pairs = normalizeAgentPurgePairs(policy.Pairs)
	return policy
}

func loadUserAgentPurgeConfig(sub string) userAgentPurgeConfig {
	cfg := userAgentPurgeConfig{PersonaID: agentpurge.PersonaID, Pairs: []agentPurgePair{}, Clients: []string{}}
	if strings.TrimSpace(sub) == "" {
		return cfg
	}
	var raw string
	err := db.QueryRow(`SELECT config_json FROM user_agent_purge_configs WHERE sub=?`, sub).Scan(&raw)
	if err != nil {
		return cfg
	}
	if json.Unmarshal([]byte(raw), &cfg) != nil {
		return userAgentPurgeConfig{PersonaID: agentpurge.PersonaID, Pairs: []agentPurgePair{}, Clients: []string{}}
	}
	cfg.Pairs = normalizeAgentPurgePairs(cfg.Pairs)
	cfg.Clients = normalizeAgentPurgeClients(cfg.Clients)
	cfg.Mode = normalizeAgentPurgeMode(cfg.Mode)
	cfg.PersonaID = agentpurge.NormalizePersonaID(cfg.PersonaID)
	return cfg
}

func loadAgentPurgeRequestState(sub string) *agentPurgeRequestState {
	if strings.TrimSpace(sub) == "" {
		return nil
	}
	policy := loadAgentPurgePolicy()
	if !policy.Enabled || len(policy.Pairs) == 0 {
		return &agentPurgeRequestState{policy: policy}
	}
	return &agentPurgeRequestState{policy: policy, user: loadUserAgentPurgeConfig(sub)}
}

func normalizeAgentPurgeMode(mode string) string {
	switch mode {
	case "replace", "strict":
		return mode
	default:
		return "append"
	}
}

func normalizeAgentPurgeModel(model string) string {
	return strings.TrimSpace(model)
}

func normalizeAgentPurgePairs(pairs []agentPurgePair) []agentPurgePair {
	seen := make(map[string]bool, len(pairs))
	out := make([]agentPurgePair, 0, len(pairs))
	for _, pair := range pairs {
		pair.Model = strings.TrimSpace(pair.Model)
		if pair.Model == "" || len(pair.Model) > 256 || pair.UpstreamID <= 0 {
			continue
		}
		key := fmt.Sprintf("%d:%s", pair.UpstreamID, normalizeAgentPurgeModel(pair.Model))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, pair)
	}
	sort.Slice(out, func(i, j int) bool {
		if strings.ToLower(out[i].Model) != strings.ToLower(out[j].Model) {
			return strings.ToLower(out[i].Model) < strings.ToLower(out[j].Model)
		}
		return out[i].UpstreamID < out[j].UpstreamID
	})
	return out
}

func normalizeAgentPurgeClients(clients []string) []string {
	allowed := make(map[string]bool)
	for _, option := range agentPurgeClientOptions() {
		if option.Available {
			allowed[option.ID] = true
		}
	}
	seen := make(map[string]bool, len(clients))
	out := make([]string, 0, len(clients))
	for _, client := range clients {
		client = strings.TrimSpace(client)
		if allowed[client] && !seen[client] {
			seen[client] = true
			out = append(out, client)
		}
	}
	sort.Strings(out)
	return out
}

func agentPurgeClientOptions() []agentPurgeClientOption {
	out := make([]agentPurgeClientOption, 0, len(requestClientNames)+len(requestSDKUARules))
	ids := make([]string, 0, len(requestClientNames))
	for id := range requestClientNames {
		if id != "unknown" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		option := agentPurgeClientOption{ID: id, Name: requestClientNames[id], Available: true}
		if id == "claude_code" {
			option.Name = "Claude Code / 兼容标识"
			option.Note = "按请求声明识别；使用相同标识的 Qwen 等代理模式也会生效"
		}
		if id == "pi" {
			option.Note = "包括复用 pi-ai 默认 User-Agent 的其他应用"
		}
		out = append(out, option)
	}
	for _, rule := range requestSDKUARules {
		out = append(out, agentPurgeClientOption{
			ID: "sdk:" + rule.id, Name: rule.name + "（SDK）", SDK: true, Available: true,
		})
	}
	return out
}

func agentPurgePairKey(upstreamID int64, model string) string {
	return fmt.Sprintf("%d:%s", upstreamID, normalizeAgentPurgeModel(model))
}

func agentPurgeModelMatches(pairModel, model string) bool {
	if strings.TrimSpace(pairModel) == strings.TrimSpace(model) {
		return true
	}
	spec := parseRouterModelSpec(model)
	return spec.BaseModel != "" && strings.TrimSpace(pairModel) == strings.TrimSpace(spec.BaseModel)
}

func shouldApplyAgentPurge(state *agentPurgeRequestState, userSub string, up *Upstream, task *relayTask) bool {
	if state == nil || up == nil || task == nil || up.ID <= 0 || userSub == "" ||
		!state.policy.Enabled || len(state.policy.Pairs) == 0 ||
		!state.user.Enabled || task.nativeOnly || task.client == nil {
		return false
	}
	if _, ok := agentpurge.Lookup(state.user.PersonaID); !ok {
		return false
	}
	client := task.client.identity
	// Client identity is self-reported. Conflicting evidence never matches.
	// The Claude-compatible option explicitly includes its shared Qwen marker.
	if client.Conflict || (client.Ambiguous && client.ID != "claude_code") {
		return false
	}
	selectedClient := false
	for _, id := range state.user.Clients {
		if (client.ID != "unknown" && id == client.ID) || (client.ID == "unknown" && client.SDKID != "" && id == "sdk:"+client.SDKID) {
			selectedClient = true
			break
		}
	}
	if !selectedClient {
		return false
	}
	model := task.effectiveCandidateModel()
	if model == "" {
		model = task.model
	}
	for _, allowed := range state.policy.Pairs {
		if allowed.UpstreamID != up.ID || !agentPurgeModelMatches(allowed.Model, model) {
			continue
		}
		for _, selected := range state.user.Pairs {
			if selected.UpstreamID == allowed.UpstreamID &&
				normalizeAgentPurgeModel(selected.Model) == normalizeAgentPurgeModel(allowed.Model) {
				return true
			}
		}
	}
	return false
}

func currentAgentPurgePair(pair agentPurgePair) (*Upstream, bool) {
	up, err := dbGetUpstream(pair.UpstreamID)
	if err != nil || up == nil || !up.Enabled || up.Weight <= 0 || !upstreamServesOverviewModel(up, pair.Model) || up.isDecisionUpstream(up.mapModel(pair.Model)) {
		return nil, false
	}
	return up, true
}

func buildAgentPurgePairOptions() ([]agentPurgePairOption, error) {
	overview, err := buildModelOverview()
	if err != nil {
		return nil, err
	}
	out := make([]agentPurgePairOption, 0)
	for _, model := range overview.Models {
		for _, source := range model.Sources {
			if source.UpstreamID <= 0 || source.Type == "openrouter" || source.Type == "systemone" || source.Type == "cloudflare" {
				continue
			}
			out = append(out, agentPurgePairOption{
				Model: model.Model, UpstreamID: source.UpstreamID,
				UpstreamName: source.Name, UpstreamType: source.Type,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if strings.ToLower(out[i].Model) != strings.ToLower(out[j].Model) {
			return strings.ToLower(out[i].Model) < strings.ToLower(out[j].Model)
		}
		if strings.ToLower(out[i].UpstreamName) != strings.ToLower(out[j].UpstreamName) {
			return strings.ToLower(out[i].UpstreamName) < strings.ToLower(out[j].UpstreamName)
		}
		return out[i].UpstreamID < out[j].UpstreamID
	})
	return out, nil
}

func saveAgentPurgePolicy(policy agentPurgePolicy) error {
	current := loadAgentPurgePolicy()
	policy.Revision = current.Revision + 1
	policy.Pairs = normalizeAgentPurgePairs(policy.Pairs)
	encoded, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	return settingsSet(agentPurgePolicySetting, string(encoded))
}

func saveUserAgentPurgeConfig(sub string, cfg userAgentPurgeConfig) error {
	if strings.TrimSpace(sub) == "" {
		return errors.New("缺少用户身份")
	}
	cfg.Pairs = normalizeAgentPurgePairs(cfg.Pairs)
	cfg.Clients = normalizeAgentPurgeClients(cfg.Clients)
	cfg.Mode = normalizeAgentPurgeMode(cfg.Mode)
	cfg.PersonaID = agentpurge.NormalizePersonaID(cfg.PersonaID)
	if _, ok := agentpurge.Lookup(cfg.PersonaID); !ok {
		return errors.New("请选择已内置的提示词模板")
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO user_agent_purge_configs(sub,config_json,updated_at) VALUES(?,?,CURRENT_TIMESTAMP)
		ON CONFLICT(sub) DO UPDATE SET config_json=excluded.config_json,updated_at=CURRENT_TIMESTAMP`, sub, string(encoded))
	return err
}

func buildUserAgentPurgeView(sub string, ak ApiKey) (gin.H, error) {
	policy := loadAgentPurgePolicy()
	cfg := loadUserAgentPurgeConfig(sub)
	persona, personaAvailable := agentpurge.Lookup(cfg.PersonaID)
	selectedPairs := make(map[string]bool)
	for _, pair := range cfg.Pairs {
		selectedPairs[agentPurgePairKey(pair.UpstreamID, pair.Model)] = true
	}
	options := make([]agentPurgePairOption, 0, len(policy.Pairs))
	if policy.Enabled {
		for _, pair := range policy.Pairs {
			if !ak.allowsModel(pair.Model) {
				continue
			}
			up, ok := currentAgentPurgePair(pair)
			if !ok {
				continue
			}
			options = append(options, agentPurgePairOption{
				Model: pair.Model, UpstreamID: pair.UpstreamID, UpstreamName: up.Name,
				UpstreamType: up.Type, Enabled: selectedPairs[agentPurgePairKey(pair.UpstreamID, pair.Model)],
			})
		}
	}
	return gin.H{
		"enabled":           policy.Enabled,
		"user_enabled":      cfg.Enabled && personaAvailable,
		"pairs":             options,
		"clients":           agentPurgeClientOptions(),
		"selected_clients":  cfg.Clients,
		"persona_id":        cfg.PersonaID,
		"persona_available": personaAvailable,
		"personas":          agentpurge.Personas(),
		"prompt_name":       persona.Name,
		"prompt_source":     persona.Source,
		"prompt":            persona.Text,
		"mode":              normalizeAgentPurgeMode(cfg.Mode),
		"revision":          policy.Revision,
	}, nil
}

func registerAgentPurgeAdminRoutes(auth *gin.RouterGroup) {
	auth.GET("/agent-purge/license", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(agentpurge.License+"\n\n"+agentpurge.Notice))
	})
	auth.GET("/agent-purge/policy", func(c *gin.Context) {
		options, err := buildAgentPurgePairOptions()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "读取可用模型与上游失败"})
			return
		}
		policy := loadAgentPurgePolicy()
		selected := make(map[string]bool, len(policy.Pairs))
		for _, pair := range policy.Pairs {
			selected[agentPurgePairKey(pair.UpstreamID, pair.Model)] = true
		}
		for i := range options {
			options[i].Enabled = selected[agentPurgePairKey(options[i].UpstreamID, options[i].Model)]
		}
		c.JSON(http.StatusOK, gin.H{"enabled": policy.Enabled, "revision": policy.Revision, "pairs": options})
	})

	auth.PUT("/agent-purge/policy", func(c *gin.Context) {
		var req struct {
			Enabled  *bool            `json:"enabled"`
			Revision *int64           `json:"revision"`
			Pairs    []agentPurgePair `json:"pairs"`
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
		if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请提交功能开关和上游模型授权列表"})
			return
		}
		agentPurgeConfigMu.Lock()
		defer agentPurgeConfigMu.Unlock()
		current := loadAgentPurgePolicy()
		if req.Revision == nil || *req.Revision != current.Revision {
			c.JSON(http.StatusConflict, gin.H{"error": "授权已被其他管理员修改，请刷新后重试"})
			return
		}
		if err := validateAgentPurgePairs(req.Pairs); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		pairs := normalizeAgentPurgePairs(req.Pairs)
		for _, pair := range pairs {
			if _, ok := currentAgentPurgePair(pair); !ok {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("模型 %s 当前不属于上游 #%d 的可用模型", pair.Model, pair.UpstreamID)})
				return
			}
		}
		if err := saveAgentPurgePolicy(agentPurgePolicy{Enabled: *req.Enabled, Pairs: pairs}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存人设功能策略失败"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "revision": loadAgentPurgePolicy().Revision})
	})
}

func registerUserAgentPurgeRoutes(userAPI *gin.RouterGroup) {
	userAPI.GET("/agent-purge", func(c *gin.Context) {
		user := c.MustGet("xauthUser").(xauthUserRecord)
		ak := c.MustGet("xauthKey").(ApiKey)
		view, err := buildUserAgentPurgeView(user.Sub, ak)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "加载个性化提示词设置失败"})
			return
		}
		c.JSON(http.StatusOK, view)
	})

	userAPI.PUT("/agent-purge", func(c *gin.Context) {
		user := c.MustGet("xauthUser").(xauthUserRecord)
		ak := c.MustGet("xauthKey").(ApiKey)
		var req struct {
			Enabled   bool             `json:"enabled"`
			Mode      string           `json:"mode"`
			PersonaID *string          `json:"persona_id"`
			Revision  *int64           `json:"revision"`
			Pairs     []agentPurgePair `json:"pairs"`
			Clients   []string         `json:"clients"`
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "设置格式无效"})
			return
		}
		if err := validateAgentPurgePairs(req.Pairs); err != nil || len(req.Clients) > 100 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "模型与上游设置无效，或客户端选项过多"})
			return
		}
		agentPurgeConfigMu.Lock()
		defer agentPurgeConfigMu.Unlock()
		policy := loadAgentPurgePolicy()
		personaID := loadUserAgentPurgeConfig(user.Sub).PersonaID
		if req.PersonaID != nil {
			personaID = agentpurge.NormalizePersonaID(*req.PersonaID)
		}
		if _, ok := agentpurge.Lookup(personaID); !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请选择已内置的提示词模板"})
			return
		}
		if req.Revision == nil || *req.Revision != policy.Revision {
			c.JSON(http.StatusConflict, gin.H{"error": "管理员已修改授权，请刷新后重新确认"})
			return
		}
		if req.Mode != "append" && req.Mode != "replace" && req.Mode != "strict" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请选择有效的提示词处理模式"})
			return
		}
		if !policy.Enabled {
			c.JSON(http.StatusForbidden, gin.H{"error": "站点尚未开放个性化提示词"})
			return
		}
		allowed := make(map[string]agentPurgePair, len(policy.Pairs))
		for _, pair := range policy.Pairs {
			allowed[agentPurgePairKey(pair.UpstreamID, pair.Model)] = pair
		}
		pairs := normalizeAgentPurgePairs(req.Pairs)
		for _, pair := range pairs {
			key := agentPurgePairKey(pair.UpstreamID, pair.Model)
			allowedPair, ok := allowed[key]
			if !ok || !ak.allowsModel(allowedPair.Model) {
				c.JSON(http.StatusForbidden, gin.H{"error": "所选模型或上游不在管理员授权范围内"})
				return
			}
			if _, active := currentAgentPurgePair(allowedPair); !active {
				c.JSON(http.StatusForbidden, gin.H{"error": "所选上游或模型当前不可用，请刷新后重试"})
				return
			}
		}
		clients := normalizeAgentPurgeClients(req.Clients)
		clientOptions := make(map[string]bool)
		for _, option := range agentPurgeClientOptions() {
			if option.Available {
				clientOptions[option.ID] = true
			}
		}
		for _, rawClient := range req.Clients {
			if !clientOptions[strings.TrimSpace(rawClient)] {
				c.JSON(http.StatusBadRequest, gin.H{"error": "包含未支持的客户端选项"})
				return
			}
		}
		if err := saveUserAgentPurgeConfig(user.Sub, userAgentPurgeConfig{
			Enabled: req.Enabled, Mode: req.Mode, PersonaID: personaID, Revision: policy.Revision, Pairs: pairs, Clients: clients,
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存个性化提示词设置失败"})
			return
		}
		view, _ := buildUserAgentPurgeView(user.Sub, ak)
		c.JSON(http.StatusOK, view)
	})
}

func validateAgentPurgePairs(pairs []agentPurgePair) error {
	if len(pairs) > 20000 {
		return errors.New("模型与上游组合过多")
	}
	for _, pair := range pairs {
		if pair.UpstreamID <= 0 || strings.TrimSpace(pair.Model) == "" || len(pair.Model) > 256 || strings.ContainsAny(pair.Model, "\r\n\t\x00") {
			return errors.New("请选择有效的站点模型与上游组合")
		}
	}
	return nil
}
