package main

import (
	"database/sql"
	"encoding/json"
	"time"
)

const publicAutoHistoryBuckets = 24

// Pick the smallest useful window containing the last retained request.
// New requests zoom back in; inactivity expands the window up to retention.
func publicAutoHistoryWindow(now, last time.Time, retentionDays int) time.Duration {
	age := now.Sub(last)
	window := time.Hour
	for _, candidate := range []time.Duration{
		time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour,
		24 * time.Hour, 2 * 24 * time.Hour, 3 * 24 * time.Hour,
		7 * 24 * time.Hour, 14 * 24 * time.Hour, 30 * 24 * time.Hour,
		90 * 24 * time.Hour, 180 * 24 * time.Hour, 365 * 24 * time.Hour,
	} {
		window = candidate
		if age <= candidate {
			break
		}
	}
	if age > window {
		window = ((age + 24*time.Hour - 1) / (24 * time.Hour)) * 24 * time.Hour
	}
	if retentionDays > 0 {
		if limit := time.Duration(retentionDays) * 24 * time.Hour; window > limit {
			window = limit
		}
	}
	return window
}

func publicHistoryBuckets(end time.Time, window time.Duration) []PublicInterval {
	history := make([]PublicInterval, publicAutoHistoryBuckets)
	start := end.Add(-window)
	for i := range history {
		history[i].Start = start.Add(window * time.Duration(i) / publicAutoHistoryBuckets).Format(time.RFC3339)
		history[i].End = start.Add(window * time.Duration(i+1) / publicAutoHistoryBuckets).Format(time.RFC3339)
	}
	return history
}

func publicAutoHistories(tx *sql.Tx, end time.Time, retentionDays int) (
	map[string][]PublicInterval, map[string]int, map[string]*float64, int64, int64, error,
) {
	histories := make(map[string][]PublicInterval)
	windows := make(map[string]int)
	speeds := make(map[string]*float64)
	type historyWindow struct {
		Model   string `json:"model"`
		Start   int64  `json:"start"`
		Seconds int64  `json:"seconds"`
	}
	definitions := []historyWindow{}
	cutoff, limited := logRetentionCutoff(retentionDays, end)
	if !limited {
		cutoff = -62135596800 // No lower limit for permanently retained details.
	}
	// Archived daily rollups cannot recover individual intervals. Only details
	// still inside retention may select a window or contribute to the trace.
	err := publicRows(tx, `SELECT model,MAX(unixepoch(created_at)) FROM call_logs
		WHERE unixepoch(created_at) BETWEEN ? AND ? GROUP BY model`,
		func(rows *sql.Rows) error {
			var model string
			var last int64
			if err := rows.Scan(&model, &last); err != nil {
				return err
			}
			window := publicAutoHistoryWindow(end, time.Unix(last, 0), retentionDays)
			histories[model] = publicHistoryBuckets(end, window)
			windows[model] = int(window / time.Minute)
			definitions = append(definitions, historyWindow{Model: model, Start: end.Add(-window).Unix(), Seconds: int64(window / time.Second)})
			return nil
		}, cutoff, end.Unix())
	if err != nil || len(definitions) == 0 {
		return histories, windows, speeds, 0, 0, err
	}
	encoded, err := json.Marshal(definitions)
	if err != nil {
		return nil, nil, nil, 0, 0, err
	}
	// A single bound JSON table batches all model windows without a query per
	// model or an SQL parameter count proportional to the catalog size.
	var calls, successes int64
	type speedTotals struct{ tokens, milliseconds int64 }
	totals := make(map[string]speedTotals)
	err = publicRows(tx, `WITH windows AS (
		SELECT json_extract(value,'$.model') AS model,
		 json_extract(value,'$.start') AS start,
		 json_extract(value,'$.seconds') AS seconds FROM json_each(?)
	)
	SELECT l.model,MIN(CAST((unixepoch(l.created_at)-w.start)*24/w.seconds AS INTEGER),23) AS slot,
	 COUNT(*),SUM(CASE WHEN l.status BETWEEN 200 AND 299 THEN 1 ELSE 0 END),
	 SUM(CASE WHEN l.status BETWEEN 200 AND 299 AND l.duration_ms>0 AND l.completion_tokens>0
	  THEN l.completion_tokens ELSE 0 END),
	 SUM(CASE WHEN l.status BETWEEN 200 AND 299 AND l.duration_ms>0 AND l.completion_tokens>0
	  THEN CASE WHEN l.first_token_ms>0 AND (l.duration_ms-l.first_token_ms)*10>=l.duration_ms
	   THEN l.duration_ms-l.first_token_ms ELSE l.duration_ms END ELSE 0 END)
	FROM windows w JOIN call_logs l ON l.model=w.model
	 AND unixepoch(l.created_at) BETWEEN w.start AND ?
	WHERE unixepoch(l.created_at)>=? GROUP BY l.model,slot`,
		func(rows *sql.Rows) error {
			var model string
			var slot int
			var count, success, completion, generating int64
			if err := rows.Scan(&model, &slot, &count, &success, &completion, &generating); err != nil {
				return err
			}
			bucket := &histories[model][slot]
			bucket.Calls, bucket.Successes = count, success
			calls += count
			successes += success
			total := totals[model]
			total.tokens += completion
			total.milliseconds += generating
			totals[model] = total
			return nil
		}, string(encoded), end.Unix(), cutoff)
	for model, total := range totals {
		speeds[model] = averageTPS(total.tokens, total.milliseconds)
	}
	return histories, windows, speeds, calls, successes, err
}
