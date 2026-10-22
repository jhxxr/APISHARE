package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const logExportLimit = 10000

type logQuery struct {
	Page, Size int
	Where      string
	Args       []any
	Export     bool
}

type logQuerySummary struct {
	Calls            int64   `json:"calls"`
	Successes        int64   `json:"successes"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

type logQueryResult struct {
	Total   int64           `json:"total"`
	Page    int             `json:"page"`
	Size    int             `json:"size"`
	Items   []CallLog       `json:"items"`
	Summary logQuerySummary `json:"summary"`
}

func parseLogQuery(values url.Values) (logQuery, error) {
	query := logQuery{Page: 1, Size: 50}
	if page, err := strconv.Atoi(values.Get("page")); err == nil && page > 0 && page <= 10000000 {
		query.Page = page
	}
	if size, err := strconv.Atoi(values.Get("size")); err == nil && size >= 1 && size <= 200 {
		query.Size = size
	}
	conditions := []string{"1=1"}
	add := func(condition string, args ...any) {
		conditions = append(conditions, condition)
		query.Args = append(query.Args, args...)
	}
	var start, end time.Time
	for _, field := range []string{"start", "end"} {
		if value := values.Get(field); value != "" {
			parsed, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return query, fmt.Errorf("时间范围格式不正确")
			}
			if field == "start" {
				start = parsed
				add("unixepoch(created_at)>=?", parsed.Unix())
			} else {
				end = parsed
				add("unixepoch(created_at)<=?", parsed.Unix())
			}
		}
	}
	if !start.IsZero() && !end.IsZero() && start.After(end) {
		return query, fmt.Errorf("开始时间不能晚于结束时间")
	}
	if text := strings.TrimSpace(values.Get("q")); text != "" {
		if len(text) > 500 {
			return query, fmt.Errorf("搜索内容过长")
		}
		idText := strings.TrimPrefix(text, "#")
		if id, err := strconv.ParseInt(idText, 10, 64); err == nil && id > 0 && strings.TrimLeft(idText, "0123456789") == "" {
			add("id=?", id)
		} else {
			add("(instr(lower(model),lower(?))>0 OR instr(lower(key_name),lower(?))>0 OR instr(lower(upstream_name),lower(?))>0 OR instr(lower(error),lower(?))>0 OR instr(lower(endpoint),lower(?))>0)", text, text, text, text, text)
		}
	}
	for _, field := range []string{"key_id", "upstream_id"} {
		if value := values.Get(field); value != "" {
			id, err := strconv.ParseInt(value, 10, 64)
			if err != nil || id < 0 {
				return query, fmt.Errorf("筛选 ID 不正确")
			}
			add(field+"=?", id)
		}
	}
	if model := strings.TrimSpace(values.Get("model")); model != "" {
		add("model=?", model)
	}
	switch values.Get("status") {
	case "":
	case "success":
		add("status BETWEEN 200 AND 299")
	case "failed":
		add("status NOT BETWEEN 200 AND 299")
	default:
		return query, fmt.Errorf("状态筛选不正确")
	}
	for _, feature := range strings.Split(values.Get("features"), ",") {
		switch feature {
		case "":
		case "failed":
			add("status NOT BETWEEN 200 AND 299")
		case "slow":
			add("(CASE WHEN duration_ms>0 THEN duration_ms ELSE latency_ms END)>=5000")
		case "cache":
			add("(CASE WHEN json_valid(billing_json) THEN CAST(json_extract(billing_json,'$.usage.cache_read') AS INTEGER) ELSE 0 END)>0")
		case "reasoning":
			add("(CASE WHEN json_valid(billing_json) THEN CAST(json_extract(billing_json,'$.usage.reasoning') AS INTEGER) ELSE 0 END)>0")
		default:
			return query, fmt.Errorf("快捷筛选不正确")
		}
	}
	if format := values.Get("export"); format != "" {
		if format != "csv" {
			return query, fmt.Errorf("不支持的导出格式")
		}
		query.Export = true
	}
	query.Where = strings.Join(conditions, " AND ")
	return query, nil
}

func queryLogs(ctx context.Context, query logQuery) (*logQueryResult, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result := &logQueryResult{Page: query.Page, Size: query.Size, Items: []CallLog{}}
	summary := &result.Summary
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN status BETWEEN 200 AND 299 THEN 1 ELSE 0 END),0),COALESCE(SUM(prompt_tokens),0),COALESCE(SUM(completion_tokens),0),COALESCE(SUM(CASE WHEN status BETWEEN 200 AND 299 THEN cost_usd ELSE 0 END),0) FROM call_logs WHERE `+query.Where, query.Args...).Scan(&summary.Calls, &summary.Successes, &summary.PromptTokens, &summary.CompletionTokens, &summary.CostUSD)
	if err != nil {
		return nil, err
	}
	result.Total = summary.Calls
	limit, offset := query.Size, (query.Page-1)*query.Size
	if query.Export {
		if result.Total > logExportLimit {
			return nil, fmt.Errorf("筛选结果超过 %d 条，请缩小时间或筛选范围后导出", logExportLimit)
		}
		limit, offset = logExportLimit, 0
	} else {
		lastPage := int((result.Total + int64(query.Size) - 1) / int64(query.Size))
		if lastPage < 1 {
			lastPage = 1
		}
		if result.Page > lastPage {
			result.Page = lastPage
			offset = (result.Page - 1) * query.Size
		}
	}
	args := append(append([]any{}, query.Args...), limit, offset)
	rows, err := tx.QueryContext(ctx, `SELECT id,key_id,key_name,upstream_id,upstream_name,model,endpoint,prompt_tokens,completion_tokens,cost_usd,status,latency_ms,first_token_ms,duration_ms,error,created_at,billing_json,trace_id FROM call_logs WHERE `+query.Where+` ORDER BY unixepoch(created_at) DESC,id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		log, err := scanQueriedLog(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		result.Items = append(result.Items, log)
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

func scanQueriedLog(rows *sql.Rows) (CallLog, error) {
	var log CallLog
	var billing string
	err := rows.Scan(&log.ID, &log.KeyID, &log.KeyName, &log.UpstreamID, &log.UpstreamName, &log.Model, &log.Endpoint, &log.PromptTokens, &log.CompletionTokens, &log.CostUSD, &log.Status, &log.LatencyMS, &log.FirstTokenMS, &log.DurationMS, &log.Error, &log.CreatedAt, &billing, &log.TraceID)
	if err == nil && billing != "" {
		_ = json.Unmarshal([]byte(billing), &log.Billing)
	}
	return log, err
}

func csvLogCell(value string) string {
	// Spreadsheet programs interpret these prefixes as formulas, even in CSV.
	if value != "" && strings.ContainsAny(value[:1], "=+-@\t\r\n") {
		return "'" + value
	}
	return value
}

func serveLogQuery(c *gin.Context) {
	query, err := parseLogQuery(c.Request.URL.Query())
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	result, err := queryLogs(c.Request.Context(), query)
	if err != nil {
		if query.Export && strings.Contains(err.Error(), "筛选结果超过") {
			c.JSON(400, gin.H{"error": err.Error()})
		} else {
			c.JSON(500, gin.H{"error": "无法读取调用记录"})
		}
		return
	}
	if !query.Export {
		c.JSON(200, result)
		return
	}
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="apishare-logs.csv"`)
	c.Writer.Write([]byte{0xef, 0xbb, 0xbf})
	writer := csv.NewWriter(c.Writer)
	writer.Write([]string{"记录ID", "时间", "模型", "上游", "API密钥名称", "端点", "状态", "输入Tokens", "输出Tokens", "费用USD", "首Token毫秒", "总耗时毫秒", "错误"})
	for _, log := range result.Items {
		duration := log.DurationMS
		if duration <= 0 {
			duration = log.LatencyMS
		}
		writer.Write([]string{strconv.FormatInt(log.ID, 10), log.CreatedAt, csvLogCell(log.Model), csvLogCell(log.UpstreamName), csvLogCell(log.KeyName), csvLogCell(log.Endpoint), strconv.Itoa(log.Status), strconv.Itoa(log.PromptTokens), strconv.Itoa(log.CompletionTokens), strconv.FormatFloat(log.CostUSD, 'f', -1, 64), strconv.FormatInt(log.FirstTokenMS, 10), strconv.FormatInt(duration, 10), csvLogCell(log.Error)})
	}
	writer.Flush()
}
