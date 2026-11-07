package main

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
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
		{"client_ip", "TEXT NOT NULL DEFAULT ''"},
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
		CREATE INDEX IF NOT EXISTS idx_logs_ip_epoch ON call_logs(client_ip,unixepoch(created_at) DESC,id DESC);
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
	auth.GET("/ip-stats", func(c *gin.Context) {
		result, status, err := queryIPStatsFromValues(c.Request.Context(), c.Request.URL.Query(), beijingNow())
		if err != nil {
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, result)
	})
}

type ipStatKeyRef struct {
	KeyID   int64  `json:"key_id"`
	KeyName string `json:"key_name"`
	Calls   int64  `json:"calls"`
}

type ipStatRow struct {
	IP          string         `json:"ip"`
	Failed      int64          `json:"failed"`
	RateLimited int64          `json:"rate_limited"`
	KeyCount    int            `json:"key_count"`
	Keys        []ipStatKeyRef `json:"keys"`
	logQuerySummary
	LastAt string `json:"last_at"`
}

type ipStatsResult struct {
	Period           string          `json:"period"`
	Start            string          `json:"start,omitempty"`
	End              string          `json:"end"`
	UniqueIPs        int             `json:"unique_ips"`
	MultiKeyIPs      int             `json:"multi_key_ips"`
	RateLimitedCalls int64           `json:"rate_limited_calls"`
	Summary          logQuerySummary `json:"summary"`
	Items            []ipStatRow     `json:"items"`
}

func queryIPStatsFromValues(ctx context.Context, values url.Values, now time.Time) (*ipStatsResult, int, error) {
	period := values.Get("period")
	startStr := strings.TrimSpace(values.Get("start"))
	endStr := strings.TrimSpace(values.Get("end"))
	var start, end time.Time
	if startStr != "" || endStr != "" {
		if period == "" {
			period = "custom"
		}
		if startStr != "" {
			parsed, err := time.Parse(time.RFC3339, startStr)
			if err != nil {
				return nil, 400, fmt.Errorf("时间范围格式不正确")
			}
			start = parsed
		}
		if endStr != "" {
			parsed, err := time.Parse(time.RFC3339, endStr)
			if err != nil {
				return nil, 400, fmt.Errorf("时间范围格式不正确")
			}
			end = parsed
		}
		if !start.IsZero() && !end.IsZero() && start.After(end) {
			return nil, 400, fmt.Errorf("开始时间不能晚于结束时间")
		}
	} else {
		if period == "" {
			period = "today"
		}
		var err error
		start, end, err = clientStatsWindow(period, now)
		if err != nil {
			return nil, 400, fmt.Errorf("IP 统计范围仅支持 today、7d、30d、all")
		}
		end = end.Add(-time.Second)
	}

	conditions := []string{"client_ip<>''"}
	var args []any
	if !start.IsZero() {
		conditions = append(conditions, "unixepoch(created_at)>=?")
		args = append(args, start.Unix())
	}
	if !end.IsZero() {
		conditions = append(conditions, "unixepoch(created_at)<=?")
		args = append(args, end.Unix())
	}
	if keyVal := strings.TrimSpace(values.Get("key_id")); keyVal != "" {
		keyID, err := strconv.ParseInt(keyVal, 10, 64)
		if err != nil || keyID < 0 {
			return nil, 400, fmt.Errorf("筛选 ID 不正确")
		}
		conditions = append(conditions, "key_id=?")
		args = append(args, keyID)
	}
	if q := strings.TrimSpace(values.Get("q")); q != "" {
		if len(q) > 200 {
			return nil, 400, fmt.Errorf("搜索内容过长")
		}
		conditions = append(conditions, "(instr(lower(client_ip),lower(?))>0 OR instr(lower(key_name),lower(?))>0)")
		args = append(args, q, q)
	}

	orderBy := "calls DESC, cost_usd DESC, client_ip ASC"
	switch values.Get("sort") {
	case "", "calls":
	case "cost":
		orderBy = "cost_usd DESC, calls DESC, client_ip ASC"
	case "failed":
		orderBy = "(calls - successes) DESC, rate_limited DESC, calls DESC, client_ip ASC"
	case "rate_limited":
		orderBy = "rate_limited DESC, (calls - successes) DESC, calls DESC, client_ip ASC"
	case "keys":
		orderBy = "key_count DESC, calls DESC, cost_usd DESC, client_ip ASC"
	default:
		return nil, 400, fmt.Errorf("IP 排序依据不正确")
	}

	where := strings.Join(conditions, " AND ")
	result := &ipStatsResult{
		Period: period,
		Items:  []ipStatRow{},
	}
	if !start.IsZero() {
		result.Start = start.In(beijingLocation).Format(time.RFC3339)
	}
	if !end.IsZero() {
		result.End = end.In(beijingLocation).Format(time.RFC3339)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 500, fmt.Errorf("无法读取 IP 统计")
	}
	defer tx.Rollback()

	groupedSQL := `SELECT client_ip,
		COUNT(*) AS calls,
		COALESCE(SUM(CASE WHEN status BETWEEN 200 AND 299 THEN 1 ELSE 0 END),0) AS successes,
		COALESCE(SUM(CASE WHEN status=429 THEN 1 ELSE 0 END),0) AS rate_limited,
		COALESCE(SUM(prompt_tokens),0) AS prompt_tokens,
		COALESCE(SUM(completion_tokens),0) AS completion_tokens,
		COALESCE(SUM(CASE WHEN status BETWEEN 200 AND 299 THEN cost_usd ELSE 0 END),0) AS cost_usd,
		COUNT(DISTINCT CASE WHEN key_id>0 THEN CAST(key_id AS TEXT) WHEN key_name<>'' THEN key_name END) AS key_count,
		COALESCE(MAX(unixepoch(created_at)),0) AS last_epoch
		FROM call_logs WHERE ` + where + ` GROUP BY client_ip`
	// Summarize the complete snapshot before limiting the displayed IP rows.
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN key_count>1 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(rate_limited),0),COALESCE(SUM(calls),0),
		COALESCE(SUM(successes),0),COALESCE(SUM(prompt_tokens),0),
		COALESCE(SUM(completion_tokens),0),COALESCE(SUM(cost_usd),0)
		FROM (`+groupedSQL+`)`, args...).Scan(
		&result.UniqueIPs, &result.MultiKeyIPs, &result.RateLimitedCalls,
		&result.Summary.Calls, &result.Summary.Successes, &result.Summary.PromptTokens,
		&result.Summary.CompletionTokens, &result.Summary.CostUSD)
	if err != nil {
		return nil, 500, fmt.Errorf("无法读取 IP 统计")
	}
	rows, err := tx.QueryContext(ctx, groupedSQL+` ORDER BY `+orderBy+` LIMIT 100`, args...)
	if err != nil {
		return nil, 500, fmt.Errorf("无法读取 IP 统计")
	}
	indexByIP := make(map[string]int)
	for rows.Next() {
		var row ipStatRow
		var lastEpoch int64
		if err := rows.Scan(&row.IP, &row.Calls, &row.Successes, &row.RateLimited, &row.PromptTokens, &row.CompletionTokens, &row.CostUSD, &row.KeyCount, &lastEpoch); err != nil {
			rows.Close()
			return nil, 500, fmt.Errorf("无法读取 IP 统计")
		}
		row.Failed = row.Calls - row.Successes
		row.Keys = []ipStatKeyRef{}
		if lastEpoch > 0 {
			row.LastAt = time.Unix(lastEpoch, 0).In(beijingLocation).Format(time.RFC3339)
		}
		indexByIP[row.IP] = len(result.Items)
		result.Items = append(result.Items, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 500, fmt.Errorf("无法读取 IP 统计")
	}

	if len(result.Items) > 0 {
		keyRows, err := tx.QueryContext(ctx, `SELECT client_ip,key_id,COALESCE(MAX(key_name),''),COUNT(*) AS calls
			FROM call_logs WHERE `+where+` AND (key_id>0 OR key_name<>'')
			GROUP BY client_ip,key_id,CASE WHEN key_id>0 THEN '' ELSE key_name END
			ORDER BY calls DESC,key_id ASC`, args...)
		if err != nil {
			return nil, 500, fmt.Errorf("无法读取 IP 统计")
		}
		for keyRows.Next() {
			var ip string
			var ref ipStatKeyRef
			if err := keyRows.Scan(&ip, &ref.KeyID, &ref.KeyName, &ref.Calls); err != nil {
				keyRows.Close()
				return nil, 500, fmt.Errorf("无法读取 IP 统计")
			}
			if idx, ok := indexByIP[ip]; ok && len(result.Items[idx].Keys) < 8 {
				result.Items[idx].Keys = append(result.Items[idx].Keys, ref)
			}
		}
		err = keyRows.Err()
		keyRows.Close()
		if err != nil {
			return nil, 500, fmt.Errorf("无法读取 IP 统计")
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, 500, fmt.Errorf("无法读取 IP 统计")
	}
	return result, 200, nil
}
