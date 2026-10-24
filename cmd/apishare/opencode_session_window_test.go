package main

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

func sessionAt(now time.Time) string {
	word := ^(uint64(now.UnixMilli())<<12 | 1)
	return fmt.Sprintf("ses_%012xABCDEFGHIJKLMN", word&0xffffffffffff)
}

func TestOpenCodeSessionTimestampWindowAndStoredRepair(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		age  time.Duration
		want bool
	}{
		{0, true}, {-29 * 24 * time.Hour, true}, {time.Hour, true},
		{-180 * 24 * time.Hour, false}, {2 * 24 * time.Hour, false},
	} {
		if got := openCodeSessionUsable(sessionAt(now.Add(tc.age)), now); got != tc.want {
			t.Errorf("age=%s usable=%v, want %v", tc.age, got, tc.want)
		}
	}
	// The truncated timestamp also works across its 36-bit wrap boundary.
	boundary := time.UnixMilli((int64(1) << 36) * 26)
	if !openCodeSessionUsable(sessionAt(boundary.Add(-time.Hour)), boundary) {
		t.Fatal("recent session was rejected across the timestamp wrap")
	}
	modelsTestDB(t)
	header := http.Header{}
	header.Set("x-session-id", "conversation")
	task := func() *relayTask { return &relayTask{apiKeyID: 1, canon: &canonReq{InputText: "hello"}} }
	first, err := openCodeSessionForTask(task(), header)
	if err != nil {
		t.Fatal(err)
	}
	stale := sessionAt(now.Add(-180 * 24 * time.Hour))
	if _, err := db.Exec(`UPDATE opencode_sessions SET session_id=? WHERE session_id=?`, stale, first); err != nil {
		t.Fatal(err)
	}
	repaired, err := openCodeSessionForTask(task(), header)
	if err != nil || repaired == stale || !openCodeSessionUsable(repaired, now) {
		t.Fatalf("stale persisted session was retained: %s %v", repaired, err)
	}
	again, err := openCodeSessionForTask(task(), header)
	if err != nil || again != repaired {
		t.Fatalf("repaired mapping was unstable: %s %v", again, err)
	}
	header.Set("x-opencode-session", stale)
	if got, err := openCodeSessionForTask(task(), header); err != nil || got == stale || !openCodeSessionUsable(got, now) {
		t.Fatalf("stale client ID bypassed repair: %s %v", got, err)
	}
}
