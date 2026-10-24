package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
)

// A nil proxy inherits the supplier's default. An explicit zero uses the
// deployment's environment, just like proxy_id=0 on a single-key upstream.
type UpstreamCredential struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	APIKey   string `json:"api_key"`
	ProxyID  *int64 `json:"proxy_id"`
	Disabled bool   `json:"disabled"`
}

type upstreamCredentialView struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	APIKey        string `json:"api_key"`
	KeyMasked     string `json:"key_masked"`
	ProxyID       *int64 `json:"proxy_id"`
	Disabled      bool   `json:"disabled"`
	Healthy       bool   `json:"healthy"`
	Fails         int    `json:"fails"`
	CooldownUntil string `json:"cooldown_until,omitempty"`
}

func (up *Upstream) credentialViews() []upstreamCredentialView {
	out := make([]upstreamCredentialView, 0, len(up.APIKeys))
	for _, key := range up.APIKeys {
		selected := up.withAPIKey(key)
		state := selected.keyHealth()
		until := state.until
		if limit := selected.openCodeRateLimit(); limit != nil && limit.until.After(until) {
			until = limit.until
		}
		view := upstreamCredentialView{ID: key.ID, Name: key.Name, KeyMasked: maskKey(key.APIKey), ProxyID: key.ProxyID,
			Disabled: key.Disabled, Healthy: !key.Disabled && selected.credentialAvailable(""), Fails: selected.relayFailures()}
		if time.Now().Before(until) {
			view.CooldownUntil = until.UTC().Format(time.RFC3339)
		}
		out = append(out, view)
	}
	return out
}

func (up *Upstream) normalizeAPIKeys(previous *Upstream) error {
	if up.KeyProxyMode != "" && up.KeyProxyMode != "shared" && up.KeyProxyMode != "individual" {
		return errors.New("密钥代理模式必须为 shared 或 individual")
	}
	if len(up.APIKeys) == 0 {
		return nil
	}
	if up.Type == "cliproxy" {
		return errors.New("CLIProxyAPI 请使用账号池管理多个账号及其代理")
	}
	if len(up.APIKeys) > 256 {
		return errors.New("每个供应商最多配置 256 把密钥")
	}
	saved := make(map[string]string)
	if previous != nil {
		for _, key := range previous.APIKeys {
			saved[key.ID] = key.APIKey
		}
		if len(previous.APIKeys) == 0 {
			saved["legacy"] = previous.APIKey
		}
	}
	ids, secrets := make(map[string]bool), make(map[string]bool)
	// Work on a copy: routing snapshots and duplicated rows share backing slices.
	up.APIKeys = append([]UpstreamCredential(nil), up.APIKeys...)
	for i := range up.APIKeys {
		key := &up.APIKeys[i]
		key.ID, key.Name, key.APIKey = strings.TrimSpace(key.ID), strings.TrimSpace(key.Name), strings.TrimSpace(key.APIKey)
		if key.ID == "" {
			key.ID = randHex(12)
		}
		if len(key.ID) > 64 || strings.ContainsAny(key.ID, " \t\r\n") || ids[key.ID] {
			return errors.New("密钥标识无效或重复")
		}
		ids[key.ID] = true
		if key.APIKey == "" {
			key.APIKey = saved[key.ID]
		}
		if key.APIKey == "" {
			return fmt.Errorf("第 %d 把密钥不能为空；已保存的密钥可留空保留", i+1)
		}
		if secrets[key.APIKey] {
			return errors.New("同一供应商不能重复添加相同密钥")
		}
		secrets[key.APIKey] = true
		if key.ProxyID != nil && *key.ProxyID < 0 {
			return errors.New("密钥代理请选择跟随默认、系统连接或有效代理")
		}
	}
	// Keep the legacy column useful to older integrations. Actual requests
	// always resolve a credential and its proxy together.
	up.APIKey = up.APIKeys[0].APIKey
	return nil
}

func (up *Upstream) validateAPIKeyProxies(previous *Upstream) error {
	for _, key := range up.APIKeys {
		if key.ProxyID == nil || *key.ProxyID == 0 {
			continue
		}
		// An unchanged disabled proxy must not prevent renaming a supplier.
		unchanged := false
		if previous != nil {
			for _, old := range previous.APIKeys {
				if old.ID == key.ID && old.ProxyID != nil && *old.ProxyID == *key.ProxyID {
					unchanged = true
					break
				}
			}
		}
		if !unchanged {
			if err := validateUpstreamProxy(*key.ProxyID); err != nil {
				return fmt.Errorf("密钥 %s：%w", key.Name, err)
			}
		}
	}
	return nil
}

func (up *Upstream) withAPIKey(key UpstreamCredential) *Upstream {
	copy := *up
	copy.APIKey, copy.selectedKeyID = key.APIKey, key.ID
	if up.KeyProxyMode == "individual" && key.ProxyID != nil {
		copy.ProxyID = *key.ProxyID
	}
	return &copy
}

func (up *Upstream) pooledKeys() bool {
	return len(up.APIKeys) > 0 && up.selectedKeyID == ""
}

type credentialHealthKey struct {
	database   *sql.DB
	upstreamID int64
	connection [32]byte
}

type credentialHealth struct {
	failures int
	until    time.Time
	failedAt time.Time
}

var upstreamKeyHealth = struct {
	sync.Mutex
	entries map[credentialHealthKey]credentialHealth
}{entries: make(map[credentialHealthKey]credentialHealth)}

func (up *Upstream) credentialHealthKey() credentialHealthKey {
	data, _ := json.Marshal([]any{up.Type, up.BaseURL, up.selectedKeyID, up.APIKey})
	return credentialHealthKey{db, up.ID, sha256.Sum256(data)}
}

func (up *Upstream) keyHealth() credentialHealth {
	upstreamKeyHealth.Lock()
	defer upstreamKeyHealth.Unlock()
	return upstreamKeyHealth.entries[up.credentialHealthKey()]
}

func (up *Upstream) markRelayFailure(status int, retryAfter time.Time) {
	if up.selectedKeyID == "" {
		healthMarkFailure(up.ID)
		healthMarkRetryAfter(up.ID, retryAfter)
		return
	}
	now := time.Now()
	upstreamKeyHealth.Lock()
	defer upstreamKeyHealth.Unlock()
	key := up.credentialHealthKey()
	state := upstreamKeyHealth.entries[key]
	state.failures++
	// A failed pooled credential cools immediately so other sessions do not
	// keep hitting the same rejected key while failover is in progress.
	delay := min(10*time.Minute, 30*time.Second<<min(state.failures-1, 5))
	until := now.Add(delay)
	if retryAfter.After(until) {
		until = retryAfter
	}
	if until.After(state.until) {
		state.until = until
	}
	state.failedAt = now
	upstreamKeyHealth.entries[key] = state
	for previous, value := range upstreamKeyHealth.entries {
		if previous.database != db || now.Sub(value.failedAt) > 24*time.Hour && now.After(value.until) {
			delete(upstreamKeyHealth.entries, previous)
		}
	}
}

func (up *Upstream) markRelaySuccess(startedAt ...time.Time) {
	if up.selectedKeyID == "" {
		healthMarkSuccess(up.ID)
		return
	}
	upstreamKeyHealth.Lock()
	if len(startedAt) > 0 && upstreamKeyHealth.entries[up.credentialHealthKey()].failedAt.After(startedAt[0]) {
		upstreamKeyHealth.Unlock()
		return
	}
	delete(upstreamKeyHealth.entries, up.credentialHealthKey())
	upstreamKeyHealth.Unlock()
}

func (up *Upstream) credentialAvailable(model string) bool {
	if time.Now().Before(up.keyHealth().until) || up.openCodeRateLimit(model) != nil {
		return false
	}
	if up.ProxyID != 0 {
		p, err := dbGetOutboundProxy(up.ProxyID)
		return err == nil && p.Enabled
	}
	return true
}

func (up *Upstream) triedKey() string {
	return fmt.Sprintf("%d:%s", up.ID, up.selectedKeyID)
}

func keyAffinityGet(skey string, upstreamID int64) string {
	var id string
	_ = db.QueryRow(`SELECT credential_id FROM upstream_key_affinity WHERE skey=? AND upstream_id=? AND expires_at>?`,
		skey, upstreamID, time.Now().Unix()).Scan(&id)
	return id
}

func keyAffinitySet(skey string, up *Upstream) {
	if skey == "" || up.selectedKeyID == "" {
		return
	}
	_, _ = db.Exec(`INSERT INTO upstream_key_affinity(skey,upstream_id,credential_id,expires_at) VALUES(?,?,?,?)
		ON CONFLICT(skey,upstream_id) DO UPDATE SET credential_id=excluded.credential_id,expires_at=excluded.expires_at`,
		skey, up.ID, up.selectedKeyID, time.Now().Add(affinityTTL()).Unix())
}

func (up *Upstream) selectAPIKey(skey, model string, tried map[string]bool) *Upstream {
	if !up.pooledKeys() {
		return up
	}
	preferred := ""
	if skey != "" {
		preferred = keyAffinityGet(skey, up.ID)
	}
	var choices []*Upstream
	for _, key := range up.APIKeys {
		selected := up.withAPIKey(key)
		if key.Disabled || tried[selected.triedKey()] || !selected.credentialAvailable(up.mapModel(model)) {
			continue
		}
		if key.ID == preferred {
			keyAffinitySet(skey, selected)
			return selected
		}
		choices = append(choices, selected)
	}
	if len(choices) == 0 {
		return nil
	}
	index := rand.Intn(len(choices))
	if skey != "" {
		// Concurrent first requests for the same conversation must agree even
		// before either request has committed its persistent binding.
		hash := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", up.ID, skey)))
		index = int(binary.BigEndian.Uint64(hash[:8]) % uint64(len(choices)))
	}
	selected := choices[index]
	keyAffinitySet(skey, selected)
	return selected
}

// Both levels of affinity are scoped by the gateway key. Exhaust credentials
// in the sticky supplier before moving to another supplier, within the existing
// retry budget. Every selected copy carries the matching secret and egress.
func pickSessionUpstream(format clientFormat, model, skey string, tried map[int64]bool, triedKeys map[string]bool, nativeOnly bool, request *canonReq) (*Upstream, error) {
	for {
		var up *Upstream
		if skey != "" {
			if id, ok := affinityGet(skey); ok && !tried[id] {
				if candidate, err := cachedRoutingUpstream(id); err == nil && candidate.Enabled && candidate.Weight > 0 &&
					candidate.servesModel(model) && candidate.availableForRelay(model) && (!nativeOnly || supportsNativeEndpoint(candidate, format)) {
					up = candidate
				}
			}
		}
		if up == nil {
			var err error
			up, err = pickUpstream(format, model, tried, nativeOnly, request)
			if err != nil {
				return nil, err
			}
		}
		selected := up.selectAPIKey(skey, model, triedKeys)
		if selected == nil {
			tried[up.ID] = true
			continue
		}
		if up.pooledKeys() {
			triedKeys[selected.triedKey()] = true
		} else {
			tried[up.ID] = true
		}
		if skey != "" {
			affinitySet(skey, selected.ID)
		}
		return selected, nil
	}
}

// Admin probes are deterministic and may inspect a cooling credential. They
// do not mutate live session bindings or health.
func (up *Upstream) probeAPIKey(id string) (*Upstream, error) {
	if !up.pooledKeys() {
		return up, nil
	}
	for _, key := range up.APIKeys {
		if id != "" && key.ID == id || id == "" && !key.Disabled {
			return up.withAPIKey(key), nil
		}
	}
	return nil, errors.New("请选择可用的密钥")
}

func (up *Upstream) redactAPIKeys(message string) string {
	message = redactModelTestError(message, up.APIKey)
	for _, key := range up.APIKeys {
		message = redactModelTestError(message, key.APIKey)
	}
	return message
}

func (up *Upstream) resetAPIKeyCooldowns() {
	for _, key := range up.APIKeys {
		selected := up.withAPIKey(key)
		selected.markRelaySuccess()
		if selected.isOpenCode() {
			rateKey := selected.openCodeRateKey()
			exitKey, hasExit := selected.openCodeFreeExitKey()
			openCodeRates.Lock()
			delete(openCodeRates.entries, rateKey)
			if hasExit {
				delete(openCodeRates.entries, exitKey)
			}
			openCodeRates.Unlock()
		}
	}
}

func retryAfterHeader(header http.Header) time.Time {
	return openCodeRetryAfter(header.Get("Retry-After"), time.Now())
}
