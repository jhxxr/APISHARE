package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
)

// dashboardTrendHours is the number of hourly buckets shown in the overview
// chart. The last bucket is the current, possibly partial, hour.
const dashboardTrendHours = 24

// Rankings return the leaders for both cost and requests so the client can
// switch metrics without a second round trip.
const dashboardRankLimit = 12

type dashboardMetric struct {
	Value  float64   `json:"value"`
	Change *float64  `json:"change"`
	Spark  []float64 `json:"spark"`
}

type dashboardHealth struct {
	RPM       float64 `json:"rpm"`
	TPM       int64   `json:"tpm"`
	ErrorRate float64 `json:"error_rate"`
	P50MS     int64   `json:"p50_ms"`
}

type dashboardKPI struct {
	RPM      dashboardMetric `json:"rpm"`
	Cost     dashboardMetric `json:"cost"`
	Requests dashboardMetric `json:"requests"`
	Success  dashboardMetric `json:"success"`
	Health   dashboardHealth `json:"health"`
}

type dashboardTrend struct {
	Labels       []string  `json:"labels"`
	CachedCost   []float64 `json:"cached_cost"`
	UncachedCost []float64 `json:"uncached_cost"`
	Success      []float64 `json:"success"`
	Failed       []float64 `json:"failed"`
	Cost         []float64 `json:"cost"`
	Calls        []int64   `json:"calls"`
	TotalCost    float64   `json:"total_cost"`
	TotalCalls   int64     `json:"total_calls"`
}

type dashboardCache struct {
	Read  int64   `json:"read"`
	Input int64   `json:"input"`
	Write int64   `json:"write"`
	Ratio float64 `json:"ratio"`
}

type dashboardRankEntry struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Cost        float64   `json:"cost"`
	Calls       int64     `json:"calls"`
	CostPrev    float64   `json:"cost_prev"`
	CallsPrev   int64     `json:"calls_prev"`
	SeriesCost  []float64 `json:"series_cost"`
	SeriesCalls []int64   `json:"series_calls"`
}

type dashboardRankings struct {
	Keys      []dashboardRankEntry `json:"keys"`
	Models    []dashboardRankEntry `json:"models"`
	Upstreams []dashboardRankEntry `json:"upstreams"`
}

type dashboardData struct {
	GeneratedAt string            `json:"generated_at"`
	KPI         dashboardKPI      `json:"kpi"`
	Trend       dashboardTrend    `json:"trend"`
	Cache       dashboardCache    `json:"cache"`
	Rankings    dashboardRankings `json:"rankings"`
}

type dashboardWindow struct {
	Calls     int64
	Successes int64
	CostUSD   float64
}

func dashboardJSONTokens(expr string) string {
	return "CASE WHEN json_valid(billing_json) THEN CAST(json_extract(billing_json,'" + expr + "') AS INTEGER) ELSE 0 END"
}

const dashboardCachedCost = "CASE WHEN status BETWEEN 200 AND 299 AND json_valid(billing_json) THEN COALESCE(CAST(json_extract(billing_json,'$.cost.cache_read') AS REAL),0)+COALESCE(CAST(json_extract(billing_json,'$.cost.cache_write') AS REAL),0) ELSE 0 END"

func queryDashboardWindow(ctx context.Context, start, end time.Time) (dashboardWindow, error) {
	var item dashboardWindow
	err := db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN status BETWEEN 200 AND 299 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status BETWEEN 200 AND 299 THEN cost_usd ELSE 0 END),0)
		FROM call_logs WHERE unixepoch(created_at)>=? AND unixepoch(created_at)<?`,
		start.Unix(), end.Unix()).Scan(&item.Calls, &item.Successes, &item.CostUSD)
	return item, err
}

func queryDashboardP50(ctx context.Context, start, end time.Time) (int64, error) {
	const durationExpr = "CASE WHEN duration_ms>0 THEN duration_ms ELSE latency_ms END"
	var count int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM call_logs
		WHERE unixepoch(created_at)>=? AND unixepoch(created_at)<? AND status BETWEEN 200 AND 299 AND `+durationExpr+`>0`,
		start.Unix(), end.Unix()).Scan(&count); err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}
	var p50 int64
	err := db.QueryRowContext(ctx, `SELECT `+durationExpr+` FROM call_logs
		WHERE unixepoch(created_at)>=? AND unixepoch(created_at)<? AND status BETWEEN 200 AND 299 AND `+durationExpr+`>0
		ORDER BY 1 LIMIT 1 OFFSET ?`, start.Unix(), end.Unix(), count/2).Scan(&p50)
	return p50, err
}

func percentChange(current, previous float64) *float64 {
	if previous <= 0 {
		return nil
	}
	change := (current - previous) / previous * 100
	return &change
}

func ratePercent(successes, calls int64) float64 {
	if calls <= 0 {
		return 0
	}
	return float64(successes) / float64(calls) * 100
}

type dashboardBucket struct {
	hour         int64
	labels       []string
	cachedCost   []float64
	uncachedCost []float64
	success      []float64
	failed       []float64
	cost         []float64
	calls        []int64
	tokens       []int64
	cacheRead    []int64
	cacheInput   []int64
	cacheWrite   []int64
}

func queryDashboardBuckets(ctx context.Context, start time.Time, hours int) (*dashboardBucket, error) {
	bucket := &dashboardBucket{
		hour:         start.Unix() / 3600,
		labels:       make([]string, hours),
		cachedCost:   make([]float64, hours),
		uncachedCost: make([]float64, hours),
		success:      make([]float64, hours),
		failed:       make([]float64, hours),
		cost:         make([]float64, hours),
		calls:        make([]int64, hours),
		tokens:       make([]int64, hours),
		cacheRead:    make([]int64, hours),
		cacheInput:   make([]int64, hours),
		cacheWrite:   make([]int64, hours),
	}
	for i := 0; i < hours; i++ {
		bucket.labels[i] = start.Add(time.Duration(i) * time.Hour).Format("15:04")
	}
	end := start.Add(time.Duration(hours) * time.Hour)
	rows, err := db.QueryContext(ctx, `SELECT CAST(unixepoch(created_at)/3600 AS INTEGER) h,
		COUNT(*),
		COALESCE(SUM(CASE WHEN status BETWEEN 200 AND 299 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status BETWEEN 200 AND 299 THEN cost_usd ELSE 0 END),0),
		COALESCE(SUM(`+dashboardCachedCost+`),0),
		COALESCE(SUM(CASE WHEN status BETWEEN 200 AND 299 THEN `+dashboardJSONTokens("$.usage.cache_read")+` ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status BETWEEN 200 AND 299 THEN `+dashboardJSONTokens("$.usage.input")+` ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status BETWEEN 200 AND 299 THEN `+dashboardJSONTokens("$.usage.cache_write")+` ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status BETWEEN 200 AND 299 THEN prompt_tokens+completion_tokens ELSE 0 END),0)
		FROM call_logs WHERE unixepoch(created_at)>=? AND unixepoch(created_at)<? GROUP BY h`,
		start.Unix(), end.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var hour, calls, successes, tokens, cacheRead, cacheInput, cacheWrite int64
		var cost, cachedCost float64
		if err := rows.Scan(&hour, &calls, &successes, &cost, &cachedCost, &cacheRead, &cacheInput, &cacheWrite, &tokens); err != nil {
			return nil, err
		}
		index := hour - bucket.hour
		if index < 0 || index >= int64(hours) {
			continue
		}
		uncached := cost - cachedCost
		if uncached < 0 {
			uncached = 0
		}
		bucket.calls[index] = calls
		bucket.success[index] = float64(successes)
		bucket.failed[index] = float64(calls - successes)
		bucket.cost[index] = cost
		bucket.cachedCost[index] = cachedCost
		bucket.uncachedCost[index] = uncached
		bucket.tokens[index] = tokens
		bucket.cacheRead[index] = cacheRead
		bucket.cacheInput[index] = cacheInput
		bucket.cacheWrite[index] = cacheWrite
	}
	return bucket, rows.Err()
}

type dashboardRankDef struct {
	// idExpr and nameExpr are evaluated against call_logs aliased as "l".
	// Names prefer the current configuration so renamed keys and upstreams
	// keep a single leaderboard row while their history stays intact.
	idExpr    string
	nameExpr  string
	fromExpr  string
	emptyName string
}

var dashboardRankDefs = map[string]dashboardRankDef{
	"keys": {
		idExpr: "l.key_id", nameExpr: "COALESCE(NULLIF(k.name,''),l.key_name)",
		fromExpr: "call_logs l LEFT JOIN api_keys k ON k.id=l.key_id", emptyName: "未知密钥",
	},
	"models": {
		idExpr: "0", nameExpr: "l.model", fromExpr: "call_logs l", emptyName: "未知模型",
	},
	"upstreams": {
		idExpr: "l.upstream_id", nameExpr: "COALESCE(NULLIF(u.name,''),l.upstream_name)",
		fromExpr: "call_logs l LEFT JOIN upstreams u ON u.id=l.upstream_id", emptyName: "未分配上游",
	},
}

func dashboardRankKey(id int64, name string) string {
	return fmt.Sprintf("%d\x00%s", id, name)
}

func queryDashboardRanking(ctx context.Context, def dashboardRankDef, currentStart, previousStart, now time.Time, bucket *dashboardBucket, hours int) ([]dashboardRankEntry, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+def.idExpr+` AS id, `+def.nameExpr+` AS name,
		COALESCE(SUM(CASE WHEN unixepoch(l.created_at)>=? THEN 1 ELSE 0 END),0) calls_cur,
		COALESCE(SUM(CASE WHEN unixepoch(l.created_at)>=? AND l.status BETWEEN 200 AND 299 THEN l.cost_usd ELSE 0 END),0) cost_cur,
		COALESCE(SUM(CASE WHEN unixepoch(l.created_at)<? THEN 1 ELSE 0 END),0) calls_prev,
		COALESCE(SUM(CASE WHEN unixepoch(l.created_at)<? AND l.status BETWEEN 200 AND 299 THEN l.cost_usd ELSE 0 END),0) cost_prev
		FROM `+def.fromExpr+` WHERE unixepoch(l.created_at)>=? GROUP BY 1,2`,
		currentStart.Unix(), currentStart.Unix(), currentStart.Unix(), currentStart.Unix(), previousStart.Unix())
	if err != nil {
		return nil, err
	}
	byKey := map[string]*dashboardRankEntry{}
	for rows.Next() {
		var item dashboardRankEntry
		if err := rows.Scan(&item.ID, &item.Name, &item.Calls, &item.Cost, &item.CallsPrev, &item.CostPrev); err != nil {
			rows.Close()
			return nil, err
		}
		if item.Name == "" {
			item.Name = def.emptyName
		}
		item.SeriesCost = make([]float64, hours)
		item.SeriesCalls = make([]int64, hours)
		if item.Calls == 0 && item.Cost == 0 {
			continue
		}
		copyItem := item
		byKey[dashboardRankKey(item.ID, item.Name)] = &copyItem
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = db.QueryContext(ctx, `SELECT `+def.idExpr+` AS id, `+def.nameExpr+` AS name,
		CAST(unixepoch(l.created_at)/3600 AS INTEGER) h, COUNT(*),
		COALESCE(SUM(CASE WHEN l.status BETWEEN 200 AND 299 THEN l.cost_usd ELSE 0 END),0)
		FROM `+def.fromExpr+` WHERE unixepoch(l.created_at)>=? AND unixepoch(l.created_at)<? GROUP BY 1,2,3`,
		currentStart.Unix(), now.Unix())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, calls, hour int64
		var name string
		var cost float64
		if err := rows.Scan(&id, &name, &hour, &calls, &cost); err != nil {
			rows.Close()
			return nil, err
		}
		if name == "" {
			name = def.emptyName
		}
		item := byKey[dashboardRankKey(id, name)]
		if item == nil {
			continue
		}
		index := hour - bucket.hour
		if index < 0 || index >= int64(hours) {
			continue
		}
		item.SeriesCalls[index] += calls
		item.SeriesCost[index] += cost
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	all := make([]dashboardRankEntry, 0, len(byKey))
	for _, item := range byKey {
		all = append(all, *item)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Cost != all[j].Cost {
			return all[i].Cost > all[j].Cost
		}
		return all[i].Calls > all[j].Calls
	})
	byCost := make([]dashboardRankEntry, len(all))
	copy(byCost, all)
	sort.SliceStable(byCost, func(i, j int) bool {
		if byCost[i].Calls != byCost[j].Calls {
			return byCost[i].Calls > byCost[j].Calls
		}
		return byCost[i].Cost > byCost[j].Cost
	})
	seen := map[string]bool{}
	result := make([]dashboardRankEntry, 0, dashboardRankLimit*2)
	add := func(item dashboardRankEntry) {
		key := dashboardRankKey(item.ID, item.Name)
		if seen[key] {
			return
		}
		seen[key] = true
		result = append(result, item)
	}
	for i := 0; i < len(all) && i < dashboardRankLimit; i++ {
		add(all[i])
	}
	for i := 0; i < len(byCost) && i < dashboardRankLimit; i++ {
		add(byCost[i])
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Cost != result[j].Cost {
			return result[i].Cost > result[j].Cost
		}
		return result[i].Calls > result[j].Calls
	})
	return result, nil
}

func buildDashboard(ctx context.Context, now time.Time) (*dashboardData, error) {
	now = now.In(beijingLocation)
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, beijingLocation)
	yesterdayStart := todayStart.AddDate(0, 0, -1)
	elapsed := now.Sub(todayStart)
	// Align the rolling window with the hourly buckets so the chart, cache
	// figures and rankings all describe exactly the same 24 hours.
	currentHour := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, beijingLocation)
	trendStart := currentHour.Add(-(dashboardTrendHours - 1) * time.Hour)
	last24h := trendStart
	prev24h := last24h.Add(-24 * time.Hour)
	lastHour := now.Add(-time.Hour)

	today, err := queryDashboardWindow(ctx, todayStart, now)
	if err != nil {
		return nil, err
	}
	yesterday, err := queryDashboardWindow(ctx, yesterdayStart, yesterdayStart.Add(elapsed))
	if err != nil {
		return nil, err
	}
	recent24, err := queryDashboardWindow(ctx, last24h, now)
	if err != nil {
		return nil, err
	}
	previous24, err := queryDashboardWindow(ctx, prev24h, last24h)
	if err != nil {
		return nil, err
	}
	// A five-minute window keeps the live RPM readable instead of flipping
	// between 0 and 1 on every refresh.
	const rpmWindow = 5 * time.Minute
	recentRPM, err := queryDashboardWindow(ctx, now.Add(-rpmWindow), now)
	if err != nil {
		return nil, err
	}
	previousRPM, err := queryDashboardWindow(ctx, now.Add(-2*rpmWindow), now.Add(-rpmWindow))
	if err != nil {
		return nil, err
	}
	rpmValue := float64(recentRPM.Calls) / rpmWindow.Minutes()
	previousRPMValue := float64(previousRPM.Calls) / rpmWindow.Minutes()
	lastHourStats, err := queryDashboardWindow(ctx, lastHour, now)
	if err != nil {
		return nil, err
	}
	p50, err := queryDashboardP50(ctx, lastHour, now)
	if err != nil {
		return nil, err
	}

	var tpm int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(prompt_tokens+completion_tokens),0) FROM call_logs
		WHERE unixepoch(created_at)>=? AND unixepoch(created_at)<? AND status BETWEEN 200 AND 299`,
		now.Add(-time.Minute).Unix(), now.Unix()).Scan(&tpm); err != nil {
		return nil, err
	}

	bucket, err := queryDashboardBuckets(ctx, trendStart, dashboardTrendHours)
	if err != nil {
		return nil, err
	}

	trend := dashboardTrend{
		Labels:       bucket.labels,
		CachedCost:   bucket.cachedCost,
		UncachedCost: bucket.uncachedCost,
		Success:      bucket.success,
		Failed:       bucket.failed,
		Cost:         bucket.cost,
		Calls:        bucket.calls,
	}
	rpmSpark := make([]float64, len(bucket.calls))
	successSpark := make([]float64, len(bucket.calls))
	callsSpark := make([]float64, len(bucket.calls))
	costSpark := make([]float64, len(bucket.calls))
	for i := range bucket.calls {
		rpmSpark[i] = float64(bucket.calls[i]) / 60
		callsSpark[i] = float64(bucket.calls[i])
		costSpark[i] = bucket.cost[i]
		if bucket.calls[i] > 0 {
			successSpark[i] = bucket.success[i] / float64(bucket.calls[i]) * 100
		}
		trend.TotalCalls += bucket.calls[i]
		trend.TotalCost += bucket.cost[i]
	}

	var cacheRead, cacheInput, cacheWrite int64
	for i := range bucket.cacheRead {
		cacheRead += bucket.cacheRead[i]
		cacheInput += bucket.cacheInput[i]
		cacheWrite += bucket.cacheWrite[i]
	}
	cacheTotal := cacheRead + cacheInput + cacheWrite
	cacheRatio := 0.0
	if cacheTotal > 0 {
		cacheRatio = float64(cacheRead) / float64(cacheTotal) * 100
	}

	result := &dashboardData{
		GeneratedAt: now.Format(time.RFC3339),
		KPI: dashboardKPI{
			RPM: dashboardMetric{
				Value:  rpmValue,
				Change: percentChange(rpmValue, previousRPMValue),
				Spark:  rpmSpark,
			},
			Cost: dashboardMetric{
				Value:  today.CostUSD,
				Change: percentChange(today.CostUSD, yesterday.CostUSD),
				Spark:  costSpark,
			},
			Requests: dashboardMetric{
				Value:  float64(today.Calls),
				Change: percentChange(float64(today.Calls), float64(yesterday.Calls)),
				Spark:  callsSpark,
			},
			Success: dashboardMetric{
				Value:  ratePercent(recent24.Successes, recent24.Calls),
				Change: percentChange(ratePercent(recent24.Successes, recent24.Calls), ratePercent(previous24.Successes, previous24.Calls)),
				Spark:  successSpark,
			},
			Health: dashboardHealth{
				RPM:       rpmValue,
				TPM:       tpm,
				ErrorRate: ratePercent(lastHourStats.Calls-lastHourStats.Successes, lastHourStats.Calls),
				P50MS:     p50,
			},
		},
		Trend: trend,
		Cache: dashboardCache{
			Read:  cacheRead,
			Input: cacheInput,
			Write: cacheWrite,
			Ratio: cacheRatio,
		},
	}

	for name, def := range dashboardRankDefs {
		ranking, err := queryDashboardRanking(ctx, def, last24h, prev24h, now, bucket, dashboardTrendHours)
		if err != nil {
			return nil, err
		}
		switch name {
		case "keys":
			result.Rankings.Keys = ranking
		case "models":
			result.Rankings.Models = ranking
		case "upstreams":
			result.Rankings.Upstreams = ranking
		}
	}
	if result.Rankings.Keys == nil {
		result.Rankings.Keys = []dashboardRankEntry{}
	}
	if result.Rankings.Models == nil {
		result.Rankings.Models = []dashboardRankEntry{}
	}
	if result.Rankings.Upstreams == nil {
		result.Rankings.Upstreams = []dashboardRankEntry{}
	}
	return result, nil
}

func registerDashboardRoutes(auth *gin.RouterGroup) {
	auth.GET("/dashboard", func(c *gin.Context) {
		data, err := buildDashboard(c.Request.Context(), beijingNow())
		if err != nil {
			c.JSON(500, gin.H{"error": "无法读取仪表盘数据"})
			return
		}
		c.JSON(200, data)
	})
}
