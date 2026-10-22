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
}

func (e *openCodeRateLimitError) Error() string {
	if e.detail != "" {
		return "OpenCode upstream rate limit: " + e.detail
	}
	return "OpenCode upstream rate limit; this credential is cooling down"
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
		egress = up.ProxyID
		if up.ProxyID != 0 && db != nil {
			if proxy, err := dbGetOutboundProxy(up.ProxyID); err == nil {
				egress = []any{proxy.URL, proxy.Username, proxy.Password}
			}
		}
	}
	raw, _ := json.Marshal([]any{origin, up.openCodeTier(), up.openCodeAPIKey(), egress})
	return openCodeRateKey{db, sha256.Sum256(raw)}
}

func (up *Upstream) openCodeRateLimit() *openCodeRateLimitError {
	if !up.isOpenCode() {
		return nil
	}
	key := up.openCodeRateKey()
	openCodeRates.Lock()
	defer openCodeRates.Unlock()
	state, exists := openCodeRates.entries[key]
	if !exists || !time.Now().Before(state.limit.until) {
		return nil
	}
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
	if !healthAvailable(up.ID) || up.openCodeRateLimit() != nil {
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
	failures := healthFails(up.ID)
	if !up.isOpenCode() {
		return failures
	}
	key := up.openCodeRateKey()
	openCodeRates.Lock()
	defer openCodeRates.Unlock()
	return max(failures, openCodeRates.entries[key].failures)
}
