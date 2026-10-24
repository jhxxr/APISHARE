package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OpenCode's official Identifier.descending() uses six timestamp bytes and
// fourteen Base62 characters. Keep actual client IDs intact; map foreign IDs
// once so subsequent turns and retries keep the same upstream conversation.
var openCodeSessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
var openCodeProjectPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var openCodeDescendingPattern = regexp.MustCompile(`^ses_[0-9a-f]{9}f[c-f][0-9a-f][0-9A-Za-z]{14}$`)
var openCodeMessagePattern = regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
var claudeCodeSessionPattern = regexp.MustCompile(`_session_([0-9A-Fa-f-]{36})$`)

const openCodeSessionTTL = 30 * 24 * time.Hour
const openCodeMessageTTL = 6 * time.Hour
const openCodeMessageLimit = 8192

// Identifier.descending stores only the low 36 timestamp bits. Compare on
// that ring rather than interpreting the truncated milliseconds as a 1970
// timestamp. Persisted and incoming IDs must also stay in a recent window.
func openCodeSessionUsable(session string, now time.Time) bool {
	if !openCodeDescendingPattern.MatchString(session) {
		return false
	}
	word, err := strconv.ParseUint(session[4:16], 16, 48)
	if err != nil {
		return false
	}
	const cycle = int64(1) << 36
	tick := int64((^word & 0xffffffffffff) >> 12)
	delta := ((tick - (now.UnixMilli() & (cycle - 1)) + cycle/2) & (cycle - 1)) - cycle/2
	return delta >= -openCodeSessionTTL.Milliseconds() && delta <= (24*time.Hour).Milliseconds()
}

func newOpenCodeSessionID() (string, error) {
	var nonce [14]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("could not generate OpenCode session ID")
	}
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	for i := range nonce {
		nonce[i] = alphabet[int(nonce[i])%len(alphabet)]
	}
	value := ^(uint64(time.Now().UnixMilli())<<12 | 1)
	return fmt.Sprintf("ses_%012x%s", value&0xffffffffffff, nonce[:]), nil
}

// OpenCode's Identifier.ascending() shares one counter per millisecond across
// every id the client mints.
var openCodeAscending = struct {
	sync.Mutex
	last    int64
	counter uint64
}{}

func newOpenCodeMessageID(now time.Time) string {
	ms := now.UnixMilli()
	openCodeAscending.Lock()
	if openCodeAscending.last != ms {
		openCodeAscending.last, openCodeAscending.counter = ms, 0
	}
	openCodeAscending.counter++
	value := uint64(ms)<<12 | openCodeAscending.counter
	openCodeAscending.Unlock()
	var nonce [14]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ""
	}
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	for i := range nonce {
		nonce[i] = alphabet[int(nonce[i])%len(alphabet)]
	}
	return fmt.Sprintf("msg_%012x%s", value&0xffffffffffff, nonce[:])
}

func explicitSessionID(header http.Header, bodySession string) string {
	for _, name := range []string{
		"x-opencode-session", "x-opencode-session-id", "x-session-affinity", "x-session-id",
		// Codex reports its conversation id under underscore names.
		"session_id", "conversation-id", "conversation_id",
	} {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			return value
		}
	}
	return strings.TrimSpace(bodySession)
}

// claudeCodeSessionID extracts the per-conversation UUID Claude Code embeds in
// metadata.user_id ("user_<hash>_account_<uuid>_session_<uuid>"), or the
// session_id field when user_id is a JSON object.
func claudeCodeSessionID(userID string) string {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ""
	}
	if match := claudeCodeSessionPattern.FindStringSubmatch(userID); match != nil {
		return match[1]
	}
	var value map[string]any
	if json.Unmarshal([]byte(userID), &value) == nil {
		if session := strings.TrimSpace(str(value["session_id"])); session != "" {
			return session
		}
	}
	return ""
}

// ---------- v1 x-opencode-request pinning ----------

type openCodeMessageEntry struct {
	id   string
	seen time.Time
}

// The v1 client repeats the user message id on every request of a turn. Pin
// one id per (session, user turn), plus the latest id of each session for
// stateful continuations that repeat no turn.
var openCodeMessages = struct {
	sync.Mutex
	turns  map[string]openCodeMessageEntry
	latest map[string]openCodeMessageEntry
}{turns: map[string]openCodeMessageEntry{}, latest: map[string]openCodeMessageEntry{}}

// openCodeUserTurn identifies the last user-typed turn. Tool results on the
// Anthropic wire arrive as user messages, so they do not start a new turn.
func openCodeUserTurn(cr *canonReq) string {
	if cr == nil {
		return ""
	}
	count := 0
	last := ""
	for _, message := range cr.Messages {
		if message.Role != "user" || message.ToolID != "" {
			continue
		}
		count++
		last = message.Text
	}
	if count == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(last))
	return fmt.Sprintf("%d:%x", count, sum[:8])
}

func openCodeMessageForTask(task *relayTask, session string, incoming http.Header) string {
	if task == nil {
		return ""
	}
	if value := strings.TrimSpace(incoming.Get("x-opencode-request")); openCodeMessagePattern.MatchString(value) {
		task.openCodeRequest = value
		return value
	}
	if openCodeMessagePattern.MatchString(task.openCodeRequest) {
		return task.openCodeRequest
	}
	turn := openCodeUserTurn(task.canon)
	now := time.Now()
	openCodeMessages.Lock()
	key := session + "\x00" + turn
	entry, exists := openCodeMessages.turns[key]
	if turn == "" {
		entry, exists = openCodeMessages.latest[session]
	}
	if !exists || now.Sub(entry.seen) > openCodeMessageTTL {
		entry = openCodeMessageEntry{id: newOpenCodeMessageID(now)}
	}
	entry.seen = now
	if turn != "" {
		openCodeMessages.turns[key] = entry
	}
	openCodeMessages.latest[session] = entry
	if len(openCodeMessages.turns) > openCodeMessageLimit || len(openCodeMessages.latest) > openCodeMessageLimit {
		for key, item := range openCodeMessages.turns {
			if now.Sub(item.seen) > openCodeMessageTTL {
				delete(openCodeMessages.turns, key)
			}
		}
		for key, item := range openCodeMessages.latest {
			if now.Sub(item.seen) > openCodeMessageTTL {
				delete(openCodeMessages.latest, key)
			}
		}
	}
	openCodeMessages.Unlock()
	task.openCodeRequest = entry.id
	return entry.id
}

func openCodeSessionForTask(task *relayTask, incoming http.Header) (string, error) {
	if openCodeSessionUsable(task.openCodeSession, time.Now()) {
		return task.openCodeSession, nil
	}
	task.openCodeSession = ""
	cr := task.canon
	if cr == nil {
		cr = &canonReq{Model: task.model}
	}
	signal := explicitSessionID(incoming, cr.SessionID)
	if openCodeSessionUsable(signal, time.Now()) {
		task.openCodeSession = signal
		return signal, nil
	}
	newID, err := newOpenCodeSessionID()
	if err != nil {
		return "", err
	}
	if signal == "" && strings.TrimSpace(cr.InputText) == "" {
		task.openCodeSession = newID
		return newID, nil
	}
	// The mapping is local to the gateway key, and does not depend on the
	// selected upstream, mapped model, or wire format used by a retry.
	scope := scopedSessionKey(task.apiKeyID, sessionKeyFromCanon("", incoming, cr))
	sum := sha256.Sum256([]byte(scope))
	if db == nil {
		return "", fmt.Errorf("OpenCode session store is unavailable")
	}
	now := time.Now()
	scopeHash := hex.EncodeToString(sum[:])
	var session string
	err = db.QueryRow(`INSERT INTO opencode_sessions(scope,session_id,expires_at) VALUES(?,?,?)
		ON CONFLICT(scope) DO UPDATE SET
		 session_id=CASE WHEN opencode_sessions.expires_at<=? THEN excluded.session_id ELSE opencode_sessions.session_id END,
		 expires_at=excluded.expires_at RETURNING session_id`, scopeHash, newID, now.Add(openCodeSessionTTL).Unix(), now.Unix()).Scan(&session)
	if err != nil {
		return "", fmt.Errorf("could not persist OpenCode session ID")
	}
	if !openCodeSessionUsable(session, time.Now()) {
		// Older releases persisted foreign encodings for up to 30 days. Repair
		// those mappings once, keeping a concurrent repair's already valid ID.
		err = db.QueryRow(`UPDATE opencode_sessions
			SET session_id=CASE WHEN session_id=? THEN ? ELSE session_id END
			WHERE scope=? RETURNING session_id`, session, newID, scopeHash).Scan(&session)
		if err != nil || !openCodeSessionUsable(session, time.Now()) {
			return "", fmt.Errorf("could not repair legacy OpenCode session ID")
		}
	}
	task.openCodeSession = session
	return session, nil
}

func setOpenCodeHeaders(req *http.Request, apiKey string, format clientFormat, session string, incoming http.Header, task *relayTask, body []byte) {
	v1 := openCodeV1Lane(req)
	if v1 {
		setOpenCodeV1Headers(req, format, session, incoming, task, body)
	} else {
		setOpenCodeV2Headers(req, format, session, incoming)
	}
	if format == FormatAnthropic {
		req.Header.Del("Authorization")
		req.Header.Set("x-api-key", apiKey)
		// The official client sends this fixed version on both gates; an older
		// or newer downstream version would change the fingerprinted shape.
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Del("x-api-key")
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

// setOpenCodeV1Headers renders the OpenCode 1.19.3 model request shape used by
// the v1 /zen gate: a model User-Agent carrying the AI SDK provider-utils
// version, Accept: */*, both session headers, the pinned user message id as
// x-opencode-request, the git root commit (or "global") as project, and the
// beta flags the AI SDK derives from the body. The v1 client sends no
// x-session-affinity or X-Session-Id.
func setOpenCodeV1Headers(req *http.Request, format clientFormat, session string, incoming http.Header, task *relayTask, body []byte) {
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", openCodeV1ModelUserAgent(format))
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Del("x-session-affinity")
	req.Header.Del("X-Session-Id")
	if session != "" {
		req.Header.Set("x-opencode-session", session)
		req.Header.Set("x-opencode-session-id", session)
	}
	message := openCodeMessageForTask(task, session, incoming)
	if message == "" {
		// Model tests and other one-shot requests have no relay task to pin to.
		message = newOpenCodeMessageID(time.Now())
	}
	if message != "" {
		req.Header.Set("x-opencode-request", message)
	}
	project := strings.TrimSpace(incoming.Get("x-opencode-project"))
	if project == "" {
		project = "global"
	}
	req.Header.Set("x-opencode-project", openCodeProjectID(project, true))
	for _, name := range []string{"x-parent-session-id", "x-opencode-parent-session-id"} {
		if parent := incoming.Get(name); openCodeSessionUsable(parent, time.Now()) {
			req.Header.Set(name, parent)
		}
	}
	if format == FormatAnthropic {
		if betas := openCodeV1AnthropicBetas(body); betas != "" {
			req.Header.Set("anthropic-beta", betas)
		} else {
			req.Header.Del("anthropic-beta")
		}
	}
}

// setOpenCodeV2Headers renders the OpenCode 2.x CLI shape used by the
// /inference and /zen/go gates: the channel User-Agent, every session header,
// a bare 40-character hex project, and no x-opencode-request. Anthropic
// requests carry the fixed beta set.
func setOpenCodeV2Headers(req *http.Request, format clientFormat, session string, incoming http.Header) {
	// Bun fetch supplies */* itself. Clear the relay's SSE default before
	// explicit administrator overrides are applied.
	req.Header.Del("Accept")
	req.Header.Set("User-Agent", openCodeV2UserAgent())
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Del("x-opencode-request")
	req.Header.Del("x-parent-session-id")
	req.Header.Del("x-opencode-parent-session-id")
	project := strings.TrimSpace(incoming.Get("x-opencode-project"))
	if project == "" {
		project = "global"
	}
	req.Header.Set("x-opencode-project", openCodeProjectID(project, false))
	if session != "" {
		setOpenCodeSessionHeaders(req, session)
		if child := incoming.Get("x-opencode-session-id"); openCodeSessionUsable(child, time.Now()) {
			req.Header.Set("x-opencode-session-id", child)
		}
	}
	for _, name := range []string{"x-parent-session-id", "x-opencode-parent-session-id"} {
		if parent := incoming.Get(name); openCodeSessionUsable(parent, time.Now()) {
			req.Header.Set(name, parent)
		}
	}
	if format == FormatAnthropic {
		req.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
	}
}

// openCodeV1AnthropicBetaRequest holds the Messages body fields from which
// the AI SDK Anthropic provider derives beta flags. Fields with variable JSON
// types stay raw so one unexpected value cannot hide the rest.
type openCodeV1AnthropicBetaRequest struct {
	Model             string            `json:"model"`
	MCPServers        []json.RawMessage `json:"mcp_servers"`
	ContextManagement *struct {
		Edits []struct {
			Type string `json:"type"`
		} `json:"edits"`
	} `json:"context_management"`
	Container    json.RawMessage `json:"container"`
	OutputConfig struct {
		TaskBudget json.RawMessage `json:"task_budget"`
	} `json:"output_config"`
	Speed     json.RawMessage `json:"speed"`
	Fallbacks json.RawMessage `json:"fallbacks"`
	Thinking  struct {
		BlockBinding json.RawMessage `json:"block_binding"`
	} `json:"thinking"`
	Tools []struct {
		Type           string          `json:"type"`
		InputExamples  json.RawMessage `json:"input_examples"`
		AllowedCallers json.RawMessage `json:"allowed_callers"`
	} `json:"tools"`
}

// openCodeV1AnthropicToolBetas maps server tool types to the beta flag the AI
// SDK sends along with each.
var openCodeV1AnthropicToolBetas = map[string]string{
	"code_execution_20250522": "code-execution-2025-05-22",
	"code_execution_20250825": "code-execution-2025-08-25",
	"computer_20241022":       "computer-use-2024-10-22",
	"computer_20250124":       "computer-use-2025-01-24",
	"computer_20251124":       "computer-use-2025-11-24",
	"text_editor_20241022":    "computer-use-2024-10-22",
	"text_editor_20250124":    "computer-use-2025-01-24",
	"text_editor_20250429":    "computer-use-2025-01-24",
	"bash_20241022":           "computer-use-2024-10-22",
	"bash_20250124":           "computer-use-2025-01-24",
	"memory_20250818":         "context-management-2025-06-27",
	"web_fetch_20250910":      "web-fetch-2025-09-10",
	"web_fetch_20260209":      "code-execution-web-tools-2026-02-09",
	"web_search_20260209":     "code-execution-web-tools-2026-02-09",
	"advisor_20260301":        "advisor-tool-2026-03-01",
}

// openCodeV1AnthropicBetas returns the Anthropic-beta value the OpenCode
// 1.19.3 client's AI SDK derives from a Messages body, in the SDK's order.
// The v1 provider sets no fixed flags: features come from the body first,
// then from the declared tools.
func openCodeV1AnthropicBetas(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var request openCodeV1AnthropicBetaRequest
	// A field of an unexpected type fails alone; the rest still decode.
	_ = json.Unmarshal(body, &request)
	var betas []string
	add := func(beta string) {
		for _, existing := range betas {
			if existing == beta {
				return
			}
		}
		betas = append(betas, beta)
	}
	if len(request.MCPServers) > 0 {
		add("mcp-client-2025-04-04")
	}
	if request.ContextManagement != nil {
		add("context-management-2025-06-27")
		for _, edit := range request.ContextManagement.Edits {
			if edit.Type == "compact_20260112" {
				add("compact-2026-01-12")
			}
		}
	}
	var container struct {
		Skills []json.RawMessage `json:"skills"`
	}
	if json.Unmarshal(request.Container, &container) == nil && len(container.Skills) > 0 {
		add("code-execution-2025-08-25")
		add("skills-2025-10-02")
		add("files-api-2025-04-14")
	}
	if openCodeJSONPresent(request.OutputConfig.TaskBudget) {
		add("task-budgets-2026-03-13")
	}
	if string(request.Speed) == `"fast"` {
		add("fast-mode-2026-02-01")
	}
	var fallbacks []json.RawMessage
	if string(request.Fallbacks) == `"default"` {
		add("server-side-fallback-2026-07-01")
	} else if json.Unmarshal(request.Fallbacks, &fallbacks) == nil && len(fallbacks) > 0 {
		add("server-side-fallback-2026-06-01")
	}
	if openCodeJSONPresent(request.Thinking.BlockBinding) {
		add("thinking-binding-controls-2026-08-01")
	}
	for _, tool := range request.Tools {
		if tool.Type == "" || tool.Type == "custom" {
			if openCodeAnthropicSupportsStructuredOutput(request.Model) {
				add("structured-outputs-2025-11-13")
			}
			if openCodeJSONPresent(tool.InputExamples) || openCodeJSONPresent(tool.AllowedCallers) {
				add("advanced-tool-use-2025-11-20")
			}
		} else if beta := openCodeV1AnthropicToolBetas[tool.Type]; beta != "" {
			add(beta)
		}
	}
	return strings.Join(betas, ",")
}

func openCodeJSONPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

var openCodeClaude3OrOlder = regexp.MustCompile(`claude-(?:instant(?:-|$)|v?2(?:$|[-.:])|3(?:$|[-.]))`)

// openCodeAnthropicSupportsStructuredOutput mirrors the AI SDK capability
// check: the dated Claude 4.0 releases and Claude 3 and older lack structured
// output, later Claude 4/5 models have it, and non-Claude models do not.
func openCodeAnthropicSupportsStructuredOutput(model string) bool {
	for _, supported := range []string{
		"claude-opus-5", "claude-opus-4-8", "claude-opus-4-7", "claude-fable-5", "claude-sonnet-5",
		"claude-sonnet-4-6", "claude-opus-4-6", "claude-sonnet-4-5", "claude-opus-4-5", "claude-haiku-4-5",
		"claude-opus-4-1",
	} {
		if strings.Contains(model, supported) {
			return true
		}
	}
	if strings.Contains(model, "claude-sonnet-4-") || strings.Contains(model, "claude-opus-4-") ||
		strings.Contains(model, "claude-3-haiku") || openCodeClaude3OrOlder.MatchString(model) {
		return false
	}
	return strings.Contains(model, "claude-")
}

func setOpenCodeSessionHeaders(req *http.Request, session string) {
	for _, name := range []string{"x-opencode-session", "x-opencode-session-id", "x-session-affinity", "X-Session-Id"} {
		req.Header.Set(name, session)
	}
}

func applyOpenCodeHeaderRules(req *http.Request, rules []headerRule, apiKey string, incoming http.Header) error {
	session, userAgent := req.Header.Get("x-opencode-session"), req.Header.Get("User-Agent")
	accept := req.Header.Get("Accept")
	actualSession := req.Header.Get("x-opencode-session-id")
	requestID := req.Header.Get("x-opencode-request")
	client, project := req.Header.Get("x-opencode-client"), req.Header.Get("x-opencode-project")
	parents := map[string]string{}
	for _, name := range []string{"x-parent-session-id", "x-opencode-parent-session-id"} {
		parents[name] = req.Header.Get(name)
	}
	if err := applyHeaderRules(req, rules, apiKey, incoming); err != nil {
		return err
	}
	// Session identity is supplied by the conversation, rather than a static
	// per-channel header. Broad passthrough must not undo its normalization.
	// The v1 /zen shape carries only the two x-opencode-session headers; the
	// v2 shape carries all four.
	legacy := openCodeLegacyRequest(req)
	if session != "" {
		if legacy {
			req.Header.Del("x-session-affinity")
			req.Header.Del("X-Session-Id")
			req.Header.Set("x-opencode-session", session)
			req.Header.Set("x-opencode-session-id", session)
			if actualSession != "" {
				req.Header.Set("x-opencode-session-id", actualSession)
			}
		} else {
			setOpenCodeSessionHeaders(req, session)
			if actualSession != "" {
				req.Header.Set("x-opencode-session-id", actualSession)
			}
		}
	}
	for name, parent := range parents {
		if parent == "" {
			req.Header.Del(name)
		} else {
			req.Header.Set(name, parent)
		}
	}
	// A deliberate exact UA override remains supported. Broad passthrough must
	// not accidentally replace the adapter's UA with another SDK's User-Agent.
	explicitUA, explicitClient, explicitProject, explicitAccept := false, false, false, false
	for _, rule := range rules {
		if strings.EqualFold(rule.name, "User-Agent") {
			explicitUA = true
		}
		if strings.EqualFold(rule.name, "x-opencode-client") {
			explicitClient = true
		}
		if strings.EqualFold(rule.name, "x-opencode-project") {
			explicitProject = true
		}
		if strings.EqualFold(rule.name, "Accept") {
			explicitAccept = true
		}
	}
	if !explicitUA && userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	if !explicitClient && client != "" {
		req.Header.Set("x-opencode-client", client)
	}
	if !explicitProject && project != "" {
		req.Header.Set("x-opencode-project", project)
	}
	if !explicitAccept {
		if accept == "" {
			req.Header.Del("Accept")
		} else {
			req.Header.Set("Accept", accept)
		}
	}
	req.Header.Set("x-opencode-project", openCodeProjectID(req.Header.Get("x-opencode-project"), legacy))
	if legacy {
		if requestID != "" {
			req.Header.Set("x-opencode-request", requestID)
		}
	} else {
		req.Header.Del("x-opencode-request")
	}
	return nil
}

// openCodeProjectID renders the project header per lane. The v1 client sends
// a git root commit hash or the literal "global"; the v2 CLI sends a bare
// 40-character lowercase hex id. Other values are hashed into the lane's
// shape so one project maps to one stable upstream project.
func openCodeProjectID(project string, legacy bool) string {
	project = strings.TrimSpace(project)
	if project == "" {
		project = "global"
	}
	if legacy {
		if project == "global" || openCodeProjectPattern.MatchString(project) {
			return project
		}
	} else if openCodeProjectPattern.MatchString(project) {
		return project
	}
	sum := sha256.Sum256([]byte("opencode-project\x00" + project))
	return hex.EncodeToString(sum[:20])
}

func openCodeLegacyRequest(req *http.Request) bool {
	return openCodeV1Lane(req)
}

func (u *Upstream) openCodeLegacy() bool {
	if !u.isOpenCode() {
		return false
	}
	probe, err := http.NewRequest(http.MethodPost, upstreamURL(u.BaseURL, "/v1/messages"), nil)
	return err == nil && openCodeLegacyRequest(probe)
}
