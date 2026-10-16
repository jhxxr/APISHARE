package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// These requests contain public metadata only. Channel credentials and header
// overrides must never be attached to the catalog or documentation requests.
var openCodeCatalogSources = [3]string{
	"https://models.opencode.ai/api.json",
	"https://raw.githubusercontent.com/anomalyco/opencode/dev/services/www/src/docs/content/console/models.mdx",
	"https://raw.githubusercontent.com/anomalyco/opencode/dev/services/www/src/docs/content/console/go.mdx",
}

type openCodeCatalog struct {
	At         time.Time                          `json:"at"`
	Tiers      map[string]map[string]clientFormat `json:"tiers"`
	FreeModels map[string]map[string]bool         `json:"free_models"`
}

type openCodeCatalogKey struct {
	db         *sql.DB
	connection string
}

type openCodeCatalogEntry struct {
	catalog openCodeCatalog
	retryAt time.Time
	done    chan struct{}
	err     error
}

var openCodeCatalogs = struct {
	sync.Mutex
	entries map[openCodeCatalogKey]*openCodeCatalogEntry
}{entries: make(map[openCodeCatalogKey]*openCodeCatalogEntry)}

func (u *Upstream) openCodeTier() string {
	if u.OpenCodeTier == "go" || u.OpenCodeTier == "zen" {
		return u.OpenCodeTier
	}
	parsed, _ := url.Parse(u.BaseURL)
	if parsed != nil && strings.Contains("/"+strings.Trim(parsed.Path, "/")+"/", "/go/") {
		return "go"
	}
	return "zen"
}

func (u *Upstream) openCodeAuto() bool {
	return u.Type == "opencode" && (u.OpenAIProtocol == "" || u.OpenAIProtocol == "auto")
}

func validOpenCodeTier(tier string) bool { return tier == "" || tier == "zen" || tier == "go" }

func openCodeSDKFormat(sdk string) clientFormat {
	sdk = strings.ToLower(strings.TrimSpace(sdk))
	switch {
	case strings.Contains(sdk, "anthropic"), strings.HasSuffix(sdk, "/messages"):
		return FormatAnthropic
	case strings.Contains(sdk, "openai-compatible"), strings.HasSuffix(sdk, "/chat"):
		return FormatOpenAIChat
	case sdk == "@ai-sdk/openai", strings.HasSuffix(sdk, "/openai"), strings.HasSuffix(sdk, "/responses"):
		return FormatOpenAIResponse
	default:
		return ""
	}
}

func parseOpenCodeCatalog(body []byte) (openCodeCatalog, error) {
	var providers map[string]struct {
		NPM    string `json:"npm"`
		Models map[string]struct {
			Status string `json:"status"`
			Cost   struct {
				Input  *float64 `json:"input"`
				Output *float64 `json:"output"`
			} `json:"cost"`
			Provider struct {
				NPM string `json:"npm"`
			} `json:"provider"`
		} `json:"models"`
	}
	result := openCodeCatalog{Tiers: map[string]map[string]clientFormat{"zen": {}, "go": {}}, FreeModels: map[string]map[string]bool{"zen": {}, "go": {}}}
	if err := json.Unmarshal(body, &providers); err != nil {
		return result, fmt.Errorf("OpenCode 模型协议目录格式错误")
	}
	for provider, tier := range map[string]string{"opencode": "zen", "opencode-go": "go"} {
		p := providers[provider]
		for id, model := range p.Models {
			sdk := p.NPM
			if model.Provider.NPM != "" {
				sdk = model.Provider.NPM
			}
			// Keep unsupported models as empty entries, so discovery can exclude
			// them instead of silently sending them to Chat Completions.
			result.Tiers[tier][id] = openCodeSDKFormat(sdk)
			if strings.Contains(strings.ToLower(id), "free") || (model.Status != "deprecated" && model.Cost.Input != nil && model.Cost.Output != nil && *model.Cost.Input == 0 && *model.Cost.Output == 0) {
				result.FreeModels[tier][id] = true
			}
		}
	}
	if len(result.Tiers["zen"])+len(result.Tiers["go"]) == 0 {
		return result, fmt.Errorf("OpenCode 模型协议目录为空")
	}
	return result, nil
}

func mergeOpenCodeProtocolDocs(catalog *openCodeCatalog, tier string, body []byte) {
	for _, line := range strings.Split(string(body), "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 5 {
			continue
		}
		id := strings.Trim(strings.TrimSpace(cells[2]), "`")
		endpoint := strings.Trim(strings.TrimSpace(cells[3]), "`")
		if id == "" || strings.ContainsAny(id, " \t<>`") {
			continue
		}
		var format clientFormat
		switch {
		case strings.HasSuffix(endpoint, "/v1/chat/completions"):
			format = FormatOpenAIChat
		case strings.HasSuffix(endpoint, "/v1/responses"):
			format = FormatOpenAIResponse
		case strings.HasSuffix(endpoint, "/v1/messages"):
			format = FormatAnthropic
		default:
			continue
		}
		catalog.Tiers[tier][id] = format
	}
}

func fetchOpenCodePublic(ctx context.Context, client *http.Client, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("OpenCode 目录地址无效")
	}
	req.Header.Set("Accept", "application/json, text/plain, text/markdown")
	req.Header.Set("User-Agent", openCodeUserAgent())
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("无法获取 OpenCode 模型协议目录")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenCode 模型协议目录返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil || len(body) > 16<<20 {
		return nil, fmt.Errorf("无法读取 OpenCode 模型协议目录")
	}
	return body, nil
}

func openCodeCatalogCacheKey(proxy *OutboundProxy) openCodeCatalogKey {
	connection := "direct"
	if proxy != nil {
		connection = fmt.Sprint(proxyFingerprint(proxy))
	}
	return openCodeCatalogKey{db, connection + openCodeUserAgent() + fmt.Sprint(openCodeCatalogSources)}
}

func openCodeCatalogSetting() string {
	sum := sha256.Sum256([]byte(fmt.Sprint(openCodeCatalogSources)))
	return fmt.Sprintf("opencode_protocol_catalog_v2_%x", sum[:8])
}

func getOpenCodeCatalog(ctx context.Context, proxy *OutboundProxy) (openCodeCatalog, error) {
	if err := ctx.Err(); err != nil {
		return openCodeCatalog{}, err
	}
	key, setting := openCodeCatalogCacheKey(proxy), openCodeCatalogSetting()
	openCodeCatalogs.Lock()
	entry := openCodeCatalogs.entries[key]
	if entry == nil {
		entry = &openCodeCatalogEntry{}
		if key.db != nil {
			var raw string
			if key.db.QueryRow(`SELECT v FROM settings WHERE k=?`, setting).Scan(&raw) == nil {
				_ = json.Unmarshal([]byte(raw), &entry.catalog)
			}
		}
		openCodeCatalogs.entries[key] = entry
	}
	if entry.catalog.Tiers != nil && time.Since(entry.catalog.At) < modelsCacheTTL || time.Now().Before(entry.retryAt) {
		catalog, err := entry.catalog, entry.err
		openCodeCatalogs.Unlock()
		if catalog.Tiers != nil {
			err = nil
		}
		return catalog, err
	}
	if entry.done == nil {
		entry.done = make(chan struct{})
		go func() {
			fetchCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			client, err := clientForProxyConfig(modelsFetchClient, proxy)
			var bodies [3][]byte
			var fetchErrors [3]error
			if err == nil {
				var workers sync.WaitGroup
				for index, endpoint := range openCodeCatalogSources {
					workers.Go(func() { bodies[index], fetchErrors[index] = fetchOpenCodePublic(fetchCtx, client, endpoint) })
				}
				workers.Wait()
				err = fetchErrors[0]
			}
			var catalog openCodeCatalog
			if err == nil {
				catalog, err = parseOpenCodeCatalog(bodies[0])
			}
			catalogFailed := err != nil
			if err != nil {
				catalog = openCodeCatalog{Tiers: map[string]map[string]clientFormat{"zen": {}, "go": {}}}
			}
			for index, tier := range []string{"zen", "go"} {
				mergeOpenCodeProtocolDocs(&catalog, tier, bodies[index+1])
			}
			// Documentation is also a fallback when the JSON catalog is down.
			if len(catalog.Tiers["zen"])+len(catalog.Tiers["go"]) > 0 {
				err = nil
				catalog.At = time.Now()
			}
			openCodeCatalogs.Lock()
			defer openCodeCatalogs.Unlock()
			if catalogFailed && err == nil && entry.catalog.Tiers != nil {
				// A partial documentation fallback must not erase the last good
				// model routes for a different tier or models absent from the docs.
				for tier, previous := range entry.catalog.Tiers {
					for model, format := range previous {
						if _, exists := catalog.Tiers[tier][model]; !exists {
							catalog.Tiers[tier][model] = format
						}
					}
				}
				catalog.FreeModels = entry.catalog.FreeModels
			}
			entry.err = err
			entry.retryAt = time.Now().Add(time.Minute)
			if err == nil {
				entry.catalog = catalog
				if key.db != nil {
					raw, _ := json.Marshal(catalog)
					_, _ = key.db.Exec(`INSERT INTO settings(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, setting, string(raw))
				}
			}
			close(entry.done)
			entry.done = nil
		}()
	}
	done := entry.done
	openCodeCatalogs.Unlock()
	select {
	case <-ctx.Done():
		return openCodeCatalog{}, ctx.Err()
	case <-done:
		openCodeCatalogs.Lock()
		catalog, err := entry.catalog, entry.err
		openCodeCatalogs.Unlock()
		if catalog.Tiers != nil {
			err = nil
		}
		return catalog, err
	}
}

func (u *Upstream) openCodeCatalog(ctx context.Context) (openCodeCatalog, error) {
	var proxy *OutboundProxy
	if u.ProxyID != 0 {
		var err error
		proxy, err = dbGetOutboundProxy(u.ProxyID)
		if err != nil || !proxy.Enabled {
			return openCodeCatalog{}, fmt.Errorf("所选代理不存在或已停用")
		}
	}
	return getOpenCodeCatalog(ctx, proxy)
}

func (u *Upstream) resolveWireFormat(ctx context.Context, in clientFormat, model string) (clientFormat, error) {
	if !u.openCodeAuto() {
		return u.wireFormat(in), nil
	}
	catalog, err := u.openCodeCatalog(ctx)
	if err != nil {
		return "", fmt.Errorf("%w；可在 OpenCode 上游中手动选择模型原生协议", err)
	}
	format := catalog.Tiers[u.openCodeTier()][model]
	if format == "" {
		return "", fmt.Errorf("OpenCode %s 未发现模型 %s 的受支持协议；请检查套餐、模型名称或手动指定协议", u.openCodeTier(), model)
	}
	return format, nil
}

// Routing may consult already loaded public metadata, but never performs a
// network fetch while choosing among channels.
func (u *Upstream) routingWireFormat(in clientFormat, model string) clientFormat {
	if !u.openCodeAuto() {
		return u.wireFormat(in)
	}
	openCodeCatalogs.Lock()
	defer openCodeCatalogs.Unlock()
	return latestOpenCodeCatalogLocked().Tiers[u.openCodeTier()][u.mapModel(model)]
}

func latestOpenCodeCatalogLocked() openCodeCatalog {
	var latest openCodeCatalog
	for key, entry := range openCodeCatalogs.entries {
		if key.db == db && entry.catalog.At.After(latest.At) {
			latest = entry.catalog
		}
	}
	return latest
}

// The official inference API separates families in the path. Preserve custom
// gateway paths and legacy /zen endpoints, and never take credential-bearing
// request origins from remote metadata.
func (u *Upstream) endpointURL(format clientFormat, endpoint string) string {
	base := u.BaseURL
	if u.Type == "opencode" {
		parsed, err := url.Parse(base)
		if err == nil {
			path := strings.TrimRight(parsed.Path, "/")
			for _, suffix := range []string{"", "/v1", "/go", "/go/v1", "/openai", "/openai/v1", "/anthropic", "/anthropic/v1", "/go/openai", "/go/openai/v1", "/go/anthropic", "/go/anthropic/v1"} {
				if path != "/inference"+suffix {
					continue
				}
				parsed.Path = "/inference"
				if u.openCodeTier() == "go" {
					parsed.Path += "/go"
				}
				if endpoint != "/v1/models" {
					if format == FormatAnthropic {
						parsed.Path += "/anthropic"
					} else {
						parsed.Path += "/openai"
					}
				}
				parsed.RawPath = ""
				base = parsed.String()
				break
			}
		}
	}
	return upstreamURL(base, endpoint)
}
