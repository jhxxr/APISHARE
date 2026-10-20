package main

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
)

type upstreamOverviewUsage struct {
	Calls            int64    `json:"calls"`
	Successes        int64    `json:"successes"`
	PromptTokens     int64    `json:"prompt_tokens"`
	CompletionTokens int64    `json:"completion_tokens"`
	CostUSD          float64  `json:"cost_usd"`
	FirstTokenTotal  int64    `json:"-"`
	FirstTokenCount  int64    `json:"-"`
	FirstTokenMS     *float64 `json:"first_token_ms"`
}

type upstreamHealthBucket struct {
	Calls     int64 `json:"calls"`
	Successes int64 `json:"successes"`
}

type upstreamOverviewItem struct {
	UpstreamID int64                  `json:"upstream_id"`
	Today      upstreamOverviewUsage  `json:"today"`
	Week       upstreamOverviewUsage  `json:"week"`
	Health     []upstreamHealthBucket `json:"health"`
}

type upstreamOverview struct {
	UpdatedAt   string                 `json:"updated_at"`
	WindowStart string                 `json:"window_start"`
	HealthStart string                 `json:"health_start"`
	Today       upstreamOverviewUsage  `json:"today"`
	Week        upstreamOverviewUsage  `json:"week"`
	Upstreams   []upstreamOverviewItem `json:"upstreams"`
}

func registerUpstreamOverviewRoutes(auth *gin.RouterGroup) {
	auth.GET("/upstreams/overview", func(c *gin.Context) {
		result, err := buildUpstreamOverview(c.Request.Context(), beijingNow())
		if err != nil {
			c.JSON(500, gin.H{"error": "无法读取上游调用统计"})
			return
		}
		c.JSON(200, result)
	})
}

func (u *upstreamOverviewUsage) add(other upstreamOverviewUsage) {
	u.Calls += other.Calls
	u.Successes += other.Successes
	u.PromptTokens += other.PromptTokens
	u.CompletionTokens += other.CompletionTokens
	u.CostUSD += other.CostUSD
	u.FirstTokenTotal += other.FirstTokenTotal
	u.FirstTokenCount += other.FirstTokenCount
}

func (u *upstreamOverviewUsage) finish() {
	if u.FirstTokenCount > 0 {
		average := float64(u.FirstTokenTotal) / float64(u.FirstTokenCount)
		u.FirstTokenMS = &average
	}
}

// Aggregate retained call details in one snapshot. Archived daily rollups have
// no upstream ID, so they cannot be attributed to a configured upstream.
// Never include deleted upstreams or requests that did not reach an upstream.
func buildUpstreamOverview(ctx context.Context, now time.Time) (*upstreamOverview, error) {
	now = now.In(beijingLocation).Truncate(time.Second)
	weekStart := now.AddDate(0, 0, -7)
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, beijingLocation)
	healthStart := now.Truncate(10 * time.Minute).Add(-190 * time.Minute)
	upstreams, err := dbListUpstreams()
	if err != nil {
		return nil, err
	}
	result := &upstreamOverview{UpdatedAt: now.Format(time.RFC3339), WindowStart: weekStart.Format(time.RFC3339), HealthStart: healthStart.Format(time.RFC3339), Upstreams: make([]upstreamOverviewItem, len(upstreams))}
	byID := make(map[int64]*upstreamOverviewItem, len(upstreams))
	for i, u := range upstreams {
		result.Upstreams[i] = upstreamOverviewItem{UpstreamID: u.ID, Health: make([]upstreamHealthBucket, 20)}
		byID[u.ID] = &result.Upstreams[i]
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Group by upstream and Beijing calendar day; this also provides today's
	// totals without issuing a separate scan or a query per upstream.
	rows, err := tx.QueryContext(ctx, `SELECT upstream_id, date(created_at,'+8 hours'), COUNT(*),
	 SUM(CASE WHEN status BETWEEN 200 AND 299 THEN 1 ELSE 0 END),
	 COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
	 COALESCE(SUM(CASE WHEN status BETWEEN 200 AND 299 THEN cost_usd ELSE 0 END),0),
	 COALESCE(SUM(CASE WHEN status BETWEEN 200 AND 299 AND first_token_ms>0 THEN first_token_ms ELSE 0 END),0),
	 SUM(CASE WHEN status BETWEEN 200 AND 299 AND first_token_ms>0 THEN 1 ELSE 0 END)
	 FROM call_logs WHERE unixepoch(created_at)>=? AND unixepoch(created_at)<=? GROUP BY upstream_id, date(created_at,'+8 hours')`, weekStart.Unix(), now.Unix())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var day string
		var usage upstreamOverviewUsage
		if err := rows.Scan(&id, &day, &usage.Calls, &usage.Successes, &usage.PromptTokens, &usage.CompletionTokens, &usage.CostUSD, &usage.FirstTokenTotal, &usage.FirstTokenCount); err != nil {
			rows.Close()
			return nil, err
		}
		if item := byID[id]; item != nil {
			item.Week.add(usage)
			result.Week.add(usage)
			if day == todayStart.Format("2006-01-02") {
				item.Today.add(usage)
				result.Today.add(usage)
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT upstream_id, (unixepoch(created_at)-?)/600, COUNT(*),
	 SUM(CASE WHEN status BETWEEN 200 AND 299 THEN 1 ELSE 0 END)
	 FROM call_logs WHERE unixepoch(created_at)>=? AND unixepoch(created_at)<=? GROUP BY upstream_id, (unixepoch(created_at)-?)/600`, healthStart.Unix(), healthStart.Unix(), now.Unix(), healthStart.Unix())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var bucket int
		var value upstreamHealthBucket
		if err := rows.Scan(&id, &bucket, &value.Calls, &value.Successes); err != nil {
			rows.Close()
			return nil, err
		}
		if item := byID[id]; item != nil && bucket >= 0 && bucket < len(item.Health) {
			item.Health[bucket] = value
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range result.Upstreams {
		result.Upstreams[i].Today.finish()
		result.Upstreams[i].Week.finish()
	}
	result.Today.finish()
	result.Week.finish()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
