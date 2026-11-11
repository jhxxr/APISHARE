package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/parquet-go/parquet-go"
)

const (
	arenaDatasetURL = "https://huggingface.co/datasets/lmarena-ai/leaderboard-dataset"
	arenaRefreshTTL = 24 * time.Hour
	arenaMaxBytes   = 16 << 20
)

type arenaBoard struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Config string `json:"config"`
}

var arenaBoards = []arenaBoard{
	{"text", "Text · 文本（风格控制）", "text_style_control"},
	{"vision", "Vision · 视觉（风格控制）", "vision_style_control"},
	{"webdev", "WebDev · 网页开发", "webdev"},
	{"agent", "Agent · 智能体", "agent"},
	{"document", "Document · 文档（风格控制）", "document_style_control"},
	{"search", "Search · 搜索（风格控制）", "search_style_control"},
	{"text-to-image", "Text to Image · 文生图", "text_to_image"},
	{"image-edit", "Image Edit · 图像编辑", "image_edit"},
	{"text-to-video", "Text to Video · 文生视频", "text_to_video"},
	{"image-to-video", "Image to Video · 图生视频", "image_to_video"},
	{"video-edit", "Video Edit · 视频编辑", "video_edit"},
}

func findArenaBoard(id string) (arenaBoard, bool) {
	for _, board := range arenaBoards {
		if board.ID == id {
			return board, true
		}
	}
	return arenaBoard{}, false
}

// Project only official dataset columns. Never mix in heuristic or Bandit scores.
type arenaDatasetRow struct {
	ModelName    string   `parquet:"model_name,optional"`
	Organization string   `parquet:"organization,optional"`
	License      string   `parquet:"license,optional"`
	Rating       *float64 `parquet:"rating,optional"`
	RatingLower  *float64 `parquet:"rating_lower,optional"`
	RatingUpper  *float64 `parquet:"rating_upper,optional"`
	VoteCount    *float64 `parquet:"vote_count,optional"`
	AgentScore   *float64 `parquet:"score,optional"`
	AgentLower   *float64 `parquet:"score_ci_lower,optional"`
	AgentUpper   *float64 `parquet:"score_ci_upper,optional"`
	Observations *float64 `parquet:"observation_count,optional"`
	Rank         *uint64  `parquet:"rank,optional"`
	Category     string   `parquet:"category,optional"`
	PublishDate  string   `parquet:"leaderboard_publish_date,optional"`
}

type arenaModel struct {
	Rank        int      `json:"rank"`
	Model       string   `json:"model"`
	Family      string   `json:"family"`
	License     string   `json:"license,omitempty"`
	Score       float64  `json:"score"`
	RatingLower *float64 `json:"rating_lower,omitempty"`
	RatingUpper *float64 `json:"rating_upper,omitempty"`
	Votes       *int64   `json:"votes,omitempty"`
}

type arenaSnapshot struct {
	PublishedAt string       `json:"published_at"`
	FetchedAt   string       `json:"fetched_at"`
	Models      []arenaModel `json:"models"`
}

func finiteArenaNumber(value *float64) bool {
	return value != nil && !math.IsNaN(*value) && !math.IsInf(*value, 0)
}

func parseArenaParquet(blob []byte, fetchedAt time.Time) (result *arenaSnapshot, err error) {
	// Reject malformed files/schema changes without taking down the refresh loop.
	defer func() {
		if recover() != nil {
			result, err = nil, errors.New("invalid Arena Parquet schema")
		}
	}()
	if len(blob) > arenaMaxBytes {
		return nil, errors.New("Arena dataset exceeds size limit")
	}
	file, err := parquet.OpenFile(bytes.NewReader(blob), int64(len(blob)))
	if err != nil {
		return nil, err
	}
	if file.NumRows() > 200000 {
		return nil, errors.New("Arena dataset exceeds row limit")
	}
	reader := parquet.NewGenericReader[arenaDatasetRow](bytes.NewReader(blob))
	defer reader.Close()
	result = &arenaSnapshot{FetchedAt: fetchedAt.UTC().Format(time.RFC3339), Models: []arenaModel{}}
	batch := make([]arenaDatasetRow, 256)
	for {
		n, readErr := reader.Read(batch)
		for _, row := range batch[:n] {
			if row.Category != "overall" {
				continue
			}
			if row.Rating == nil && row.AgentScore != nil {
				row.Rating, row.RatingLower, row.RatingUpper, row.VoteCount = row.AgentScore, row.AgentLower, row.AgentUpper, row.Observations
			}
			if _, err := time.Parse("2006-01-02", row.PublishDate); err != nil {
				return nil, errors.New("invalid Arena publication date")
			}
			if row.PublishDate < result.PublishedAt {
				continue
			}
			if row.PublishDate > result.PublishedAt {
				result.PublishedAt, result.Models = row.PublishDate, []arenaModel{}
			}
			if strings.TrimSpace(row.ModelName) == "" || row.Rank == nil || *row.Rank == 0 || *row.Rank > 200000 || !finiteArenaNumber(row.Rating) {
				return nil, errors.New("invalid Arena model score or rank")
			}
			// Display names are not unique identifiers in the official dataset
			// (e.g. WebDev harness entries). Keep each source row and its rank.
			model := arenaModel{Rank: int(*row.Rank), Model: row.ModelName, Family: row.Organization,
				License: row.License, Score: *row.Rating}
			if finiteArenaNumber(row.RatingLower) {
				lower := *row.RatingLower
				model.RatingLower = &lower
			}
			if finiteArenaNumber(row.RatingUpper) {
				upper := *row.RatingUpper
				model.RatingUpper = &upper
			}
			if finiteArenaNumber(row.VoteCount) && *row.VoteCount >= 0 && *row.VoteCount < float64(math.MaxInt64) {
				votes := int64(*row.VoteCount)
				model.Votes = &votes
			}
			result.Models = append(result.Models, model)
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return nil, readErr
			}
			break
		}
	}
	if len(result.Models) == 0 {
		return nil, errors.New("Arena dataset has no overall rankings")
	}
	sort.SliceStable(result.Models, func(i, j int) bool {
		if result.Models[i].Rank != result.Models[j].Rank {
			return result.Models[i].Rank < result.Models[j].Rank
		}
		return result.Models[i].Model < result.Models[j].Model
	})
	return result, nil
}

func fetchArenaSnapshot(ctx context.Context, client *http.Client, url string) (*arenaSnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "APIShare-Arena-Leaderboard/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Arena dataset returned HTTP %d", resp.StatusCode)
	}
	blob, err := io.ReadAll(io.LimitReader(resp.Body, arenaMaxBytes+1))
	if err != nil {
		return nil, err
	}
	return parseArenaParquet(blob, time.Now())
}

type arenaCacheEntry struct {
	snapshot    *arenaSnapshot
	refreshing  bool
	failed      bool
	lastAttempt time.Time
}

var arenaLeaderboards = struct {
	sync.Mutex
	database *sql.DB
	entries  map[string]*arenaCacheEntry
}{}

func arenaSettingsKey(board string) string { return "arena_leaderboard_v1_" + board }

// Call under lock. A new namespace prevents old guessed scores being reloaded.
func arenaEntryLocked(database *sql.DB, board string) *arenaCacheEntry {
	if arenaLeaderboards.database != database || arenaLeaderboards.entries == nil {
		arenaLeaderboards.database = database
		arenaLeaderboards.entries = make(map[string]*arenaCacheEntry)
	}
	if entry := arenaLeaderboards.entries[board]; entry != nil {
		return entry
	}
	entry := &arenaCacheEntry{}
	if database != nil {
		var raw string
		if database.QueryRow("SELECT v FROM settings WHERE k=?", arenaSettingsKey(board)).Scan(&raw) == nil {
			var snapshot arenaSnapshot
			if json.Unmarshal([]byte(raw), &snapshot) == nil && len(snapshot.Models) > 0 && snapshot.PublishedAt != "" {
				entry.snapshot = &snapshot
			}
		}
	}
	arenaLeaderboards.entries[board] = entry
	return entry
}

func getArenaSnapshot(database *sql.DB, board string) (*arenaSnapshot, bool, bool) {
	arenaLeaderboards.Lock()
	defer arenaLeaderboards.Unlock()
	entry := arenaEntryLocked(database, board)
	return entry.snapshot, entry.refreshing, entry.failed
}

func storeArenaSnapshot(database *sql.DB, board string, snapshot *arenaSnapshot) error {
	if snapshot == nil || len(snapshot.Models) == 0 {
		return errors.New("empty Arena snapshot")
	}
	blob, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	arenaLeaderboards.Lock()
	defer arenaLeaderboards.Unlock()
	if arenaLeaderboards.database != database {
		return errors.New("Arena database changed during refresh")
	}
	entry := arenaEntryLocked(database, board)
	if entry.snapshot != nil && entry.snapshot.PublishedAt > snapshot.PublishedAt {
		return errors.New("refusing older Arena snapshot")
	}
	if database != nil {
		if _, err := database.Exec(`INSERT INTO settings(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
			arenaSettingsKey(board), string(blob)); err != nil {
			return err
		}
	}
	entry.snapshot, entry.failed = snapshot, false
	return nil
}

func refreshArenaBoard(ctx context.Context, database *sql.DB, board arenaBoard, client *http.Client) {
	arenaLeaderboards.Lock()
	entry := arenaEntryLocked(database, board.ID)
	if entry.refreshing {
		arenaLeaderboards.Unlock()
		return
	}
	now := time.Now()
	if !entry.lastAttempt.IsZero() && sameArenaSyncDay(entry.lastAttempt, now) {
		arenaLeaderboards.Unlock()
		return
	}
	if entry.snapshot != nil {
		if fetched, err := time.Parse(time.RFC3339, entry.snapshot.FetchedAt); err == nil && sameArenaSyncDay(fetched, now) {
			arenaLeaderboards.Unlock()
			return
		}
	}
	entry.lastAttempt = now
	entry.refreshing = true
	arenaLeaderboards.Unlock()
	url := arenaDatasetURL + "/resolve/main/" + board.Config + "/latest-00000-of-00001.parquet"
	snapshot, err := fetchArenaSnapshot(ctx, client, url)
	if err == nil {
		err = storeArenaSnapshot(database, board.ID, snapshot)
	}
	arenaLeaderboards.Lock()
	if arenaLeaderboards.database == database {
		entry.refreshing, entry.failed = false, err != nil
	}
	arenaLeaderboards.Unlock()
	if err != nil && ctx.Err() == nil {
		log.Printf("[arena] %s refresh failed: %v", board.ID, err)
	}
}

func arenaLeaderboardRefreshLoop(ctx context.Context) {
	database := db
	client := &http.Client{Timeout: 45 * time.Second}
	refresh := func() {
		for _, board := range arenaBoards {
			if ctx.Err() != nil {
				return
			}
			refreshArenaBoard(ctx, database, board, client)
		}
	}
	for {
		refresh()
		timer := time.NewTimer(time.Until(nextArenaSync(time.Now())))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func sameArenaSyncDay(a, b time.Time) bool {
	return a.In(beijingLocation).Format("2006-01-02") == b.In(beijingLocation).Format("2006-01-02")
}

// One scheduled update each day at 04:00 Beijing time, plus initial cache fill.
func nextArenaSync(now time.Time) time.Time {
	now = now.In(beijingLocation)
	next := time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, beijingLocation)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}
