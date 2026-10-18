package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// OpenCode's official Identifier.descending() uses six timestamp bytes and
// fourteen Base62 characters. Keep actual client IDs intact; map foreign IDs
// once so subsequent turns and retries keep the same upstream conversation.
var openCodeSessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
var openCodeProjectPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var openCodeDescendingPattern = regexp.MustCompile(`^ses_[0-9a-f]{9}f[c-f][0-9a-f][0-9A-Za-z]{14}$`)

const openCodeSessionTTL = 30 * 24 * time.Hour
const openCodeFallbackVersion = "2.0.29"

func openCodeUserAgent() string {
	if ua, err := clientVersions.userAgent("opencode"); err == nil && ua != "" {
		return ua
	}
	return clientUserAgent("opencode", openCodeFallbackVersion)
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

func explicitSessionID(header http.Header, bodySession string) string {
	for _, name := range []string{"x-opencode-session", "x-opencode-session-id", "x-session-affinity", "X-Session-Id", "X-Conversation-Id", "conversation-id", "conversation_id"} {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			return value
		}
	}
	return strings.TrimSpace(bodySession)
}

func openCodeSessionForTask(task *relayTask, incoming http.Header) (string, error) {
	if openCodeDescendingPattern.MatchString(task.openCodeSession) {
		return task.openCodeSession, nil
	}
	task.openCodeSession = ""
	cr := task.canon
	if cr == nil {
		cr = &canonReq{Model: task.model}
	}
	signal := explicitSessionID(incoming, cr.SessionID)
	if openCodeDescendingPattern.MatchString(signal) {
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
	scope := fmt.Sprintf("%d:%s", task.apiKeyID, sessionKeyFromCanon("", incoming, cr))
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
	if !openCodeDescendingPattern.MatchString(session) {
		// Older releases persisted foreign encodings for up to 30 days. Repair
		// those mappings once, keeping a concurrent repair's already valid ID.
		err = db.QueryRow(`UPDATE opencode_sessions
			SET session_id=CASE WHEN session_id=? THEN ? ELSE session_id END
			WHERE scope=? RETURNING session_id`, session, newID, scopeHash).Scan(&session)
		if err != nil || !openCodeDescendingPattern.MatchString(session) {
			return "", fmt.Errorf("could not repair legacy OpenCode session ID")
		}
	}
	task.openCodeSession = session
	return session, nil
}

func setOpenCodeHeaders(req *http.Request, apiKey string, format clientFormat, session string, incoming http.Header) {
	req.Header.Set("User-Agent", openCodeUserAgent())
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-project", openCodeProjectID("global", false))
	if project := strings.TrimSpace(incoming.Get("x-opencode-project")); project != "" && validHeaderValue(project) {
		project = openCodeProjectID(project, false)
		req.Header.Set("x-opencode-project", project)
	}
	if session != "" {
		setOpenCodeSessionHeaders(req, session)
		if child := incoming.Get("x-opencode-session-id"); openCodeDescendingPattern.MatchString(child) {
			req.Header.Set("x-opencode-session-id", child)
		}
	}
	for _, name := range []string{"x-parent-session-id", "x-opencode-parent-session-id"} {
		if parent := incoming.Get(name); openCodeDescendingPattern.MatchString(parent) {
			req.Header.Set(name, parent)
		}
	}
	if openCodeLegacyRequest(req) {
		req.Header.Set("User-Agent", fmt.Sprintf("opencode/1.18.31 (%s %s; %s)", runtime.GOOS, runtime.GOARCH, runtime.Version()))
		req.Header.Del("x-opencode-session-id")
		project := incoming.Get("x-opencode-project")
		if project == "" {
			project = "global"
		}
		req.Header.Set("x-opencode-project", openCodeProjectID(project, true))
		var requestNonce [16]byte
		if _, requestErr := rand.Read(requestNonce[:]); requestErr == nil {
			req.Header.Set("x-opencode-request", "req_"+hex.EncodeToString(requestNonce[:]))
		}
	}
	if format == FormatAnthropic {
		req.Header.Del("Authorization")
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		if version := incoming.Get("anthropic-version"); version != "" {
			req.Header.Set("anthropic-version", version)
		}
		// The current official inference API accepts Console bearer tokens for
		// every family; legacy Zen Messages uses the Anthropic API-key header.
		if strings.HasPrefix(req.URL.Path, "/inference/") {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		beta := "interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14"
		if extra := incoming.Get("anthropic-beta"); extra != "" {
			beta += "," + extra
		}
		req.Header.Set("anthropic-beta", beta)
	} else {
		req.Header.Del("x-api-key")
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

func setOpenCodeSessionHeaders(req *http.Request, session string) {
	for _, name := range []string{"x-opencode-session", "x-opencode-session-id", "x-session-affinity", "X-Session-Id"} {
		req.Header.Set(name, session)
	}
}

func applyOpenCodeHeaderRules(req *http.Request, rules []headerRule, apiKey string, incoming http.Header) error {
	session, userAgent := req.Header.Get("x-opencode-session"), req.Header.Get("User-Agent")
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
	if session != "" {
		setOpenCodeSessionHeaders(req, session)
		if actualSession != "" {
			req.Header.Set("x-opencode-session-id", actualSession)
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
	explicitUA, explicitClient, explicitProject := false, false, false
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
	legacy := openCodeLegacyRequest(req)
	req.Header.Set("x-opencode-project", openCodeProjectID(req.Header.Get("x-opencode-project"), legacy))
	if legacy {
		req.Header.Del("x-opencode-session-id")
		if requestID != "" {
			req.Header.Set("x-opencode-request", requestID)
		}
	} else {
		req.Header.Del("x-opencode-request")
	}
	return nil
}

func openCodeProjectID(project string, legacy bool) string {
	if project == "" {
		project = "global"
	}
	if !legacy && openCodeProjectPattern.MatchString(project) {
		return project
	}
	if legacy && strings.HasPrefix(project, "prj_") && len(project) == 28 {
		return project
	}
	sum := sha256.Sum256([]byte("opencode-project\x00" + project))
	if legacy {
		return "prj_" + hex.EncodeToString(sum[:12])
	}
	return hex.EncodeToString(sum[:20])
}

func openCodeLegacyRequest(req *http.Request) bool {
	return strings.Contains(strings.ToLower(req.URL.Path), "/zen/v1/")
}

func (u *Upstream) openCodeLegacy() bool {
	if !u.isOpenCode() {
		return false
	}
	probe, err := http.NewRequest(http.MethodPost, upstreamURL(u.BaseURL, "/v1/messages"), nil)
	return err == nil && openCodeLegacyRequest(probe)
}
