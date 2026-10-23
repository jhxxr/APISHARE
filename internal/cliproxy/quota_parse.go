package cliproxy

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
)

func quotaObject(value any) map[string]any {
	result, _ := value.(map[string]any)
	if result == nil {
		return map[string]any{}
	}
	return result
}
func quotaArray(value any) []any { result, _ := value.([]any); return result }
func quotaText(value any) string {
	text, _ := value.(string)
	text = strings.TrimSpace(text)
	if len([]rune(text)) > 80 {
		return ""
	}
	for _, ch := range text {
		if unicode.IsControl(ch) {
			return ""
		}
	}
	return text
}
func quotaNumber(value any) *float64 {
	var number float64
	var err error
	switch typed := value.(type) {
	case float64:
		number = typed
	case json.Number:
		number, err = typed.Float64()
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		number, err = strconv.ParseFloat(strings.TrimSpace(typed), 64)
	default:
		return nil
	}
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
		return nil
	}
	return &number
}
func quotaPick(document map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := document[key]; ok && value != nil {
			return value
		}
	}
	return nil
}
func quotaReset(document map[string]any, now time.Time) string {
	for _, key := range []string{"reset_at", "resetAt", "resets_at", "reset_time", "resetTime", "periodEnd", "resetAtUnix", "dailyQuotaResetAtUnix", "weeklyQuotaResetAtUnix"} {
		value, exists := document[key]
		if !exists || value == nil {
			continue
		}
		if text, ok := value.(string); ok {
			if at, err := time.Parse(time.RFC3339Nano, text); err == nil {
				return timestamp(at)
			}
		}
		if number := quotaNumber(value); number != nil && *number > 0 {
			unix := *number
			if unix > 1e12 {
				unix /= 1000
			}
			if unix >= 946684800 && unix < 32503680000 {
				return timestamp(time.Unix(int64(unix), 0))
			}
		}
	}
	for _, key := range []string{"reset_after_seconds", "resetAfterSeconds", "reset_in", "resetIn", "ttl"} {
		if seconds := quotaNumber(document[key]); seconds != nil && *seconds <= 366*86400 {
			return timestamp(now.Add(time.Duration(*seconds * float64(time.Second))))
		}
	}
	return ""
}
func quotaWindow(id, label, scope string, seconds int64, used *float64, reset string) QuotaWindow {
	out := QuotaWindow{ID: id, Label: label, Scope: scope, PeriodSeconds: seconds, ResetAt: reset}
	if used != nil {
		value := math.Min(100, *used)
		remaining := 100 - value
		out.UsedPercent = &value
		out.RemainingPercent = &remaining
	}
	return out
}
func quotaPeriodLabel(seconds int64, fallback string) string {
	switch {
	case seconds == 18000:
		return "5 小时额度"
	case seconds == 604800:
		return "每周额度"
	case seconds >= 28*86400 && seconds <= 31*86400:
		return "每月额度"
	case seconds == 86400:
		return "每日额度"
	case seconds > 0 && seconds%3600 == 0:
		return strconv.FormatInt(seconds/3600, 10) + " 小时额度"
	case seconds > 0 && seconds%60 == 0:
		return strconv.FormatInt(seconds/60, 10) + " 分钟额度"
	}
	return fallback
}
func parseProviderQuota(provider string, payload map[string]any, now time.Time) AccountQuota {
	out := AccountQuota{Provider: provider, Supported: true, ObservedAt: timestamp(now), Windows: make([]QuotaWindow, 0)}
	appendWindow := func(window QuotaWindow) {
		if len(out.Windows) < 64 && (window.UsedPercent != nil || window.Remaining != nil || window.ResetAt != "") {
			out.Windows = append(out.Windows, window)
		}
	}
	switch provider {
	case "codex":
		limits := []struct {
			scope string
			data  map[string]any
		}{{"", quotaObject(quotaPick(payload, "rate_limit", "rateLimit"))}, {"代码审查", quotaObject(quotaPick(payload, "code_review_rate_limit", "codeReviewRateLimit"))}}
		for _, extra := range quotaArray(quotaPick(payload, "additional_rate_limits", "additionalRateLimits")) {
			entry := quotaObject(extra)
			scope := quotaText(quotaPick(entry, "limit_name", "limitName", "metered_feature", "meteredFeature"))
			if scope == "" {
				scope = "额外模型"
			}
			limits = append(limits, struct {
				scope string
				data  map[string]any
			}{scope, quotaObject(quotaPick(entry, "rate_limit", "rateLimit"))})
			if len(limits) >= 24 {
				break
			}
		}
		for index, limit := range limits {
			for _, slot := range []struct {
				keys []string
				name string
			}{{[]string{"primary_window", "primaryWindow"}, "主窗口"}, {[]string{"secondary_window", "secondaryWindow"}, "次窗口"}} {
				document := quotaObject(quotaPick(limit.data, slot.keys...))
				if len(document) == 0 {
					continue
				}
				seconds := int64(0)
				if number := quotaNumber(quotaPick(document, "limit_window_seconds", "limitWindowSeconds")); number != nil {
					seconds = int64(*number)
				}
				used := quotaNumber(quotaPick(document, "used_percent", "usedPercent"))
				// Only an explicit exhausted signal permits deriving zero left
				// without a utilization number.
				if used == nil && quotaReset(document, now) != "" && (limit.data["limit_reached"] == true || limit.data["limitReached"] == true || limit.data["allowed"] == false) {
					value := 100.0
					used = &value
				}
				appendWindow(quotaWindow(strconv.Itoa(index)+"-"+slot.name, quotaPeriodLabel(seconds, slot.name), limit.scope, seconds, used, quotaReset(document, now)))
			}
		}
		out.Plan = quotaText(quotaPick(payload, "plan_type", "planType"))
		credits := quotaObject(payload["credits"])
		out.Balance = quotaNumber(credits["balance"])
		out.Unlimited = credits["unlimited"] == true
	case "claude":
		windows := []struct {
			key, label string
			seconds    int64
		}{
			{"five_hour", "5 小时额度", 18000}, {"seven_day", "每周额度", 604800},
			{"seven_day_oauth_apps", "OAuth 应用 · 每周", 604800}, {"seven_day_opus", "Opus · 每周", 604800},
			{"seven_day_sonnet", "Sonnet · 每周", 604800}, {"seven_day_cowork", "Cowork · 每周", 604800},
		}
		for _, key := range windows {
			document := quotaObject(payload[key.key])
			if len(document) == 0 {
				continue
			}
			appendWindow(quotaWindow(key.key, key.label, "", key.seconds, quotaNumber(document["utilization"]), quotaReset(document, now)))
		}
		// Newer Claude responses include separately scoped weekly model limits.
		for index, limit := range quotaArray(payload["limits"]) {
			entry := quotaObject(limit)
			if entry["kind"] != "weekly_scoped" {
				continue
			}
			scope := quotaText(quotaObject(quotaObject(entry["scope"])["model"])["display_name"])
			appendWindow(quotaWindow("scoped-"+strconv.Itoa(index), "每周额度", scope, 604800, quotaNumber(entry["percent"]), quotaReset(entry, now)))
		}
		for _, key := range []string{"iguana_necktie", "additional_credit_balance"} {
			document := quotaObject(payload[key])
			if len(document) == 0 {
				continue
			}
			if key == "iguana_necktie" && quotaNumber(document["limit_dollars"]) == nil && quotaNumber(document["remaining_dollars"]) == nil && quotaNumber(document["used_dollars"]) == nil {
				covered := false
				for _, window := range out.Windows {
					if strings.Contains(strings.ToLower(window.Scope), "fable") {
						covered = true
						break
					}
				}
				if !covered {
					appendWindow(quotaWindow("seven-day-fable", "Fable · 每周", "", 604800, quotaNumber(document["utilization"]), quotaReset(document, now)))
				}
				continue
			}
			window := quotaWindow(key, map[bool]string{true: "额外余额", false: "云端会话额度"}[key == "additional_credit_balance"], "", 0, quotaNumber(document["utilization"]), quotaReset(document, now))
			window.Used = quotaNumber(document["used_dollars"])
			window.Limit = quotaNumber(document["limit_dollars"])
			window.Remaining = quotaNumber(document["remaining_dollars"])
			window.Unit = "USD"
			appendWindow(window)
		}
		extra := quotaObject(payload["extra_usage"])
		if extra["is_enabled"] == true {
			window := quotaWindow("extra_usage", "额外用量 · 每月", "", 0, quotaNumber(extra["utilization"]), "")
			// Claude reports these credit amounts in cents; keep the declared
			// unit explicit rather than presenting them as whole dollars.
			window.Used = quotaNumber(extra["used_credits"])
			window.Limit = quotaNumber(extra["monthly_limit"])
			window.Unit = "credits"
			if window.Used != nil && window.Limit != nil {
				remaining := math.Max(0, *window.Limit-*window.Used)
				window.Remaining = &remaining
			}
			if strings.EqualFold(quotaText(extra["currency"]), "USD") {
				window.Unit = "USD"
				for _, amount := range []*float64{window.Used, window.Limit, window.Remaining} {
					if amount != nil {
						*amount /= 100
					}
				}
			}
			appendWindow(window)
		}
	case "kimi", "kimi-ai":
		addKimi := func(id, label string, document map[string]any, seconds int64) {
			if len(document) == 0 {
				return
			}
			used, limit, remaining := quotaNumber(document["used"]), quotaNumber(document["limit"]), quotaNumber(document["remaining"])
			if used == nil && remaining != nil && limit != nil {
				value := math.Max(0, *limit-*remaining)
				used = &value
			}
			if remaining == nil && used != nil && limit != nil {
				value := math.Max(0, *limit-*used)
				remaining = &value
			}
			var percent *float64
			if used != nil && limit != nil && *limit > 0 {
				value := *used / *limit * 100
				percent = &value
			}
			window := quotaWindow(id, quotaPeriodLabel(seconds, label), "", seconds, percent, quotaReset(document, now))
			window.Used = used
			window.Limit = limit
			window.Remaining = remaining
			appendWindow(window)
		}
		for index, limit := range quotaArray(payload["limits"]) {
			entry := quotaObject(limit)
			detail := quotaObject(entry["detail"])
			if len(detail) == 0 {
				detail = entry
			}
			window := quotaObject(entry["window"])
			seconds := int64(0)
			duration := quotaNumber(quotaPick(window, "duration"))
			if duration == nil {
				duration = quotaNumber(quotaPick(entry, "duration"))
			}
			if duration != nil {
				multiplier := 60.0
				switch strings.ToUpper(quotaText(quotaPick(window, "timeUnit", "time_unit"))) {
				case "SECONDS", "SECOND":
					multiplier = 1
				case "HOURS", "HOUR":
					multiplier = 3600
				case "DAYS", "DAY":
					multiplier = 86400
				case "WEEK", "WEEKS":
					multiplier = 604800
				}
				seconds = int64(*duration * multiplier)
			}
			label := quotaText(quotaPick(detail, "name", "title", "scope"))
			if label == "" {
				label = "额度窗口 " + strconv.Itoa(index+1)
			}
			addKimi("limit-"+strconv.Itoa(index), label, detail, seconds)
		}
		addKimi("weekly", "每周额度", quotaObject(payload["usage"]), 604800)
		monthly := quotaObject(quotaObject(payload["usages"])["limit_month_total"])
		if ratio := quotaNumber(monthly["used_ratio"]); ratio != nil {
			used := *ratio * 100
			appendWindow(quotaWindow("monthly", "每月额度", "", 30*86400, &used, quotaReset(monthly, now)))
		}
	case "antigravity":
		for i, group := range quotaArray(payload["groups"]) {
			entry := quotaObject(group)
			scope := quotaText(quotaPick(entry, "displayName", "display_name"))
			if scope == "" {
				scope = "模型组 " + strconv.Itoa(i+1)
			}
			for j, bucket := range quotaArray(entry["buckets"]) {
				document := quotaObject(bucket)
				fraction := quotaNumber(quotaPick(document, "remainingFraction", "remaining_fraction"))
				if fraction == nil || *fraction > 1 {
					continue
				}
				period := int64(0)
				name := quotaText(document["window"])
				switch strings.ToLower(name) {
				case "5h", "five-hour", "five_hour":
					period = 18000
				case "week", "weekly":
					period = 604800
				}
				reset := quotaReset(map[string]any{"reset_time": quotaPick(document, "resetTime", "reset_time")}, now)
				used := 100 - *fraction*100
				appendWindow(quotaWindow(strconv.Itoa(i)+"-"+strconv.Itoa(j), quotaPeriodLabel(period, name), scope, period, &used, reset))
			}
		}
	case "devin":
		status := quotaObject(quotaObject(payload["userStatus"])["planStatus"])
		for _, period := range []struct {
			key     string
			seconds int64
		}{{"daily", 86400}, {"weekly", 604800}} {
			left := quotaNumber(status[period.key+"QuotaRemainingPercent"])
			var used *float64
			if left != nil && *left <= 100 {
				value := 100 - *left
				used = &value
			}
			reset := quotaReset(map[string]any{"resetAtUnix": status[period.key+"QuotaResetAtUnix"]}, now)
			appendWindow(quotaWindow(period.key, quotaPeriodLabel(period.seconds, period.key), "", period.seconds, used, reset))
		}
		out.Plan = quotaText(quotaObject(status["planInfo"])["planName"])
	case "meta":
		usage := quotaObject(payload["subs_usage"])
		for _, period := range []string{"window", "weekly"} {
			document := quotaObject(usage[period])
			if len(document) == 0 {
				continue
			}
			seconds := int64(604800)
			if period == "window" {
				seconds = 0
				if minutes := quotaNumber(document["window_duration_mins"]); minutes != nil {
					seconds = int64(*minutes * 60)
				}
			}
			appendWindow(quotaWindow(period, quotaPeriodLabel(seconds, "短时额度"), "", seconds, quotaNumber(document["used_percent"]), quotaReset(document, now)))
		}
		out.Plan = quotaText(payload["subs_tier_name"])
	case "xai":
		config := quotaObject(payload["config"])
		period := quotaObject(quotaPick(config, "currentPeriod", "current_period", "period"))
		seconds := int64(0)
		id := strings.ToLower(quotaText(period["type"]))
		if strings.Contains(id, "week") {
			seconds = 604800
		} else if strings.Contains(id, "month") {
			seconds = 30 * 86400
		}
		reset := quotaReset(map[string]any{"periodEnd": quotaPick(period, "end", "endTime", "periodEnd")}, now)
		appendWindow(quotaWindow(id, quotaPeriodLabel(seconds, "账单额度"), "", seconds, quotaNumber(quotaPick(config, "creditUsagePercent", "credit_usage_percent", "usagePercent")), reset))
		money := func(value any) *float64 {
			if entry, ok := value.(map[string]any); ok {
				return quotaNumber(entry["val"])
			}
			return quotaNumber(value)
		}
		if used, limit := money(config["used"]), money(quotaPick(config, "monthlyLimit", "monthly_limit")); used != nil && limit != nil && *limit > 0 {
			percent := *used / *limit * 100
			monthlyReset := quotaReset(map[string]any{"periodEnd": quotaPick(config, "billingPeriodEnd", "billing_period_end")}, now)
			window := quotaWindow("monthly-balance", "每月账单", "", 0, &percent, monthlyReset)
			window.Used = used
			window.Limit = limit
			remaining := math.Max(0, *limit-*used)
			window.Remaining = &remaining
			window.Unit = "cents"
			appendWindow(window)
		}
	}
	return out
}
