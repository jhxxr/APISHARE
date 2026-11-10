package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
)

// Only public model metadata and site-wide aggregate observations belong here.
// In particular, never expose raw bandit arms, personal channels or upstream IDs.
type PublicModelRanking struct {
	Rank          int                   `json:"rank"`
	Model         string                `json:"model"`
	Family        string                `json:"family"`
	Capabilities  ModelCapabilityVector `json:"capabilities"`
	BanditCalls   int64                 `json:"bandit_calls"`
	BanditWinRate float64               `json:"bandit_win_rate,omitempty"`
	EWMATTFTMS    float64               `json:"ewma_ttft_ms,omitempty"`
	EWMATPS       float64               `json:"ewma_tps,omitempty"`
}

type PublicRankings struct {
	UpdatedAt     string               `json:"updated_at"`
	ModelsLoading bool                 `json:"models_loading"`
	Models        []PublicModelRanking `json:"models"`
}

func buildPublicRankings(now time.Time) (*PublicRankings, error) {
	snapshot, err := getRoutingSnapshot()
	if err != nil {
		return nil, err
	}
	catalog, _, loading := publicModelCatalog(snapshot)
	cfg := getAutoRouterConfig()
	result := &PublicRankings{
		UpdatedAt:     now.In(beijingLocation).Format(time.RFC3339),
		ModelsLoading: loading,
		Models:        make([]PublicModelRanking, 0, len(catalog)),
	}
	for model, family := range catalog {
		// Virtual routers are policies, not underlying model capabilities.
		if model == cfg.VirtualModel {
			continue
		}
		winRate, calls, ttft, tps := summarizeModelBanditStats(model)
		result.Models = append(result.Models, PublicModelRanking{
			Model:         model,
			Family:        family,
			Capabilities:  resolveModelCapabilityVector(model, cfg.ModelCapabilities),
			BanditCalls:   calls,
			BanditWinRate: winRate,
			EWMATTFTMS:    ttft,
			EWMATPS:       tps,
		})
	}
	sort.Slice(result.Models, func(i, j int) bool {
		a, b := result.Models[i], result.Models[j]
		if a.Capabilities.OverallScore != b.Capabilities.OverallScore {
			return a.Capabilities.OverallScore > b.Capabilities.OverallScore
		}
		return a.Model < b.Model
	})
	for i := range result.Models {
		result.Models[i].Rank = i + 1
	}
	return result, nil
}

func publicRankingsHandler(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	body, err := publicOverviews.get(c.Request.Context(), db, "rankings", func() ([]byte, error) {
		rankings, err := buildPublicRankings(time.Now())
		if err != nil {
			return nil, err
		}
		return json.Marshal(rankings)
	})
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "暂时无法读取模型排行，请稍后重试。"})
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}
