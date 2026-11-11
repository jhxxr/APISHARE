package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"
)

func arenaFloat(value float64) *float64 { return &value }
func arenaRank(value uint64) *uint64    { return &value }

func arenaTestParquet(t *testing.T, rows []arenaDatasetRow) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := parquet.Write(&buf, rows); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestArenaParquetPreservesSourceRanksAndFiltersOverall(t *testing.T) {
	rows := []arenaDatasetRow{
		{ModelName: "old", Rating: arenaFloat(1900), Rank: arenaRank(1), Category: "overall", PublishDate: "2026-11-08"},
		{ModelName: "coding-only", Rating: arenaFloat(1800), Rank: arenaRank(1), Category: "coding", PublishDate: "2026-11-09"},
		{ModelName: "B", Rating: arenaFloat(1432.25), Rank: arenaRank(2), Category: "overall", PublishDate: "2026-11-09", RatingLower: arenaFloat(1420), RatingUpper: arenaFloat(1445), VoteCount: arenaFloat(12000)},
		{ModelName: "A", Rating: arenaFloat(1440), Rank: arenaRank(2), Category: "overall", PublishDate: "2026-11-09"},
	}
	got, err := parseArenaParquet(arenaTestParquet(t, rows), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 2 || got.Models[0].Rank != 2 || got.Models[1].Rank != 2 || got.Models[1].Score != 1432.25 ||
		got.Models[1].Votes == nil || *got.Models[1].Votes != 12000 || got.PublishedAt != "2026-11-09" {
		t.Fatalf("source data altered: %+v", got)
	}
	for _, blob := range [][]byte{[]byte("not parquet"), arenaTestParquet(t, []arenaDatasetRow{{Category: "overall", PublishDate: "2026-11-09"}})} {
		if _, err := parseArenaParquet(blob, time.Now()); err == nil {
			t.Fatal("invalid source accepted")
		}
	}
}

func TestArenaAgentUsesIPSScoreWithoutRescaling(t *testing.T) {
	row := arenaDatasetRow{ModelName: "Agent", AgentScore: arenaFloat(0.654321), AgentLower: arenaFloat(0.63),
		AgentUpper: arenaFloat(0.68), Observations: arenaFloat(500), Rank: arenaRank(1), Category: "overall", PublishDate: "2026-11-09"}
	got, err := parseArenaParquet(arenaTestParquet(t, []arenaDatasetRow{row}), time.Now())
	if err != nil || got.Models[0].Score != 0.654321 || *got.Models[0].Votes != 500 {
		t.Fatalf("agent score rescaled or lost: %+v / %v", got, err)
	}
}

func TestArenaDailySchedule(t *testing.T) {
	for _, tc := range []struct{ now, want string }{
		{"2026-11-10T03:59:00+08:00", "2026-11-10T04:00:00+08:00"},
		{"2026-11-10T04:00:00+08:00", "2026-11-11T04:00:00+08:00"},
		{"2026-11-10T22:00:00+08:00", "2026-11-11T04:00:00+08:00"},
	} {
		now, _ := time.Parse(time.RFC3339, tc.now)
		if got := nextArenaSync(now).Format(time.RFC3339); got != tc.want {
			t.Fatalf("%s: next sync %s, want %s", tc.now, got, tc.want)
		}
	}
	setupAutoRouterTestDB(t)
	board, _ := findArenaBoard("text")
	setArenaFixture(t, "text", []arenaModel{{Rank: 1, Model: "cached", Score: 1500}})
	// A fresh daily cache prevents even constructing a remote request.
	refreshArenaBoard(context.Background(), db, board, nil)
	cached, _, _ := getArenaSnapshot(db, "text")
	if cached == nil || cached.Models[0].Model != "cached" {
		t.Fatal("same-day cache was unexpectedly refreshed")
	}
}

func TestArenaFetchAndPersistentFallback(t *testing.T) {
	setupAutoRouterTestDB(t)
	blob := arenaTestParquet(t, []arenaDatasetRow{{ModelName: "Example", Rating: arenaFloat(1450), Rank: arenaRank(1), Category: "overall", PublishDate: "2026-11-09"}})
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(blob)
	}))
	defer server.Close()
	got, err := fetchArenaSnapshot(context.Background(), server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	getArenaSnapshot(db, "text")
	if err := storeArenaSnapshot(db, "text", got); err != nil {
		t.Fatal(err)
	}
	status = http.StatusServiceUnavailable
	if _, err := fetchArenaSnapshot(context.Background(), server.Client(), server.URL); err == nil {
		t.Fatal("HTTP error should not be accepted as a ranking")
	}
	arenaLeaderboards.Lock()
	arenaLeaderboards.entries = nil
	arenaLeaderboards.Unlock()
	saved, _, _ := getArenaSnapshot(db, "text")
	if saved == nil || saved.Models[0].Score != 1450 {
		t.Fatal("last known good source data did not survive reload")
	}
	older := *got
	older.PublishedAt = "2026-11-08"
	if err := storeArenaSnapshot(db, "text", &older); err == nil {
		t.Fatal("older snapshot replaced newer data")
	}
}

func TestArenaOfficialLiveSources(t *testing.T) {
	if os.Getenv("APISHARE_ARENA_LIVE_TEST") != "1" {
		t.Skip("set APISHARE_ARENA_LIVE_TEST=1 to verify official datasets")
	}
	for _, board := range arenaBoards {
		t.Run(board.ID, func(t *testing.T) {
			snapshot, err := fetchArenaSnapshot(context.Background(), &http.Client{Timeout: 45 * time.Second},
				arenaDatasetURL+"/resolve/main/"+board.Config+"/latest-00000-of-00001.parquet")
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %s, %d models, first=%s score=%f", board.ID, snapshot.PublishedAt, len(snapshot.Models), snapshot.Models[0].Model, snapshot.Models[0].Score)
		})
	}
}
