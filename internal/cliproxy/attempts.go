package cliproxy

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	sdkapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/api"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const TraceHeader = "X-Apishare-Trace"

type AccountAttempt struct {
	ID           int64  `json:"id"`
	RequestID    string `json:"request_id"`
	TraceID      string `json:"trace_id"`
	AuthID       string `json:"auth_id"`
	Model        string `json:"model"`
	CreatedAt    string `json:"created_at"`
	Status       int    `json:"status"`
	Reason       string `json:"reason,omitempty"`
	LatencyMS    int64  `json:"latency_ms"`
	FirstTokenMS int64  `json:"first_token_ms"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Attempt      int    `json:"attempt"`
	PoolID       int64  `json:"pool_id,omitempty"`
	PoolName     string `json:"pool_name,omitempty"`
}
type AttemptPage struct {
	Items  []AccountAttempt `json:"items"`
	NextID int64            `json:"next_id"`
}

func (r *accountUptimeRecorder) initAttempts() error {
	_, err := r.db.Exec(`CREATE TABLE IF NOT EXISTS account_attempts(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		request_id TEXT NOT NULL UNIQUE,trace_id TEXT NOT NULL,auth_id TEXT NOT NULL,model TEXT NOT NULL,
		observed_at INTEGER NOT NULL,status INTEGER NOT NULL,latency_ms INTEGER NOT NULL,first_token_ms INTEGER NOT NULL,
		input_tokens INTEGER NOT NULL,output_tokens INTEGER NOT NULL);
		CREATE INDEX IF NOT EXISTS idx_attempts_time ON account_attempts(observed_at);
		CREATE INDEX IF NOT EXISTS idx_attempts_trace ON account_attempts(trace_id,id);
		CREATE INDEX IF NOT EXISTS idx_attempts_account ON account_attempts(auth_id,id);
		CREATE TABLE IF NOT EXISTS trace_links(sdk_trace TEXT PRIMARY KEY,trace_id TEXT NOT NULL,observed_at INTEGER NOT NULL);`)
	return err
}

func validTraceID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (r *accountUptimeRecorder) traceOption() sdkapi.ServerOption {
	return sdkapi.WithMiddleware(func(c *gin.Context) {
		trace := c.GetHeader(TraceHeader)
		if validTraceID(trace) {
			// This is the pinned SDK version's Gin logging key. Snapshot the
			// value now; never retain a pooled Gin context in async usage jobs.
			if value, ok := c.Get("__request_id__"); ok {
				if sdkTrace, ok := value.(string); ok && sdkTrace != "" {
					_, _ = r.db.Exec(`INSERT OR REPLACE INTO trace_links(sdk_trace,trace_id,observed_at) VALUES(?,?,?)`, sdkTrace, trace, time.Now().UnixMilli())
				}
			}
		}
		c.Next()
	})
}

func (r *accountUptimeRecorder) HandleUsage(ctx context.Context, record usage.Record) {
	if value, ok := r.flushers.LoadAndDelete(record.RequestID); ok {
		close(value.(chan struct{}))
		return
	}
	if record.AuthID == "" || record.RequestID == "" {
		return
	}
	trace := record.TraceID
	var linked string
	if r.db.QueryRow(`SELECT trace_id FROM trace_links WHERE sdk_trace=?`, trace).Scan(&linked) == nil {
		trace = linked
	}
	if trace == "" {
		trace = record.RequestID
	}
	status := 200
	if record.Failed {
		status = record.Fail.StatusCode
		if status < 400 {
			status = 502
		}
	}
	at := record.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	_, err := r.db.Exec(`INSERT INTO account_attempts(request_id,trace_id,auth_id,model,observed_at,status,latency_ms,first_token_ms,input_tokens,output_tokens)
		VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(request_id) DO UPDATE SET
		status=excluded.status,latency_ms=excluded.latency_ms,first_token_ms=excluded.first_token_ms,input_tokens=excluded.input_tokens,output_tokens=excluded.output_tokens`,
		record.RequestID, trace, record.AuthID, record.Model, at.UnixMilli(), status, max(0, record.Latency.Milliseconds()), max(0, record.TTFT.Milliseconds()),
		max(0, record.Detail.InputTokens), max(0, record.Detail.OutputTokens))
	if err != nil {
		r.warned.Store(true)
	}
}

func (r *accountUptimeRecorder) flush(ctx context.Context) error {
	id, err := randomKey()
	if err != nil {
		return err
	}
	done := make(chan struct{})
	r.flushers.Store(id, done)
	defer r.flushers.Delete(id)
	usage.PublishRecord(ctx, usage.Record{RequestID: id, Provider: "apishare-flush"})
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Read only this pool's isolated database. Credentials and request content are
// intentionally absent from these rows; failed attempts retain numeric status.
func (m *Manager) Attempts(ctx context.Context, account, trace string, before int64, limit int) (AttemptPage, error) {
	page := AttemptPage{Items: make([]AccountAttempt, 0)}
	if limit < 1 || limit > 200 {
		limit = 50
	}
	path := filepath.Join(m.root, accountUptimeFile)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return page, nil
	}
	database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return page, err
	}
	defer database.Close()
	var exists int
	if err := database.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='account_attempts'`).Scan(&exists); err != nil {
		return page, err
	}
	if exists == 0 {
		return page, nil
	}
	query := `SELECT id,request_id,trace_id,auth_id,model,observed_at,status,latency_ms,first_token_ms,input_tokens,output_tokens,
		(SELECT count(*) FROM account_attempts earlier WHERE earlier.trace_id=a.trace_id AND earlier.id<=a.id)
		FROM account_attempts a WHERE 1=1`
	args := make([]any, 0)
	if account != "" {
		query += " AND auth_id=?"
		args = append(args, account)
	}
	if trace != "" {
		query += " AND trace_id=?"
		args = append(args, trace)
	}
	if before > 0 {
		query += " AND id<?"
		args = append(args, before)
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit+1)
	rows, err := database.QueryContext(ctx, query, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var item AccountAttempt
		var at int64
		if err := rows.Scan(&item.ID, &item.RequestID, &item.TraceID, &item.AuthID, &item.Model, &at, &item.Status, &item.LatencyMS, &item.FirstTokenMS, &item.InputTokens, &item.OutputTokens, &item.Attempt); err != nil {
			return page, err
		}
		item.CreatedAt = time.UnixMilli(at).UTC().Format(time.RFC3339Nano)
		if item.Status >= 400 {
			item.Reason = publicFailureReason(item.Status)
		}
		page.Items = append(page.Items, item)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextID = page.Items[limit-1].ID
	}
	return page, rows.Err()
}

var _ usage.Plugin = (*accountUptimeRecorder)(nil)
