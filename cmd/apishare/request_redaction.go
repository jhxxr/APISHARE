package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"apishare/internal/requestredact"
	"github.com/gin-gonic/gin"
)

const (
	requestRedactionPolicyKey  = "request_redaction_policy"
	requestRedactionUserHeader = "X-APIShare-Request"
)

type requestRedactionPolicy struct {
	SchemaVersion   int                              `json:"schema_version"`
	Revision        int64                            `json:"revision"`
	RulesVersion    string                           `json:"rules_version"`
	Mode            string                           `json:"mode"`
	Coverage        string                           `json:"coverage"`
	Scope           requestRedactionScope            `json:"scope"`
	Categories      []string                         `json:"categories"`
	Replacements    map[string]string                `json:"replacements"`
	ToolArguments   string                           `json:"tool_arguments"`
	Headers         requestRedactionHeaders          `json:"headers"`
	Rules           []requestredact.Rule             `json:"rules"`
	ChannelOverlays []requestRedactionChannelOverlay `json:"channel_overlays"`
}

type requestRedactionScope struct {
	KeyIDs []int64  `json:"key_ids"`
	Models []string `json:"models"`
}

type requestRedactionHeaders struct {
	StripForwardingIdentity bool `json:"strip_forwarding_identity"`
	ScanCustomText          bool `json:"scan_custom_text"`
}

type requestRedactionChannelOverlay struct {
	UpstreamID int64                `json:"upstream_id"`
	Rules      []requestredact.Rule `json:"rules"`
}

type userRequestRedactionConfig struct {
	Sub      string
	Enabled  bool
	Revision int64
}

type requestRedactionSnapshot struct {
	Source       string
	Revision     int64
	RulesVersion string
	Config       requestredact.Config
	Compiled     *requestredact.Compiled
	Headers      requestRedactionHeaders
	Overlays     map[int64]*requestredact.Compiled
	Receipt      requestRedactionReceipt
}

type requestRedactionReceipt struct {
	PolicySource   string                  `json:"policy_source"`
	PolicyRevision int64                   `json:"policy_revision,omitempty"`
	UserRevision   int64                   `json:"user_revision,omitempty"`
	RulesVersion   string                  `json:"rules_version"`
	Mode           string                  `json:"mode"`
	Coverage       string                  `json:"coverage"`
	Status         string                  `json:"status"`
	Stages         []requestRedactionStage `json:"stages,omitempty"`
	Unchecked      []string                `json:"unchecked,omitempty"`
}

type requestRedactionStage struct {
	Stage    string                  `json:"stage"`
	Findings []requestredact.Finding `json:"findings,omitempty"`
	Status   string                  `json:"status,omitempty"`
}

type requestRedactionState struct {
	Snapshot *requestRedactionSnapshot
	Receipt  requestRedactionReceipt
}

type requestRedactionError struct {
	status  int
	code    string
	message string
}

func (e *requestRedactionError) Error() string { return e.message }

var requestRedactionConfigMu sync.Mutex

func defaultRequestRedactionPolicy() requestRedactionPolicy {
	cfg := requestredact.DefaultAdminConfig()
	return requestRedactionPolicy{
		SchemaVersion: 1, RulesVersion: "builtin-v1", Mode: cfg.Mode, Coverage: cfg.Coverage,
		Scope:      requestRedactionScope{KeyIDs: []int64{}, Models: []string{}},
		Categories: cfg.Categories, Replacements: cfg.Replacements, ToolArguments: cfg.ToolArguments,
		Headers: requestRedactionHeaders{StripForwardingIdentity: true, ScanCustomText: true},
		Rules:   []requestredact.Rule{}, ChannelOverlays: []requestRedactionChannelOverlay{},
	}
}

func policyConfig(policy requestRedactionPolicy) requestredact.Config {
	return requestredact.Config{
		Mode: policy.Mode, Coverage: policy.Coverage, Categories: append([]string(nil), policy.Categories...),
		Replacements: policy.Replacements, Rules: append([]requestredact.Rule(nil), policy.Rules...), ToolArguments: policy.ToolArguments,
	}
}

func loadRequestRedactionPolicy() (requestRedactionPolicy, error) {
	policy := defaultRequestRedactionPolicy()
	raw, err := settingsGet(requestRedactionPolicyKey)
	if err != nil || raw == "" {
		return policy, err
	}
	var stored requestRedactionPolicy
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return policy, fmt.Errorf("saved request redaction policy is invalid")
	}
	if stored.SchemaVersion != 1 || stored.Revision < 0 || stored.RulesVersion == "" {
		return policy, fmt.Errorf("saved request redaction policy has an unsupported version")
	}
	if stored.Scope.KeyIDs == nil {
		stored.Scope.KeyIDs = []int64{}
	}
	if stored.Scope.Models == nil {
		stored.Scope.Models = []string{}
	}
	if stored.Categories == nil {
		stored.Categories = []string{}
	}
	if stored.Rules == nil {
		stored.Rules = []requestredact.Rule{}
	}
	if stored.ChannelOverlays == nil {
		stored.ChannelOverlays = []requestRedactionChannelOverlay{}
	}
	if _, err := requestredact.Compile(policyConfig(stored)); err != nil {
		return policy, fmt.Errorf("saved request redaction rules are invalid")
	}
	for _, overlay := range stored.ChannelOverlays {
		cfg := policyConfig(stored)
		cfg.Rules = append(cfg.Rules, overlay.Rules...)
		if _, err := requestredact.Compile(cfg); err != nil {
			return policy, fmt.Errorf("saved channel redaction rules are invalid")
		}
	}
	return stored, nil
}

func saveRequestRedactionPolicy(policy requestRedactionPolicy, expectedRevision int64) (requestRedactionPolicy, error) {
	requestRedactionConfigMu.Lock()
	defer requestRedactionConfigMu.Unlock()
	current, err := loadRequestRedactionPolicy()
	if err != nil {
		return current, err
	}
	if current.Revision != expectedRevision {
		return current, errRequestRedactionRevisionConflict
	}
	policy.SchemaVersion = 1
	policy.Revision = current.Revision + 1
	policy.RulesVersion = "builtin-v1"
	if policy.Scope.KeyIDs == nil {
		policy.Scope.KeyIDs = []int64{}
	}
	if policy.Scope.Models == nil {
		policy.Scope.Models = []string{}
	}
	if policy.Categories == nil {
		policy.Categories = []string{}
	}
	if policy.Rules == nil {
		policy.Rules = []requestredact.Rule{}
	}
	if policy.ChannelOverlays == nil {
		policy.ChannelOverlays = []requestRedactionChannelOverlay{}
	}
	if err := validateRequestRedactionPolicy(policy); err != nil {
		return current, err
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return current, err
	}
	if err := settingsSet(requestRedactionPolicyKey, string(encoded)); err != nil {
		return current, err
	}
	return policy, nil
}

var errRequestRedactionRevisionConflict = errors.New("request redaction settings changed; reload and try again")

func validateRequestRedactionPolicy(policy requestRedactionPolicy) error {
	if policy.SchemaVersion != 1 || policy.RulesVersion != "builtin-v1" || policy.Revision < 0 {
		return errors.New("invalid redaction policy version")
	}
	if len(policy.Scope.KeyIDs) > 20000 || len(policy.Scope.Models) > 20000 || len(policy.ChannelOverlays) > 20000 {
		return errors.New("redaction policy scope is too large")
	}
	seenKeys := map[int64]bool{}
	for _, id := range policy.Scope.KeyIDs {
		if id <= 0 || seenKeys[id] {
			return errors.New("invalid or duplicate API key ID in redaction scope")
		}
		seenKeys[id] = true
	}
	seenModels := map[string]bool{}
	for _, model := range policy.Scope.Models {
		model = strings.TrimSpace(model)
		if model == "" || len(model) > 256 || strings.ContainsAny(model, "\r\n\t\x00") || seenModels[strings.ToLower(model)] {
			return errors.New("invalid or duplicate model in redaction scope")
		}
		seenModels[strings.ToLower(model)] = true
	}
	if _, err := requestredact.Compile(policyConfig(policy)); err != nil {
		return err
	}
	seenUpstreams := map[int64]bool{}
	for _, overlay := range policy.ChannelOverlays {
		if overlay.UpstreamID <= 0 || seenUpstreams[overlay.UpstreamID] || len(overlay.Rules) == 0 {
			return errors.New("invalid or duplicate channel redaction overlay")
		}
		seenUpstreams[overlay.UpstreamID] = true
		cfg := policyConfig(policy)
		cfg.Rules = append(cfg.Rules, overlay.Rules...)
		if _, err := requestredact.Compile(cfg); err != nil {
			return err
		}
	}
	return nil
}

func migrateRequestRedactionSchema() error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS user_request_redaction_configs (
		sub TEXT PRIMARY KEY,
		enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
		revision INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`)
	return err
}

func loadUserRequestRedactionConfig(sub string) (userRequestRedactionConfig, error) {
	config := userRequestRedactionConfig{Sub: strings.TrimSpace(sub)}
	if config.Sub == "" {
		return config, errors.New("missing XAuth identity")
	}
	var enabled int
	err := db.QueryRow(`SELECT enabled,revision FROM user_request_redaction_configs WHERE sub=?`, config.Sub).Scan(&enabled, &config.Revision)
	if errors.Is(err, errNoRows()) {
		return config, nil
	}
	if err != nil {
		return config, err
	}
	if enabled != 0 && enabled != 1 {
		return config, errors.New("invalid personal redaction setting")
	}
	config.Enabled = enabled == 1
	return config, nil
}

func saveUserRequestRedactionConfig(sub string, enabled bool, revision int64) (userRequestRedactionConfig, error) {
	config, err := loadUserRequestRedactionConfig(sub)
	if err != nil {
		return config, err
	}
	if config.Revision != revision {
		return config, errRequestRedactionRevisionConflict
	}
	if enabled {
		if _, err := requestredact.Compile(requestredact.PersonalConfig()); err != nil {
			return config, errors.New("personal redaction rules are unavailable")
		}
	}
	config.Enabled = enabled
	config.Revision++
	result, err := db.Exec(`INSERT INTO user_request_redaction_configs(sub,enabled,revision,updated_at) VALUES(?,?,?,CURRENT_TIMESTAMP)
		ON CONFLICT(sub) DO UPDATE SET enabled=excluded.enabled,revision=excluded.revision,updated_at=CURRENT_TIMESTAMP
		WHERE user_request_redaction_configs.revision=?`, config.Sub, boolToInt(enabled), config.Revision, revision)
	if err != nil {
		return userRequestRedactionConfig{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return userRequestRedactionConfig{}, err
	}
	if changed != 1 {
		return userRequestRedactionConfig{}, errRequestRedactionRevisionConflict
	}
	return config, nil
}

func errNoRows() error { return sql.ErrNoRows }

func resolveRequestRedaction(ak ApiKey, model string) (*requestRedactionSnapshot, error) {
	if ak.XAuthUser != nil && strings.TrimSpace(ak.XAuthUser.Sub) == "" {
		return nil, errors.New("invalid authenticated personal identity")
	}
	if sub := userSubForKey(ak); sub != "" {
		personal, err := loadUserRequestRedactionConfig(sub)
		if err != nil {
			return nil, fmt.Errorf("unable to load personal redaction settings")
		}
		if !personal.Enabled {
			cfg := requestredact.PersonalConfig()
			cfg.Mode = requestredact.ModeOff
			return &requestRedactionSnapshot{Source: "user", Revision: personal.Revision, RulesVersion: "personal-builtin-v1", Config: cfg,
				Headers: requestRedactionHeaders{StripForwardingIdentity: true, ScanCustomText: true}}, nil
		}
		cfg := requestredact.PersonalConfig()
		compiled, err := requestredact.Compile(cfg)
		if err != nil {
			return nil, fmt.Errorf("personal redaction rules are unavailable")
		}
		return &requestRedactionSnapshot{Source: "user", Revision: personal.Revision, RulesVersion: "personal-builtin-v1", Config: cfg, Compiled: compiled,
			Headers: requestRedactionHeaders{StripForwardingIdentity: true, ScanCustomText: true}}, nil
	}
	policy, err := loadRequestRedactionPolicy()
	if err != nil {
		return nil, errors.New("administrator request redaction policy is unavailable")
	}
	if policy.Mode == requestredact.ModeOff || !requestRedactionScopeMatches(policy.Scope, ak.ID, model) {
		cfg := policyConfig(policy)
		cfg.Mode = requestredact.ModeOff
		return &requestRedactionSnapshot{Source: "global", Revision: policy.Revision, RulesVersion: policy.RulesVersion, Config: cfg, Headers: policy.Headers}, nil
	}
	return compileRedactionSnapshot("global", policy.Revision, policy.RulesVersion, policyConfig(policy), policy.Headers, policy.ChannelOverlays)
}

func compileRedactionSnapshot(source string, revision int64, rulesVersion string, cfg requestredact.Config, headers requestRedactionHeaders, overlays []requestRedactionChannelOverlay) (*requestRedactionSnapshot, error) {
	compiled, err := requestredact.Compile(cfg)
	if err != nil {
		return nil, err
	}
	snapshot := &requestRedactionSnapshot{Source: source, Revision: revision, RulesVersion: rulesVersion, Config: cfg, Compiled: compiled, Headers: headers}
	if len(overlays) > 0 {
		snapshot.Overlays = make(map[int64]*requestredact.Compiled, len(overlays))
		for _, overlay := range overlays {
			overlayConfig := cfg
			overlayConfig.Rules = append(append([]requestredact.Rule(nil), cfg.Rules...), overlay.Rules...)
			compiledOverlay, err := requestredact.Compile(overlayConfig)
			if err != nil {
				return nil, errors.New("channel redaction rules are unavailable")
			}
			snapshot.Overlays[overlay.UpstreamID] = compiledOverlay
		}
	}
	return snapshot, nil
}

func requestRedactionScopeMatches(scope requestRedactionScope, keyID int64, model string) bool {
	keyMatch, modelMatch := len(scope.KeyIDs) == 0, len(scope.Models) == 0
	for _, id := range scope.KeyIDs {
		if id == keyID {
			keyMatch = true
			break
		}
	}
	for _, candidate := range scope.Models {
		if strings.EqualFold(strings.TrimSpace(candidate), strings.TrimSpace(model)) {
			modelMatch = true
			break
		}
	}
	return keyMatch && modelMatch
}

func (snapshot *requestRedactionSnapshot) process(format clientFormat, body []byte, upstreamID int64) (requestredact.Result, error) {
	if snapshot == nil {
		return requestredact.Result{Body: body}, nil
	}
	compiled := snapshot.Compiled
	if snapshot.Source == "global" {
		if overlay := snapshot.Overlays[upstreamID]; overlay != nil {
			compiled = overlay
		}
	}
	if snapshot.Config.Mode == requestredact.ModeOff {
		return requestredact.Result{Body: body}, nil
	}
	result, err := compiled.Process(string(format), body)
	if err != nil {
		var typed *requestredact.Error
		if !errors.As(err, &typed) {
			err = &requestredact.Error{Kind: "unavailable"}
		}
	}
	return result, err
}

func (snapshot *requestRedactionSnapshot) makeReceipt() requestRedactionReceipt {
	if snapshot == nil {
		return requestRedactionReceipt{Status: "disabled"}
	}
	receipt := requestRedactionReceipt{
		PolicySource: snapshot.Source, RulesVersion: snapshot.RulesVersion,
		Mode: snapshot.Config.Mode, Coverage: snapshot.Config.Coverage, Status: "checked",
	}
	if snapshot.Source == "user" {
		receipt.UserRevision = snapshot.Revision
	} else {
		receipt.PolicyRevision = snapshot.Revision
	}
	if snapshot.Config.Mode == requestredact.ModeOff {
		receipt.Status = "disabled"
	}
	return receipt
}

func (snapshot *requestRedactionSnapshot) processStage(format clientFormat, body []byte, upstreamID int64, stage string) (requestredact.Result, requestRedactionReceipt, error) {
	result, err := snapshot.process(format, body, upstreamID)
	receipt := snapshot.Receipt
	if receipt.PolicySource == "" {
		receipt = snapshot.makeReceipt()
	}
	receipt.Stages = append([]requestRedactionStage(nil), receipt.Stages...)
	receipt.Unchecked = append([]string(nil), receipt.Unchecked...)
	if err != nil {
		receipt.Status = "rejected"
		receipt.Stages = append(receipt.Stages, requestRedactionStage{Stage: stage, Status: "rejected"})
		return result, receipt, err
	}
	if receipt.Status != "disabled" {
		stageReceipt := requestRedactionStage{Stage: stage, Findings: result.Findings, Status: "checked"}
		if result.Changed {
			stageReceipt.Status = "redacted"
		}
		receipt.Stages = append(receipt.Stages, stageReceipt)
		if snapshot.Config.Mode == requestredact.ModeObserve {
			receipt.Status = "observed"
		}
		if result.Changed {
			receipt.Status = "redacted"
		}
		for _, unchecked := range result.Unchecked {
			receipt.Unchecked = appendUnique(receipt.Unchecked, unchecked)
		}
		if len(receipt.Unchecked) > 0 && receipt.Status == "checked" {
			receipt.Status = "unchecked"
		}
	}
	return result, receipt, nil
}

func appendUnique(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}

func failureRedactionHeaders(headers http.Header) http.Header {
	out := make(http.Header, len(headers))
	for name := range headers {
		switch strings.ToLower(name) {
		case "accept", "openai-beta", "anthropic-version", "anthropic-beta", "user-agent", "content-type":
			out[name] = append([]string(nil), headers.Values(name)...)
		}
	}
	return out
}

func stripForwardingIdentityHeaders(headers http.Header) {
	for name := range headers {
		switch strings.ToLower(name) {
		case "cookie", "forwarded", "x-forwarded-for", "x-real-ip", "true-client-ip", "cf-connecting-ip":
			headers.Del(name)
		}
	}
}

// Reject credential copies based on their origin, not on detector coverage.
func checkRedactionHeaders(snapshot *requestRedactionSnapshot, up *Upstream, incoming, headers http.Header) error {
	if snapshot == nil || snapshot.Config.Mode != requestredact.ModeEnforce {
		return nil
	}
	rules, err := parseHeaderOverrides(up.HeaderOverrides)
	if err != nil {
		return &requestredact.Error{Kind: "unavailable"}
	}
	for _, rule := range rules {
		if rule.copy && credentialHeader(rule.name) {
			return &requestredact.Error{Kind: "header"}
		}
		for _, match := range headerVariable.FindAllStringSubmatch(rule.value, -1) {
			if credentialHeader(match[2]) {
				return &requestredact.Error{Kind: "header"}
			}
		}
	}
	for name, values := range incoming {
		if !credentialHeader(name) {
			continue
		}
		for _, secret := range values {
			if secret == "" {
				continue
			}
			for _, outgoing := range headers {
				for _, value := range outgoing {
					if strings.Contains(value, secret) {
						return &requestredact.Error{Kind: "header"}
					}
				}
			}
		}
	}
	if snapshot.Headers.StripForwardingIdentity {
		stripForwardingIdentityHeaders(headers)
	}
	return scanHeaderValues(snapshot, headers)
}

func scanHeaderValues(snapshot *requestRedactionSnapshot, headers http.Header) error {
	if snapshot == nil || snapshot.Config.Mode != requestredact.ModeEnforce || !snapshot.Headers.ScanCustomText {
		return nil
	}
	for name, values := range headers {
		if credentialHeader(name) || transportHeader(name) || requestRedactionSafeHeader(name) {
			continue
		}
		for _, value := range values {
			updated, findings, err := requestredact.ProcessText(value, snapshot.Config)
			if err != nil {
				return err
			}
			if len(findings) > 0 && updated != value {
				return &requestredact.Error{Kind: "header"}
			}
		}
	}
	return nil
}

func requestRedactionSafeHeader(name string) bool {
	switch strings.ToLower(name) {
	case "accept", "content-type", "user-agent", "anthropic-version", "anthropic-beta", "openai-beta", "accept-encoding":
		return true
	default:
		return false
	}
}

func recordRequestRedactionRejection(c *gin.Context, ak ApiKey, format clientFormat, model string, snapshot *requestRedactionSnapshot, err error) {
	status, _ := requestRedactionErrorStatus(err)
	receipt := snapshot.makeReceipt()
	receipt.Status = "rejected"
	receipt.Stages = []requestRedactionStage{{Stage: "ingress", Status: "rejected"}}
	state := requestClientFor(c)
	state.recorded.Store(true)
	dbInsertLog(&CallLog{Client: state.identity, KeyID: ak.ID, KeyName: ak.Name, Model: model, Endpoint: string(format),
		Status: status, Error: "request rejected by redaction policy", Billing: &billingDetails{Source: format, RequestRedaction: &receipt}})
}

func requestRedactionErrorStatus(err error) (int, string) {
	var problem *requestredact.Error
	if errors.As(err, &problem) {
		switch problem.Kind {
		case "unsupported":
			return 400, "request_redaction_unsupported"
		case "limit":
			return 413, "request_redaction_limit_exceeded"
		case "unavailable":
			return 503, "request_redaction_unavailable"
		default:
			return 400, "request_redaction_rejected"
		}
	}
	return 503, "request_redaction_unavailable"
}

func handleRequestRedactionError(c *gin.Context, format clientFormat, err error) {
	status, code := requestRedactionErrorStatus(err)
	message := "request rejected by redaction policy; review content and coverage settings"
	c.Header("X-APISHARE-Error-Code", code)
	if format == FormatAnthropic && messagesState(c) != nil && c.Writer.Written() {
		writeMessagesStreamError(c, message)
		return
	}
	switch format {
	case FormatAnthropic:
		c.JSON(status, gin.H{"type": "error", "error": gin.H{"type": "invalid_request_error", "code": code, "message": message}})
	case FormatGemini:
		c.JSON(status, gin.H{"error": gin.H{"code": status, "message": message, "status": "INVALID_ARGUMENT", "details": []gin.H{{"reason": code}}}})
	default:
		c.JSON(status, gin.H{"error": gin.H{"type": "invalid_request_error", "code": code, "message": message}})
	}
}

func decodeRequestRedactionBody(body io.Reader, target any) error {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid trailing JSON")
	}
	return nil
}

func publicRequestRedactionView(config userRequestRedactionConfig) gin.H {
	personal := requestredact.PersonalConfig()
	mode := requestredact.ModeOff
	if config.Enabled {
		mode = personal.Mode
	}
	return gin.H{
		"enabled": config.Enabled, "revision": config.Revision,
		"mode": mode, "coverage": personal.Coverage, "rules_version": "personal-builtin-v1",
		"categories": personal.Categories, "overrides": personal.Replacements,
		"scope": "本人密钥的所有客户端与有权限模型", "unchecked": []string{"附件、签名内容及远端历史可能未检查"},
	}
}

func requestRedactionDryRunView(result requestredact.Result) gin.H {
	view := gin.H{"changed": result.Changed, "matched": result.Matched, "findings": result.Findings, "unchecked": result.Unchecked}
	if len(result.Body) > 0 && json.Valid(result.Body) {
		view["body"] = json.RawMessage(result.Body)
	}
	return view
}

func registerUserRequestRedactionRoutes(userAPI *gin.RouterGroup) {
	userAPI.GET("/request-processing", func(c *gin.Context) {
		user := c.MustGet("xauthUser").(xauthUserRecord)
		config, err := loadUserRequestRedactionConfig(user.Sub)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "无法加载个人脱敏设置"})
			return
		}
		c.JSON(http.StatusOK, publicRequestRedactionView(config))
	})
	userAPI.PUT("/request-processing/redaction", func(c *gin.Context) {
		user := c.MustGet("xauthUser").(xauthUserRecord)
		var request struct {
			Enabled  *bool  `json:"enabled"`
			Revision *int64 `json:"revision"`
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
		if decodeRequestRedactionBody(c.Request.Body, &request) != nil || request.Enabled == nil || request.Revision == nil || *request.Revision < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请提交脱敏开关与个人设置版本"})
			return
		}
		config, err := saveUserRequestRedactionConfig(user.Sub, *request.Enabled, *request.Revision)
		if errors.Is(err, errRequestRedactionRevisionConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": "个人脱敏设置已在其他页面修改，请刷新后重试"})
			return
		}
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "保存个人脱敏设置失败"})
			return
		}
		c.JSON(http.StatusOK, publicRequestRedactionView(config))
	})
	userAPI.POST("/request-processing/redaction/dry-run", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		var request struct {
			Format string          `json:"format"`
			Body   json.RawMessage `json:"body"`
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256<<10)
		if decodeRequestRedactionBody(c.Request.Body, &request) != nil || len(request.Body) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "试运行请求格式无效"})
			return
		}
		result, err := requestredact.Process(request.Format, request.Body, requestredact.PersonalConfig())
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "样例未通过脱敏检查"})
			return
		}
		c.JSON(http.StatusOK, requestRedactionDryRunView(result))
	})
}

func registerAdminRequestRedactionRoutes(auth *gin.RouterGroup) {
	auth.GET("/request-processing/redaction", func(c *gin.Context) {
		policy, err := loadRequestRedactionPolicy()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "管理员脱敏策略损坏，请恢复有效配置后重试"})
			return
		}
		keys, err := dbListKeys()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "无法读取 API Key 范围"})
			return
		}
		ordinary := make([]gin.H, 0, len(keys))
		for _, key := range keys {
			if key.XAuthUser == nil {
				ordinary = append(ordinary, gin.H{"id": key.ID, "name": key.Name})
			}
		}
		// Preserve the saved scope even if keys were deleted or became personal.
		// Dropping the final ID would turn a restricted policy into all keys.
		c.JSON(http.StatusOK, gin.H{"policy": policy, "ordinary_keys": ordinary, "revision": policy.Revision, "categories": requestredact.Categories()})
	})
	auth.PUT("/request-processing/redaction", func(c *gin.Context) {
		var policy requestRedactionPolicy
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
		if decodeRequestRedactionBody(c.Request.Body, &policy) != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "脱敏策略格式无效"})
			return
		}
		if policy.Revision < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "脱敏策略版本无效"})
			return
		}
		saved, err := saveRequestRedactionPolicy(policy, policy.Revision)
		if errors.Is(err, errRequestRedactionRevisionConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": "脱敏策略已被修改，请刷新并重新确认"})
			return
		}
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "revision": saved.Revision})
	})
	auth.GET("/request-processing/redaction/catalog", func(c *gin.Context) {
		personal := requestredact.PersonalConfig()
		c.JSON(http.StatusOK, gin.H{"categories": requestredact.Categories(), "personal_categories": personal.Categories, "rules_version": "builtin-v1", "formats": []string{"openai-chat", "openai-responses", "anthropic", "gemini"}})
	})
	auth.POST("/request-processing/redaction/dry-run", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		var request struct {
			Format string                  `json:"format"`
			Body   json.RawMessage         `json:"body"`
			Policy *requestRedactionPolicy `json:"policy"`
			KeyID  int64                   `json:"key_id"`
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256<<10)
		if decodeRequestRedactionBody(c.Request.Body, &request) != nil || len(request.Body) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "试运行请求格式无效"})
			return
		}
		var policy requestRedactionPolicy
		if request.Policy != nil {
			policy = *request.Policy
		}
		if request.KeyID > 0 {
			key, keyErr := dbGetKeyByID(request.KeyID)
			if keyErr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "指定的 API Key 不存在"})
				return
			}
			if key.XAuthUser != nil {
				personal, personalErr := loadUserRequestRedactionConfig(key.XAuthUser.Sub)
				if personalErr != nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "无法读取该用户的个人设置"})
					return
				}
				compiledPolicy := requestredact.PersonalConfig()
				if !personal.Enabled {
					compiledPolicy.Mode = requestredact.ModeOff
				}
				result, runErr := requestredact.Process(request.Format, request.Body, compiledPolicy)
				if runErr != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": "样例未通过个人脱敏检查"})
					return
				}
				c.JSON(http.StatusOK, gin.H{"policy_source": "user", "enabled": personal.Enabled, "result": requestRedactionDryRunView(result)})
				return
			}
			if request.Policy == nil {
				var loadErr error
				policy, loadErr = loadRequestRedactionPolicy()
				if loadErr != nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "管理员脱敏策略损坏"})
					return
				}
			}
			var sample struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(request.Body, &sample)
			if !requestRedactionScopeMatches(policy.Scope, key.ID, sample.Model) && policy.Mode != requestredact.ModeOff {
				c.JSON(http.StatusBadRequest, gin.H{"error": "所选 Key 不在全局策略生效范围内"})
				return
			}
		}
		if request.Policy == nil {
			var loadErr error
			policy, loadErr = loadRequestRedactionPolicy()
			if loadErr != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "管理员脱敏策略损坏"})
				return
			}
		}
		if err := validateRequestRedactionPolicy(policy); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		result, err := requestredact.Process(request.Format, request.Body, policyConfig(policy))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "样例未通过脱敏检查"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"policy_source": "global", "enabled": policy.Mode != requestredact.ModeOff, "result": requestRedactionDryRunView(result)})
	})
}

func normalizeRequestRedactionKeyScope(ids []int64, keys []ApiKey) []int64 {
	allowed := map[int64]bool{}
	for _, key := range keys {
		if key.XAuthUser == nil {
			allowed[key.ID] = true
		}
	}
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if allowed[id] {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
