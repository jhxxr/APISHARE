// Package cliproxy isolates the CLIProxyAPI v8 management contract from the gateway.
package cliproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const ManagementPath = "/v8/management"

type Client struct {
	BaseURL string
	Secret  string
	HTTP    *http.Client
}

func (c Client) Do(ctx context.Context, method, path string, body any, out any) error {
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+ManagementPath+path, payload)
	if err != nil {
		return fmt.Errorf("无法创建核心请求")
	}
	req.Header.Set("Authorization", "Bearer "+c.Secret)
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("无法连接 CLIProxyAPI 核心，请检查运行状态")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(b) > 4<<20 {
		return fmt.Errorf("核心响应读取失败或超过大小限制")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Do not surface upstream bodies: they may contain credentials or configuration.
		return fmt.Errorf("CLIProxyAPI 管理接口返回 %d；请检查核心版本、凭据和操作参数", resp.StatusCode)
	}
	if out != nil && json.Unmarshal(b, out) != nil {
		return fmt.Errorf("CLIProxyAPI 返回了无法识别的数据，请检查版本兼容性")
	}
	return nil
}

// Credential intentionally omits tokens and arbitrary provider metadata.
type Credential struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	Label            string          `json:"label,omitempty"`
	Email            string          `json:"email,omitempty"`
	Provider         string          `json:"provider"`
	Status           string          `json:"status"`
	Disabled         bool            `json:"disabled"`
	Unavailable      bool            `json:"unavailable"`
	RuntimeOnly      bool            `json:"runtime_only"`
	ProxyID          int64           `json:"proxy_id"`
	EffectiveProxyID int64           `json:"effective_proxy_id"`
	Uptime           *AccountUptime  `json:"uptime,omitempty"`
	Details          *AccountRuntime `json:"details,omitempty"`
}

type Login struct {
	Status    string `json:"status"`
	URL       string `json:"url"`
	State     string `json:"state"`
	Flow      string `json:"flow,omitempty"`
	UserCode  string `json:"user_code,omitempty"`
	ExpiresIn int    `json:"expires_in,omitempty"`
}
