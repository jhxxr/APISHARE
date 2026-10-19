package main

// 会话粘性：同一对话的所有请求固定路由到同一上游，保证上游 prompt cache 命中。
//
// 会话标识优先级：
//  1. 客户端显式会话头（含 OpenCode）或请求体会话标识
//  2. 对话前缀指纹：hash(endpoint + model + system + 首条 user 消息)
//     —— 同一对话的多轮请求共享相同前缀，因此指纹稳定。
//
// 粘性表持久化在 SQLite（服务重启不丢粘性），TTL 滑动续期：
// 每次命中都会把过期时间延长为 now+TTL，TTL 建议覆盖上游 cache 生命周期
// （Anthropic 默认 5 分钟、可加钱到 1 小时；OpenAI 自动缓存几分钟到数小时）。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"
)

const affinityCleanupInterval = 10 * time.Minute

func affinityTTL() time.Duration {
	s := getSettings()
	if s.AffinityTTLMin <= 0 {
		return 30 * time.Minute
	}
	return time.Duration(s.AffinityTTLMin) * time.Minute
}

func affinityGet(skey string) (int64, bool) {
	var upID int64
	var exp int64
	err := db.QueryRow(`SELECT upstream_id, expires_at FROM session_affinity WHERE skey=?`, skey).Scan(&upID, &exp)
	if err != nil {
		return 0, false
	}
	if time.Now().Unix() >= exp {
		db.Exec(`DELETE FROM session_affinity WHERE skey=?`, skey)
		return 0, false
	}
	newExp := time.Now().Add(affinityTTL()).Unix()
	db.Exec(`UPDATE session_affinity SET expires_at=? WHERE skey=?`, newExp, skey)
	return upID, true
}

func affinitySet(skey string, upID int64) {
	exp := time.Now().Add(affinityTTL()).Unix()
	db.Exec(`INSERT INTO session_affinity(skey,upstream_id,expires_at) VALUES(?,?,?)
		ON CONFLICT(skey) DO UPDATE SET upstream_id=excluded.upstream_id, expires_at=excluded.expires_at`,
		skey, upID, exp)
}

func affinityCleanupLoop() {
	for {
		time.Sleep(affinityCleanupInterval)
		db.Exec(`DELETE FROM session_affinity WHERE expires_at < ?`, time.Now().Unix())
		db.Exec(`DELETE FROM opencode_sessions WHERE expires_at < ?`, time.Now().Unix())
	}
}

// ---------- 会话指纹 ----------

func sessionFingerprint(endpoint, model, inputText string) string {
	h := sha256.Sum256([]byte(endpoint + "|" + model + "|" + inputText))
	return hex.EncodeToString(h[:])
}

// sessionKeyFromCanon 决定本次请求的会话键。
func sessionKeyFromCanon(endpoint string, header http.Header, cr *canonReq) string {
	if sid := explicitSessionID(header, cr.SessionID); sid != "" {
		return "sid:" + sid
	}
	return "fp:" + sessionFingerprint(endpoint, cr.Model, cr.InputText)
}

// scopedSessionKey makes routing affinity local to the gateway API key. Without
// this, two callers whose conversations start with the same text share a
// fingerprint and are routed to the same upstream key, proxy egress and
// upstream session. The empty key ID keeps the unscoped form for callers that
// do not have an authenticated key (tests and internal probes).
func scopedSessionKey(apiKeyID int64, skey string) string {
	if apiKeyID <= 0 {
		return skey
	}
	return fmt.Sprintf("key:%d:%s", apiKeyID, skey)
}
