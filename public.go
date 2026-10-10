package main

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Public response types deliberately contain no key, key name/id/count,
// upstream address, credential, error body, or administration URL.
type PublicDay struct {
	Date             string `json:"date"`
	Calls            int64  `json:"calls"`
	Successes        int64  `json:"successes"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
}
type PublicUsage struct {
	Model            string `json:"model"`
	Calls            int64  `json:"calls"`
	Successes        int64  `json:"successes"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
}
type PublicInterval struct {
	Start     string `json:"start"`
	End       string `json:"end"`
	Calls     int64  `json:"calls"`
	Successes int64  `json:"successes"`
}
type PublicModel struct {
	PublicUsage
	Family   string           `json:"family"`
	Status   string           `json:"status"`
	Uptime   *float64         `json:"uptime_percent"`
	LastCall string           `json:"last_call"`
	History  []PublicInterval `json:"history"`
}
type PublicSummary struct {
	Configured       bool     `json:"configured"`
	Unlimited        bool     `json:"unlimited"`
	RemainingUSD     *float64 `json:"remaining_usd"`
	QuotaUSD         float64  `json:"quota_usd"`
	UsedUSD          float64  `json:"used_usd"`
	Calls            int64    `json:"calls"`
	Successes        int64    `json:"successes"`
	PromptTokens     int64    `json:"prompt_tokens"`
	CompletionTokens int64    `json:"completion_tokens"`
	Uptime           *float64 `json:"uptime_percent"`
}
type PublicOverview struct {
	UpdatedAt            string        `json:"updated_at"`
	WindowDays           int           `json:"window_days"`
	HistoryInterval      string        `json:"history_interval"`
	HistoryWindowMinutes int           `json:"history_window_minutes"`
	ModelsLoading        bool          `json:"models_loading"`
	Summary              PublicSummary `json:"summary"`
	Daily                []PublicDay   `json:"daily"`
	Usage                []PublicUsage `json:"usage"`
	Models               []PublicModel `json:"models"`
}

func registerPublic(r *gin.Engine) {
	registerSetupRoutes(r)
	sub, _ := fs.Sub(webFS, "web/public")
	r.GET("/", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if adminSetupRequired() {
			c.Redirect(http.StatusTemporaryRedirect, "/setup/")
			return
		}
		serveAdminFile(c, sub, "index.html")
	})
	// The 3D station view reads the same anonymous aggregate endpoint as the root page.
	r.GET("/metro", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if adminSetupRequired() {
			c.Redirect(http.StatusTemporaryRedirect, "/setup/")
			return
		}
		serveAdminFile(c, sub, "metro.html")
	})
	r.GET("/public/static/*filepath", func(c *gin.Context) {
		name := strings.TrimPrefix(c.Param("filepath"), "/")
		// Vendored libraries live in version-named directories, so they can be cached for good.
		if strings.HasPrefix(name, "vendor/") {
			c.Header("Cache-Control", "public, max-age=31536000, immutable")
		}
		serveAdminFile(c, sub, name)
	})
	r.GET("/api/public/overview", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		interval := c.DefaultQuery("interval", "hour")
		if interval != "hour" && interval != "minute" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请选择按小时或分钟查看调用历史。"})
			return
		}
		overview, err := buildPublicOverview(time.Now(), interval)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "暂时无法读取服务数据，请稍后重试。"})
			return
		}
		c.JSON(http.StatusOK, overview)
	})
}
func requestUptime(successes, calls int64) *float64 {
	if calls == 0 {
		return nil
	}
	rate := float64(successes) / float64(calls) * 100
	return &rate
}
func publicFamily(model, fallback string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "claude"):
		return "Anthropic"
	case strings.Contains(m, "gemini"):
		return "Google"
	case strings.Contains(m, "deepseek"):
		return "DeepSeek"
	case strings.Contains(m, "grok"):
		return "xAI"
	case strings.HasPrefix(m, "gpt-"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"), strings.Contains(m, "chatgpt"):
		return "OpenAI"
	case strings.Contains(m, "qwen"):
		return "Qwen"
	case strings.Contains(m, "glm"):
		return "GLM"
	}
	switch fallback {
	case "anthropic":
		return "Anthropic"
	case "gemini":
		return "Google"
	}
	return "OpenAI 兼容"
}

var publicDiscovery = struct {
	sync.Mutex
	pending map[modelCacheKey]bool
}{pending: make(map[modelCacheKey]bool)}

// Discover wildcard catalogs in the background. Discovery never fabricates
// request success/uptime and never makes page loading wait for an upstream.
func publicWildcardModels(u Upstream) ([]string, bool) {
	key := upstreamModelCacheKey(&u)
	modelsCache.Lock()
	cached, ok := modelsCache.m[key]
	modelsCache.Unlock()
	if ok && time.Since(cached.at) < modelsCacheTTL {
		return cached.ids, false
	}
	publicDiscovery.Lock()
	if !publicDiscovery.pending[key] {
		publicDiscovery.pending[key] = true
		go func() {
			_, _ = cachedUpstreamModels(context.Background(), &u)
			publicDiscovery.Lock()
			delete(publicDiscovery.pending, key)
			publicDiscovery.Unlock()
		}()
	}
	publicDiscovery.Unlock()
	return cached.ids, true
}
func publicRows(tx *sql.Tx, query string, scan func(*sql.Rows) error, args ...any) error {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
func buildPublicOverview(now time.Time, interval string) (*PublicOverview, error) {
	now = now.In(beijingLocation)
	bucketSize := time.Hour
	switch interval {
	case "hour":
	case "minute":
		bucketSize = time.Minute
	default:
		return nil, fmt.Errorf("unsupported public history interval: %q", interval)
	}
	// Both resolutions cover the same rolling 24 hours. Request timestamps are
	// stored to the second; switching resolution only changes the grouping.
	windowEnd := now.Truncate(time.Second)
	historyStart := windowEnd.Add(-24 * time.Hour)
	bucketCount := int(24 * time.Hour / bucketSize)
	newHistory := func() []PublicInterval {
		history := make([]PublicInterval, bucketCount)
		for i := range history {
			start := historyStart.Add(time.Duration(i) * bucketSize)
			history[i].Start = start.Format(time.RFC3339)
			history[i].End = start.Add(bucketSize).Format(time.RFC3339)
		}
		return history
	}
	ups, err := dbListUpstreams()
	if err != nil {
		return nil, err
	}
	catalog := make(map[string]string)
	wildcard := false
	loading := false
	for _, u := range ups {
		if !u.Enabled || u.Weight <= 0 {
			continue
		}
		add := func(model string) {
			model = strings.TrimSpace(model)
			if model != "" {
				catalog[model] = publicFamily(model, u.Type)
			}
		}
		if strings.TrimSpace(u.Models) == "" {
			wildcard = true
			ids, pending := publicWildcardModels(u)
			loading = loading || pending
			for _, id := range ids {
				add(id)
			}
		} else {
			for _, id := range strings.Split(u.Models, ",") {
				add(id)
			}
		}
		for client := range u.modelMap() {
			if u.servesModel(client) {
				add(client)
			}
		}
	}
	result := &PublicOverview{UpdatedAt: now.Format(time.RFC3339), WindowDays: 30, ModelsLoading: loading,
		HistoryInterval: interval, HistoryWindowMinutes: int(bucketSize/time.Minute) * bucketCount,
		Daily: make([]PublicDay, 30), Usage: []PublicUsage{}, Models: []PublicModel{}}
	dayIndex := make(map[string]int)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, beijingLocation)
	for i := range result.Daily {
		date := today.AddDate(0, 0, i-29).Format("2006-01-02")
		result.Daily[i].Date = date
		dayIndex[date] = i
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var selected, unlimited int
	var remaining float64
	err = tx.QueryRow(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN quota_usd<=0 THEN 1 ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN quota_usd>0 THEN MAX(quota_usd-used_usd,0) ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN quota_usd>0 THEN quota_usd ELSE 0 END),0),COALESCE(SUM(used_usd),0)
 FROM api_keys WHERE show_on_home=1`).Scan(&selected, &unlimited, &remaining, &result.Summary.QuotaUSD, &result.Summary.UsedUSD)
	if err != nil {
		return nil, err
	}
	result.Summary.Configured = selected > 0
	result.Summary.Unlimited = unlimited > 0
	if selected > 0 && unlimited == 0 {
		result.Summary.RemainingUSD = &remaining
	}
	usage := make(map[string]PublicUsage)
	err = publicRows(tx, `SELECT l.model,COUNT(*),SUM(CASE WHEN l.status BETWEEN 200 AND 299 THEN 1 ELSE 0 END),
 COALESCE(SUM(l.prompt_tokens),0),COALESCE(SUM(l.completion_tokens),0)
 FROM call_logs l JOIN api_keys k ON k.id=l.key_id AND k.show_on_home=1 GROUP BY l.model`,
		func(rows *sql.Rows) error {
			var u PublicUsage
			if err := rows.Scan(&u.Model, &u.Calls, &u.Successes, &u.PromptTokens, &u.CompletionTokens); err != nil {
				return err
			}
			usage[u.Model] = u
			result.Usage = append(result.Usage, u)
			result.Summary.Calls += u.Calls
			result.Summary.Successes += u.Successes
			result.Summary.PromptTokens += u.PromptTokens
			result.Summary.CompletionTokens += u.CompletionTokens
			if wildcard && u.Successes > 0 {
				if _, ok := catalog[u.Model]; !ok {
					catalog[u.Model] = publicFamily(u.Model, "")
				}
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	err = publicRows(tx, `SELECT date(l.created_at,'+8 hours'),COUNT(*),
 SUM(CASE WHEN l.status BETWEEN 200 AND 299 THEN 1 ELSE 0 END),
 COALESCE(SUM(l.prompt_tokens),0),COALESCE(SUM(l.completion_tokens),0)
 FROM call_logs l JOIN api_keys k ON k.id=l.key_id AND k.show_on_home=1
 WHERE unixepoch(l.created_at)>=? AND unixepoch(l.created_at)<? GROUP BY date(l.created_at,'+8 hours')`,
		func(rows *sql.Rows) error {
			var day PublicDay
			if err := rows.Scan(&day.Date, &day.Calls, &day.Successes, &day.PromptTokens, &day.CompletionTokens); err != nil {
				return err
			}
			index, exists := dayIndex[day.Date]
			if !exists {
				return nil
			}
			all := &result.Daily[index]
			all.Calls += day.Calls
			all.Successes += day.Successes
			all.PromptTokens += day.PromptTokens
			all.CompletionTokens += day.CompletionTokens
			return nil
		}, today.AddDate(0, 0, -29).Unix(), today.AddDate(0, 0, 1).Unix())
	if err != nil {
		return nil, err
	}
	histories := make(map[string][]PublicInterval)
	var allCalls, allSuccess int64
	bucketSeconds := int64(bucketSize / time.Second)
	err = publicRows(tx, `SELECT l.model,MIN(CAST((unixepoch(l.created_at)-?)/? AS INTEGER),?) AS slot,
 COUNT(*),SUM(CASE WHEN l.status BETWEEN 200 AND 299 THEN 1 ELSE 0 END)
 FROM call_logs l JOIN api_keys k ON k.id=l.key_id AND k.show_on_home=1
 WHERE unixepoch(l.created_at) BETWEEN ? AND ? GROUP BY l.model,slot`,
		func(rows *sql.Rows) error {
			var model string
			var slot, calls, successes int64
			if err := rows.Scan(&model, &slot, &calls, &successes); err != nil {
				return err
			}
			if histories[model] == nil {
				histories[model] = newHistory()
			}
			bucket := &histories[model][slot]
			bucket.Calls, bucket.Successes = calls, successes
			allCalls += calls
			allSuccess += successes
			return nil
		}, historyStart.Unix(), bucketSeconds, bucketCount-1, historyStart.Unix(), windowEnd.Unix())
	if err != nil {
		return nil, err
	}
	type lastCall struct {
		status int
		at     string
	}
	latest := make(map[string]lastCall)
	err = publicRows(tx, `SELECT model,status,created_at FROM (
 SELECT l.model,l.status,l.created_at,
 ROW_NUMBER() OVER (PARTITION BY l.model ORDER BY unixepoch(l.created_at) DESC,l.id DESC) AS position
 FROM call_logs l JOIN api_keys k ON k.id=l.key_id AND k.show_on_home=1
 WHERE unixepoch(l.created_at)<=?) WHERE position=1`, func(rows *sql.Rows) error {
		var model string
		var last lastCall
		if err := rows.Scan(&model, &last.status, &last.at); err != nil {
			return err
		}
		latest[model] = last
		return nil
	}, windowEnd.Unix())
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	result.Summary.Uptime = requestUptime(allSuccess, allCalls)
	for model, family := range catalog {
		u, ok := usage[model]
		if !ok {
			u.Model = model
		}
		history := histories[model]
		if history == nil {
			history = newHistory()
		}
		var calls, success int64
		for _, d := range history {
			calls += d.Calls
			success += d.Successes
		}
		last := latest[model]
		status := "idle"
		if at, err := time.Parse(time.RFC3339, last.at); err == nil && !at.Before(historyStart) {
			if last.status >= 200 && last.status < 300 {
				status = "operational"
			} else {
				status = "incident"
			}
		}
		result.Models = append(result.Models, PublicModel{PublicUsage: u, Family: family, Status: status,
			Uptime: requestUptime(success, calls), LastCall: last.at, History: history})
	}
	sort.Slice(result.Models, func(i, j int) bool {
		if result.Models[i].Family != result.Models[j].Family {
			return result.Models[i].Family < result.Models[j].Family
		}
		return result.Models[i].Model < result.Models[j].Model
	})
	sort.Slice(result.Usage, func(i, j int) bool {
		a, b := result.Usage[i], result.Usage[j]
		ta, tb := a.PromptTokens+a.CompletionTokens, b.PromptTokens+b.CompletionTokens
		if ta == tb {
			return a.Model < b.Model
		}
		return ta > tb
	})
	return result, nil
}
