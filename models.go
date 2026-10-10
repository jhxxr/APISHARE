package main

import (
	"context"
	"crypto/sha256"
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

	"github.com/gin-gonic/gin"
)

const modelsCacheTTL = 5 * time.Minute

func registerModelRoutes(v1 *gin.RouterGroup) {
	v1.GET("/models", modelsHandler)
	v1.GET("/models/:model", modelHandler)
	// Register preflight routes so corsV1 runs before key authentication.
	preflight := func(c *gin.Context) { c.Status(http.StatusNoContent) }
	v1.OPTIONS("/models", preflight)
	v1.OPTIONS("/models/:model", preflight)
}

type modelCacheKey struct {
	id         int64
	connection [32]byte
}

func upstreamModelCacheKey(u *Upstream) modelCacheKey {
	var p *OutboundProxy
	if u.ProxyID != 0 {
		p, _ = dbGetOutboundProxy(u.ProxyID)
	}
	return upstreamModelCacheKeyForProxy(u, p)
}

func upstreamModelCacheKeyForProxy(u *Upstream, p *OutboundProxy) modelCacheKey {
	var proxyConnection any = u.ProxyID
	if p != nil {
		proxyConnection = proxyFingerprint(p)
	}
	connection, _ := json.Marshal([]any{u.Type, u.BaseURL, u.APIKey, proxyConnection, u.HeaderOverrides, clientHeaderVersionFingerprint(u.HeaderOverrides)})
	return modelCacheKey{u.ID, sha256.Sum256(connection)}
}

type cachedModels struct {
	ids []string
	at  time.Time
}

type modelFetch struct {
	done chan struct{}
	ids  []string
	err  error
}

var modelsCache = struct {
	sync.Mutex
	m       map[modelCacheKey]cachedModels
	pending map[modelCacheKey]*modelFetch
}{m: make(map[modelCacheKey]cachedModels), pending: make(map[modelCacheKey]*modelFetch)}

var modelsFetchClient = &http.Client{
	Timeout: 15 * time.Second,
	// Never forward an API key to a redirect destination.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// probeUpstreamModels always uses the supplied credentials, including unsaved edits.
// The deadline covers all pages; a partial catalog is never reported as complete.
func probeUpstreamModels(ctx context.Context, upType, baseURL, apiKey string) ([]string, error) {
	return probeUpstreamModelsWithProxy(ctx, upType, baseURL, apiKey, 0)
}

func probeUpstreamModelsWithProxy(ctx context.Context, upType, baseURL, apiKey string, proxyID int64, headerOverrides ...string) ([]string, error) {
	client, err := clientForUpstream(modelsFetchClient, proxyID)
	if err != nil {
		return nil, err
	}
	return probeUpstreamModelsWithClient(ctx, upType, baseURL, apiKey, client, headerOverrides...)
}

func probeUpstreamModelsWithClient(ctx context.Context, upType, baseURL, apiKey string, client *http.Client, headerOverrides ...string) ([]string, error) {
	headers := ""
	if len(headerOverrides) > 0 {
		headers = headerOverrides[0]
	}
	if _, err := parseHeaderOverrides(headers); err != nil {
		return nil, err
	}
	upType, baseURL, apiKey = strings.TrimSpace(upType), strings.TrimSpace(baseURL), strings.TrimSpace(apiKey)
	if upType != "openai" && upType != "anthropic" && upType != "gemini" {
		return nil, fmt.Errorf("type must be openai, anthropic or gemini")
	}
	base, err := url.Parse(baseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("base_url must be an HTTP(S) URL without credentials, query or fragment")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("api_key is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	path := "/v1/models"
	if upType == "gemini" {
		path = "/v1beta/models"
	}
	endpoint, _ := url.Parse(upstreamURL(baseURL, path))
	ids := make([]string, 0)
	seenIDs, seenCursors := map[string]bool{}, map[string]bool{}
	cursor := ""
	for page := 0; page < 100; page++ {
		query := endpoint.Query()
		if upType == "anthropic" {
			query.Set("limit", "1000")
			if cursor != "" {
				query.Set("after_id", cursor)
			}
		} else if upType == "gemini" && cursor != "" {
			query.Set("pageToken", cursor)
		}
		endpoint.RawQuery = query.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("invalid model request")
		}
		req.Header.Set("Accept", "application/json")
		switch upType {
		case "anthropic":
			req.Header.Set("x-api-key", apiKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		case "gemini":
			req.Header.Set("x-goog-api-key", apiKey)
		default:
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		if err := applyHeaderOverrides(req, headers, apiKey, nil); err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("model request timed out or was canceled")
			}
			return nil, fmt.Errorf("could not connect to upstream models API")
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("upstream models API returned HTTP %d", resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("could not read upstream model list")
		}
		if len(body) > 8<<20 {
			return nil, fmt.Errorf("upstream model list is too large")
		}
		var result struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
			HasMore       bool   `json:"has_more"`
			LastID        string `json:"last_id"`
			NextPageToken string `json:"nextPageToken"`
		}
		if json.Unmarshal(body, &result) != nil || (upType == "gemini" && result.Models == nil) || (upType != "gemini" && result.Data == nil) {
			return nil, fmt.Errorf("upstream returned an invalid model list")
		}
		add := func(id string) {
			id = strings.TrimSpace(id)
			if id != "" && !seenIDs[id] {
				seenIDs[id] = true
				ids = append(ids, id)
			}
		}
		for _, model := range result.Data {
			if upType != "gemini" {
				add(model.ID)
			}
		}
		for _, model := range result.Models {
			if upType == "gemini" {
				add(strings.TrimPrefix(model.Name, "models/"))
			}
		}
		cursor = ""
		switch {
		case upType == "anthropic" && result.HasMore:
			cursor = strings.TrimSpace(result.LastID)
			if cursor == "" || len(result.Data) == 0 {
				return nil, fmt.Errorf("upstream returned invalid model pagination")
			}
		case upType == "gemini":
			cursor = result.NextPageToken
		}
		if cursor == "" {
			sort.Strings(ids)
			return ids, nil
		}
		if seenCursors[cursor] {
			return nil, fmt.Errorf("upstream repeated a model pagination cursor")
		}
		seenCursors[cursor] = true
	}
	return nil, fmt.Errorf("upstream model list exceeded the pagination limit")
}

// Share discovery across clients and the public page, with cache isolation for
// every connection/key. An older in-flight request cannot populate a new key's cache.
func cachedUpstreamModels(ctx context.Context, u *Upstream) ([]string, error) {
	var proxyConfig *OutboundProxy
	if u.ProxyID != 0 {
		var err error
		proxyConfig, err = dbGetOutboundProxy(u.ProxyID)
		if err != nil {
			return nil, fmt.Errorf("所选代理不存在或无法读取")
		}
		if !proxyConfig.Enabled {
			return nil, fmt.Errorf("所选代理已停用")
		}
	}
	key := upstreamModelCacheKeyForProxy(u, proxyConfig)
	modelsCache.Lock()
	if cached, ok := modelsCache.m[key]; ok && time.Since(cached.at) < modelsCacheTTL {
		modelsCache.Unlock()
		return cached.ids, nil
	}
	flight, waiting := modelsCache.pending[key]
	if !waiting {
		flight = &modelFetch{done: make(chan struct{})}
		modelsCache.pending[key] = flight
		connection := *u
		go func() {
			client, err := clientForProxyConfig(modelsFetchClient, proxyConfig)
			if err != nil {
				flight.err = err
			} else {
				flight.ids, flight.err = probeUpstreamModelsWithClient(context.Background(), connection.Type, connection.BaseURL, connection.APIKey, client, connection.HeaderOverrides)
			}
			modelsCache.Lock()
			defer modelsCache.Unlock()
			for k, cached := range modelsCache.m {
				if time.Since(cached.at) >= modelsCacheTTL {
					delete(modelsCache.m, k)
				}
			}
			if flight.err == nil {
				modelsCache.m[key] = cachedModels{ids: flight.ids, at: time.Now()}
			}
			delete(modelsCache.pending, key)
			close(flight.done)
		}()
	}
	modelsCache.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-flight.done:
		return flight.ids, flight.err
	}
}

func isClaudeModelRequest(c *gin.Context) bool {
	return c.GetHeader("anthropic-version") != "" || c.GetHeader("anthropic-beta") != "" ||
		(c.GetHeader("x-api-key") != "" && c.GetHeader("Authorization") == "")
}

func modelError(c *gin.Context, status int, message string) {
	if !isClaudeModelRequest(c) {
		errJSON(c, status, message)
		return
	}
	typ := "api_error"
	switch status {
	case 400:
		typ = "invalid_request_error"
	case 401:
		typ = "authentication_error"
	case 402, 403:
		typ = "permission_error"
	case 404:
		typ = "not_found_error"
	case 429:
		typ = "rate_limit_error"
	}
	c.JSON(status, gin.H{"type": "error", "error": gin.H{"type": typ, "message": message}})
}

type catalogModel struct{ ID, Owner string }

func availableModels(ctx context.Context) ([]catalogModel, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ups, err := dbListUpstreams()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	data := make([]catalogModel, 0)
	failed := false
	for _, u := range ups {
		if !u.Enabled || u.Weight <= 0 {
			continue
		}
		add := func(id string) {
			id = strings.TrimSpace(id)
			if id != "" && !seen[id] {
				seen[id] = true
				data = append(data, catalogModel{id, u.Type})
			}
		}
		if list := strings.TrimSpace(u.Models); list != "" {
			for _, id := range strings.Split(list, ",") {
				add(id)
			}
		} else {
			ids, err := cachedUpstreamModels(ctx, &u)
			if err != nil {
				failed = true
			}
			for _, id := range ids {
				add(id)
			}
		}
		for client := range u.modelMap() {
			if u.servesModel(client) {
				add(client)
			}
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if len(data) == 0 && failed {
		return nil, fmt.Errorf("unable to fetch models from configured upstreams")
	}
	sort.Slice(data, func(i, j int) bool { return data[i].ID < data[j].ID })
	return data, nil
}

func modelView(model catalogModel, claude bool) gin.H {
	if claude {
		return gin.H{"id": model.ID, "type": "model", "display_name": model.ID, "created_at": "1970-01-01T00:00:00Z"}
	}
	return gin.H{"id": model.ID, "object": "model", "created": 0, "owned_by": model.Owner}
}

func modelsHandler(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	claude := isClaudeModelRequest(c)
	limit, after, before := 20, c.Query("after_id"), c.Query("before_id")
	if claude {
		if raw, provided := c.GetQuery("limit"); provided {
			var err error
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > 1000 {
				modelError(c, 400, "limit must be between 1 and 1000")
				return
			}
		}
		if after != "" && before != "" {
			modelError(c, 400, "use either after_id or before_id")
			return
		}
	}
	models, err := availableModels(c.Request.Context())
	if err != nil {
		modelError(c, 502, "unable to load available models; check upstream connections")
		return
	}
	start, end, more := 0, len(models), false
	if claude {
		if after != "" || before != "" {
			cursor := after
			if before != "" {
				cursor = before
			}
			index := sort.Search(len(models), func(i int) bool { return models[i].ID >= cursor })
			if index == len(models) || models[index].ID != cursor {
				modelError(c, 400, "unknown model pagination cursor")
				return
			}
			if after != "" {
				start = index + 1
			} else {
				end = index
			}
		}
		if end-start > limit {
			more = true
			if before != "" {
				start = end - limit
			} else {
				end = start + limit
			}
		}
	}
	data := make([]gin.H, 0, end-start)
	for _, model := range models[start:end] {
		data = append(data, modelView(model, claude))
	}
	if !claude {
		c.JSON(200, gin.H{"object": "list", "data": data})
		return
	}
	var firstID, lastID any
	if len(data) > 0 {
		firstID = models[start].ID
		lastID = models[end-1].ID
	}
	c.JSON(200, gin.H{"data": data, "has_more": more, "first_id": firstID, "last_id": lastID})
}

func modelHandler(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	models, err := availableModels(c.Request.Context())
	if err != nil {
		modelError(c, 502, "unable to load available models; check upstream connections")
		return
	}
	for _, model := range models {
		if model.ID == c.Param("model") {
			c.JSON(200, modelView(model, isClaudeModelRequest(c)))
			return
		}
	}
	modelError(c, 404, "model not found")
}
