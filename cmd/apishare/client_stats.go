package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
)

// Client aggregates are separate from the existing model aggregates so neither
// model totals nor their historical primary keys change during this migration.
func migrateRequestClientSchema() error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, column := range []struct{ name, definition string }{
		{"client_id", "TEXT NOT NULL DEFAULT 'unknown'"},
		{"client_json", "TEXT NOT NULL DEFAULT ''"},
	} {
		var exists int
		if err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('call_logs') WHERE name=?`, column.name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := tx.Exec(`ALTER TABLE call_logs ADD COLUMN ` + column.name + ` ` + column.definition); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_logs_client_epoch ON call_logs(client_id,unixepoch(created_at) DESC,id DESC);
		CREATE TABLE IF NOT EXISTS call_log_client_daily (
		 day TEXT NOT NULL, client_id TEXT NOT NULL,
		 calls INTEGER NOT NULL DEFAULT 0, successes INTEGER NOT NULL DEFAULT 0,
		 prompt_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0,
		 cost_usd REAL NOT NULL DEFAULT 0, last_at TEXT NOT NULL DEFAULT '',
		 PRIMARY KEY(day,client_id)
		);`); err != nil {
		return err
	}
	const marker = "migration_request_client_v1"
	var applied int
	if err := tx.QueryRow(`SELECT count(*) FROM settings WHERE k=?`, marker).Scan(&applied); err != nil {
		return err
	}
	if applied == 0 {
		// Pre-feature archived calls have no recoverable client information.
		// Seed them once as unknown, in the same transaction as the marker.
		if _, err := tx.Exec(`INSERT INTO call_log_client_daily(day,client_id,calls,successes,prompt_tokens,completion_tokens,cost_usd,last_at)
			SELECT day,'unknown',SUM(calls),SUM(successes),SUM(prompt_tokens),SUM(completion_tokens),SUM(cost_usd),
			 COALESCE(strftime('%Y-%m-%dT%H:%M:%S+08:00',MAX(unixepoch(last_at)),'unixepoch','+8 hours'),'')
			FROM call_log_daily GROUP BY day
			ON CONFLICT(day,client_id) DO NOTHING`); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO settings(k,v) VALUES(?, '1')`, marker); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type requestClientOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func requestClientOptions() []requestClientOption {
	items := make([]requestClientOption, 0, len(requestClientNames))
	for id, name := range requestClientNames {
		items = append(items, requestClientOption{id, name})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].ID == "unknown" || items[j].ID == "unknown" {
			return items[i].ID == "unknown"
		}
		return items[i].Name < items[j].Name
	})
	return items
}

type clientStatRow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	logQuerySummary
	LastAt string `json:"last_at"`
}

type clientStatsResult struct {
	Period  string          `json:"period"`
	Start   string          `json:"start,omitempty"`
	End     string          `json:"end"`
	Summary logQuerySummary `json:"summary"`
	Items   []clientStatRow `json:"items"`
}

func clientStatsWindow(period string, now time.Time) (start, end time.Time, err error) {
	now = now.In(beijingLocation)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, beijingLocation)
	end = today.AddDate(0, 0, 1)
	switch period {
	case "today":
		start = today
	case "7d":
		start = today.AddDate(0, 0, -6)
	case "30d":
		start = today.AddDate(0, 0, -29)
	case "all":
	default:
		err = fmt.Errorf("客户端统计范围仅支持 today、7d、30d、all")
	}
	return
}

func queryClientStats(ctx context.Context, period string, now time.Time) (*clientStatsResult, error) {
	start, end, err := clientStatsWindow(period, now)
	if err != nil {
		return nil, err
	}
	liveWhere, archiveWhere := "unixepoch(created_at)<?", "day<?"
	args := []any{end.Unix()}
	archiveArgs := []any{end.Format("2006-01-02")}
	result := &clientStatsResult{Period: period, End: end.Add(-time.Second).Format(time.RFC3339), Items: []clientStatRow{}}
	if !start.IsZero() {
		liveWhere += " AND unixepoch(created_at)>=?"
		archiveWhere += " AND day>=?"
		args = append(args, start.Unix())
		archiveArgs = append(archiveArgs, start.Format("2006-01-02"))
		result.Start = start.Format(time.RFC3339)
	}
	args = append(args, archiveArgs...)
	// One read snapshot prevents double counting while retention moves rows
	// between detail and archive tables in a concurrent transaction.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT client_id,SUM(calls),SUM(successes),
		SUM(prompt_tokens),SUM(completion_tokens),SUM(cost_usd),MAX(last_epoch)
		FROM (
		 SELECT client_id,COUNT(*) calls,
		  SUM(CASE WHEN status BETWEEN 200 AND 299 THEN 1 ELSE 0 END) successes,
		  COALESCE(SUM(prompt_tokens),0) prompt_tokens,COALESCE(SUM(completion_tokens),0) completion_tokens,
		  COALESCE(SUM(CASE WHEN status BETWEEN 200 AND 299 THEN cost_usd ELSE 0 END),0) cost_usd,
		  COALESCE(MAX(unixepoch(created_at)),0) last_epoch
		 FROM call_logs WHERE `+liveWhere+` GROUP BY client_id
		 UNION ALL
		 SELECT client_id,calls,successes,prompt_tokens,completion_tokens,cost_usd,
		  COALESCE(unixepoch(last_at),0) last_epoch
		 FROM call_log_client_daily WHERE `+archiveWhere+`
		) GROUP BY client_id ORDER BY SUM(calls) DESC,SUM(cost_usd) DESC,client_id`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var row clientStatRow
		var lastEpoch int64
		if err := rows.Scan(&row.ID, &row.Calls, &row.Successes, &row.PromptTokens, &row.CompletionTokens, &row.CostUSD, &lastEpoch); err != nil {
			rows.Close()
			return nil, err
		}
		row.Name = requestClientNames[row.ID]
		if row.Name == "" {
			row.Name = requestClientNames["unknown"]
		}
		if lastEpoch > 0 {
			row.LastAt = time.Unix(lastEpoch, 0).In(beijingLocation).Format(time.RFC3339)
		}
		result.Items = append(result.Items, row)
		result.Summary.Calls += row.Calls
		result.Summary.Successes += row.Successes
		result.Summary.PromptTokens += row.PromptTokens
		result.Summary.CompletionTokens += row.CompletionTokens
		result.Summary.CostUSD += row.CostUSD
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func registerClientStatsRoutes(auth *gin.RouterGroup) {
	auth.GET("/clients", func(c *gin.Context) {
		c.JSON(200, gin.H{"items": requestClientOptions()})
	})
	auth.GET("/client-stats", func(c *gin.Context) {
		period := c.DefaultQuery("period", "30d")
		if _, _, err := clientStatsWindow(period, beijingNow()); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		result, err := queryClientStats(c.Request.Context(), period, beijingNow())
		if err != nil {
			c.JSON(500, gin.H{"error": "无法读取客户端统计"})
			return
		}
		c.JSON(200, result)
	})
}
