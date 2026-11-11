package main

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
)

type PublicModelRanking struct {
	arenaModel
	SiteAvailable bool     `json:"site_available"`
	SiteModels    []string `json:"site_models,omitempty"`
}
type PublicRankings struct {
	UpdatedAt     string               `json:"updated_at"`
	PublishedAt   string               `json:"published_at,omitempty"`
	FetchedAt     string               `json:"fetched_at,omitempty"`
	SourceName    string               `json:"source_name"`
	SourceURL     string               `json:"source_url"`
	Board         string               `json:"board"`
	BoardLabel    string               `json:"board_label"`
	ScoreKind     string               `json:"score_kind"`
	ScoreLabel    string               `json:"score_label"`
	SampleLabel   string               `json:"sample_label"`
	Boards        []arenaBoard         `json:"boards"`
	Loading       bool                 `json:"loading"`
	Stale         bool                 `json:"stale"`
	ModelsLoading bool                 `json:"models_loading"`
	Models        []PublicModelRanking `json:"models"`
}

// Ignore presentation separators, not versions, dates, Pro/Mini or thinking modes.
func arenaModelIdentity(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if index := strings.LastIndex(model, "/"); index >= 0 && !strings.ContainsAny(model[:index], " ()") {
		model = model[index+1:]
	}
	var normalized strings.Builder
	var previous rune
	for _, r := range model {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			previous = 0
			continue
		}
		if normalized.Len() > 0 && (previous == 0 || unicode.IsDigit(previous) != unicode.IsDigit(r)) {
			normalized.WriteByte('-')
		}
		normalized.WriteRune(r)
		previous = r
	}
	return normalized.String()
}
func arenaSiteModelIndex(snapshot *routingSnapshot, catalog map[string]string) map[string][]string {
	index := make(map[string][]string)
	add := func(identity, clientModel string) {
		key := arenaModelIdentity(identity)
		if key == "" {
			return
		}
		for _, existing := range index[key] {
			if existing == clientModel {
				return
			}
		}
		index[key] = append(index[key], clientModel)
	}
	for model := range catalog {
		add(model, model)
		for i := range snapshot.upstreams {
			up := &snapshot.upstreams[i]
			if up.Enabled && up.Weight > 0 && servesRouterCandidate(up, model) {
				add(up.mapModel(resolveUpstreamCandidateModel(up, model)), model)
			}
		}
	}
	for _, models := range index {
		sort.Strings(models)
	}
	return index
}
func buildPublicRankings(now time.Time, board arenaBoard) (*PublicRankings, error) {
	routing, err := getRoutingSnapshot()
	if err != nil {
		return nil, err
	}
	catalog, _, loading := publicModelCatalogWithDiscovery(routing, false)
	delete(catalog, getAutoRouterConfig().VirtualModel)
	siteModels := arenaSiteModelIndex(routing, catalog)
	snapshot, refreshing, failed := getArenaSnapshot(db, board.ID)
	result := &PublicRankings{UpdatedAt: now.In(beijingLocation).Format(time.RFC3339), SourceName: "Arena 官方历史数据集 · overall", SourceURL: arenaDatasetURL, Board: board.ID, BoardLabel: board.Label, Boards: arenaBoards, Loading: refreshing, Stale: failed, ModelsLoading: loading, Models: []PublicModelRanking{}}
	result.ScoreKind, result.ScoreLabel, result.SampleLabel = "arena", "Arena Score", "对战样本"
	if board.ID == "agent" {
		result.ScoreKind, result.ScoreLabel, result.SampleLabel = "ips", "Agent IPS Score", "观测样本"
	}
	if snapshot == nil {
		return result, nil
	}
	result.PublishedAt, result.FetchedAt = snapshot.PublishedAt, snapshot.FetchedAt
	fetchedAt, err := time.Parse(time.RFC3339, snapshot.FetchedAt)
	result.Stale = failed || err != nil || now.Sub(fetchedAt) > arenaRefreshTTL
	// All source models, preserving source ranks and ties. Site membership is only a badge.
	for _, model := range snapshot.Models {
		matched := siteModels[arenaModelIdentity(model.Model)]
		result.Models = append(result.Models, PublicModelRanking{arenaModel: model, SiteAvailable: len(matched) > 0, SiteModels: matched})
	}
	return result, nil
}
func publicRankingsHandler(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	board, ok := findArenaBoard(c.DefaultQuery("board", "text"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不支持此排行榜"})
		return
	}
	body, err := publicOverviews.get(c.Request.Context(), db, "arena-rankings:"+board.ID, func() ([]byte, error) {
		rankings, err := buildPublicRankings(time.Now(), board)
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
