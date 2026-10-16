package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// OpenCode's official Identifier.descending() uses six timestamp bytes and
// fourteen Base62 characters. Keep actual client IDs intact; map foreign IDs
// once so subsequent turns and retries keep the same upstream conversation.
var openCodeSessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
var openCodeProjectPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

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
	if task.openCodeSession != "" {
		return task.openCodeSession, nil
	}
	cr := task.canon
	if cr == nil {
		cr = &canonReq{Model: task.model}
	}
	signal := explicitSessionID(incoming, cr.SessionID)
	if openCodeSessionPattern.MatchString(signal) {
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
	err = db.QueryRow(`INSERT INTO opencode_sessions(scope,session_id,expires_at) VALUES(?,?,?)
		ON CONFLICT(scope) DO UPDATE SET
		 session_id=CASE WHEN opencode_sessions.expires_at<=? THEN excluded.session_id ELSE opencode_sessions.session_id END,
		 expires_at=excluded.expires_at RETURNING session_id`, hex.EncodeToString(sum[:]), newID, now.Add(openCodeSessionTTL).Unix(), now.Unix()).Scan(&task.openCodeSession)
	if err != nil {
		return "", fmt.Errorf("could not persist OpenCode session ID")
	}
	return task.openCodeSession, nil
}

func setOpenCodeHeaders(req *http.Request, apiKey string, format clientFormat, session string, incoming http.Header) {
	req.Header.Set("User-Agent", openCodeUserAgent())
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json, text/event-stream")
	}
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-project", "global")
	if project := strings.TrimSpace(incoming.Get("x-opencode-project")); project != "" && validHeaderValue(project) {
		if project != "global" && !openCodeProjectPattern.MatchString(project) {
			sum := sha256.Sum256([]byte(project))
			project = hex.EncodeToString(sum[:20])
		}
		req.Header.Set("x-opencode-project", project)
	}
	if session != "" {
		setOpenCodeSessionHeaders(req, session)
		if child := incoming.Get("x-opencode-session-id"); openCodeSessionPattern.MatchString(child) {
			req.Header.Set("x-opencode-session-id", child)
		}
	}
	for _, name := range []string{"x-parent-session-id", "x-opencode-parent-session-id"} {
		if parent := incoming.Get(name); openCodeSessionPattern.MatchString(parent) {
			req.Header.Set(name, parent)
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
	req.Header.Del("x-opencode-request")
	return nil
}
