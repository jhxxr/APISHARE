package cliproxy

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	_ "modernc.org/sqlite"
)

const accountUptimeFile = "account-uptime.db"

type AccountUptimeBucket struct {
	Start         string   `json:"start"`
	End           string   `json:"end"`
	Calls         int64    `json:"calls"`
	Successes     int64    `json:"successes"`
	Failures      int64    `json:"failures"`
	UptimePercent *float64 `json:"uptime_percent"`
}

type AccountUptime struct {
	WindowStart   string                `json:"window_start"`
	UpdatedAt     string                `json:"updated_at"`
	Calls         int64                 `json:"calls"`
	Successes     int64                 `json:"successes"`
	Failures      int64                 `json:"failures"`
	UptimePercent *float64              `json:"uptime_percent"`
	LastCall      string                `json:"last_call,omitempty"`
	History       []AccountUptimeBucket `json:"history"`
}

func emptyAccountUptime(now time.Time) *AccountUptime {
	end := time.UnixMilli(now.UnixMilli()).UTC()
	start := end.Add(-24 * time.Hour)
	result := &AccountUptime{
		WindowStart: start.Format(time.RFC3339Nano), UpdatedAt: end.Format(time.RFC3339Nano),
		History: make([]AccountUptimeBucket, 24),
	}
	for i := range result.History {
		at := start.Add(time.Duration(i) * time.Hour)
		result.History[i].Start = at.Format(time.RFC3339Nano)
		result.History[i].End = at.Add(time.Hour).Format(time.RFC3339Nano)
	}
	return result
}

func accountUptimePercent(successes, calls int64) *float64 {
	if calls == 0 {
		return nil
	}
	value := 100 * float64(successes) / float64(calls)
	return &value
}

// The SDK reports the credential actually selected for each attempt, including
// failures followed by a retry on another credential. No token, prompt, error
// message or user API key is persisted in this database.
type accountUptimeRecorder struct {
	db        *sql.DB
	lastPrune atomic.Int64
	warned    atomic.Bool
}

func openAccountUptimeRecorder(root string) (*accountUptimeRecorder, error) {
	path := filepath.Join(root, accountUptimeFile)
	database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS account_results (
		id INTEGER PRIMARY KEY,
		auth_id TEXT NOT NULL,
		observed_at INTEGER NOT NULL,
		success INTEGER NOT NULL CHECK(success IN (0,1))
	);
	CREATE INDEX IF NOT EXISTS account_results_time ON account_results(observed_at);
	CREATE INDEX IF NOT EXISTS account_results_auth_time ON account_results(auth_id,observed_at);`); err != nil {
		database.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		database.Close()
		return nil, err
	}
	recorder := &accountUptimeRecorder{db: database}
	if err := recorder.prune(time.Now()); err != nil {
		database.Close()
		return nil, err
	}
	return recorder, nil
}

func (r *accountUptimeRecorder) prune(now time.Time) error {
	_, err := r.db.Exec(`DELETE FROM account_results WHERE observed_at<?`, now.Add(-48*time.Hour).UnixMilli())
	if err == nil {
		r.lastPrune.Store(now.UnixMilli())
	}
	return err
}

func (r *accountUptimeRecorder) record(now time.Time, authID string, success bool) error {
	value := 0
	if success {
		value = 1
	}
	if _, err := r.db.Exec(`INSERT INTO account_results(auth_id,observed_at,success) VALUES(?,?,?)`, authID, now.UnixMilli(), value); err != nil {
		return err
	}
	last := r.lastPrune.Load()
	if now.UnixMilli()-last >= time.Hour.Milliseconds() && r.lastPrune.CompareAndSwap(last, now.UnixMilli()) {
		return r.prune(now)
	}
	return nil
}

func (r *accountUptimeRecorder) ApplyResultPolicy(ctx context.Context, result coreauth.Result) coreauth.Result {
	// Token counting and client cancellation do not measure account availability.
	if result.AuthID == "" || result.SkipQuotaObservation ||
		ctx != nil && errors.Is(ctx.Err(), context.Canceled) ||
		result.Error != nil && (result.Error.HTTPStatus == 499 ||
			result.Error.HTTPStatus == 0 && (result.Error.Message == context.Canceled.Error() ||
				strings.HasSuffix(result.Error.Message, ": "+context.Canceled.Error()))) {
		return result
	}
	if err := r.record(time.Now(), result.AuthID, result.Success); err != nil {
		if !r.warned.Swap(true) {
			log.Print("[cliproxy] account uptime recording is temporarily unavailable")
		}
	} else {
		r.warned.Store(false)
	}
	return result // Observing an outcome must never alter SDK routing or retries.
}

// AccountUptimes reads a snapshot independently of the pool process, so history
// remains available while a pool is stopped and survives gateway restarts.
func (m *Manager) AccountUptimes(ctx context.Context, now time.Time) (map[string]*AccountUptime, error) {
	out := make(map[string]*AccountUptime)
	path := filepath.Join(m.root, accountUptimeFile)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return out, nil
	} else if err != nil {
		return nil, err
	}
	database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil, err
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	end, start := now.UnixMilli(), now.Add(-24*time.Hour).UnixMilli()
	rows, err := database.QueryContext(ctx, `SELECT auth_id,(observed_at-?)/3600000,COUNT(*),SUM(success),MAX(observed_at)
		FROM account_results WHERE observed_at>=? AND observed_at<=?
		GROUP BY auth_id,(observed_at-?)/3600000`, start, start, end, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lastCalls := make(map[string]int64)
	for rows.Next() {
		var authID string
		var bucket int
		var calls, successes, last int64
		if err := rows.Scan(&authID, &bucket, &calls, &successes, &last); err != nil {
			return nil, err
		}
		// An outcome at the exact observation millisecond belongs to the last
		// hour rather than becoming an invisible 25th bucket.
		bucket = min(23, max(0, bucket))
		summary := out[authID]
		if summary == nil {
			summary = emptyAccountUptime(now)
			out[authID] = summary
		}
		summary.Calls += calls
		summary.Successes += successes
		summary.Failures += calls - successes
		hour := &summary.History[bucket]
		hour.Calls += calls
		hour.Successes += successes
		hour.Failures += calls - successes
		if last > lastCalls[authID] {
			lastCalls[authID] = last
			summary.LastCall = time.UnixMilli(last).UTC().Format(time.RFC3339Nano)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, summary := range out {
		summary.UptimePercent = accountUptimePercent(summary.Successes, summary.Calls)
		for i := range summary.History {
			hour := &summary.History[i]
			hour.UptimePercent = accountUptimePercent(hour.Successes, hour.Calls)
		}
	}
	return out, nil
}

func (m *Manager) AttachAccountUptimes(ctx context.Context, credentials []Credential, now time.Time) error {
	history, err := m.AccountUptimes(ctx, now)
	if err != nil {
		return err
	}
	for i := range credentials {
		summary := history[credentials[i].ID]
		if summary == nil {
			summary = history[credentials[i].Name]
		}
		if summary == nil {
			summary = emptyAccountUptime(now)
		}
		credentials[i].Uptime = summary
	}
	return nil
}
