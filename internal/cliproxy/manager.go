package cliproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	_ "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
)

// Version is the exact module version compiled into API Share, never a runtime
// download. go.mod and go.sum are the release's source of truth.
func Version() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dependency := range info.Deps {
			if dependency.Path == "github.com/router-for-me/CLIProxyAPI/v8" {
				return dependency.Version
			}
		}
	}
	return "v8 (embedded)"
}

type diskState struct {
	AutoStart     bool   `json:"auto_start"`
	Port          int    `json:"port"`
	ManagementKey string `json:"management_key"`
	APIKey        string `json:"api_key"`
}

type Status struct {
	Version   string `json:"version"`
	Embedded  bool   `json:"embedded"`
	Running   bool   `json:"running"`
	AutoStart bool   `json:"auto_start"`
	Error     string `json:"error,omitempty"`
	// ProxyID is filled by the gateway from its outbound proxy binding.
	ProxyID int64 `json:"proxy_id"`
}

type Manager struct {
	root      string
	op        sync.Mutex
	mu        sync.Mutex
	state     diskState
	cancel    context.CancelFunc
	done      chan struct{}
	lastError string
}

func NewManager(root string, port int) (*Manager, error) {
	if port < 1024 || port > 65535 {
		return nil, fmt.Errorf("CLIProxyAPI 端口必须在 1024–65535 之间")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	m := &Manager{root: abs}
	b, err := os.ReadFile(filepath.Join(abs, "state.json"))
	if err == nil {
		if err = json.Unmarshal(b, &m.state); err != nil {
			return nil, fmt.Errorf("CLIProxyAPI 状态文件无效")
		}
		if m.state.Port < 1024 || m.state.Port > 65535 || m.state.ManagementKey == "" || m.state.APIKey == "" {
			return nil, fmt.Errorf("CLIProxyAPI 状态文件字段无效")
		}
	} else if os.IsNotExist(err) {
		management, err := randomKey()
		if err != nil {
			return nil, err
		}
		api, err := randomKey()
		if err != nil {
			return nil, err
		}
		m.state = diskState{AutoStart: true, Port: port, ManagementKey: management, APIKey: api}
		if err = m.saveState(m.state); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	return m, nil
}

func randomKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func writePrivate(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}

func (m *Manager) saveState(s diskState) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(m.root, "state.json"), b)
}
func (m *Manager) snapshot() diskState { m.mu.Lock(); defer m.mu.Unlock(); return m.state }
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Status{Version: Version(), Embedded: true, Running: m.cancel != nil, AutoStart: m.state.AutoStart, Error: m.lastError}
}
func (m *Manager) Client() Client {
	s := m.snapshot()
	return Client{BaseURL: "http://127.0.0.1:" + strconv.Itoa(s.Port), Secret: s.ManagementKey}
}
func (m *Manager) Connection() (string, string) {
	s := m.snapshot()
	return "http://127.0.0.1:" + strconv.Itoa(s.Port), s.APIKey
}

func (m *Manager) ensureConfig() error {
	path := filepath.Join(m.root, "config.yaml")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	s := m.snapshot()
	if err := os.MkdirAll(filepath.Join(m.root, "auths"), 0700); err != nil {
		return err
	}
	// JSON is valid YAML. SDK writes may save YAML; preserve the same file across releases.
	settings := map[string]any{
		"config-version": 8,
		"server":         map[string]any{"host": "127.0.0.1", "port": s.Port},
		"management":     map[string]any{"allow-remote": false, "secret-key": s.ManagementKey, "disable-control-panel": true},
		"access":         map[string]any{"api-keys": []string{s.APIKey}},
		"oauth":          map[string]any{"auth-dir": filepath.Join(m.root, "auths")},
		"requests":       map[string]any{"proxy-url": ""},
		"routing":        map[string]any{"strategy": "round-robin", "session-affinity": true, "retry": map[string]any{"request-retry": 0}},
	}
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(path, b)
}

// SetProxyURL applies the gateway-selected egress proxy to the embedded core.
// The value uses the CLIProxyAPI form (http/https/socks5/socks5h, optionally
// with URL credentials, or an empty string to follow the environment). While
// the core is running the change goes through its own management API so the
// SDK persists config.yaml and reloads without dropping in-flight requests.
func (m *Manager) SetProxyURL(ctx context.Context, proxyURL string) error {
	m.op.Lock()
	defer m.op.Unlock()
	if err := m.ensureConfig(); err != nil {
		return err
	}
	proxyURL = strings.TrimSpace(proxyURL)
	if m.Status().Running {
		if ctx == nil {
			ctx = context.Background()
		}
		return m.Client().Do(ctx, "PUT", "/config/requests/proxy-url", proxyURL, nil)
	}
	return config.SaveConfigPreserveCommentsUpdateNestedScalar(filepath.Join(m.root, "config.yaml"), []string{"requests", "proxy-url"}, proxyURL)
}

func (m *Manager) start(ctx context.Context) error {
	if err := m.ensureConfig(); err != nil {
		return err
	}
	s := m.snapshot()
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(s.Port))
	if err != nil {
		return fmt.Errorf("内置核心端口 %d 已被占用", s.Port)
	}
	l.Close()
	path := filepath.Join(m.root, "config.yaml")
	cfg, err := config.LoadConfig(path)
	if err != nil {
		return fmt.Errorf("内置核心配置读取失败，请检查 cliproxy-data/config.yaml")
	}
	// Enforce the gateway's private listener and disable upstream UI regardless of
	// old configuration. Credentials remain owned and refreshed by the SDK.
	cfg.Host, cfg.Port = "127.0.0.1", s.Port
	cfg.RemoteManagement.AllowRemote = false
	cfg.RemoteManagement.DisableControlPanel = true
	cfg.AuthDir = filepath.Join(m.root, "auths")
	service, err := sdk.NewBuilder().WithConfig(cfg).WithConfigPath(path).Build()
	if err != nil {
		return fmt.Errorf("内置 CLIProxyAPI 初始化失败：%w", err)
	}
	run, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.mu.Lock()
	m.cancel, m.done, m.lastError = cancel, done, ""
	m.mu.Unlock()
	go func() {
		err := service.Run(run)
		cancel()
		m.mu.Lock()
		m.cancel = nil
		if err != nil && !errors.Is(err, context.Canceled) {
			m.lastError = "内置核心已停止，请检查网关日志"
		}
		close(done)
		m.mu.Unlock()
	}()
	ready, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	client := m.Client()
	client.HTTP = &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		var settings struct {
			Version int `json:"config-version"`
		}
		if err := client.Do(ready, "GET", "/config", nil, &settings); err == nil && settings.Version == 8 {
			return nil
		}
		select {
		case <-done:
			return fmt.Errorf("内置核心在就绪前停止，请检查网关日志")
		case <-ready.Done():
			m.stop()
			return fmt.Errorf("内置核心就绪验证失败")
		case <-ticker.C:
		}
	}
}

func (m *Manager) stop() {
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

func (m *Manager) Start(ctx context.Context) error {
	m.op.Lock()
	defer m.op.Unlock()
	if m.Status().Running {
		return nil
	}
	if err := m.start(ctx); err != nil {
		m.mu.Lock()
		m.lastError = err.Error()
		m.mu.Unlock()
		return err
	}
	s := m.snapshot()
	s.AutoStart = true
	if err := m.saveState(s); err != nil {
		m.stop()
		return err
	}
	m.mu.Lock()
	m.state = s
	m.mu.Unlock()
	return nil
}
func (m *Manager) Stop() error {
	m.op.Lock()
	defer m.op.Unlock()
	s := m.snapshot()
	s.AutoStart = false
	if err := m.saveState(s); err != nil {
		return err
	}
	m.mu.Lock()
	m.state = s
	m.mu.Unlock()
	m.stop()
	return nil
}

// Close keeps AutoStart unchanged, so an ordinary gateway restart resumes the SDK.
func (m *Manager) Close() { m.op.Lock(); defer m.op.Unlock(); m.stop() }
