package main

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	autoRouterCodeSignalRe = regexp.MustCompile(`(?i)` +
		"```" + `|\b(def |class |func |function |interface |struct |import |package |SELECT |FROM |WHERE |INSERT |UPDATE |async |await |Promise|traceback|exception|stack trace|segfault|panic:|NullPointerException|TypeError|SyntaxError|ReferenceError|Dockerfile|Makefile|CMakeLists)\b|` +
		`(报错|异常|堆栈|死锁|内存泄漏|重构|多文件|并发|架构设计|源码|编译失败|单元测试|证明|推导|微积分|矩阵|特征值|拉普拉斯|傅里叶|马尔可夫|贝叶斯|凸优化|动态规划|复杂度分析|定理|引理)`)

	autoRouterReasoningRe = regexp.MustCompile(`(?i)` +
		`\b(prove |proof |theorem|lemma|corollary|mathematical induction|formal verification|step-by-step derivation|time complexity proof|NP-hard|NP-complete|game theory|Nash equilibrium|differential equation|eigenvalue|lagrangian|stochastic|bayesian inference)\b|` +
		`(数学证明|严格证明|数学归纳法|形式化验证|一步步推导|逐步推导|公式推导|定理证明|博弈论|纳什均衡|微分方程|偏微分|拉格朗日|随机过程|贝叶斯推导|理论推导|完备性证明)`)

	autoRouterComplexRe = regexp.MustCompile(`(?i)` +
		`\b(refactor|architecture|microservice|distributed|concurrency|race condition|deadlock|memory leak|performance optimization|connection pool|retry logic|multi-file|code review|security audit|vulnerability|benchmark|compiler|interpreter|state machine|consensus|raft|paxos|eeg|signal processing)\b|` +
		`(重构|架构|分布式|并发控制|竞态条件|死锁|内存泄漏|性能优化|连接池|重试机制|多文件|代码审查|安全审计|漏洞排查|状态机|信号处理|脑电|论文推导|系统设计|全栈|迁移方案|调优)`)

	autoRouterMediumRe = regexp.MustCompile(`(?i)` +
		`\b(summarize|summary|write a |draft |explain |compare |pros and cons|regex|regular expression|sql query|bash script|shell script|python script|unit test|parse|json|yaml|markdown|email|outline|blog post)\b|` +
		`(总结|概括|写一篇|起草|解释一下|对比|优缺点|正则表达式|脚本|单元测试|解析|邮件|大纲|周报|润色|改写|短代码)`)

	autoRouterSimpleRe = regexp.MustCompile(`(?i)` +
		`\b(translate|translation|what is|who is|when was|where is|hello|hi |hey |thanks|thank you|good morning|spell check|grammar|define |synonym|antonym)\b|` +
		`(翻译|怎么说|是什么|是谁|在哪里|你好|您好|谢谢|早上好|晚上好|同义词|反义词|拼音|首都|几月几号|等于多少)`)

	autoRouterHousekeepingRe = regexp.MustCompile(`(?i)` +
		`((generate|write|create|give) a (short |concise |brief |3-5 word |few-word )*(title|name|summary|topic)|` +
		`summarize this conversation in a few words|` +
		`生成一个简短的标题|为这段对话起一个标题|总结为简短标题|生成对话标题)`)
)

// extractClassifierState extracts the latest user message text and a bounded
// multi-turn state string (up to contextUserTurns user messages) from canonReq.
func extractClassifierState(cr *canonReq, contextUserTurns int, maxChars int) (lastUser string, state string) {
	if cr == nil {
		return "", ""
	}
	if maxChars <= 0 {
		maxChars = 6000
	}
	if contextUserTurns <= 0 {
		contextUserTurns = 1
	}
	var userTexts []string
	for i := len(cr.Messages) - 1; i >= 0; i-- {
		m := cr.Messages[i]
		if m.Role != "user" {
			continue
		}
		text := strings.TrimSpace(m.Text)
		if text == "" && len(m.Parts) > 0 {
			var sb strings.Builder
			for _, p := range m.Parts {
				if p.Text != "" {
					if sb.Len() > 0 {
						sb.WriteByte(' ')
					}
					sb.WriteString(p.Text)
				}
			}
			text = strings.TrimSpace(sb.String())
		}
		if text == "" {
			continue
		}
		userTexts = append(userTexts, text)
		if len(userTexts) >= contextUserTurns {
			break
		}
	}
	if len(userTexts) == 0 {
		fallback := strings.TrimSpace(cr.InputText)
		if fallback != "" {
			return truncateTail(fallback, maxChars), truncateTail(fallback, maxChars)
		}
		return "", ""
	}
	lastUser = userTexts[0]
	if len(userTexts) == 1 {
		return truncateTail(lastUser, maxChars), truncateTail(lastUser, maxChars)
	}
	var sb strings.Builder
	for i := len(userTexts) - 1; i >= 0; i-- {
		if sb.Len() > 0 {
			sb.WriteString("\n\n---\n\n")
		}
		sb.WriteString(userTexts[i])
	}
	return truncateTail(lastUser, maxChars), truncateTail(sb.String(), maxChars)
}

func truncateTail(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[len(r)-maxRunes:])
}

// isToolResultTurn reports whether the latest turn in the request is a tool
// output callback rather than a fresh human prompt.
func isToolResultTurn(cr *canonReq) bool {
	if cr == nil || len(cr.Messages) == 0 {
		return false
	}
	last := cr.Messages[len(cr.Messages)-1]
	if last.ToolID != "" || last.ToolName != "" {
		return true
	}
	for _, p := range last.Parts {
		if p.Kind == "tool_result" {
			return true
		}
	}
	return false
}

// isHousekeepingRequest detects coding-agent background housekeeping tasks
// (such as generating a short chat title) that should go straight to SIMPLE.
func isHousekeepingRequest(cr *canonReq, lastUser string) bool {
	if cr == nil || len(cr.Tools) > 0 {
		return false
	}
	if cr.MaxTokens > 0 && cr.MaxTokens <= 64 && utf8.RuneCountInString(lastUser) < 600 {
		if autoRouterHousekeepingRe.MatchString(cr.System) || autoRouterHousekeepingRe.MatchString(lastUser) {
			return true
		}
	}
	combined := cr.System + "\n" + lastUser
	return utf8.RuneCountInString(combined) < 500 && autoRouterHousekeepingRe.MatchString(combined)
}

// shouldShortCircuitSimple checks if the user prompt is short and has no
// code, debugging, or mathematical reasoning markers.
func shouldShortCircuitSimple(lastUser string, skipShortChars int) bool {
	if skipShortChars <= 0 {
		return false
	}
	trimmed := strings.TrimSpace(lastUser)
	if trimmed == "" {
		return true
	}
	n := utf8.RuneCountInString(trimmed)
	if n > skipShortChars {
		return false
	}
	return !autoRouterCodeSignalRe.MatchString(trimmed)
}

// classifyHeuristic scores a request locally in <1ms without network calls.
// Returns one of: SIMPLE, MEDIUM, COMPLEX, REASONING and a numeric complexity score.
func classifyHeuristic(lastUser, fullState string, cr *canonReq) (tier string, score float64) {
	text := strings.TrimSpace(fullState)
	if text == "" {
		text = strings.TrimSpace(lastUser)
	}
	if text == "" {
		return TierSimple, -0.5
	}

	runes := utf8.RuneCountInString(text)
	lastRunes := utf8.RuneCountInString(strings.TrimSpace(lastUser))

	// Feature regexes
	reasoningMatches := len(autoRouterReasoningRe.FindAllStringIndex(text, 6))
	complexMatches := len(autoRouterComplexRe.FindAllStringIndex(text, 8))
	codeMatches := len(autoRouterCodeSignalRe.FindAllStringIndex(text, 8))
	mediumMatches := len(autoRouterMediumRe.FindAllStringIndex(text, 6))
	simpleMatches := len(autoRouterSimpleRe.FindAllStringIndex(lastUser, 4))

	// Base score by prompt length (only penalize short prompts when no strong technical/reasoning signals exist)
	switch {
	case lastRunes < 40 && runes < 120 && reasoningMatches == 0 && complexMatches == 0 && codeMatches == 0:
		score -= 0.35
	case lastRunes < 120 && runes < 300 && reasoningMatches == 0 && complexMatches == 0:
		score -= 0.10
	case runes > 3500:
		score += 0.45
	case runes > 1500:
		score += 0.30
	case runes > 600:
		score += 0.15
	}

	// Code fences and multi-block structure
	fences := strings.Count(text, "```")
	if fences >= 4 {
		score += 0.45
	} else if fences >= 2 {
		score += 0.25
	}

	score += float64(reasoningMatches) * 0.45
	score += float64(complexMatches) * 0.28
	score += float64(codeMatches) * 0.14
	score += float64(mediumMatches) * 0.10
	if reasoningMatches == 0 && complexMatches == 0 && codeMatches == 0 {
		score -= float64(simpleMatches) * 0.20
	}

	// Tool definitions / multi-turn depth in coding agents
	if cr != nil {
		if len(cr.Tools) >= 5 {
			score += 0.20
		} else if len(cr.Tools) > 0 {
			score += 0.10
		}
		if cr.ReasoningEffort == "high" || cr.ThinkingMode == "enabled" {
			score += 0.35
		}
	}

	switch {
	case reasoningMatches >= 2 || (reasoningMatches >= 1 && score >= 0.65) || score >= 0.95:
		return TierReasoning, roundScore(score)
	case score >= 0.35 || complexMatches >= 2 || (complexMatches >= 1 && codeMatches >= 1):
		return TierComplex, roundScore(score)
	case score >= -0.05 || mediumMatches >= 1 || codeMatches >= 1 || lastRunes >= 140:
		return TierMedium, roundScore(score)
	default:
		return TierSimple, roundScore(score)
	}
}

func roundScore(v float64) float64 {
	return float64(int(v*100)) / 100
}
