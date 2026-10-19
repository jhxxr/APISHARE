package main

import (
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Admin model overview: which configured upstreams can serve each model, plus
// real outcomes from the same rolling 24-hour window used by the public uptime.
// Wildcard model discovery is shared with the public page and never blocks the
// response; the response reports that a background refresh is still running.

const modelOverviewWindow = 24 * time.Hour

type modelOverviewSource struct {
	UpstreamID    int64    `json:"upstream_id"`
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	Configured    bool     `json:"configured"`
	Explicit      bool     `json:"explicit"`
	UpstreamModel string   `json:"upstream_model"`
	Healthy       bool     `json:"healthy"`
	Cooldown      bool     `json:"cooldown"`
	RateLimited   bool     `json:"rate_limited"`
	Fails         int      `json:"fails"`
	Calls         int64    `json:"calls"`
	Successes     int64    `json:"successes"`
	Uptime        *float64 `json:"uptime_percent"`
	AvgTPS        *float64 `json:"avg_tps"`
	LastStatus    int      `json:"last_status"`
	LastCall      string   `json:"last_call"`
}

type modelOverviewItem struct {
	Model      string                `json:"model"`
	Family     string                `json:"family"`
	Status     string                `json:"status"`
	Calls      int64                 `json:"calls"`
	Successes  int64                 `json:"successes"`
	Uptime     *float64              `json:"uptime_percent"`
	AvgTPS     *float64              `json:"avg_tps"`
	LastStatus int                   `json:"last_status"`
	LastCall   string                `json:"last_call"`
	Sources    []modelOverviewSource `json:"sources"`
}

type modelOverviewResponse struct {
	UpdatedAt     string              `json:"updated_at"`
	WindowHours   int                 `json:"window_hours"`
	ModelsLoading bool                `json:"models_loading"`
	Models        []modelOverviewItem `json:"models"`
}

func registerModelOverviewRoutes(auth *gin.RouterGroup) {
	auth.GET("/models/overview", func(c *gin.Context) {
		overview, err := buildModelOverview()
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, overview)
	})
}

// Explicit allowlist entries and model-map keys are shown even when the
// OpenCode protocol catalog has not loaded yet; wildcard entries already come
// from discovery, which applies the catalog and free-model filters.
func upstreamServesOverviewModel(u *Upstream, model string) bool {
	serves, _ := u.servesModelExplicit(model)
	if !serves {
		return false
	}
	if (u.openCodePublic() || u.openCodeFreeOnly()) && !u.openCodeFreeModel(u.mapModel(model)) {
		return false
	}
	return true
}

func buildModelOverview() (*modelOverviewResponse, error) {
	now := beijingNow().Truncate(time.Second)
	windowStart := now.Add(-modelOverviewWindow)
	snapshot, err := getRoutingSnapshot()
	if err != nil {
		return nil, err
	}

	sources := make(map[string]map[int64]*modelOverviewSource)
	sourceFor := func(model string, upstreamID int64) *modelOverviewSource {
		byID := sources[model]
		if byID == nil {
			byID = make(map[int64]*modelOverviewSource)
			sources[model] = byID
		}
		source := byID[upstreamID]
		if source == nil {
			source = &modelOverviewSource{UpstreamID: upstreamID}
			byID[upstreamID] = source
		}
		return source
	}
	modelsLoading := false
	for i := range snapshot.upstreams {
		u := &snapshot.upstreams[i]
		if !u.Enabled || u.Weight <= 0 {
			continue
		}
		add := func(model string) {
			model = strings.TrimSpace(model)
			if model == "" || !upstreamServesOverviewModel(u, model) {
				return
			}
			source := sourceFor(model, u.ID)
			source.Name, source.Type, source.Configured = u.Name, u.Type, true
			_, source.Explicit = u.servesModelExplicit(model)
			source.UpstreamModel = u.mapModel(model)
		}
		if list := strings.TrimSpace(u.Models); list != "" {
			for _, model := range strings.Split(list, ",") {
				add(model)
			}
		} else {
			ids, pending := publicWildcardModels(*u)
			modelsLoading = modelsLoading || pending
			for _, model := range ids {
				add(model)
			}
		}
		for client := range u.modelMap() {
			if u.servesModel(client) {
				add(client)
			}
		}
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT model,upstream_id,upstream_name,COUNT(*),
		SUM(CASE WHEN status BETWEEN 200 AND 299 THEN 1 ELSE 0 END)
		FROM call_logs WHERE unixepoch(created_at) BETWEEN ? AND ?
		GROUP BY model,upstream_id,upstream_name`, windowStart.Unix(), now.Unix())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var model, name string
		var upstreamID, calls, successes int64
		if err := rows.Scan(&model, &upstreamID, &name, &calls, &successes); err != nil {
			rows.Close()
			return nil, err
		}
		source := sourceFor(model, upstreamID)
		if source.Name == "" {
			source.Name = name
		}
		source.Calls += calls
		source.Successes += successes
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// Newest row per model/source, including calls from upstreams that have
	// since been removed from the configuration.
	rows, err = tx.Query(`SELECT model,upstream_id,status,created_at FROM (
		SELECT model,upstream_id,status,created_at,
		 ROW_NUMBER() OVER (PARTITION BY model,upstream_id ORDER BY unixepoch(created_at) DESC,id DESC) AS position
		FROM call_logs WHERE unixepoch(created_at) BETWEEN ? AND ?
	) WHERE position=1`, windowStart.Unix(), now.Unix())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var model, at string
		var upstreamID int64
		var status int
		if err := rows.Scan(&model, &upstreamID, &status, &at); err != nil {
			rows.Close()
			return nil, err
		}
		source := sourceFor(model, upstreamID)
		source.LastStatus, source.LastCall = status, at
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	modelTPS := make(map[string][2]int64)
	rows, err = tx.Query(`SELECT model,upstream_id,COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(CASE WHEN first_token_ms>0 AND (duration_ms-first_token_ms)*10>=duration_ms
		 THEN duration_ms-first_token_ms ELSE duration_ms END),0)
		FROM call_logs
		WHERE status BETWEEN 200 AND 299 AND duration_ms>0 AND completion_tokens>0
		 AND unixepoch(created_at) BETWEEN ? AND ?
		GROUP BY model,upstream_id`, windowStart.Unix(), now.Unix())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var model string
		var upstreamID, completion, generating int64
		if err := rows.Scan(&model, &upstreamID, &completion, &generating); err != nil {
			rows.Close()
			return nil, err
		}
		sourceFor(model, upstreamID).AvgTPS = averageTPS(completion, generating)
		sum := modelTPS[model]
		sum[0] += completion
		sum[1] += generating
		modelTPS[model] = sum
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	models := make([]modelOverviewItem, 0, len(sources))
	for model, byID := range sources {
		item := modelOverviewItem{Model: model, Sources: make([]modelOverviewSource, 0, len(byID))}
		fallback := ""
		for upstreamID, source := range byID {
			if u, ok := snapshot.byID[upstreamID]; ok {
				if source.Name == "" {
					source.Name = u.Name
				}
				source.Type = u.Type
				if u.Enabled && u.Weight > 0 && upstreamServesOverviewModel(&u, model) {
					source.Configured = true
					if source.UpstreamModel == "" {
						source.UpstreamModel = u.mapModel(model)
						_, source.Explicit = u.servesModelExplicit(model)
					}
				}
				source.Healthy = u.availableForRelay()
				source.Cooldown = !healthAvailable(upstreamID)
				source.RateLimited = u.openCodeRateLimit() != nil
				source.Fails = u.relayFailures()
			} else {
				source.Healthy = false
				source.Fails = healthFails(upstreamID)
				source.Cooldown = !healthAvailable(upstreamID)
			}
			source.Uptime = requestUptime(source.Successes, source.Calls)
			item.Calls += source.Calls
			item.Successes += source.Successes
			if source.Calls > 0 && modelCallAfter(source.LastCall, item.LastCall) {
				item.LastCall, item.LastStatus = source.LastCall, source.LastStatus
			}
			if fallback == "" && source.Configured {
				fallback = source.Type
			}
			item.Sources = append(item.Sources, *source)
		}
		sort.Slice(item.Sources, func(i, j int) bool {
			a, b := item.Sources[i], item.Sources[j]
			if a.Configured != b.Configured {
				return a.Configured
			}
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			return a.UpstreamID < b.UpstreamID
		})
		item.Uptime = requestUptime(item.Successes, item.Calls)
		if sum := modelTPS[model]; sum[0] > 0 || sum[1] > 0 {
			item.AvgTPS = averageTPS(sum[0], sum[1])
		}
		switch {
		case item.LastCall == "":
			item.Status = "idle"
		case item.LastStatus >= 200 && item.LastStatus < 300:
			item.Status = "operational"
		default:
			item.Status = "incident"
		}
		item.Family = publicFamily(model, fallback)
		models = append(models, item)
	}
	statusRank := map[string]int{"incident": 0, "operational": 1, "idle": 2}
	sort.Slice(models, func(i, j int) bool {
		if statusRank[models[i].Status] != statusRank[models[j].Status] {
			return statusRank[models[i].Status] < statusRank[models[j].Status]
		}
		if models[i].Calls != models[j].Calls {
			return models[i].Calls > models[j].Calls
		}
		return models[i].Model < models[j].Model
	})
	return &modelOverviewResponse{UpdatedAt: now.Format(time.RFC3339), WindowHours: 24, ModelsLoading: modelsLoading, Models: models}, nil
}

// Log timestamps normally carry the same +08:00 offset, but legacy rows may
// use another offset; parse before comparing so "newest" stays chronological.
func modelCallAfter(candidate, current string) bool {
	if candidate == "" {
		return false
	}
	if current == "" {
		return true
	}
	candidateTime, candidateErr := time.Parse(time.RFC3339, candidate)
	currentTime, currentErr := time.Parse(time.RFC3339, current)
	if candidateErr != nil || currentErr != nil {
		return candidate > current
	}
	return candidateTime.After(currentTime)
}
