package cliproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Only normalized usage observations cross the gateway boundary. In particular,
// upstream /key responses may contain api_key, tokens and PII; never retain them.
type QuotaWindow struct {
	ID               string   `json:"id"`
	Label            string   `json:"label"`
	Scope            string   `json:"scope,omitempty"`
	PeriodSeconds    int64    `json:"period_seconds,omitempty"`
	UsedPercent      *float64 `json:"used_percent"`
	RemainingPercent *float64 `json:"remaining_percent"`
	Used             *float64 `json:"used,omitempty"`
	Limit            *float64 `json:"limit,omitempty"`
	Remaining        *float64 `json:"remaining,omitempty"`
	Unit             string   `json:"unit,omitempty"`
	ResetAt          string   `json:"reset_at,omitempty"`
}
type AccountQuota struct {
	Provider    string        `json:"provider"`
	Supported   bool          `json:"supported"`
	ObservedAt  string        `json:"observed_at,omitempty"`
	AttemptedAt string        `json:"attempted_at,omitempty"`
	Windows     []QuotaWindow `json:"windows"`
	Plan        string        `json:"plan,omitempty"`
	Balance     *float64      `json:"balance,omitempty"`
	Unlimited   bool          `json:"unlimited,omitempty"`
	Stale       bool          `json:"stale"`
	Error       string        `json:"error,omitempty"`
	HTTPStatus  int           `json:"http_status,omitempty"`
}
type quotaCacheEntry struct {
	value    AccountQuota
	revision uint64
	at       time.Time
	identity string
}
type quotaRequest struct {
	method string
	url    string
	header map[string]string
	data   string
}

func liveQuotaSupported(provider string) bool {
	switch provider {
	case "codex", "claude", "kimi", "kimi-ai", "antigravity", "xai", "meta", "devin":
		return true
	}
	return false
}
func (m *Manager) CachedQuota(name string) *AccountQuota {
	m.quotaMu.Lock()
	entry, ok := m.quotaCache[name]
	m.quotaMu.Unlock()
	if !ok || entry.identity != m.quotaIdentityFor(name) {
		return nil
	}
	value := entry.value
	value.Stale = value.Stale || !m.Running() || entry.revision != m.Revision() || time.Since(entry.at) > 5*time.Minute
	return &value
}
func (m *Manager) QueryQuota(ctx context.Context, name string, force bool) (AccountQuota, error) {
	if filepath.Base(name) != name || !strings.HasSuffix(name, ".json") {
		return AccountQuota{}, errors.New("凭据文件名无效")
	}
	for {
		identity := m.quotaIdentityFor(name)
		m.quotaMu.Lock()
		cached, exists := m.quotaCache[name]
		exists = exists && cached.identity == identity
		fresh := exists && cached.revision == m.Revision() && time.Since(cached.at) < time.Minute
		// Explicit refresh can bypass the cache, but repeated clicks never
		// multiply provider requests or bypass a short retry interval.
		if exists && ((!force && fresh) || time.Since(cached.at) < 3*time.Second) {
			value := cached.value
			m.quotaMu.Unlock()
			if !m.Running() {
				value.Stale = true
				value.Error = "pool_stopped"
			}
			return value, nil
		}
		if pending := m.quotaFlights[name]; pending != nil {
			m.quotaMu.Unlock()
			select {
			case <-ctx.Done():
				return AccountQuota{}, ctx.Err()
			case <-pending:
			}
			force = false
			continue
		}
		if m.quotaFlights == nil {
			m.quotaFlights = make(map[string]chan struct{})
		}
		if m.quotaSlots == nil {
			m.quotaSlots = make(chan struct{}, 3)
		}
		slots := m.quotaSlots
		done := make(chan struct{})
		m.quotaFlights[name] = done
		m.quotaMu.Unlock()
		go func() {
			requestCtx, cancel := context.WithTimeout(context.Background(), 22*time.Second)
			defer cancel()
			revision := m.Revision()
			value := AccountQuota{Windows: []QuotaWindow{}, AttemptedAt: timestamp(time.Now())}
			var err error
			select {
			case slots <- struct{}{}:
				value, err = m.fetchQuota(requestCtx, name, nil)
				<-slots
			case <-requestCtx.Done():
				err = errors.New("timeout")
			}
			if err != nil {
				value.Error = err.Error()
			}
			m.quotaMu.Lock()
			if value.Error != "" && value.Error != "credential_changed" && exists && cached.revision == revision && len(cached.value.Windows) > 0 {
				failedAt, reason, status := value.AttemptedAt, value.Error, value.HTTPStatus
				value = cached.value
				value.Stale = true
				value.AttemptedAt = failedAt
				value.Error = reason
				value.HTTPStatus = status
			}
			if m.quotaCache == nil {
				m.quotaCache = make(map[string]quotaCacheEntry)
			}
			m.quotaCache[name] = quotaCacheEntry{value: value, revision: revision, at: time.Now(), identity: identity}
			delete(m.quotaFlights, name)
			close(done)
			m.quotaMu.Unlock()
		}()
		select {
		case <-ctx.Done():
			return AccountQuota{}, ctx.Err()
		case <-done:
		}
		m.quotaMu.Lock()
		value := m.quotaCache[name].value
		m.quotaMu.Unlock()
		return value, nil
	}
}
func quotaIdentity(document map[string]any, raw json.RawMessage) string {
	identity := strings.Join([]string{quotaText(document["type"]), quotaText(document["email"]), quotaText(document["account_id"]), quotaText(document["user_id"]), quotaText(document["domain"])}, "|")
	if quotaText(document["email"]) == "" && quotaText(document["account_id"]) == "" && quotaText(document["user_id"]) == "" {
		return quotaCredentialFingerprint(raw)
	}
	return identity
}
func (m *Manager) quotaIdentityFor(name string) string {
	if filepath.Base(name) != name {
		return ""
	}
	path := filepath.Join(m.root, "auths", name)
	info, err := os.Stat(path)
	if err != nil || info.Size() > 2<<20 {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var document map[string]any
	if json.Unmarshal(raw, &document) != nil {
		return ""
	}
	return quotaIdentity(document, raw)
}
func quotaCredentialFingerprint(raw json.RawMessage) string {
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}
func (m *Manager) fetchQuota(ctx context.Context, name string, override []quotaRequest) (AccountQuota, error) {
	now := time.Now().UTC()
	view := AccountQuota{Windows: make([]QuotaWindow, 0), AttemptedAt: timestamp(now)}
	files, err := m.ReadCredentials()
	if err != nil {
		return view, errors.New("credential_unavailable")
	}
	raw, ok := files[name]
	if !ok {
		return view, errors.New("account_not_found")
	}
	var metadata map[string]any
	if json.Unmarshal(raw, &metadata) != nil {
		return view, errors.New("credential_unavailable")
	}
	view.Provider = strings.ToLower(quotaText(metadata["type"]))
	if view.Provider == "kimi.com" {
		view.Provider = "kimi"
	}
	if view.Provider == "kimi.ai" {
		view.Provider = "kimi-ai"
	}
	view.Supported = liveQuotaSupported(view.Provider)
	if !view.Supported {
		return view, nil
	}
	if !m.Running() {
		view.Stale = true
		return view, errors.New("pool_stopped")
	}
	var filesView struct {
		Files []struct {
			Name  string `json:"name"`
			Index string `json:"auth_index"`
		}
	}
	if err := m.Client().Do(ctx, "GET", "/credentials", nil, &filesView); err != nil {
		return view, errors.New("core_unavailable")
	}
	index := ""
	for _, file := range filesView.Files {
		if file.Name == name {
			index = file.Index
			break
		}
	}
	if index == "" {
		return view, errors.New("credential_unavailable")
	}
	requests, err := quotaRequests(view.Provider, metadata)
	if override != nil {
		requests = override
		err = nil
	}
	if err != nil {
		return view, err
	}
	lastFailure := "quota_unavailable"
	for _, request := range requests {
		var response struct {
			Status int    `json:"status_code"`
			Body   string `json:"body"`
		}
		err = m.Client().Do(ctx, "POST", "/requests/api-call", map[string]any{
			"auth_index": index, "method": request.method, "url": request.url, "header": request.header, "data": request.data,
		}, &response)
		if err != nil {
			lastFailure = "network_error"
			continue
		}
		if response.Status < 200 || response.Status >= 300 {
			view.HTTPStatus = response.Status
			lastFailure = publicFailureReason(response.Status)
			if response.Status == 401 || response.Status == 403 {
				break
			}
			continue
		}
		var payload map[string]any
		if json.Unmarshal([]byte(response.Body), &payload) != nil {
			lastFailure = "invalid_quota_response"
			continue
		}
		parsed := parseProviderQuota(view.Provider, payload, now)
		if len(parsed.Windows) == 0 && parsed.Balance == nil && !parsed.Unlimited {
			lastFailure = "quota_unavailable"
			continue
		}
		view.Windows = append(view.Windows, parsed.Windows...)
		if parsed.Plan != "" {
			view.Plan = parsed.Plan
		}
		if parsed.Balance != nil {
			view.Balance = parsed.Balance
		}
		view.Unlimited = view.Unlimited || parsed.Unlimited
		view.HTTPStatus = 0
		// Only xAI has independent weekly and monthly read endpoints.
		if view.Provider != "xai" {
			break
		}
	}
	if len(view.Windows) == 0 && view.Balance == nil && !view.Unlimited {
		return view, errors.New(lastFailure)
	}
	current, err := m.ReadCredentials()
	if err != nil || current[name] == nil {
		return AccountQuota{Provider: view.Provider, Supported: true, Windows: []QuotaWindow{}}, errors.New("credential_changed")
	}
	// Refreshing an OAuth token can legitimately change its file. Compare
	// stable identity fields when available; otherwise require identical bytes.
	var currentMeta map[string]any
	json.Unmarshal(current[name], &currentMeta)
	if quotaIdentity(currentMeta, current[name]) != quotaIdentity(metadata, raw) {
		return AccountQuota{Provider: view.Provider, Supported: true, Windows: []QuotaWindow{}}, errors.New("credential_changed")
	}
	view.ObservedAt = timestamp(now)
	dedup := make(map[string]bool)
	windows := make([]QuotaWindow, 0, len(view.Windows))
	for _, window := range view.Windows {
		key := window.ID + "|" + window.Scope + "|" + window.Label
		if !dedup[key] {
			dedup[key] = true
			windows = append(windows, window)
		}
	}
	view.Windows = windows
	return view, nil
}
func quotaRequests(provider string, metadata map[string]any) ([]quotaRequest, error) {
	header := map[string]string{"Authorization": "Bearer $TOKEN$", "Accept": "application/json"}
	makeRequest := func(method, url, data string) []quotaRequest {
		return []quotaRequest{{method: method, url: url, header: header, data: data}}
	}
	switch provider {
	case "codex":
		header["User-Agent"] = "codex-tui/0.149.1"
		header["Content-Type"] = "application/json"
		if account := quotaText(metadata["account_id"]); account != "" {
			header["Chatgpt-Account-Id"] = account
		}
		return makeRequest("GET", "https://chatgpt.com/backend-api/wham/usage", ""), nil
	case "claude":
		header["User-Agent"] = "claude-cli/2.1.280 (external, cli)"
		header["anthropic-beta"] = "oauth-2025-04-20"
		return makeRequest("GET", "https://api.anthropic.com/api/oauth/usage", ""), nil
	case "kimi", "kimi-ai":
		domain := strings.ToLower(quotaText(metadata["domain"]))
		endpoint := "https://api.kimi.com/coding/v1/usages"
		international := provider == "kimi-ai"
		if domain != "" {
			international = domain == "ai" || domain == "kimi.ai" || domain == "kimi-ai" || strings.HasSuffix(domain, ".kimi.ai")
		} else {
			if base, err := url.Parse(quotaText(metadata["base_url"])); err == nil {
				host := strings.ToLower(base.Hostname())
				if host == "kimi.ai" || strings.HasSuffix(host, ".kimi.ai") {
					international = true
				} else if host == "kimi.com" || strings.HasSuffix(host, ".kimi.com") {
					international = false
				}
			}
		}
		if international {
			endpoint = "https://api.kimi.ai/coding/v1/usages"
		}
		return makeRequest("GET", endpoint, ""), nil
	case "antigravity":
		project := quotaText(metadata["project_id"])
		if project == "" {
			return nil, errors.New("missing_project")
		}
		body, _ := json.Marshal(map[string]string{"project": project})
		header["Content-Type"] = "application/json"
		header["User-Agent"] = "antigravity/hub/2.23.0 darwin/arm64"
		requests := make([]quotaRequest, 0, 3)
		for _, host := range []string{"daily-cloudcode-pa.googleapis.com", "daily-cloudcode-pa.sandbox.googleapis.com", "cloudcode-pa.googleapis.com"} {
			requests = append(requests, quotaRequest{method: "POST", url: "https://" + host + "/v1internal:retrieveUserQuotaSummary", header: header, data: string(body)})
		}
		return requests, nil
	case "devin":
		header = map[string]string{"Content-Type": "application/json", "Connect-Protocol-Version": "1"}
		return []quotaRequest{{method: "POST", url: "https://server.codeium.com/exa.seat_management_pb.SeatManagementService/GetUserStatus", header: header,
			data: `{"metadata":{"ideName":"chisel","ideVersion":"3000.10.21","apiKey":"$TOKEN$","locale":"en","os":"darwin","extensionVersion":"3000.10.21","clientName":"chisel"}}`}}, nil
	case "meta":
		token, _ := metadata["dca_token"].(string)
		token = strings.TrimSpace(token)
		if !strings.HasPrefix(token, "dca:") || len(token) > 16384 || strings.ContainsAny(token, "\r\n\t ") {
			return nil, errors.New("missing_dca_token")
		}
		header["Authorization"] = "Bearer " + token
		header["Content-Type"] = "application/json"
		header["x-api-version"] = "1.0.0"
		return makeRequest("POST", "https://api.meta.ai/muse-code/key", "{}"), nil
	case "xai":
		header["x-xai-token-auth"] = "xai-grok-cli"
		header["x-grok-client-version"] = "1.0.46"
		header["User-Agent"] = "grok-pager/1.0.46 grok-shell/1.0.46 (macos; aarch64)"
		user := quotaText(quotaPick(metadata, "sub", "subject", "user_id", "userId"))
		if user == "" {
			user = quotaText(quotaPick(quotaObject(metadata["oauth"]), "sub", "subject", "user_id", "userId"))
		}
		if user == "" {
			user = quotaText(quotaPick(quotaObject(metadata["user"]), "sub", "id"))
		}
		if user != "" {
			header["x-userid"] = user
		}
		return []quotaRequest{{method: "GET", url: "https://cli-chat-proxy.grok.com/v1/billing?format=credits", header: header},
			{method: "GET", url: "https://cli-chat-proxy.grok.com/v1/billing", header: header}}, nil
	}
	return nil, fmt.Errorf("unsupported_provider")
}
