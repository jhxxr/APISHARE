package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const clientVersionsSetting = "client_versions_v1"
const clientVersionCheckInterval = time.Hour
const clientVersionRetryInterval = 5 * time.Minute

type clientVersionSource struct {
	ID, Name, Package, URL string
}

var officialClientVersionSources = []clientVersionSource{
	{"codex", "Codex", "@openai/codex", "https://registry.npmjs.org/@openai/codex/latest"},
	{"claude_code", "Claude Code", "@anthropic-ai/claude-code", "https://registry.npmjs.org/@anthropic-ai/claude-code/latest"},
}

var stableClientVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type clientVersion struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	UserAgent string    `json:"user_agent"`
	CheckedAt time.Time `json:"checked_at"`
	Source    string    `json:"source"`
	Error     string    `json:"error,omitempty"`
}

type clientVersionService struct {
	sync.RWMutex
	sources   []clientVersionSource
	client    *http.Client
	entries   map[string]clientVersion
	attempted time.Time
	pending   chan struct{}
	save      func(string) error
}

func newClientVersionService(client *http.Client, sources []clientVersionSource, save func(string) error) *clientVersionService {
	s := &clientVersionService{client: client, sources: sources, entries: make(map[string]clientVersion), save: save}
	for _, source := range sources {
		s.entries[source.ID] = clientVersion{ID: source.ID, Name: source.Name, Source: source.URL}
	}
	return s
}

var clientVersions = newClientVersionService(&http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}, officialClientVersionSources, func(raw string) error { return settingsSet(clientVersionsSetting, raw) })

func clientUserAgent(id, version string) string {
	switch id {
	case "codex":
		return "codex_cli_rs/" + version
	case "claude_code":
		return "claude-cli/" + version + " (external, cli)"
	}
	return ""
}

func (s *clientVersionService) restore(raw string) error {
	if raw == "" {
		return nil
	}
	var saved []clientVersion
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		return fmt.Errorf("客户端版本缓存格式无效")
	}
	s.Lock()
	defer s.Unlock()
	for _, entry := range saved {
		known, exists := s.entries[entry.ID]
		if !exists || len(entry.Version) > 64 || !stableClientVersion.MatchString(entry.Version) || entry.CheckedAt.IsZero() || entry.CheckedAt.After(time.Now()) {
			continue
		}
		known.Version, known.CheckedAt = entry.Version, entry.CheckedAt
		known.UserAgent = clientUserAgent(entry.ID, entry.Version)
		s.entries[entry.ID] = known
	}
	return nil
}

func loadClientVersionCache() {
	raw, err := settingsGet(clientVersionsSetting)
	if err == nil {
		err = clientVersions.restore(raw)
	}
	if err != nil {
		log.Printf("[client-versions] could not load cached versions: %v", err)
	}
}

func (s *clientVersionService) snapshot() ([]clientVersion, bool) {
	s.RLock()
	defer s.RUnlock()
	entries := make([]clientVersion, 0, len(s.sources))
	for _, source := range s.sources {
		entries = append(entries, s.entries[source.ID])
	}
	return entries, s.pending != nil
}

// Coalesce refreshes and never put registry access on the relay request path.
// Canceling one administrator's request does not cancel a shared refresh.
func (s *clientVersionService) refresh(ctx context.Context, force bool) error {
	s.Lock()
	done := s.pending
	if done == nil {
		interval := clientVersionCheckInterval
		for _, entry := range s.entries {
			if entry.Error != "" || entry.Version == "" {
				interval = clientVersionRetryInterval
				break
			}
		}
		if !force && !s.attempted.IsZero() && time.Since(s.attempted) < interval {
			s.Unlock()
			return nil
		}
		done = make(chan struct{})
		s.pending, s.attempted = done, time.Now()
		go s.fetchAll(done)
	}
	s.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

func (s *clientVersionService) fetchVersion(ctx context.Context, source clientVersionSource) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
	if err != nil {
		return "", fmt.Errorf("官方版本源地址无效")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "APIShare-VersionCheck/1.0")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("无法连接官方版本源，请稍后重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("官方版本源返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (128<<10)+1))
	if err != nil || len(body) > 128<<10 {
		return "", fmt.Errorf("无法读取官方版本信息")
	}
	var pkg struct{ Name, Version string }
	if json.Unmarshal(body, &pkg) != nil || pkg.Name != source.Package || len(pkg.Version) > 64 || !stableClientVersion.MatchString(pkg.Version) {
		return "", fmt.Errorf("官方版本源未返回有效的稳定版本")
	}
	return pkg.Version, nil
}

func (s *clientVersionService) fetchAll(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type result struct {
		id, version string
		err         error
	}
	results := make(chan result, len(s.sources))
	for _, source := range s.sources {
		go func(source clientVersionSource) {
			version, err := s.fetchVersion(ctx, source)
			results <- result{source.ID, version, err}
		}(source)
	}
	for range s.sources {
		result := <-results
		s.Lock()
		entry := s.entries[result.id]
		if result.err != nil {
			entry.Error = result.err.Error()
		} else {
			entry.Version, entry.CheckedAt, entry.Error = result.version, time.Now(), ""
			entry.UserAgent = clientUserAgent(result.id, result.version)
		}
		s.entries[result.id] = entry
		s.Unlock()
	}
	entries, _ := s.snapshot()
	if s.save != nil {
		raw, _ := json.Marshal(entries)
		if err := s.save(string(raw)); err != nil {
			log.Printf("[client-versions] could not save cached versions: %v", err)
		}
	}
	s.Lock()
	s.pending = nil
	close(done)
	s.Unlock()
}

func clientVersionRefreshLoop() {
	ticker := time.NewTicker(clientVersionRetryInterval)
	defer ticker.Stop()
	for {
		_ = clientVersions.refresh(context.Background(), false)
		<-ticker.C
	}
}

func (s *clientVersionService) userAgent(id string) (string, error) {
	s.RLock()
	defer s.RUnlock()
	entry := s.entries[id]
	if entry.Version == "" {
		return "", fmt.Errorf("%s 最新版本尚未获取，请先在请求头模板中检查更新", entry.Name)
	}
	return entry.UserAgent, nil
}

func clientHeaderVersionFingerprint(raw string) string {
	clientVersions.RLock()
	defer clientVersions.RUnlock()
	var parts []string
	for _, id := range []string{"codex", "claude_code"} {
		if strings.Contains(raw, "{"+id+"_user_agent}") {
			parts = append(parts, id+":"+clientVersions.entries[id].Version)
		}
	}
	return strings.Join(parts, ";")
}

func registerClientVersionRoutes(auth *gin.RouterGroup) {
	handler := func(force bool) gin.HandlerFunc {
		return func(c *gin.Context) {
			if err := clientVersions.refresh(c.Request.Context(), force); err != nil {
				c.JSON(http.StatusRequestTimeout, gin.H{"error": "版本检测请求已取消，请重试"})
				return
			}
			entries, refreshing := clientVersions.snapshot()
			c.Header("Cache-Control", "no-store")
			c.JSON(http.StatusOK, gin.H{"clients": entries, "refreshing": refreshing})
		}
	}
	auth.GET("/client-versions", handler(false))
	auth.POST("/client-versions/refresh", handler(true))
}
