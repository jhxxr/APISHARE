package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	xauthFlowCookie        = "apishare_xauth_flow"
	xauthUserSessionCookie = "apishare_user_session"
	xauthUserRequestHeader = "X-APIShare-Request"
	xauthFlowLifetime      = 10 * time.Minute
	xauthSessionLifetime   = 7 * 24 * time.Hour
)

type xauthUserInfo struct {
	Sub               string `json:"sub"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	Email             string `json:"email"`
	EmailVerified     bool   `json:"email_verified"`
}

type xauthUserRecord struct {
	Sub         string `json:"sub"`
	Email       string `json:"email"`
	Name        string `json:"name"`
	KeyID       int64  `json:"key_id"`
	GiftClaimed bool   `json:"gift_claimed"`
	CreatedAt   string `json:"created_at"`
	LastLoginAt string `json:"last_login_at"`
}

func xauthConfigured(s AppSettings) bool {
	return s.XAuthEnabled &&
		strings.TrimSpace(s.XAuthIssuer) != "" &&
		strings.TrimSpace(s.XAuthClientID) != ""
}

func normalizeXAuthIssuer(raw string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return "", nil
	}
	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("XAuth Issuer 地址需为有效的 http:// 或 https:// URL")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("XAuth Issuer 地址不能包含查询参数或锚点")
	}
	return trimmed, nil
}

func xauthRequestOrigin(c *gin.Context) string {
	scheme := "http"
	if c.Request.TLS != nil || strings.EqualFold(strings.TrimSpace(c.GetHeader("X-Forwarded-Proto")), "https") {
		scheme = "https"
	}
	host := strings.TrimSpace(c.GetHeader("X-Forwarded-Host"))
	if host != "" {
		if idx := strings.IndexByte(host, ','); idx >= 0 {
			host = strings.TrimSpace(host[:idx])
		}
	}
	if host == "" {
		host = c.Request.Host
	}
	return scheme + "://" + host
}

func xauthRedirectURI(c *gin.Context) string {
	return xauthRequestOrigin(c) + "/api/auth/callback"
}

func xauthFlowSignKey() []byte {
	secret, _ := settingsGet("session_secret")
	if secret == "" {
		secret = "apishare-xauth-default-flow-secret"
	}
	return []byte("xauth-flow:" + secret)
}

func makeXAuthPKCE() (verifier, challenge string, err error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", "", err
	}
	// 32 bytes raw URL base64 -> 43 chars matching ^[A-Za-z0-9_-]{43}$ required by XAuth.
	verifier = base64.RawURLEncoding.EncodeToString(buf[:])
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func encodeXAuthFlowCookie(state, verifier string, exp int64) string {
	payload := fmt.Sprintf("%s|%s|%d", state, verifier, exp)
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	mac := hmac.New(sha256.New, xauthFlowSignKey())
	mac.Write([]byte(encoded))
	sig := hex.EncodeToString(mac.Sum(nil))
	return encoded + "." + sig
}

func decodeXAuthFlowCookie(raw, wantState string) (verifier string, ok bool) {
	parts := strings.SplitN(raw, ".", 2)
	if len(parts) != 2 || wantState == "" {
		return "", false
	}
	mac := hmac.New(sha256.New, xauthFlowSignKey())
	mac.Write([]byte(parts[0]))
	wantSig := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(wantSig), []byte(parts[1])) {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	fields := strings.SplitN(string(decoded), "|", 3)
	if len(fields) != 3 {
		return "", false
	}
	state, v, expStr := fields[0], fields[1], fields[2]
	if state != wantState || v == "" {
		return "", false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	return v, true
}

func setXAuthFlowCookie(c *gin.Context, value string, maxAge int) {
	secure := c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https")
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(xauthFlowCookie, value, maxAge, "/api/auth", "", secure, true)
}

func setXAuthUserSessionCookie(c *gin.Context, token string, maxAge int) {
	secure := c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https")
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(xauthUserSessionCookie, token, maxAge, "/", "", secure, true)
}

func xauthSessionTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func validXAuthUserMutation(c *gin.Context) bool {
	h := c.GetHeader(xauthUserRequestHeader)
	return h == "user-console" || h == "key-view"
}

func xauthHTTPClient() *http.Client {
	return &http.Client{Timeout: 12 * time.Second}
}

func exchangeXAuthCode(ctx context.Context, s AppSettings, code, redirectURI, verifier string) (accessToken, idToken string, err error) {
	issuer := strings.TrimRight(strings.TrimSpace(s.XAuthIssuer), "/")
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	secret := strings.TrimSpace(s.XAuthClientSecret)
	clientID := strings.TrimSpace(s.XAuthClientID)
	if secret == "" {
		form.Set("client_id", clientID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if secret != "" {
		req.SetBasicAuth(clientID, secret)
	}
	resp, err := xauthHTTPClient().Do(req)
	if err != nil {
		return "", "", fmt.Errorf("连接认证服务失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var tokResp struct {
		AccessToken      string `json:"access_token"`
		IDToken          string `json:"id_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &tokResp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || tokResp.AccessToken == "" {
		msg := tokResp.ErrorDescription
		if msg == "" {
			msg = tokResp.Error
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return "", "", fmt.Errorf("令牌交换失败 (%s)", msg)
	}
	return tokResp.AccessToken, tokResp.IDToken, nil
}

func fetchXAuthUserInfo(ctx context.Context, s AppSettings, accessToken, idToken string) (*xauthUserInfo, error) {
	issuer := strings.TrimRight(strings.TrimSpace(s.XAuthIssuer), "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/userinfo", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := xauthHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("获取用户信息失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("获取用户信息失败 (HTTP %d)", resp.StatusCode)
	}
	var info xauthUserInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("解析用户信息失败: %w", err)
	}
	if strings.TrimSpace(info.Sub) == "" && idToken != "" {
		if fallback, ok := parseIDTokenClaims(idToken); ok {
			if info.Sub == "" {
				info.Sub = fallback.Sub
			}
			if info.Email == "" {
				info.Email = fallback.Email
			}
			if info.Name == "" {
				info.Name = fallback.Name
			}
		}
	}
	info.Sub = strings.TrimSpace(info.Sub)
	info.Email = strings.TrimSpace(info.Email)
	info.Name = strings.TrimSpace(info.Name)
	if info.Name == "" {
		info.Name = strings.TrimSpace(info.PreferredUsername)
	}
	if info.Sub == "" {
		return nil, fmt.Errorf("认证服务未返回有效的用户标识 (sub)")
	}
	return &info, nil
}

func parseIDTokenClaims(idToken string) (xauthUserInfo, bool) {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return xauthUserInfo{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return xauthUserInfo{}, false
	}
	var info xauthUserInfo
	if json.Unmarshal(payload, &info) != nil {
		return xauthUserInfo{}, false
	}
	return info, info.Sub != ""
}

func xauthDisplayName(info *xauthUserInfo) string {
	if info.Name != "" {
		return info.Name
	}
	if info.Email != "" {
		if idx := strings.IndexByte(info.Email, '@'); idx > 0 {
			return info.Email[:idx]
		}
		return info.Email
	}
	if len(info.Sub) > 12 {
		return info.Sub[:12]
	}
	return info.Sub
}

func xauthDefaultKeyName(info *xauthUserInfo) string {
	display := xauthDisplayName(info)
	if info.Email != "" && info.Email != display {
		return fmt.Sprintf("XAuth · %s (%s)", display, info.Email)
	}
	return fmt.Sprintf("XAuth · %s", display)
}

func buildXAuthDefaultModelQuotas(models string, configured map[string]ModelQuota) map[string]ModelQuota {
	out := make(map[string]ModelQuota)
	if len(configured) == 0 {
		return out
	}
	allowed := make(map[string]bool)
	for _, m := range keyModelIDs(models) {
		allowed[m] = true
	}
	for m, q := range configured {
		if allowed[m] && validQuota(q.QuotaUSD) {
			out[m] = ModelQuota{QuotaUSD: q.QuotaUSD}
		}
	}
	return out
}

// provisionOrLoginXAuthUser provisions a new key on first login, preserves existing
// keys on repeat login, and recreates a 0-quota key if the admin deleted the previous key.
func provisionOrLoginXAuthUser(info *xauthUserInfo, s AppSettings) (*xauthUserRecord, *ApiKey, error) {
	now := nowStr()
	var existing xauthUserRecord
	var giftClaimed int
	err := db.QueryRow(`SELECT sub,email,name,key_id,gift_claimed,created_at,last_login_at FROM xauth_users WHERE sub=?`, info.Sub).
		Scan(&existing.Sub, &existing.Email, &existing.Name, &existing.KeyID, &giftClaimed, &existing.CreatedAt, &existing.LastLoginAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	isNewUser := errors.Is(err, sql.ErrNoRows)
	existing.GiftClaimed = giftClaimed == 1

	qps := s.XAuthDefaultQPS
	if qps <= 0 {
		qps = 3
	}
	conc := s.XAuthDefaultConcurrency
	if conc <= 0 {
		conc = 5
	}
	display := xauthDisplayName(info)

	if !isNewUser && existing.KeyID > 0 {
		ak, keyErr := dbGetKeyByID(existing.KeyID)
		if keyErr == nil {
			if !ak.Enabled {
				return nil, nil, fmt.Errorf("您的专属密钥已被管理员停用，请联系管理员")
			}
			_, err := db.Exec(`UPDATE xauth_users SET email=?,name=?,last_login_at=? WHERE sub=?`,
				info.Email, display, now, info.Sub)
			if err != nil {
				return nil, nil, err
			}
			existing.Email = info.Email
			existing.Name = display
			existing.LastLoginAt = now
			ak.XAuthUser = &XAuthKeyUser{
				Sub:         info.Sub,
				Email:       info.Email,
				Name:        display,
				LastLoginAt: now,
			}
			return &existing, ak, nil
		}
		if !errors.Is(keyErr, sql.ErrNoRows) {
			return nil, nil, keyErr
		}
	}

	// Either a brand-new user (grant initial gift) or a returning user whose key was deleted by admin (reset quota to 0).
	ak := &ApiKey{
		Name:        xauthDefaultKeyName(info),
		Key:         "sk-" + randHex(24),
		QPS:         qps,
		Concurrency: conc,
		Enabled:     true,
	}
	identityNote := info.Email
	if identityNote == "" {
		identityNote = info.Sub
	}
	if !isNewUser && existing.GiftClaimed {
		// Admin deleted the user's previous key: recreate key with 0 usable quota (no bound models and 0 shared quota).
		ak.QuotaUSD = 0
		ak.Models = ""
		ak.ModelQuotas = map[string]ModelQuota{}
		ak.Note = fmt.Sprintf("XAuth 重新生成（原密钥已删除，额度已置 0）· %s", identityNote)
	} else {
		quota := s.XAuthDefaultQuotaUSD
		if !validQuota(quota) {
			quota = 0
		}
		models := normalizeKeyModels(s.XAuthDefaultModels)
		ak.QuotaUSD = quota
		ak.Models = models
		ak.ModelQuotas = buildXAuthDefaultModelQuotas(models, s.XAuthDefaultModelQuotas)
		ak.Note = fmt.Sprintf("XAuth 自动开通 · %s", identityNote)
	}

	if err := dbInsertKey(ak); err != nil {
		return nil, nil, err
	}
	createdAt := existing.CreatedAt
	if createdAt == "" {
		createdAt = now
	}
	_, err = db.Exec(`INSERT INTO xauth_users(sub,email,name,key_id,gift_claimed,created_at,last_login_at)
		VALUES(?,?,?,?,1,?,?)
		ON CONFLICT(sub) DO UPDATE SET email=excluded.email,name=excluded.name,key_id=excluded.key_id,gift_claimed=1,last_login_at=excluded.last_login_at`,
		info.Sub, info.Email, display, ak.ID, createdAt, now)
	if err != nil {
		return nil, nil, err
	}
	rec := &xauthUserRecord{
		Sub:         info.Sub,
		Email:       info.Email,
		Name:        display,
		KeyID:       ak.ID,
		GiftClaimed: true,
		CreatedAt:   createdAt,
		LastLoginAt: now,
	}
	ak, err = dbGetKeyByID(ak.ID)
	if err != nil {
		return nil, nil, err
	}
	return rec, ak, nil
}

func createXAuthUserSession(sub string, keyID int64, oldToken string) (string, error) {
	token := randHex(32)
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if _, err := tx.Exec(`DELETE FROM xauth_user_sessions WHERE expires_at<=?`, now); err != nil {
		return "", err
	}
	if oldToken != "" {
		if _, err := tx.Exec(`DELETE FROM xauth_user_sessions WHERE token_hash=?`, xauthSessionTokenHash(oldToken)); err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(`INSERT INTO xauth_user_sessions(token_hash,sub,key_id,expires_at) VALUES(?,?,?,?)`,
		xauthSessionTokenHash(token), sub, keyID, time.Now().Add(xauthSessionLifetime).Unix()); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return token, nil
}

func authenticateXAuthSession(c *gin.Context) (*xauthUserRecord, *ApiKey, error) {
	token, _ := c.Cookie(xauthUserSessionCookie)
	if token == "" {
		return nil, nil, sql.ErrNoRows
	}
	hash := xauthSessionTokenHash(token)
	var sub string
	var sessionKeyID int64
	err := db.QueryRow(`SELECT sub,key_id FROM xauth_user_sessions WHERE token_hash=? AND expires_at>?`,
		hash, time.Now().Unix()).Scan(&sub, &sessionKeyID)
	if err != nil {
		return nil, nil, err
	}
	var rec xauthUserRecord
	var gift int
	err = db.QueryRow(`SELECT sub,email,name,key_id,gift_claimed,created_at,last_login_at FROM xauth_users WHERE sub=?`, sub).
		Scan(&rec.Sub, &rec.Email, &rec.Name, &rec.KeyID, &gift, &rec.CreatedAt, &rec.LastLoginAt)
	if err != nil {
		_, _ = db.Exec(`DELETE FROM xauth_user_sessions WHERE token_hash=?`, hash)
		return nil, nil, err
	}
	rec.GiftClaimed = gift == 1
	keyID := rec.KeyID
	if keyID <= 0 {
		keyID = sessionKeyID
	}
	ak, err := dbGetKeyByID(keyID)
	if err != nil || !ak.Enabled {
		_, _ = db.Exec(`DELETE FROM xauth_user_sessions WHERE token_hash=?`, hash)
		if err == nil {
			return nil, nil, sql.ErrNoRows
		}
		return nil, nil, err
	}
	return &rec, ak, nil
}

func redirectAuthError(c *gin.Context, msg string) {
	c.Redirect(http.StatusFound, "/?auth_error="+url.QueryEscape(msg))
}

func registerXAuthPublicRoutes(r *gin.Engine, publicSub fs.FS) {
	r.GET("/user", func(c *gin.Context) {
		c.Redirect(http.StatusMovedPermanently, "/user/")
	})
	r.GET("/user/", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if adminSetupRequired() {
			c.Redirect(http.StatusTemporaryRedirect, "/setup/")
			return
		}
		serveAdminFile(c, publicSub, "user.html")
	})

	authGroup := r.Group("/api/auth")
	authGroup.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Next()
	})

	authGroup.GET("/status", func(c *gin.Context) {
		s := getSettings()
		enabled := xauthConfigured(s)
		rec, ak, err := authenticateXAuthSession(c)
		if err != nil || rec == nil || ak == nil {
			c.JSON(http.StatusOK, gin.H{
				"enabled":       enabled,
				"authenticated": false,
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"enabled":       enabled,
			"authenticated": true,
			"user": gin.H{
				"sub":        rec.Sub,
				"email":      rec.Email,
				"name":       rec.Name,
				"key_id":     ak.ID,
				"key_suffix": buildKeyOverview(ak).KeySuffix,
			},
		})
	})

	authGroup.GET("/login", func(c *gin.Context) {
		s := getSettings()
		if !xauthConfigured(s) {
			redirectAuthError(c, "当前站点未开启 Auth 登录")
			return
		}
		state := randHex(16)
		verifier, challenge, err := makeXAuthPKCE()
		if err != nil {
			redirectAuthError(c, "无法初始化登录请求，请稍后重试")
			return
		}
		exp := time.Now().Add(xauthFlowLifetime).Unix()
		setXAuthFlowCookie(c, encodeXAuthFlowCookie(state, verifier, exp), int(xauthFlowLifetime/time.Second))

		issuer := strings.TrimRight(strings.TrimSpace(s.XAuthIssuer), "/")
		q := url.Values{
			"client_id":             {strings.TrimSpace(s.XAuthClientID)},
			"redirect_uri":          {xauthRedirectURI(c)},
			"response_type":         {"code"},
			"scope":                 {"openid profile email"},
			"state":                 {state},
			"nonce":                 {"n-" + state},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
		}
		c.Redirect(http.StatusFound, issuer+"/authorize?"+q.Encode())
	})

	authGroup.GET("/callback", func(c *gin.Context) {
		s := getSettings()
		flowRaw, _ := c.Cookie(xauthFlowCookie)
		setXAuthFlowCookie(c, "", -1)

		if !xauthConfigured(s) {
			redirectAuthError(c, "当前站点未开启 Auth 登录")
			return
		}
		if errCode := strings.TrimSpace(c.Query("error")); errCode != "" {
			desc := strings.TrimSpace(c.Query("error_description"))
			switch {
			case errCode == "access_denied":
				redirectAuthError(c, "当前账号未被授权访问此应用")
			case desc != "":
				redirectAuthError(c, "认证未通过："+desc)
			default:
				redirectAuthError(c, "认证未通过 ("+errCode+")")
			}
			return
		}
		code := strings.TrimSpace(c.Query("code"))
		state := strings.TrimSpace(c.Query("state"))
		if code == "" || state == "" {
			redirectAuthError(c, "登录回调参数不完整，请重新发起登录")
			return
		}
		verifier, ok := decodeXAuthFlowCookie(flowRaw, state)
		if !ok {
			redirectAuthError(c, "登录会话已过期或状态校验失败，请重新点击登录")
			return
		}
		accessToken, idToken, err := exchangeXAuthCode(c.Request.Context(), s, code, xauthRedirectURI(c), verifier)
		if err != nil {
			redirectAuthError(c, err.Error())
			return
		}
		userInfo, err := fetchXAuthUserInfo(c.Request.Context(), s, accessToken, idToken)
		if err != nil {
			redirectAuthError(c, err.Error())
			return
		}
		rec, ak, err := provisionOrLoginXAuthUser(userInfo, s)
		if err != nil {
			redirectAuthError(c, err.Error())
			return
		}
		oldSession, _ := c.Cookie(xauthUserSessionCookie)
		sessionTok, err := createXAuthUserSession(rec.Sub, ak.ID, oldSession)
		if err != nil {
			redirectAuthError(c, "创建用户会话失败，请稍后重试")
			return
		}
		setXAuthUserSessionCookie(c, sessionTok, int(xauthSessionLifetime/time.Second))
		// Also set key_view session cookie so the homepage widget recognizes the user's key automatically.
		var kvTok [32]byte
		if _, err := rand.Read(kvTok[:]); err == nil {
			kvHex := hex.EncodeToString(kvTok[:])
			_, _ = db.Exec(`INSERT INTO key_view_sessions(token_hash,key_id,expires_at) VALUES(?,?,?)`,
				keyViewTokenHash(kvHex), ak.ID, time.Now().Add(keyViewLifetime).Unix())
			setKeyViewCookie(c, kvHex, int(keyViewLifetime/time.Second))
		}
		c.Redirect(http.StatusFound, "/user/")
	})

	authGroup.POST("/logout", func(c *gin.Context) {
		if !validXAuthUserMutation(c) {
			c.JSON(http.StatusForbidden, gin.H{"error": "请求缺少安全校验头，请刷新页面后重试"})
			return
		}
		if token, _ := c.Cookie(xauthUserSessionCookie); token != "" {
			_, _ = db.Exec(`DELETE FROM xauth_user_sessions WHERE token_hash=?`, xauthSessionTokenHash(token))
		}
		setXAuthUserSessionCookie(c, "", -1)
		if kv, _ := c.Cookie(keyViewCookie); kv != "" {
			_, _ = db.Exec(`DELETE FROM key_view_sessions WHERE token_hash=?`, keyViewTokenHash(kv))
			setKeyViewCookie(c, "", -1)
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// ---------- User Exclusive Console APIs ----------
	userAPI := r.Group("/api/user")
	userAPI.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		if c.Request.Method != http.MethodGet && !validXAuthUserMutation(c) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "请求不完整，请刷新页面后重试。"})
			return
		}
		rec, ak, err := authenticateXAuthSession(c)
		if err != nil || rec == nil || ak == nil {
			setXAuthUserSessionCookie(c, "", -1)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "登录已失效或密钥不可用，请重新通过 Auth 登录。"})
			return
		}
		c.Set("xauthUser", *rec)
		c.Set("xauthKey", *ak)
		c.Next()
	})

	userAPI.GET("/dashboard", func(c *gin.Context) {
		rec := c.MustGet("xauthUser").(xauthUserRecord)
		ak := c.MustGet("xauthKey").(ApiKey)
		dash, err := buildUserDashboard(beijingNow(), &rec, &ak)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "暂时无法加载个人控制台数据，请稍后重试。"})
			return
		}
		c.JSON(http.StatusOK, dash)
	})

	userAPI.POST("/key/regenerate", func(c *gin.Context) {
		rec := c.MustGet("xauthUser").(xauthUserRecord)
		ak := c.MustGet("xauthKey").(ApiKey)
		newKeyStr := "sk-" + randHex(24)
		tx, err := db.Begin()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "暂时无法重置密钥，请稍后重试。"})
			return
		}
		defer tx.Rollback()
		res, err := tx.Exec(`UPDATE api_keys SET key=? WHERE id=? AND enabled=1`, newKeyStr, ak.ID)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "重置密钥失败，请稍后重试。"})
			return
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "密钥已停用或不存在。"})
			return
		}
		// Invalidate old key_view_sessions for this key so leaked old keys lose viewer access too.
		_, _ = tx.Exec(`DELETE FROM key_view_sessions WHERE key_id=?`, ak.ID)
		if err := tx.Commit(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "保存新密钥失败，请稍后重试。"})
			return
		}
		updated, err := dbGetKeyByID(ak.ID)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "读取新密钥失败。"})
			return
		}
		_ = rec
		c.JSON(http.StatusOK, gin.H{
			"ok":       true,
			"key":      updated.Key,
			"overview": buildKeyOverview(updated),
		})
	})

	userAPI.GET("/logs", func(c *gin.Context) {
		ak := c.MustGet("xauthKey").(ApiKey)
		values := make(url.Values)
		for k, v := range c.Request.URL.Query() {
			values[k] = v
		}
		// Force key_id to the logged-in user's own key and strip upstream_id filter.
		values.Set("key_id", strconv.FormatInt(ak.ID, 10))
		values.Del("upstream_id")
		values.Del("export")
		query, err := parseLogQuery(values)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		result, err := queryLogs(c.Request.Context(), query)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "无法读取调用记录"})
			return
		}
		// Redact internal upstream details from user-facing logs.
		for i := range result.Items {
			result.Items[i].UpstreamID = 0
			result.Items[i].UpstreamName = ""
		}
		c.JSON(http.StatusOK, result)
	})
}

type userDailyPoint struct {
	Date             string  `json:"date"`
	Calls            int64   `json:"calls"`
	Successes        int64   `json:"successes"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

type userModelUsage struct {
	Model            string  `json:"model"`
	Family           string  `json:"family"`
	Calls            int64   `json:"calls"`
	Successes        int64   `json:"successes"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	LastCall         string  `json:"last_call"`
	Bound            bool    `json:"bound"`
	QuotaMode        string  `json:"quota_mode,omitempty"`
	Pool             *KeyQuotaPool `json:"pool,omitempty"`
}

type userDashboardResponse struct {
	UpdatedAt string          `json:"updated_at"`
	User      xauthUserRecord `json:"user"`
	Key       struct {
		ID            int64                 `json:"id"`
		Name          string                `json:"name"`
		Key           string                `json:"key"`
		KeyMasked     string                `json:"key_masked"`
		Enabled       bool                  `json:"enabled"`
		QPS           float64               `json:"qps"`
		Concurrency   int                   `json:"concurrency"`
		QuotaUSD      float64               `json:"quota_usd"`
		UsedUSD       float64               `json:"used_usd"`
		SharedUsedUSD float64               `json:"shared_used_usd"`
		Models        []string              `json:"models"`
		ModelQuotas   map[string]ModelQuota `json:"model_quotas"`
		CreatedAt     string                `json:"created_at"`
	} `json:"key"`
	Overview   *KeyOverview     `json:"overview"`
	Summary    struct {
		Calls            int64   `json:"calls"`
		Successes        int64   `json:"successes"`
		PromptTokens     int64   `json:"prompt_tokens"`
		CompletionTokens int64   `json:"completion_tokens"`
		CostUSD          float64 `json:"cost_usd"`
		TodayCalls       int64   `json:"today_calls"`
		TodayCostUSD     float64 `json:"today_cost_usd"`
	} `json:"summary"`
	Daily      []userDailyPoint `json:"daily"`
	ModelUsage []userModelUsage `json:"model_usage"`
}

func buildUserDashboard(now time.Time, rec *xauthUserRecord, ak *ApiKey) (*userDashboardResponse, error) {
	now = now.In(beijingLocation)
	overview := buildKeyOverview(ak)
	boundModels := keyModelIDs(ak.Models)
	if boundModels == nil {
		boundModels = []string{}
	}
	out := &userDashboardResponse{
		UpdatedAt:  now.Format(time.RFC3339),
		User:       *rec,
		Overview:   overview,
		Daily:      make([]userDailyPoint, 30),
		ModelUsage: []userModelUsage{},
	}
	out.Key.ID = ak.ID
	out.Key.Name = ak.Name
	out.Key.Key = ak.Key
	out.Key.KeyMasked = maskKey(ak.Key)
	out.Key.Enabled = ak.Enabled
	out.Key.QPS = ak.QPS
	out.Key.Concurrency = ak.Concurrency
	out.Key.QuotaUSD = ak.QuotaUSD
	out.Key.UsedUSD = ak.UsedUSD
	out.Key.SharedUsedUSD = ak.SharedUsedUSD
	out.Key.Models = boundModels
	out.Key.ModelQuotas = ak.ModelQuotas
	out.Key.CreatedAt = ak.CreatedAt

	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, beijingLocation)
	todayKey := today.Format("2006-01-02")
	dayIndex := make(map[string]int, 30)
	for i := range out.Daily {
		d := today.AddDate(0, 0, i-29).Format("2006-01-02")
		out.Daily[i].Date = d
		dayIndex[d] = i
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	byModel := make(map[string]*userModelUsage)
	ensureModel := func(m string) *userModelUsage {
		if item, ok := byModel[m]; ok {
			return item
		}
		item := &userModelUsage{
			Model:  m,
			Family: publicFamily(m, ""),
		}
		byModel[m] = item
		return item
	}
	for _, m := range boundModels {
		item := ensureModel(m)
		item.Bound = true
		if mq, ok := overview.Models[m]; ok {
			item.QuotaMode = mq.Mode
			if mq.Mode == "independent" {
				item.Pool = mq.Pool
			} else if overview.Shared != nil {
				item.Pool = overview.Shared
			}
		}
	}

	err = publicRows(tx, `SELECT model,COUNT(*),SUM(CASE WHEN status BETWEEN 200 AND 299 THEN 1 ELSE 0 END),
		COALESCE(SUM(prompt_tokens),0),COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(CASE WHEN status BETWEEN 200 AND 299 THEN cost_usd ELSE 0 END),0),
		COALESCE(MAX(created_at),'')
		FROM call_logs WHERE key_id=? GROUP BY model
		UNION ALL
		SELECT model,SUM(calls),SUM(successes),COALESCE(SUM(prompt_tokens),0),COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(cost_usd),0),COALESCE(MAX(last_at),'')
		FROM call_log_key_daily WHERE key_id=? GROUP BY model`,
		func(rows *sql.Rows) error {
			var m, lastAt string
			var calls, succ, pt, ct int64
			var cost float64
			if err := rows.Scan(&m, &calls, &succ, &pt, &ct, &cost, &lastAt); err != nil {
				return err
			}
			item := ensureModel(m)
			item.Calls += calls
			item.Successes += succ
			item.PromptTokens += pt
			item.CompletionTokens += ct
			item.CostUSD += cost
			if lastAt > item.LastCall {
				item.LastCall = lastAt
			}
			out.Summary.Calls += calls
			out.Summary.Successes += succ
			out.Summary.PromptTokens += pt
			out.Summary.CompletionTokens += ct
			out.Summary.CostUSD += cost
			return nil
		}, ak.ID, ak.ID)
	if err != nil {
		return nil, err
	}

	startUnix := today.AddDate(0, 0, -29).Unix()
	endUnix := today.AddDate(0, 0, 1).Unix()
	startDay := today.AddDate(0, 0, -29).Format("2006-01-02")
	endDay := today.AddDate(0, 0, 1).Format("2006-01-02")
	err = publicRows(tx, `SELECT date(created_at,'+8 hours'),COUNT(*),
		SUM(CASE WHEN status BETWEEN 200 AND 299 THEN 1 ELSE 0 END),
		COALESCE(SUM(prompt_tokens),0),COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(CASE WHEN status BETWEEN 200 AND 299 THEN cost_usd ELSE 0 END),0)
		FROM call_logs WHERE key_id=? AND unixepoch(created_at)>=? AND unixepoch(created_at)<?
		GROUP BY date(created_at,'+8 hours')
		UNION ALL
		SELECT day,SUM(calls),SUM(successes),COALESCE(SUM(prompt_tokens),0),COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(cost_usd),0)
		FROM call_log_key_daily WHERE key_id=? AND day>=? AND day<? GROUP BY day`,
		func(rows *sql.Rows) error {
			var p userDailyPoint
			if err := rows.Scan(&p.Date, &p.Calls, &p.Successes, &p.PromptTokens, &p.CompletionTokens, &p.CostUSD); err != nil {
				return err
			}
			if idx, ok := dayIndex[p.Date]; ok {
				slot := &out.Daily[idx]
				slot.Calls += p.Calls
				slot.Successes += p.Successes
				slot.PromptTokens += p.PromptTokens
				slot.CompletionTokens += p.CompletionTokens
				slot.CostUSD += p.CostUSD
			}
			if p.Date == todayKey {
				out.Summary.TodayCalls += p.Calls
				out.Summary.TodayCostUSD += p.CostUSD
			}
			return nil
		}, ak.ID, startUnix, endUnix, ak.ID, startDay, endDay)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	for _, item := range byModel {
		out.ModelUsage = append(out.ModelUsage, *item)
	}
	sort.Slice(out.ModelUsage, func(i, j int) bool {
		a, b := out.ModelUsage[i], out.ModelUsage[j]
		if a.Bound != b.Bound {
			return a.Bound
		}
		if a.Calls != b.Calls {
			return a.Calls > b.Calls
		}
		return a.Model < b.Model
	})
	return out, nil
}

// ---------- Admin XAuth & Batch Key Model Routes ----------

func registerXAuthAdminRoutes(auth *gin.RouterGroup) {
	// Test XAuth connectivity via OIDC Discovery (/.well-known/openid-configuration)
	auth.POST("/settings/xauth/test", func(c *gin.Context) {
		var req struct {
			Issuer   string `json:"xauth_issuer"`
			ClientID string `json:"xauth_client_id"`
		}
		_ = c.ShouldBindJSON(&req)
		s := getSettings()
		issuerRaw := strings.TrimSpace(req.Issuer)
		if issuerRaw == "" {
			issuerRaw = s.XAuthIssuer
		}
		issuer, err := normalizeXAuthIssuer(issuerRaw)
		if err != nil || issuer == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "请先填写有效的 XAuth Issuer 地址"})
			return
		}
		start := time.Now()
		httpReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": err.Error()})
			return
		}
		resp, err := xauthHTTPClient().Do(httpReq)
		latency := time.Since(start).Milliseconds()
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"ok": false, "latency_ms": latency, "error": "连接失败: " + err.Error()})
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
		if resp.StatusCode != http.StatusOK {
			c.JSON(http.StatusOK, gin.H{"ok": false, "latency_ms": latency, "error": fmt.Sprintf("认证发现端点返回 HTTP %d", resp.StatusCode)})
			return
		}
		var disco struct {
			Issuer                string `json:"issuer"`
			AuthorizationEndpoint string `json:"authorization_endpoint"`
			TokenEndpoint         string `json:"token_endpoint"`
			UserinfoEndpoint      string `json:"userinfo_endpoint"`
		}
		if err := json.Unmarshal(body, &disco); err != nil || disco.AuthorizationEndpoint == "" || disco.TokenEndpoint == "" {
			c.JSON(http.StatusOK, gin.H{"ok": false, "latency_ms": latency, "error": "返回内容不是有效的 OIDC Discovery 配置"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"ok":                     true,
			"latency_ms":             latency,
			"issuer":                 disco.Issuer,
			"authorization_endpoint": disco.AuthorizationEndpoint,
			"token_endpoint":         disco.TokenEndpoint,
			"userinfo_endpoint":      disco.UserinfoEndpoint,
			"redirect_uri":           xauthRedirectURI(c),
		})
	})

	// Batch add models to keys (existing models on a key are skipped, missing models are added).
	auth.POST("/keys/batch-models", func(c *gin.Context) {
		var req struct {
			Scope       string                `json:"scope"` // "selected" | "xauth" | "all"
			KeyIDs      []int64               `json:"key_ids"`
			Models      string                `json:"models"`
			ModelQuotas map[string]ModelQuota `json:"model_quotas"`
		}
		if c.BindJSON(&req) != nil {
			return
		}
		normalized := normalizeKeyModels(req.Models)
		addList := keyModelIDs(normalized)
		if len(addList) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请至少选择或输入一个要追加的模型"})
			return
		}
		for m, q := range req.ModelQuotas {
			if !validQuota(q.QuotaUSD) {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("模型 %q 的独立额度必须是大于或等于 0 的有效金额", m)})
				return
			}
		}
		allKeys, err := dbListKeys()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		selectedSet := make(map[int64]bool, len(req.KeyIDs))
		for _, id := range req.KeyIDs {
			selectedSet[id] = true
		}
		scope := strings.TrimSpace(req.Scope)
		if scope == "" {
			if len(selectedSet) > 0 {
				scope = "selected"
			} else {
				scope = "all"
			}
		}
		var targets []ApiKey
		for _, k := range allKeys {
			switch scope {
			case "selected":
				if selectedSet[k.ID] {
					targets = append(targets, k)
				}
			case "xauth":
				if k.XAuthUser != nil {
					targets = append(targets, k)
				}
			case "all":
				targets = append(targets, k)
			default:
				c.JSON(http.StatusBadRequest, gin.H{"error": "批量作用范围不正确"})
				return
			}
		}
		if len(targets) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "没有符合筛选条件的目标密钥"})
			return
		}

		updatedKeys := 0
		skippedKeys := 0
		addedModelsTotal := 0
		for _, ak := range targets {
			existing := keyModelIDs(ak.Models)
			has := make(map[string]bool, len(existing))
			for _, m := range existing {
				has[m] = true
			}
			if ak.ModelQuotas == nil {
				ak.ModelQuotas = make(map[string]ModelQuota)
			}
			addedForThisKey := 0
			for _, m := range addList {
				if has[m] {
					// Already present on this key -> skip and preserve existing quota mode/amount!
					continue
				}
				existing = append(existing, m)
				has[m] = true
				addedForThisKey++
				if req.ModelQuotas != nil {
					if q, ok := req.ModelQuotas[m]; ok {
						ak.ModelQuotas[m] = ModelQuota{QuotaUSD: q.QuotaUSD}
					}
				}
			}
			if addedForThisKey == 0 {
				skippedKeys++
				continue
			}
			ak.Models = normalizeKeyModels(strings.Join(existing, ","))
			if err := validateKeyQuotas(&ak); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			if err := dbUpdateKey(&ak); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			updatedKeys++
			addedModelsTotal += addedForThisKey
		}
		c.JSON(http.StatusOK, gin.H{
			"ok":            true,
			"matched_keys":  len(targets),
			"updated_keys":  updatedKeys,
			"skipped_keys":  skippedKeys,
			"added_models":  addedModelsTotal,
		})
	})
}
