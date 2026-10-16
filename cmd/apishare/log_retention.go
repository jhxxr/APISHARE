package main

// 调用明细保留：删除的是 call_logs 的逐条详情，累计数据（调用次数、
// Token、费用、最后一次调用）会先汇总到 call_log_daily 再删除，只增不减。

import (
	"log"
	"time"
)

// 调用明细保留期预设：7 天是默认值；0 表示永久保留（不自动删除）。
type logRetentionOption struct {
	days   int
	label  string
	detail string
}

var logRetentionOptions = []logRetentionOption{
	{days: 7, label: "7 天（默认）", detail: "调用记录保留最近 7 天，超期的调用详情自动删除；累计 Token、调用次数与费用不受影响。"},
	{days: 30, label: "30 天", detail: "调用记录保留最近 30 天，超期的调用详情自动删除；累计 Token、调用次数与费用不受影响。"},
	{days: 90, label: "90 天", detail: "调用记录保留最近 90 天，超期的调用详情自动删除；累计 Token、调用次数与费用不受影响。"},
	{days: 180, label: "180 天", detail: "调用记录保留最近 180 天，超期的调用详情自动删除；累计 Token、调用次数与费用不受影响。"},
	{days: 365, label: "365 天", detail: "调用记录保留最近 365 天，超期的调用详情自动删除；累计 Token、调用次数与费用不受影响。"},
	{days: 0, label: "永久保留", detail: "调用记录永久保留，不自动删除。数据库体积会随调用量持续增长。"},
}

// 默认保留 7 天。
const defaultLogRetentionDays = 7

// logRetentionSweepInterval 是保留期清理任务的执行间隔。
const logRetentionSweepInterval = time.Hour

func retentionOptionFor(days int) (logRetentionOption, bool) {
	for _, option := range logRetentionOptions {
		if option.days == days {
			return option, true
		}
	}
	return logRetentionOption{}, false
}

// retentionOption 把任意取值归一到预设选项；非法取值回退到默认值。
func retentionOption(days int) logRetentionOption {
	if option, ok := retentionOptionFor(days); ok {
		return option
	}
	option, _ := retentionOptionFor(defaultLogRetentionDays)
	return option
}

func validLogRetentionDays(days int) bool {
	_, ok := retentionOptionFor(days)
	return ok
}

// logRetentionCutoff 返回保留期的删除边界（Unix 秒）：created_at 早于该值
// 的调用明细会被汇总后删除。0 表示永久保留，返回 ok=false。
func logRetentionCutoff(days int, now time.Time) (cutoff int64, ok bool) {
	if days <= 0 {
		return 0, false
	}
	return now.In(beijingLocation).AddDate(0, 0, -days).Unix(), true
}

// rollupExpiredLogs 把早于 cutoff 的调用明细汇总到 call_log_daily 后删除，
// 两步在同一事务内完成：汇总与删除要么同时成功，要么同时回滚，既不会
// 丢失累计数据，也不会重复汇总。
func rollupExpiredLogs(cutoff int64) (deleted int64, err error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// cost_usd 只统计成功调用（上游返回 2xx 且经过扣费流程的调用），
	// 与后台费用图表“成功计费”的口径一致；Token 与调用次数统计全部调用，
	// 与公开首页累计用量的口径一致。
	const rollup = `INSERT INTO call_log_daily(day,model,calls,successes,prompt_tokens,completion_tokens,cost_usd,last_at,last_status)
	 SELECT date(created_at,'+8 hours'),model,COUNT(*),
	  SUM(CASE WHEN status BETWEEN 200 AND 299 THEN 1 ELSE 0 END),
	  COALESCE(SUM(prompt_tokens),0),COALESCE(SUM(completion_tokens),0),
	  COALESCE(SUM(CASE WHEN status BETWEEN 200 AND 299 THEN cost_usd ELSE 0 END),0),
	  (SELECT created_at FROM call_logs newer
	   WHERE newer.model=aged.model
	    AND date(newer.created_at,'+8 hours')=date(aged.created_at,'+8 hours')
	    AND unixepoch(newer.created_at)<=?
	   ORDER BY unixepoch(newer.created_at) DESC,newer.id DESC LIMIT 1),
	  COALESCE((SELECT status FROM call_logs newer
	   WHERE newer.model=aged.model
	    AND date(newer.created_at,'+8 hours')=date(aged.created_at,'+8 hours')
	    AND unixepoch(newer.created_at)<=?
	   ORDER BY unixepoch(newer.created_at) DESC,newer.id DESC LIMIT 1),0)
	 FROM call_logs aged WHERE unixepoch(created_at)<?
	 GROUP BY date(created_at,'+8 hours'),model
	 ON CONFLICT(day,model) DO UPDATE SET
	  calls=calls+excluded.calls,
	  successes=successes+excluded.successes,
	  prompt_tokens=prompt_tokens+excluded.prompt_tokens,
	  completion_tokens=completion_tokens+excluded.completion_tokens,
	  cost_usd=cost_usd+excluded.cost_usd,
	  last_at=CASE WHEN unixepoch(excluded.last_at)>unixepoch(last_at) THEN excluded.last_at ELSE last_at END,
	  last_status=CASE WHEN unixepoch(excluded.last_at)>unixepoch(last_at) THEN excluded.last_status ELSE last_status END`
	if _, err := tx.Exec(rollup, cutoff, cutoff, cutoff); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM call_logs WHERE unixepoch(created_at)<?`, cutoff)
	if err != nil {
		return 0, err
	}
	deleted, err = res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

// sweepCallLogs 按当前保留期设置清理一次过期调用明细。
func sweepCallLogs(now time.Time) (int64, error) {
	cutoff, ok := logRetentionCutoff(getSettings().LogRetentionDays, now)
	if !ok {
		return 0, nil
	}
	return rollupExpiredLogs(cutoff)
}

// logRetentionKick 用于在后台保存新的保留期后立即触发一次清理，不必等
// 到下一个整点。带缓冲且非阻塞，没有循环消费时发送会被丢弃。
var logRetentionKick = make(chan struct{}, 1)

func kickLogRetentionSweep() {
	select {
	case logRetentionKick <- struct{}{}:
	default:
	}
}

// logRetentionLoop 启动后立即清理一次，之后每小时检查一次；保存新的保留期
// 设置时会立即再清理一次。删除的是调用详情，累计数据已先汇总到
// call_log_daily，不会丢失。
func logRetentionLoop() {
	sweep := func() {
		if deleted, err := sweepCallLogs(beijingNow()); err != nil {
			log.Printf("[logs] retention sweep failed: %v", err)
		} else if deleted > 0 {
			log.Printf("[logs] retention sweep removed %d expired call records", deleted)
			invalidatePublicOverviews()
		}
	}
	sweep()
	ticker := time.NewTicker(logRetentionSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-logRetentionKick:
		}
		sweep()
	}
}
