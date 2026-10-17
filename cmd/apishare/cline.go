package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Protocol references: cline/cline and luawei1/cline2api (auth.go,
// models_sync.go and proxy.go). Account credentials are distinct from API keys:
// promotional models require an account token, sent with the workos: prefix.
const clineDefaultBase = "https://api.cline.bot/api/v1"
const clineWorkOSClientID = "client_01K3A541FN8TA3EPPHTD2325AR"

func clineCredential(raw string) (mode, value string) {
	raw = strings.TrimSpace(raw)
	for _, prefix := range []string{"api-key:", "workos:"} {
		if strings.HasPrefix(raw, prefix) {
			return strings.TrimSuffix(prefix, ":"), strings.TrimSpace(strings.TrimPrefix(raw, prefix))
		}
	}
	// Accept a single account exported by cline2api, as well as a bare refresh token.
	var account struct {
		RefreshToken string `json:"refreshToken"`
		RefreshSnake string `json:"refresh_token"`
	}
	if json.Unmarshal([]byte(raw), &account) == nil {
		if account.RefreshToken != "" {
			return "refresh", account.RefreshToken
		}
		if account.RefreshSnake != "" {
			return "refresh", account.RefreshSnake
		}
	}
	return "refresh", raw
}

type clineTokenState struct {
	Access  string
	Refresh string
	Expires int64
}

// Serialize refreshes per account, including copied upstream rows. Persist the
// rotated token separately so an unsaved probe cannot consume and lose it.
type clineTokenLockKey struct {
	database *sql.DB
	hash     string
}

var clineTokenLocks sync.Map

func (up *Upstream) clineToken(ctx context.Context, rejected string) (string, error) {
	mode, credential := clineCredential(up.APIKey)
	if credential == "" {
		return "", fmt.Errorf("Cline 凭据不能为空")
	}
	if mode == "api-key" {
		return credential, nil
	}
	if mode == "workos" {
		return "workos:" + credential, nil
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimRight(up.BaseURL, "/")+"\x00"+credential)))
	lock, _ := clineTokenLocks.LoadOrStore(clineTokenLockKey{db, hash}, make(chan struct{}, 1))
	gate := lock.(chan struct{})
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	state := clineTokenState{Refresh: credential}
	if db == nil {
		return "", fmt.Errorf("Cline 令牌存储未就绪")
	}
	err := db.QueryRowContext(ctx, `SELECT access_token,refresh_token,expires_at FROM cline_tokens WHERE credential_hash=?`, hash).Scan(&state.Access, &state.Refresh, &state.Expires)
	if err != nil && err != sql.ErrNoRows {
		return "", fmt.Errorf("无法读取 Cline 令牌")
	}
	if state.Access != "" && time.Now().Unix() < state.Expires-60 && "workos:"+state.Access != rejected {
		return "workos:" + state.Access, nil
	}
	client, err := clientForUpstream(modelsFetchClient, up.ProxyID)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var response struct {
		Data struct {
			Access  string `json:"accessToken"`
			Refresh string `json:"refreshToken"`
			Expires any    `json:"expiresAt"`
		} `json:"data"`
	}
	status, err := clineAuthJSON(ctx, client, upstreamURL(up.BaseURL, "/v1/auth/refresh"), map[string]string{"refreshToken": state.Refresh, "grantType": "refresh_token"}, &response)
	if err != nil {
		return "", err
	}
	if status == 400 || status == 401 || status == 403 {
		// Only official-host accounts may fall back to WorkOS. Never disclose
		// a custom service's credentials to a different host.
		if !strings.HasPrefix(strings.TrimRight(up.BaseURL, "/")+"/", clineDefaultBase+"/") {
			return "", fmt.Errorf("Cline 刷新凭据被拒绝（HTTP %d），请重新登录获取 RefreshToken", status)
		}
		var fallback struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
		}
		status, err = clineAuthJSON(ctx, client, "https://api.workos.com/user_management/authenticate", map[string]string{"grant_type": "refresh_token", "client_id": clineWorkOSClientID, "refresh_token": state.Refresh}, &fallback)
		if err != nil {
			return "", err
		}
		response.Data.Access, response.Data.Refresh = fallback.Access, fallback.Refresh
	}
	if status != 200 || response.Data.Access == "" {
		return "", fmt.Errorf("Cline 令牌刷新失败（HTTP %d），请检查凭据和代理", status)
	}
	state.Access = response.Data.Access
	if response.Data.Refresh != "" {
		state.Refresh = response.Data.Refresh
	}
	state.Expires = clineTokenExpiry(response.Data.Expires, state.Access)
	// Persist rotation even if the downstream disconnects after authentication.
	err = saveClineTokenState(up.BaseURL, credential, state)
	if err != nil {
		return "", fmt.Errorf("无法保存 Cline 已轮换的令牌")
	}
	return "workos:" + state.Access, nil
}

func saveClineTokenState(base, credential string, state clineTokenState) error {
	if db == nil {
		return fmt.Errorf("Cline 令牌存储未就绪")
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimRight(base, "/")+"\x00"+credential)))
	_, err := db.Exec(`INSERT INTO cline_tokens(credential_hash,access_token,refresh_token,expires_at) VALUES(?,?,?,?) ON CONFLICT(credential_hash) DO UPDATE SET access_token=excluded.access_token,refresh_token=excluded.refresh_token,expires_at=excluded.expires_at`, hash, state.Access, state.Refresh, state.Expires)
	return err
}

func clineAuthJSON(ctx context.Context, client *http.Client, endpoint string, payload any, result any) (int, error) {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("Cline 认证地址无效")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("连接 Cline 认证服务失败，请检查代理和网络")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return resp.StatusCode, nil
	}
	body, err = readProtocolBody(resp.Body, 1<<20)
	if err != nil || json.Unmarshal(body, result) != nil {
		return 0, fmt.Errorf("Cline 认证服务返回无效数据")
	}
	return resp.StatusCode, nil
}

func clineTokenExpiry(value any, token string) int64 {
	var expires int64
	switch v := value.(type) {
	case float64:
		expires = int64(v)
	case string:
		if parsed, err := time.Parse(time.RFC3339Nano, v); err == nil {
			expires = parsed.Unix()
		} else {
			expires, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	if expires > 1e12 {
		expires /= 1000
	}
	// WorkOS JWT exp is a scheduling hint only; the upstream verifies the token.
	if expires == 0 {
		parts := strings.Split(token, ".")
		if len(parts) == 3 {
			payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var claims struct {
				Exp int64 `json:"exp"`
			}
			if json.Unmarshal(payload, &claims) == nil {
				expires = claims.Exp
			}
		}
	}
	if expires == 0 {
		expires = time.Now().Add(5 * time.Minute).Unix()
	}
	return expires
}

func (up *Upstream) applyClineHeaders(req *http.Request, rules []headerRule, incoming http.Header) error {
	token, err := up.clineToken(req.Context(), "")
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("HTTP-Referer", "https://cline.bot")
	req.Header.Set("X-Title", "API Share")
	mode, _ := clineCredential(up.APIKey)
	clientHeaders := map[string]string{}
	if mode != "api-key" {
		// Match cline2api's defaultProxyConfig, including CLI identification.
		// API-key requests retain this gateway's own application identity.
		clientHeaders = map[string]string{
			"User-Agent": "Cline/3.0.47", "X-Title": "Cline",
			"X-IS-MULTIROOT": "false", "X-CLIENT-TYPE": "cline-cli",
			"X-CLIENT-VERSION": "3.0.47", "X-PLATFORM": "terminal",
			"X-PLATFORM-VERSION": "3.0.47", "X-CORE-VERSION": "0.0.66",
		}
		for name, value := range clientHeaders {
			req.Header.Set(name, value)
		}
	}
	// Protect task/client identity from broad passthrough; explicit overrides work.
	filtered := incoming.Clone()
	filtered.Del("X-Task-ID")
	filtered.Del("HTTP-Referer")
	filtered.Del("X-Title")
	for name := range clientHeaders {
		filtered.Del(name)
	}
	return applyHeaderRules(req, rules, token, filtered)
}

func retryClineUnauthorized(client *http.Client, req *http.Request, resp *http.Response, up *Upstream, send ...func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	mode, _ := clineCredential(up.APIKey)
	if up.Type != "cline" || mode != "refresh" || resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	token, err := up.clineToken(req.Context(), strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	resp.Body.Close()
	retry := req.Clone(req.Context())
	if req.GetBody != nil {
		retry.Body, err = req.GetBody()
		if err != nil {
			return nil, err
		}
	}
	retry.Header.Set("Authorization", "Bearer "+token)
	if len(send) > 0 {
		return send[0](retry)
	}
	return client.Do(retry)
}

func shapeClineRequest(body []byte, scope string, explicitSession ...string) ([]byte, string, error) {
	m, err := decodeProtocolObject(body)
	if err != nil {
		return nil, "", err
	}
	session := str(m["session_id"])
	if session == "" {
		session = str(object(m["metadata"])["session_id"])
	}
	if session == "" {
		session = str(m["conversation_id"])
	}
	if session == "" && len(explicitSession) > 0 {
		session = explicitSession[0]
	}
	if session == "" {
		for _, message := range array(m["messages"]) {
			if str(object(message)["role"]) == "user" {
				session = jsonText(message)
				break
			}
		}
	}
	session = fmt.Sprintf("%x", sha256.Sum256([]byte(scope+"\x00"+session)))[:32]
	m["session_id"] = session
	// Cline defaults to streaming when omitted; explicitly preserve client mode.
	if _, exists := m["stream"]; !exists {
		m["stream"] = false
	}
	if effort := str(m["reasoning_effort"]); effort == "none" || effort == "" {
		delete(m, "reasoning_effort")
	}
	if _, exists := m["max_tokens"]; !exists {
		if value, ok := m["max_completion_tokens"]; ok {
			m["max_tokens"] = value
		}
	}
	delete(m, "max_completion_tokens")
	if value, ok := protocolFloat(m["max_tokens"]); ok && value > 0 && value < 16 {
		m["max_tokens"] = 16
	}
	encoded, err := json.Marshal(m)
	return encoded, session, err
}

// Cline may wrap both JSON responses and individual SSE chunks in {data:...}.
// Unwrap only completion/error envelopes; preserve every native field inside.
func unwrapClineJSON(raw []byte) []byte {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || envelope["choices"] != nil {
		return raw
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(envelope["data"], &data) == nil && (data["choices"] != nil || data["error"] != nil || data["usage"] != nil) {
		return envelope["data"]
	}
	return raw
}

type clineResponseBody struct {
	source  io.ReadCloser
	reader  *bufio.Reader
	sse     bool
	pending []byte
	err     error
	limit   int64
}

func normalizeClineResponse(resp *http.Response, limits ...int64) {
	limit := int64(64 << 20)
	if len(limits) > 0 {
		limit = limits[0]
	}
	resp.Body = &clineResponseBody{source: resp.Body, reader: bufio.NewReader(resp.Body), sse: strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream"), limit: limit}
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
}

func (body *clineResponseBody) Read(output []byte) (int, error) {
	if len(output) == 0 {
		return 0, nil
	}
	for len(body.pending) == 0 && body.err == nil {
		if !body.sse {
			raw, err := readProtocolBody(body.source, body.limit)
			if err != nil {
				body.err = err
				break
			}
			body.pending, body.err = unwrapClineJSON(raw), io.EOF
		} else {
			var frame []byte
			lineStart := 0
			for {
				fragment, err := body.reader.ReadSlice('\n')
				frame = append(frame, fragment...)
				if int64(len(frame)) > body.limit {
					body.err = fmt.Errorf("Cline SSE event is too large")
					return 0, body.err
				}
				if err == bufio.ErrBufferFull {
					continue
				}
				body.err = err
				if err != nil || len(bytes.TrimSpace(frame[lineStart:])) == 0 {
					break
				}
				lineStart = len(frame)
			}
			body.pending = unwrapClineFrame(frame)
		}
	}
	if len(body.pending) == 0 {
		return 0, body.err
	}
	n := copy(output, body.pending)
	body.pending = body.pending[n:]
	return n, nil
}

func (body *clineResponseBody) Close() error { return body.source.Close() }

func unwrapClineFrame(frame []byte) []byte {
	lines := bytes.SplitAfter(frame, []byte("\n"))
	var data [][]byte
	first := -1
	for index, line := range lines {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		if first < 0 {
			first = index
		}
		value := bytes.TrimRight(line[5:], "\r\n")
		data = append(data, bytes.TrimPrefix(value, []byte(" ")))
	}
	if first < 0 {
		return frame
	}
	raw := bytes.Join(data, []byte("\n"))
	unwrapped := unwrapClineJSON(raw)
	if bytes.Equal(raw, unwrapped) {
		return frame
	}
	var compact bytes.Buffer
	if json.Compact(&compact, unwrapped) != nil {
		return frame
	}
	var result bytes.Buffer
	for index, line := range lines {
		if index == first {
			result.WriteString("data: ")
			result.Write(compact.Bytes())
			result.WriteByte('\n')
		} else if !bytes.HasPrefix(line, []byte("data:")) {
			result.Write(line)
		}
	}
	return result.Bytes()
}

func probeClineModels(ctx context.Context, up *Upstream, client *http.Client) ([]string, error) {
	base, err := url.Parse(up.BaseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("base_url must be an HTTP(S) URL without credentials, query or fragment")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	mode, _ := clineCredential(up.APIKey)
	path := "/v1/ai/cline/recommended-models"
	if mode == "api-key" {
		path = "/v1/models"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL(up.BaseURL, path), nil)
	if err != nil {
		return nil, fmt.Errorf("Cline 模型地址无效")
	}
	req.Header.Set("Accept", "application/json")
	if err := up.applyHeaders(req, nil); err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err == nil {
		resp, err = retryClineUnauthorized(client, req, resp, up)
	}
	if err != nil {
		return nil, fmt.Errorf("无法获取 Cline 模型，请检查凭据、代理和网络")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Cline models API returned HTTP %d", resp.StatusCode)
	}
	body, err := readProtocolBody(resp.Body, 8<<20)
	if err != nil {
		return nil, err
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil, fmt.Errorf("Cline 返回无效模型列表")
	}
	var models []struct {
		ID string `json:"id"`
	}
	if mode == "api-key" {
		if json.Unmarshal(root["data"], &models) != nil {
			return nil, fmt.Errorf("Cline 返回无效模型列表")
		}
	} else {
		if root["free"] == nil {
			var data map[string]json.RawMessage
			if json.Unmarshal(root["data"], &data) == nil {
				root = data
			}
		}
		if root["free"] == nil || json.Unmarshal(root["free"], &models) != nil {
			return nil, fmt.Errorf("Cline 返回无效免费模型列表")
		}
	}
	ids := make([]string, 0, len(models))
	seen := make(map[string]bool)
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id != "" && !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	sort.Strings(ids)
	return ids, nil
}
