package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type openCodeRateLimitError struct {
	until  time.Time
	detail string
	status int
}

func (e *openCodeRateLimitError) Error() string {
	if e.status == http.StatusServiceUnavailable {
		message := "OpenCode 免费模型当前出口被上游封禁，请切换出口或等待冷却结束"
		if e.detail != "" {
			message += ": " + e.detail
		}
		return message
	}
	if e.detail != "" {
		return "OpenCode upstream rate limit: " + e.detail
	}
	return "OpenCode upstream rate limit; this credential is cooling down"
}

func (e *openCodeRateLimitError) statusCode() int {
	if e.status != 0 {
		return e.status
	}
	return http.StatusTooManyRequests
}

func (e *openCodeRateLimitError) retryAfter() string {
	delay := time.Until(e.until)
	seconds := int64(delay / time.Second)
	if delay > 0 && delay%time.Second != 0 {
		seconds++
	}
	return fmt.Sprint(max(int64(1), seconds))
}

type openCodeRateKey struct {
	database   *sql.DB
	connection [32]byte
}

type openCodeRateState struct {
	failures int
	failedAt time.Time
	limit    openCodeRateLimitError
}

var openCodeRates = struct {
	sync.Mutex
	entries map[openCodeRateKey]openCodeRateState
}{entries: make(map[openCodeRateKey]openCodeRateState)}

// A paid credential shares quota across models and copied channel rows. Public
// credentials share it across channels using the same configured proxy route.
// Never put raw credentials or proxy authentication into logs or cache labels.
func (up *Upstream) openCodeRateKey() openCodeRateKey {
	origin := strings.TrimRight(up.BaseURL, "/")
	if parsed, err := url.Parse(up.BaseURL); err == nil {
		origin = strings.ToLower(parsed.Scheme + "://" + parsed.Host)
		if !strings.EqualFold(parsed.Hostname(), "opencode.ai") {
			origin += strings.TrimRight(parsed.Path, "/")
		}
	}
	var egress any
	if up.openCodePublic() {
		egress = up.openCodeEgress()
	}
	raw, _ := json.Marshal([]any{origin, up.openCodeTier(), up.openCodeAPIKey(), egress})
	return openCodeRateKey{db, sha256.Sum256(raw)}
}

func (up *Upstream) openCodeEgress() any {
	if up.ProxyID != 0 && db != nil {
		if proxy, err := dbGetOutboundProxy(up.ProxyID); err == nil {
			return []any{proxy.URL, proxy.Username, proxy.Password}
		}
	}
	return up.ProxyID
}

// The official free gateway blocks the exit IP across both Zen entry points
// and credentials. Keep paid/Go/custom traffic outside this cooldown.
func (up *Upstream) openCodeFreeExitKey() (openCodeRateKey, bool) {
	parsed, err := url.Parse(up.BaseURL)
	if !up.isOpenCode() || err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "opencode.ai") || up.openCodeTier() != "zen" {
		return openCodeRateKey{}, false
	}
	path := strings.TrimRight(parsed.Path, "/")
	if path == "/zen/go" || strings.HasPrefix(path, "/zen/go/") || path == "/inference/go" || strings.HasPrefix(path, "/inference/go/") {
		return openCodeRateKey{}, false
	}
	if path != "/zen" && !strings.HasPrefix(path, "/zen/") && path != "/inference" && !strings.HasPrefix(path, "/inference/") {
		return openCodeRateKey{}, false
	}
	raw, _ := json.Marshal([]any{"opencode-free-egress", up.openCodeEgress()})
	return openCodeRateKey{db, sha256.Sum256(raw)}, true
}

func (up *Upstream) openCodeRateLimit(models ...string) *openCodeRateLimitError {
	if !up.isOpenCode() {
		return nil
	}
	if up.pooledKeys() {
		var earliest *openCodeRateLimitError
		for _, key := range up.APIKeys {
			if key.Disabled {
				continue
			}
			limit := up.withAPIKey(key).openCodeRateLimit(models...)
			if limit == nil {
				return nil
			}
			if earliest == nil || limit.until.Before(earliest.until) {
				earliest = limit
			}
		}
		return earliest
	}
	keys := []openCodeRateKey{up.openCodeRateKey()}
	if up.openCodeFreeOnly() || up.openCodePublic() || len(models) > 0 && up.openCodeFreeModel(models[0]) {
		if key, ok := up.openCodeFreeExitKey(); ok {
			keys = append(keys, key)
		}
	}
	openCodeRates.Lock()
	defer openCodeRates.Unlock()
	var limit *openCodeRateLimitError
	now := time.Now()
	for _, key := range keys {
		state, exists := openCodeRates.entries[key]
		if exists && now.Before(state.limit.until) && (limit == nil || state.limit.until.After(limit.until)) {
			copy := state.limit
			limit = &copy
		}
	}
	return limit
}

func recordOpenCodeFreeExitBlock(up *Upstream, retryAt time.Time) *openCodeRateLimitError {
	key, ok := up.openCodeFreeExitKey()
	if !ok {
		return nil
	}
	now := time.Now()
	until := now.Add(15 * time.Minute)
	if retryAt.After(until) {
		until = retryAt
	}
	openCodeRates.Lock()
	defer openCodeRates.Unlock()
	state := openCodeRates.entries[key]
	if state.limit.until.After(until) {
		until = state.limit.until
	}
	state.failures++
	state.failedAt = now
	state.limit = openCodeRateLimitError{until: until, status: http.StatusServiceUnavailable}
	openCodeRates.entries[key] = state
	limit := state.limit
	return &limit
}

func recordOpenCodeRateLimit(up *Upstream, retryAt time.Time) *openCodeRateLimitError {
	key, now := up.openCodeRateKey(), time.Now()
	openCodeRates.Lock()
	defer openCodeRates.Unlock()
	state := openCodeRates.entries[key]
	state.failures++
	delay := 15 * time.Second << min(state.failures-1, 3)
	until := now.Add(delay)
	if retryAt.After(until) {
		until = retryAt
	}
	if state.limit.until.After(until) {
		until = state.limit.until
	}
	state.failedAt = now
	state.limit = openCodeRateLimitError{until: until}
	openCodeRates.entries[key] = state
	for previous, cached := range openCodeRates.entries {
		if previous.database != db || now.Sub(cached.failedAt) > 24*time.Hour {
			delete(openCodeRates.entries, previous)
		}
	}
	limit := state.limit
	return &limit
}

func rememberOpenCodeRateDetail(up *Upstream, limit *openCodeRateLimitError, body []byte, headers http.Header) {
	message := redactModelTestError(string(body), up.APIKey)
	for _, name := range []string{"Authorization", "x-api-key", "x-goog-api-key"} {
		credential := headers.Get(name)
		if credential == "" {
			continue
		}
		message = strings.ReplaceAll(message, credential, "[REDACTED]")
		if strings.HasPrefix(credential, "Bearer ") {
			message = strings.ReplaceAll(message, strings.TrimPrefix(credential, "Bearer "), "[REDACTED]")
		}
	}
	limit.detail = truncate(strings.TrimSpace(message), 2000)
	key := up.openCodeRateKey()
	if limit.status == http.StatusServiceUnavailable {
		key, _ = up.openCodeFreeExitKey()
	}
	openCodeRates.Lock()
	defer openCodeRates.Unlock()
	state := openCodeRates.entries[key]
	if state.limit.until.Equal(limit.until) {
		state.limit = *limit
		openCodeRates.entries[key] = state
	}
}

func markOpenCodeRateSuccess(up *Upstream, started time.Time) {
	if !up.isOpenCode() {
		return
	}
	key := up.openCodeRateKey()
	openCodeRates.Lock()
	defer openCodeRates.Unlock()
	// An older in-flight success cannot undo a newer request's 429 cooldown.
	if state, exists := openCodeRates.entries[key]; exists && !state.failedAt.After(started) {
		delete(openCodeRates.entries, key)
	}
}

func (up *Upstream) availableForRelay(models ...string) bool {
	if up.pooledKeys() {
		model := ""
		if len(models) > 0 {
			model = up.mapModel(models[0])
		}
		for _, key := range up.APIKeys {
			if !key.Disabled && up.withAPIKey(key).credentialAvailable(model) {
				return true
			}
		}
		return false
	}
	if up.selectedKeyID != "" {
		model := ""
		if len(models) > 0 {
			model = up.mapModel(models[0])
		}
		return up.credentialAvailable(model)
	}
	var mapped []string
	if len(models) > 0 {
		mapped = []string{up.mapModel(models[0])}
	}
	if !healthAvailable(up.ID) || up.openCodeRateLimit(mapped...) != nil {
		return false
	}
	if up.Type != "cliproxy" {
		return true
	}
	if len(models) > 0 {
		return cliProxyRelayAvailable(up.ID, up.mapModel(models[0]))
	}
	return cliProxyRelayAvailable(up.ID)
}

func (up *Upstream) relayFailures() int {
	if up.pooledKeys() {
		failures := 0
		for _, key := range up.APIKeys {
			failures += up.withAPIKey(key).relayFailures()
		}
		return failures
	}
	failures := healthFails(up.ID)
	if up.selectedKeyID != "" {
		failures = up.keyHealth().failures
	}
	if !up.isOpenCode() {
		return failures
	}
	key := up.openCodeRateKey()
	openCodeRates.Lock()
	defer openCodeRates.Unlock()
	return max(failures, openCodeRates.entries[key].failures)
}
