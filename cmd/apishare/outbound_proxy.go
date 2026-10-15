package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

type OutboundProxy struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Username  string `json:"username"`
	Password  string `json:"-"`
	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"created_at"`
}

// Serialize configuration mutations with upstream bindings so a proxy cannot
// be removed between validation and saving an upstream.
var proxyConfigMu sync.Mutex

func normalizeOutboundProxy(p *OutboundProxy) error {
	p.Name, p.URL, p.Username = strings.TrimSpace(p.Name), strings.TrimSpace(p.URL), strings.TrimSpace(p.Username)
	if p.Name == "" || utf8.RuneCountInString(p.Name) > 120 {
		return errors.New("代理名称必填，最多 120 个字符")
	}
	u, err := url.Parse(p.URL)
	if err != nil || u.Hostname() == "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("代理地址应为 协议://主机:端口，或 协议://用户名:密码@主机:端口")
	}
	if u.User != nil {
		p.Username = u.User.Username()
		p.Password, _ = u.User.Password()
		if p.Username == "" {
			return errors.New("标准代理链接必须包含用户名")
		}
		u.User = nil
	}
	u.Scheme = strings.ToLower(u.Scheme)
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return errors.New("代理协议支持 HTTP、HTTPS、SOCKS5 和 SOCKS5h")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return errors.New("请填写有效的代理端口（1–65535）")
	}
	if p.Username == "" && p.Password != "" {
		return errors.New("填写代理密码时也需要用户名")
	}
	if (u.Scheme == "socks5" || u.Scheme == "socks5h") && (len(p.Username) > 255 || len(p.Password) > 255) {
		return errors.New("SOCKS5 用户名和密码最多 255 字节")
	}
	u.Path = ""
	p.URL = u.String()
	return nil
}

func dbGetOutboundProxy(id int64) (*OutboundProxy, error) {
	p := &OutboundProxy{}
	var enabled int
	err := db.QueryRow(`SELECT id,name,url,username,password,enabled,created_at FROM outbound_proxies WHERE id=?`, id).
		Scan(&p.ID, &p.Name, &p.URL, &p.Username, &p.Password, &enabled, &p.CreatedAt)
	p.Enabled = enabled == 1
	return p, err
}

func validateUpstreamProxy(id int64) error {
	if id == 0 {
		return nil
	}
	if id < 0 {
		return errors.New("请选择有效的代理")
	}
	p, err := dbGetOutboundProxy(id)
	if err != nil {
		return errors.New("所选代理不存在")
	}
	if !p.Enabled {
		return errors.New("所选代理已停用，请启用代理或选择其他连接方式")
	}
	return nil
}

type proxyTransportEntry struct {
	fingerprint [32]byte
	transport   *http.Transport
}

var proxyTransports = struct {
	sync.Mutex
	m map[int64]proxyTransportEntry
}{m: make(map[int64]proxyTransportEntry)}

func proxyFingerprint(p *OutboundProxy) [32]byte {
	data, _ := json.Marshal([]any{p.ID, p.URL, p.Username, p.Password, p.Enabled})
	return sha256.Sum256(data)
}

func closeProxyTransport(id int64) {
	proxyTransports.Lock()
	defer proxyTransports.Unlock()
	if entry, ok := proxyTransports.m[id]; ok {
		entry.transport.CloseIdleConnections()
		delete(proxyTransports.m, id)
	}
}

// Clone the established client settings, keeping streaming without a total
// timeout and model discovery with its existing deadline and redirect policy.
func clientForUpstream(base *http.Client, proxyID int64) (*http.Client, error) {
	if proxyID == 0 {
		return base, nil // Preserve HTTP(S)_PROXY / NO_PROXY behavior for existing deployments.
	}
	if proxyID < 0 {
		return nil, errors.New("请选择有效的代理")
	}
	p, err := dbGetOutboundProxy(proxyID)
	if err != nil {
		return nil, errors.New("所选代理不存在或无法读取")
	}
	return clientForProxyConfig(base, p)
}

func clientForProxyConfig(base *http.Client, p *OutboundProxy) (*http.Client, error) {
	if p == nil {
		return base, nil
	}
	config := *p
	p = &config
	if !p.Enabled {
		return nil, errors.New("所选代理已停用")
	}
	if err := normalizeOutboundProxy(p); err != nil {
		return nil, err
	}
	fingerprint := proxyFingerprint(p)
	proxyTransports.Lock()
	defer proxyTransports.Unlock()
	entry, exists := proxyTransports.m[p.ID]
	if !exists || entry.fingerprint != fingerprint {
		if exists {
			entry.transport.CloseIdleConnections()
		}
		transport := upstreamHTTPClient.Transport.(*http.Transport).Clone()
		endpoint, _ := url.Parse(p.URL)
		if p.Username != "" {
			endpoint.User = url.UserPassword(p.Username, p.Password)
		}
		// Go's transport supports HTTP CONNECT, HTTPS proxies and SOCKS5 with
		// remote DNS and username/password authentication. This fixed proxy is
		// used even for loopback destinations and is independent of NO_PROXY.
		transport.Proxy = http.ProxyURL(endpoint)
		entry = proxyTransportEntry{fingerprint: fingerprint, transport: transport}
		proxyTransports.m[p.ID] = entry
	}
	client := *base
	client.Transport = entry.transport
	return &client, nil
}

func registerOutboundProxyRoutes(auth *gin.RouterGroup) {
	auth.GET("/proxies", func(c *gin.Context) {
		rows, err := db.Query(`SELECT p.id,p.name,p.url,p.username,p.password,p.enabled,p.created_at,
			(SELECT COUNT(*) FROM upstreams u WHERE u.proxy_id=p.id) FROM outbound_proxies p ORDER BY p.id`)
		if err != nil {
			c.JSON(500, gin.H{"error": "无法读取代理列表"})
			return
		}
		defer rows.Close()
		type proxyView struct {
			OutboundProxy
			HasPassword   bool `json:"has_password"`
			UpstreamCount int  `json:"upstream_count"`
		}
		out := make([]proxyView, 0)
		for rows.Next() {
			var view proxyView
			var enabled int
			if err := rows.Scan(&view.ID, &view.Name, &view.URL, &view.Username, &view.Password, &enabled, &view.CreatedAt, &view.UpstreamCount); err != nil {
				c.JSON(500, gin.H{"error": "无法读取代理列表"})
				return
			}
			view.Enabled, view.HasPassword = enabled == 1, view.Password != ""
			out = append(out, view)
		}
		if rows.Err() != nil {
			c.JSON(500, gin.H{"error": "无法读取代理列表"})
			return
		}
		c.JSON(200, out)
	})

	auth.POST("/proxies", func(c *gin.Context) {
		var req struct {
			Name     string `json:"name"`
			URL      string `json:"url"`
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if c.ShouldBindJSON(&req) != nil {
			c.JSON(400, gin.H{"error": "代理配置格式不正确"})
			return
		}
		p := &OutboundProxy{Name: req.Name, URL: req.URL, Username: req.Username, Password: req.Password, Enabled: true}
		if err := normalizeOutboundProxy(p); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		proxyConfigMu.Lock()
		defer proxyConfigMu.Unlock()
		res, err := db.Exec(`INSERT INTO outbound_proxies(name,url,username,password,enabled,created_at) VALUES(?,?,?,?,1,?)`,
			p.Name, p.URL, p.Username, p.Password, nowStr())
		if err != nil {
			c.JSON(500, gin.H{"error": "保存代理失败"})
			return
		}
		id, _ := res.LastInsertId()
		c.JSON(200, gin.H{"ok": true, "id": id})
	})

	auth.PATCH("/proxies/:id", func(c *gin.Context) {
		proxyConfigMu.Lock()
		defer proxyConfigMu.Unlock()
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		p, err := dbGetOutboundProxy(id)
		if err != nil {
			c.JSON(404, gin.H{"error": "代理不存在"})
			return
		}
		var req struct {
			Name          *string `json:"name"`
			URL           *string `json:"url"`
			Username      *string `json:"username"`
			Password      *string `json:"password"`
			ClearPassword bool    `json:"clear_password"`
			Enabled       *bool   `json:"enabled"`
		}
		if c.ShouldBindJSON(&req) != nil {
			c.JSON(400, gin.H{"error": "代理配置格式不正确"})
			return
		}
		if req.Name != nil {
			p.Name = *req.Name
		}
		if req.URL != nil {
			p.URL = *req.URL
		}
		if req.Username != nil {
			p.Username = *req.Username
		}
		if req.Password != nil && *req.Password != "" {
			p.Password = *req.Password
		}
		if req.ClearPassword || strings.TrimSpace(p.Username) == "" {
			p.Password = ""
		}
		if req.Enabled != nil {
			p.Enabled = *req.Enabled
		}
		if err := normalizeOutboundProxy(p); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		_, err = db.Exec(`UPDATE outbound_proxies SET name=?,url=?,username=?,password=?,enabled=? WHERE id=?`, p.Name, p.URL, p.Username, p.Password, boolToInt(p.Enabled), id)
		if err != nil {
			c.JSON(500, gin.H{"error": "保存代理失败"})
			return
		}
		closeProxyTransport(id)
		c.JSON(200, gin.H{"ok": true})
	})

	auth.DELETE("/proxies/:id", func(c *gin.Context) {
		proxyConfigMu.Lock()
		defer proxyConfigMu.Unlock()
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if _, err := dbGetOutboundProxy(id); err != nil {
			c.JSON(404, gin.H{"error": "代理不存在"})
			return
		}
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM upstreams WHERE proxy_id=?`, id).Scan(&count); err != nil {
			c.JSON(500, gin.H{"error": "无法检查代理关联"})
			return
		}
		if count > 0 {
			c.JSON(409, gin.H{"error": fmt.Sprintf("此代理仍被 %d 个上游使用，请先修改这些上游的代理选择", count)})
			return
		}
		if _, err := db.Exec(`DELETE FROM outbound_proxies WHERE id=?`, id); err != nil {
			c.JSON(500, gin.H{"error": "删除代理失败"})
			return
		}
		closeProxyTransport(id)
		c.JSON(200, gin.H{"ok": true})
	})
}
