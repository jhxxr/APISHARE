package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const clineDeviceEndpoint = "https://api.workos.com/user_management/authorize/device"
const clineAuthenticateEndpoint = "https://api.workos.com/user_management/authenticate"
const clineManualCallback = "http://127.0.0.1:48801/auth"

type clineOAuthSession struct {
	owner      [32]byte
	expires    time.Time
	cancel     context.CancelFunc
	done       bool
	err        string
	email      string
	refresh    string
	manual     bool
	exchanging bool
	client     *http.Client
}

var clineOAuthSessions = struct {
	sync.Mutex
	items map[string]*clineOAuthSession
	slots chan struct{}
}{items: make(map[string]*clineOAuthSession), slots: make(chan struct{}, 32)}

func registerClineOAuthRoutes(auth *gin.RouterGroup) {
	auth.POST("/cline/oauth/start", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		var input struct {
			ProxyID int64  `json:"proxy_id"`
			BaseURL string `json:"base_url"`
			Mode    string `json:"mode"`
		}
		if c.ShouldBindJSON(&input) != nil {
			c.JSON(400, gin.H{"error": "登录参数无效"})
			return
		}
		if input.Mode != "" && input.Mode != "device" && input.Mode != "manual" {
			c.JSON(400, gin.H{"error": "登录方式无效"})
			return
		}
		if input.BaseURL != "" && strings.TrimRight(input.BaseURL, "/") != clineDefaultBase {
			c.JSON(400, gin.H{"error": "浏览器登录仅支持 Cline 官方默认地址"})
			return
		}
		if err := validateUpstreamProxy(input.ProxyID); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		client, err := clientForUpstream(modelsFetchClient, input.ProxyID)
		if err != nil {
			c.JSON(400, gin.H{"error": "无法使用所选代理"})
			return
		}
		clineOAuthSessions.Lock()
		for id, session := range clineOAuthSessions.items {
			if time.Now().After(session.expires) {
				session.cancel()
				delete(clineOAuthSessions.items, id)
			}
		}
		full := len(clineOAuthSessions.items) >= 128
		clineOAuthSessions.Unlock()
		if full {
			c.JSON(429, gin.H{"error": "登录会话过多，请稍后重试"})
			return
		}
		if input.Mode == "manual" {
			startClineManualLogin(c, client)
			return
		}
		select {
		case clineOAuthSessions.slots <- struct{}{}:
		default:
			c.JSON(429, gin.H{"error": "正在处理的登录过多，请稍后重试"})
			return
		}
		started := false
		defer func() {
			if !started {
				<-clineOAuthSessions.slots
			}
		}()
		var device struct {
			Code        string `json:"device_code"`
			UserCode    string `json:"user_code"`
			URI         string `json:"verification_uri"`
			CompleteURI string `json:"verification_uri_complete"`
			Interval    int    `json:"interval"`
			Expires     int    `json:"expires_in"`
		}
		status, err := clineOAuthForm(c.Request.Context(), client, clineDeviceEndpoint, url.Values{"client_id": {clineWorkOSClientID}}, &device)
		if err != nil || status != 200 {
			c.JSON(502, gin.H{"error": "无法发起 Cline 登录，请检查所选代理和网络"})
			return
		}
		uri := device.CompleteURI
		if uri == "" {
			uri = device.URI
		}
		if device.Code == "" || device.UserCode == "" || !validClineVerificationURL(uri) {
			c.JSON(502, gin.H{"error": "Cline 返回无效的授权链接或验证码"})
			return
		}
		if device.Interval < 5 {
			device.Interval = 5
		}
		if device.Expires <= 0 {
			device.Expires = 300
		}
		if device.Expires > 900 {
			device.Expires = 900
		}
		var nonce [32]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			c.JSON(500, gin.H{"error": "无法生成登录会话"})
			return
		}
		id := hex.EncodeToString(nonce[:])
		expires := time.Now().Add(time.Duration(device.Expires) * time.Second)
		ctx, cancel := context.WithDeadline(context.Background(), expires)
		// Allow time to finish Cline registration after the browser grants access
		// near the device-code deadline. Polling itself still obeys that deadline.
		session := &clineOAuthSession{owner: sha256.Sum256([]byte(adminRequestToken(c))), expires: expires.Add(time.Minute), cancel: cancel}
		clineOAuthSessions.Lock()
		clineOAuthSessions.items[id] = session
		clineOAuthSessions.Unlock()
		started = true
		go runClineDeviceLogin(ctx, client, session, device.Code, time.Duration(device.Interval)*time.Second)
		c.JSON(200, gin.H{"session_id": id, "verification_uri": uri, "user_code": device.UserCode, "expires_at": expires.UTC().Format(time.RFC3339), "interval": device.Interval})
	})
	auth.POST("/cline/oauth/:session/complete", completeClineManualLogin)

	auth.GET("/cline/oauth/:session", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		clineOAuthSessions.Lock()
		session := clineOAuthSessions.items[c.Param("session")]
		if session == nil || session.owner != sha256.Sum256([]byte(adminRequestToken(c))) {
			clineOAuthSessions.Unlock()
			c.JSON(404, gin.H{"error": "登录会话不存在，请重新发起登录"})
			return
		}
		if time.Now().After(session.expires) {
			session.cancel()
			delete(clineOAuthSessions.items, c.Param("session"))
			clineOAuthSessions.Unlock()
			c.JSON(410, gin.H{"error": "登录已过期，请重新发起登录"})
			return
		}
		result := gin.H{"done": session.done}
		if session.done {
			if session.err != "" {
				result["error"] = session.err
			} else {
				result["refresh_token"], result["email"] = session.refresh, session.email
			}
		}
		clineOAuthSessions.Unlock()
		c.JSON(200, result)
	})

	auth.DELETE("/cline/oauth/:session", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		clineOAuthSessions.Lock()
		session := clineOAuthSessions.items[c.Param("session")]
		if session != nil && session.owner == sha256.Sum256([]byte(adminRequestToken(c))) {
			session.cancel()
			delete(clineOAuthSessions.items, c.Param("session"))
		}
		clineOAuthSessions.Unlock()
		c.Status(http.StatusNoContent)
	})
}

func startClineManualLogin(c *gin.Context, client *http.Client) {
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		c.JSON(500, gin.H{"error": "无法生成登录会话"})
		return
	}
	id := hex.EncodeToString(nonce[:])
	expires := time.Now().Add(10 * time.Minute)
	_, cancel := context.WithDeadline(context.Background(), expires)
	session := &clineOAuthSession{owner: sha256.Sum256([]byte(adminRequestToken(c))), expires: expires, cancel: cancel, manual: true, client: client}
	clineOAuthSessions.Lock()
	clineOAuthSessions.items[id] = session
	clineOAuthSessions.Unlock()
	query := url.Values{"client_type": {"extension"}, "callback_url": {clineManualCallback}, "redirect_uri": {clineManualCallback}}
	c.JSON(200, gin.H{"session_id": id, "mode": "manual", "verification_uri": clineDefaultBase + "/auth/authorize?" + query.Encode(), "expires_at": expires.UTC().Format(time.RFC3339)})
}

// The callback address is parsed locally and is never fetched. Keep the exact
// loopback redirect URI used to initiate this authorization when exchanging it.
func parseClineAuthorizationInput(raw string) (code, provider string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 16<<10 {
		return "", "", fmt.Errorf("请粘贴授权码或完整回调地址")
	}
	if strings.Contains(raw, "://") {
		callback, parseErr := url.Parse(raw)
		if parseErr != nil || callback.Scheme != "http" || callback.Host != "127.0.0.1:48801" || callback.Path != "/auth" || callback.User != nil {
			return "", "", fmt.Errorf("请粘贴本次授权返回的 http://127.0.0.1:48801/auth 回调地址")
		}
		values, parseErr := url.ParseQuery(callback.RawQuery)
		if parseErr != nil {
			return "", "", fmt.Errorf("回调地址格式无效，请完整复制地址栏内容")
		}
		if values.Get("error") != "" {
			return "", "", fmt.Errorf("本次授权失败或已被拒绝，请重新授权")
		}
		code, provider = values.Get("code"), values.Get("provider")
	} else {
		code = raw
	}
	if code == "" || len(code) > 8192 || strings.ContainsAny(code, "\r\n\t ") || len(provider) > 80 {
		return "", "", fmt.Errorf("未找到有效授权码；请填写授权码或包含 code 参数的完整回调地址")
	}
	return code, provider, nil
}

func completeClineManualLogin(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var input struct {
		Authorization string `json:"authorization"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	if c.ShouldBindJSON(&input) != nil {
		c.JSON(400, gin.H{"error": "回填内容无效或过长"})
		return
	}
	code, provider, err := parseClineAuthorizationInput(input.Authorization)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	clineOAuthSessions.Lock()
	session := clineOAuthSessions.items[c.Param("session")]
	if session == nil || session.owner != sha256.Sum256([]byte(adminRequestToken(c))) || !session.manual {
		clineOAuthSessions.Unlock()
		c.JSON(404, gin.H{"error": "手动授权会话不存在，请重新生成授权链接"})
		return
	}
	if time.Now().After(session.expires) {
		clineOAuthSessions.Unlock()
		c.JSON(410, gin.H{"error": "手动授权已过期，请重新生成链接并授权"})
		return
	}
	if session.done {
		result := gin.H{"done": true, "refresh_token": session.refresh, "email": session.email}
		clineOAuthSessions.Unlock()
		c.JSON(200, result)
		return
	}
	if session.exchanging {
		clineOAuthSessions.Unlock()
		c.JSON(409, gin.H{"error": "正在处理回填，请稍候"})
		return
	}
	select {
	case clineOAuthSessions.slots <- struct{}{}:
	default:
		clineOAuthSessions.Unlock()
		c.JSON(429, gin.H{"error": "正在处理的登录过多，请稍后重试"})
		return
	}
	session.exchanging = true
	clineOAuthSessions.Unlock()
	defer func() {
		clineOAuthSessions.Lock()
		session.exchanging = false
		clineOAuthSessions.Unlock()
		<-clineOAuthSessions.slots
	}()
	payload := map[string]string{"grant_type": "authorization_code", "code": code, "client_type": "extension", "redirect_uri": clineManualCallback}
	if provider != "" {
		payload["provider"] = provider
	}
	var token struct {
		Data struct {
			Access  string `json:"accessToken"`
			Refresh string `json:"refreshToken"`
			Expires any    `json:"expiresAt"`
			User    struct {
				Email string `json:"email"`
			} `json:"userInfo"`
		} `json:"data"`
	}
	// Finish and persist the one-time exchange even if the browser disconnects.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	status, err := clineAuthJSON(ctx, session.client, clineDefaultBase+"/auth/token", payload, &token)
	if err != nil {
		c.JSON(502, gin.H{"error": "连接 Cline 认证服务失败，请检查代理后重试；授权码可能需要重新获取"})
		return
	}
	if status == 429 || status >= 500 {
		c.JSON(502, gin.H{"error": fmt.Sprintf("Cline 认证服务暂时不可用（HTTP %d），请稍后重试", status)})
		return
	}
	if status == 200 && (token.Data.Access == "" || token.Data.Refresh == "") {
		c.JSON(502, gin.H{"error": "Cline 未返回完整账号凭据，请重新授权"})
		return
	}
	if status != 200 || token.Data.Access == "" || token.Data.Refresh == "" {
		c.JSON(400, gin.H{"error": "授权码无效、已使用或已过期，请重新打开授权页并粘贴新的回调地址"})
		return
	}
	state := clineTokenState{Access: token.Data.Access, Refresh: token.Data.Refresh, Expires: clineTokenExpiry(token.Data.Expires, token.Data.Access)}
	if err := saveClineTokenState(clineDefaultBase, state.Refresh, state); err != nil {
		c.JSON(500, gin.H{"error": "无法保存登录凭据，请检查数据库后重新授权"})
		return
	}
	clineOAuthSessions.Lock()
	session.done, session.refresh, session.email = true, state.Refresh, token.Data.User.Email
	clineOAuthSessions.Unlock()
	c.JSON(200, gin.H{"done": true, "refresh_token": state.Refresh, "email": token.Data.User.Email})
}

func validClineVerificationURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "cline.bot" || strings.HasSuffix(host, ".cline.bot") || host == "api.workos.com" || strings.HasSuffix(host, ".authkit.app")
}

func clineOAuthForm(ctx context.Context, client *http.Client, endpoint string, form url.Values, result any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, fmt.Errorf("授权请求无效")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("无法连接授权服务")
	}
	defer resp.Body.Close()
	body, err := readProtocolBody(resp.Body, 64<<10)
	if err != nil || json.Unmarshal(body, result) != nil {
		return resp.StatusCode, fmt.Errorf("授权服务返回无效数据")
	}
	return resp.StatusCode, nil
}

func runClineDeviceLogin(ctx context.Context, client *http.Client, session *clineOAuthSession, deviceCode string, interval time.Duration) {
	defer func() { session.cancel(); <-clineOAuthSessions.slots }()
	finish := func(refresh, email, message string) {
		clineOAuthSessions.Lock()
		session.done, session.refresh, session.email, session.err = true, refresh, email, message
		clineOAuthSessions.Unlock()
	}
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			finish("", "", "登录已取消或过期，请重新登录")
			return
		case <-timer.C:
		}
		var token struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
			Error   string `json:"error"`
		}
		status, err := clineOAuthForm(ctx, client, clineAuthenticateEndpoint, url.Values{
			"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {deviceCode}, "client_id": {clineWorkOSClientID},
		}, &token)
		if err != nil || status >= 500 || status == 429 {
			continue
		}
		if status != 200 {
			switch token.Error {
			case "authorization_pending":
				continue
			case "slow_down":
				interval += 5 * time.Second
				continue
			case "access_denied":
				finish("", "", "你已拒绝授权，可重新发起登录")
				return
			case "expired_token":
				finish("", "", "验证码已过期，请重新登录")
				return
			default:
				finish("", "", "Cline 授权失败，请重新登录")
				return
			}
		}
		if token.Access == "" || token.Refresh == "" {
			finish("", "", "授权服务未返回有效账号凭据")
			return
		}
		var registered struct {
			Data struct {
				Access  string `json:"accessToken"`
				Refresh string `json:"refreshToken"`
				Expires any    `json:"expiresAt"`
				User    struct {
					Email string `json:"email"`
				} `json:"userInfo"`
			} `json:"data"`
		}
		// Finish registration independently of the browser connection. The
		// resulting credentials are persisted before exposing them to the UI.
		registerCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		status, err = clineAuthJSON(registerCtx, client, clineDefaultBase+"/auth/register", map[string]string{"accessToken": token.Access, "refreshToken": token.Refresh}, &registered)
		cancel()
		if err != nil || status != 200 || registered.Data.Access == "" {
			finish("", "", "浏览器已授权，但 Cline 账号注册失败，请检查代理后重新登录")
			return
		}
		if registered.Data.Refresh == "" {
			registered.Data.Refresh = token.Refresh
		}
		state := clineTokenState{Access: registered.Data.Access, Refresh: registered.Data.Refresh, Expires: clineTokenExpiry(registered.Data.Expires, registered.Data.Access)}
		if err := saveClineTokenState(clineDefaultBase, state.Refresh, state); err != nil {
			finish("", "", "无法保存登录凭据，请检查网关数据库后重新登录")
			return
		}
		finish(state.Refresh, registered.Data.User.Email, "")
		return
	}
}
