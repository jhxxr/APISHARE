package main

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var db *sql.DB

func openDB(path string) error {
	dsn := "file:" + filepathToSlash(path) + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	// 使用单个写连接，避免并发写入触发 SQLITE_BUSY。
	d.SetMaxOpenConns(1)
	if err := d.Ping(); err != nil {
		return err
	}
	db = d
	return migrate()
}

func filepathToSlash(p string) string {
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '\\' {
			c = '/'
		}
		out = append(out, c)
	}
	return string(out)
}

func migrate() error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS api_keys (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL DEFAULT '',
  key TEXT NOT NULL UNIQUE,
  quota_usd REAL NOT NULL DEFAULT 0,
  used_usd REAL NOT NULL DEFAULT 0,
  shared_used_usd REAL NOT NULL DEFAULT 0,
  qps REAL NOT NULL DEFAULT 3,
  concurrency INTEGER NOT NULL DEFAULT 5,
  enabled INTEGER NOT NULL DEFAULT 1,
  show_on_home INTEGER NOT NULL DEFAULT 0,
  models TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS upstreams (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL DEFAULT '',
  type TEXT NOT NULL DEFAULT 'openai',
  base_url TEXT NOT NULL DEFAULT '',
  api_key TEXT NOT NULL DEFAULT '',
  weight INTEGER NOT NULL DEFAULT 1,
  models TEXT NOT NULL DEFAULT '',
  model_map TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  proxy_id INTEGER NOT NULL DEFAULT 0,
  header_overrides TEXT NOT NULL DEFAULT '',
  openai_protocol TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS outbound_proxies (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL DEFAULT '',
  url TEXT NOT NULL DEFAULT '',
  username TEXT NOT NULL DEFAULT '',
  password TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS call_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  key_id INTEGER NOT NULL DEFAULT 0,
  key_name TEXT NOT NULL DEFAULT '',
  upstream_id INTEGER NOT NULL DEFAULT 0,
  upstream_name TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  endpoint TEXT NOT NULL DEFAULT '',
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cost_usd REAL NOT NULL DEFAULT 0,
  status INTEGER NOT NULL DEFAULT 0,
  latency_ms INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_logs_created ON call_logs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_logs_key_created ON call_logs(key_id,created_at);
CREATE INDEX IF NOT EXISTS idx_logs_epoch ON call_logs(unixepoch(created_at));
CREATE INDEX IF NOT EXISTS idx_logs_model_epoch ON call_logs(model,unixepoch(created_at) DESC,id DESC,status,prompt_tokens,completion_tokens);
CREATE TABLE IF NOT EXISTS api_key_model_quotas (
  key_id INTEGER NOT NULL,
  model TEXT NOT NULL,
  quota_usd REAL,
  used_usd REAL NOT NULL DEFAULT 0,
  PRIMARY KEY(key_id,model)
);
CREATE TABLE IF NOT EXISTS key_view_sessions (
  token_hash TEXT PRIMARY KEY,
  key_id INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_key_view_expiry ON key_view_sessions(expires_at);
CREATE TABLE IF NOT EXISTS settings (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS announcements (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  title TEXT NOT NULL,
  content TEXT NOT NULL,
  published INTEGER NOT NULL DEFAULT 0,
  pinned INTEGER NOT NULL DEFAULT 0,
  published_at TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_announcements_public ON announcements(published,pinned DESC,published_at DESC,id DESC);
CREATE TABLE IF NOT EXISTS session_affinity (
  skey TEXT PRIMARY KEY,
  upstream_id INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
`)
	if err != nil {
		return err
	}
	// Keep legacy visibility columns for database compatibility; telemetry is now site-wide.
	rows, err := db.Query(`PRAGMA table_info(api_keys)`)
	if err != nil {
		return err
	}
	hasHome, hasModels, hasSharedUsed := false, false, false
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "show_on_home" {
			hasHome = true
		}
		if name == "models" {
			hasModels = true
		}
		if name == "shared_used_usd" {
			hasSharedUsed = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !hasHome {
		_, err = db.Exec(`ALTER TABLE api_keys ADD COLUMN show_on_home INTEGER NOT NULL DEFAULT 0`)
	}
	if err != nil {
		return err
	}
	// An empty allowlist denies model access until an administrator binds models.
	if !hasModels {
		if _, err = db.Exec(`ALTER TABLE api_keys ADD COLUMN models TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !hasSharedUsed {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`ALTER TABLE api_keys ADD COLUMN shared_used_usd REAL NOT NULL DEFAULT 0`); err != nil {
			return err
		}
		// Before independent budgets existed, all historical spending was shared.
		if _, err := tx.Exec(`UPDATE api_keys SET shared_used_usd=used_usd`); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	rows, err = db.Query(`SELECT name FROM pragma_table_info('upstreams') WHERE name='proxy_id'`)
	if err != nil {
		return err
	}
	hasProxy := rows.Next()
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !hasProxy {
		_, err = db.Exec(`ALTER TABLE upstreams ADD COLUMN proxy_id INTEGER NOT NULL DEFAULT 0`)
	}
	if err != nil {
		return err
	}
	rows, err = db.Query(`SELECT name FROM pragma_table_info('upstreams') WHERE name='header_overrides'`)
	if err != nil {
		return err
	}
	hasHeaders := rows.Next()
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !hasHeaders {
		if _, err = db.Exec(`ALTER TABLE upstreams ADD COLUMN header_overrides TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	rows, err = db.Query(`SELECT name FROM pragma_table_info('upstreams') WHERE name='openai_protocol'`)
	if err != nil {
		return err
	}
	hasProtocol := rows.Next()
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !hasProtocol {
		if _, err = db.Exec(`ALTER TABLE upstreams ADD COLUMN openai_protocol TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if err := migrateBillingLogSchema(); err != nil {
		return err
	}
	return migrateTimestampsToBeijing()
}

// ---------- models ----------

type ApiKey struct {
	ID            int64                 `json:"id"`
	Name          string                `json:"name"`
	Key           string                `json:"key"`
	QuotaUSD      float64               `json:"quota_usd"`
	UsedUSD       float64               `json:"used_usd"`
	SharedUsedUSD float64               `json:"shared_used_usd"`
	QPS           float64               `json:"qps"`
	Concurrency   int                   `json:"concurrency"`
	Enabled       bool                  `json:"enabled"`
	ShowOnHome    bool                  `json:"-"`
	Models        string                `json:"models"` // comma-separated client model IDs; empty denies access
	ModelQuotas   map[string]ModelQuota `json:"model_quotas"`
	Note          string                `json:"note"`
	CreatedAt     string                `json:"created_at"`
}

type Upstream struct {
	config          *upstreamConfig // Immutable parsed runtime configuration; never exposed by APIs.
	ID              int64           `json:"id"`
	Name            string          `json:"name"`
	Type            string          `json:"type"` // provider: openai | anthropic | gemini; OpenAIProtocol selects its wire format
	BaseURL         string          `json:"base_url"`
	APIKey          string          `json:"api_key"`
	Weight          int             `json:"weight"`
	Models          string          `json:"models"`
	ModelMap        string          `json:"model_map"`
	Enabled         bool            `json:"enabled"`
	ProxyID         int64           `json:"proxy_id"`
	HeaderOverrides string          `json:"header_overrides"`
	OpenAIProtocol  string          `json:"openai_protocol"` // auto (legacy), chat, responses
	CreatedAt       string          `json:"created_at"`
}

type CallLog struct {
	ID               int64           `json:"id"`
	KeyID            int64           `json:"key_id"`
	KeyName          string          `json:"key_name"`
	UpstreamID       int64           `json:"upstream_id"`
	UpstreamName     string          `json:"upstream_name"`
	Model            string          `json:"model"`
	Endpoint         string          `json:"endpoint"`
	PromptTokens     int             `json:"prompt_tokens"`
	CompletionTokens int             `json:"completion_tokens"`
	CostUSD          float64         `json:"cost_usd"`
	Billing          *billingDetails `json:"billing,omitempty"`
	Status           int             `json:"status"`
	LatencyMS        int64           `json:"latency_ms"`
	Error            string          `json:"error"`
	CreatedAt        string          `json:"created_at"`
}

func nowStr() string { return beijingNow().Format(time.RFC3339) }

func migrateTimestampsToBeijing() error {
	const migrationKey = "migration_timestamps_beijing_v1"
	var applied string
	err := db.QueryRow(`SELECT v FROM settings WHERE k=?`, migrationKey).Scan(&applied)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"api_keys", "upstreams", "outbound_proxies", "call_logs"} {
		query := `UPDATE ` + table + `
			SET created_at=strftime('%Y-%m-%dT%H:%M:%f+08:00',created_at,'+8 hours')
			WHERE created_at<>'' AND substr(created_at,-6)<> '+08:00'
			AND julianday(created_at) IS NOT NULL`
		if _, err := tx.Exec(query); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO settings(k,v) VALUES(?,?)`, migrationKey, "1"); err != nil {
		return err
	}
	return tx.Commit()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------- api_keys ----------

func dbGetKeyByKey(key string) (*ApiKey, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	row := tx.QueryRow(`SELECT id,name,key,quota_usd,used_usd,qps,concurrency,enabled,note,created_at,show_on_home,models,shared_used_usd
		FROM api_keys WHERE key=?`, key)
	ak := &ApiKey{}
	var enabled, home int
	if err := row.Scan(&ak.ID, &ak.Name, &ak.Key, &ak.QuotaUSD, &ak.UsedUSD, &ak.QPS,
		&ak.Concurrency, &enabled, &ak.Note, &ak.CreatedAt, &home, &ak.Models, &ak.SharedUsedUSD); err != nil {
		return nil, err
	}
	ak.Enabled = enabled == 1
	ak.ShowOnHome = home == 1
	if err := dbLoadModelQuotas(tx, ak); err != nil {
		return nil, err
	}
	return ak, tx.Commit()
}

func dbGetKeyByID(id int64) (*ApiKey, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	row := tx.QueryRow(`SELECT id,name,key,quota_usd,used_usd,qps,concurrency,enabled,note,created_at,show_on_home,models,shared_used_usd
		FROM api_keys WHERE id=?`, id)
	ak := &ApiKey{}
	var enabled, home int
	if err := row.Scan(&ak.ID, &ak.Name, &ak.Key, &ak.QuotaUSD, &ak.UsedUSD, &ak.QPS,
		&ak.Concurrency, &enabled, &ak.Note, &ak.CreatedAt, &home, &ak.Models, &ak.SharedUsedUSD); err != nil {
		return nil, err
	}
	ak.Enabled = enabled == 1
	ak.ShowOnHome = home == 1
	if err := dbLoadModelQuotas(tx, ak); err != nil {
		return nil, err
	}
	return ak, tx.Commit()
}

func dbListKeys() ([]ApiKey, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id,name,key,quota_usd,used_usd,qps,concurrency,enabled,note,created_at,show_on_home,models,shared_used_usd
		FROM api_keys ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApiKey
	for rows.Next() {
		var ak ApiKey
		var enabled, home int
		if err := rows.Scan(&ak.ID, &ak.Name, &ak.Key, &ak.QuotaUSD, &ak.UsedUSD, &ak.QPS,
			&ak.Concurrency, &enabled, &ak.Note, &ak.CreatedAt, &home, &ak.Models, &ak.SharedUsedUSD); err != nil {
			return nil, err
		}
		ak.Enabled = enabled == 1
		ak.ShowOnHome = home == 1
		out = append(out, ak)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		if err := dbLoadModelQuotas(tx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit()
}

func dbInsertKey(ak *ApiKey) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Imported keys without independent budgets retain their existing shared spend.
	if len(ak.ModelQuotas) == 0 && ak.SharedUsedUSD == 0 {
		ak.SharedUsedUSD = ak.UsedUSD
	}
	res, err := tx.Exec(`INSERT INTO api_keys(name,key,quota_usd,used_usd,qps,concurrency,enabled,note,created_at,show_on_home,models,shared_used_usd)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		ak.Name, ak.Key, ak.QuotaUSD, ak.UsedUSD, ak.QPS, ak.Concurrency, boolToInt(ak.Enabled), ak.Note, nowStr(), boolToInt(ak.ShowOnHome), ak.Models, ak.SharedUsedUSD)
	if err != nil {
		return err
	}
	ak.ID, err = res.LastInsertId()
	if err != nil {
		return err
	}
	if err := dbSaveModelQuotas(tx, ak); err != nil {
		return err
	}
	return tx.Commit()
}

func dbUpdateKey(ak *ApiKey) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`UPDATE api_keys SET name=?,quota_usd=?,qps=?,concurrency=?,enabled=?,note=?,show_on_home=?,models=? WHERE id=?`,
		ak.Name, ak.QuotaUSD, ak.QPS, ak.Concurrency, boolToInt(ak.Enabled), ak.Note, boolToInt(ak.ShowOnHome), ak.Models, ak.ID)
	if err != nil {
		return err
	}
	if err := dbSaveModelQuotas(tx, ak); err != nil {
		return err
	}
	if !ak.Enabled {
		if _, err := tx.Exec(`DELETE FROM key_view_sessions WHERE key_id=?`, ak.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func dbDeleteKey(id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM key_view_sessions WHERE key_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM api_key_model_quotas WHERE key_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM api_keys WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func dbAddUsedUSD(keyID int64, cost float64) error {
	return dbChargeKey(keyID, "", cost, false)
}

// ---------- upstreams ----------

func dbGetUpstream(id int64) (*Upstream, error) {
	row := db.QueryRow(`SELECT id,name,type,base_url,api_key,weight,models,model_map,enabled,created_at,proxy_id,header_overrides,openai_protocol
		FROM upstreams WHERE id=?`, id)
	u := &Upstream{}
	var enabled int
	if err := row.Scan(&u.ID, &u.Name, &u.Type, &u.BaseURL, &u.APIKey, &u.Weight,
		&u.Models, &u.ModelMap, &enabled, &u.CreatedAt, &u.ProxyID, &u.HeaderOverrides, &u.OpenAIProtocol); err != nil {
		return nil, err
	}
	u.Enabled = enabled == 1
	return u, nil
}

func dbListUpstreams() ([]Upstream, error) {
	rows, err := db.Query(`SELECT id,name,type,base_url,api_key,weight,models,model_map,enabled,created_at,proxy_id,header_overrides,openai_protocol
		FROM upstreams ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Upstream
	for rows.Next() {
		var u Upstream
		var enabled int
		if err := rows.Scan(&u.ID, &u.Name, &u.Type, &u.BaseURL, &u.APIKey, &u.Weight,
			&u.Models, &u.ModelMap, &enabled, &u.CreatedAt, &u.ProxyID, &u.HeaderOverrides, &u.OpenAIProtocol); err != nil {
			return nil, err
		}
		u.Enabled = enabled == 1
		out = append(out, u)
	}
	return out, rows.Err()
}

func dbInsertUpstream(u *Upstream) error {
	res, err := db.Exec(`INSERT INTO upstreams(name,type,base_url,api_key,weight,models,model_map,enabled,created_at,proxy_id,header_overrides,openai_protocol)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.Name, u.Type, u.BaseURL, u.APIKey, u.Weight, u.Models, u.ModelMap, boolToInt(u.Enabled), nowStr(), u.ProxyID, u.HeaderOverrides, u.OpenAIProtocol)
	if err == nil {
		u.ID, _ = res.LastInsertId()
		invalidateRoutingSnapshot()
		invalidatePublicOverviews()
	}
	return err
}

func dbUpdateUpstream(u *Upstream) error {
	_, err := db.Exec(`UPDATE upstreams SET name=?,type=?,base_url=?,api_key=?,weight=?,models=?,model_map=?,enabled=?,proxy_id=?,header_overrides=?,openai_protocol=? WHERE id=?`,
		u.Name, u.Type, u.BaseURL, u.APIKey, u.Weight, u.Models, u.ModelMap, boolToInt(u.Enabled), u.ProxyID, u.HeaderOverrides, u.OpenAIProtocol, u.ID)
	if err == nil {
		invalidateRoutingSnapshot()
		invalidatePublicOverviews()
	}
	return err
}

func dbDeleteUpstream(id int64) error {
	_, err := db.Exec(`DELETE FROM upstreams WHERE id=?`, id)
	if err == nil {
		invalidateRoutingSnapshot()
		invalidatePublicOverviews()
	}
	return err
}

// ---------- logs ----------

func dbInsertLog(l *CallLog) error {
	return insertCallLog(db, l)
}

type logExecutor interface {
	Exec(string, ...any) (sql.Result, error)
}

func insertCallLog(executor logExecutor, l *CallLog) error {
	billingJSON := ""
	if l.Billing != nil {
		encoded, err := json.Marshal(l.Billing)
		if err != nil {
			return err
		}
		billingJSON = string(encoded)
	}
	_, err := executor.Exec(`INSERT INTO call_logs(key_id,key_name,upstream_id,upstream_name,model,endpoint,
		prompt_tokens,completion_tokens,cost_usd,status,latency_ms,error,created_at,billing_json)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		l.KeyID, l.KeyName, l.UpstreamID, l.UpstreamName, l.Model, l.Endpoint,
		l.PromptTokens, l.CompletionTokens, l.CostUSD, l.Status, l.LatencyMS, l.Error, nowStr(), billingJSON)
	return err
}

func dbListLogs(limit, offset int) ([]CallLog, error) {
	rows, err := db.Query(`SELECT id,key_id,key_name,upstream_id,upstream_name,model,endpoint,
		prompt_tokens,completion_tokens,cost_usd,status,latency_ms,error,created_at,billing_json
		FROM call_logs ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CallLog
	for rows.Next() {
		var l CallLog
		var billingJSON string
		if err := rows.Scan(&l.ID, &l.KeyID, &l.KeyName, &l.UpstreamID, &l.UpstreamName, &l.Model, &l.Endpoint,
			&l.PromptTokens, &l.CompletionTokens, &l.CostUSD, &l.Status, &l.LatencyMS, &l.Error, &l.CreatedAt, &billingJSON); err != nil {
			return nil, err
		}
		if billingJSON != "" {
			_ = json.Unmarshal([]byte(billingJSON), &l.Billing)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func dbCountLogs() (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM call_logs`).Scan(&n)
	return n, err
}

// ---------- settings ----------

func settingsGet(k string) (string, error) {
	var v string
	err := db.QueryRow(`SELECT v FROM settings WHERE k=?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func settingsSet(k, v string) error {
	_, err := db.Exec(`INSERT INTO settings(k,v) VALUES(?,?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

// ---------- runtime settings cache ----------

type Price struct {
	In             float64  `json:"in"`  // USD / 1M prompt tokens
	Out            float64  `json:"out"` // USD / 1M completion tokens
	CacheRead      *float64 `json:"cache_read,omitempty"`
	CacheWrite     *float64 `json:"cache_write,omitempty"`
	CacheWrite1h   *float64 `json:"cache_write_1h,omitempty"`
	Reasoning      *float64 `json:"reasoning,omitempty"`
	InAudio        *float64 `json:"in_audio,omitempty"`
	OutAudio       *float64 `json:"out_audio,omitempty"`
	CacheReadAudio *float64 `json:"cache_read_audio,omitempty"`
}

func (price *Price) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return fmt.Errorf("model price must be an object")
	}
	type plainPrice Price
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode((*plainPrice)(price)); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, field := range []string{"in", "out"} {
		if len(fields[field]) == 0 || bytes.Equal(bytes.TrimSpace(fields[field]), []byte("null")) {
			return fmt.Errorf("model price requires numeric %s", field)
		}
	}
	return nil
}

type AppSettings struct {
	AffinityTTLMin     int              `json:"affinity_ttl_min"`
	Retry              int              `json:"retry"`
	InjectStreamUsage  bool             `json:"inject_stream_usage"`
	Pricing            map[string]Price `json:"pricing"` // 手动覆盖 + default；其余模型走官方定价
	AutoPricingEnabled bool             `json:"auto_pricing_enabled"`
	AutoPricingURL     string           `json:"auto_pricing_url"`
}

var defaultManualPricing = map[string]Price{
	"default": {In: 0, Out: 0},
}

var (
	settingsMu    sync.RWMutex
	settingsCache = AppSettings{
		AffinityTTLMin: 30, Retry: 2, InjectStreamUsage: true,
		Pricing:            defaultManualPricing,
		AutoPricingEnabled: true, AutoPricingURL: defaultAutoPricingURL,
	}
)

func loadSettingsCache() error {
	if v, err := settingsGet("affinity_ttl_min"); err == nil && v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			settingsCache.AffinityTTLMin = n
		}
	}
	if v, err := settingsGet("retry"); err == nil && v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			settingsCache.Retry = n
		}
	}
	if v, err := settingsGet("inject_stream_usage"); err == nil && v != "" {
		settingsCache.InjectStreamUsage = v == "1"
	}
	if v, err := settingsGet("pricing"); err == nil && v != "" {
		var p map[string]Price
		if json.Unmarshal([]byte(v), &p) == nil && len(p) > 0 {
			settingsCache.Pricing = p
		}
	}
	if v, err := settingsGet("auto_pricing_enabled"); err == nil && v != "" {
		settingsCache.AutoPricingEnabled = v == "1"
	}
	if v, err := settingsGet("auto_pricing_url"); err == nil && v != "" {
		settingsCache.AutoPricingURL = v
	}
	return nil
}

func persistSettings(s AppSettings) error {
	settingsMu.Lock()
	settingsCache = s
	settingsMu.Unlock()
	if err := settingsSet("affinity_ttl_min", fmt.Sprintf("%d", s.AffinityTTLMin)); err != nil {
		return err
	}
	if err := settingsSet("retry", fmt.Sprintf("%d", s.Retry)); err != nil {
		return err
	}
	v := "0"
	if s.InjectStreamUsage {
		v = "1"
	}
	if err := settingsSet("inject_stream_usage", v); err != nil {
		return err
	}
	av := "0"
	if s.AutoPricingEnabled {
		av = "1"
	}
	if err := settingsSet("auto_pricing_enabled", av); err != nil {
		return err
	}
	if err := settingsSet("auto_pricing_url", s.AutoPricingURL); err != nil {
		return err
	}
	b, _ := json.Marshal(s.Pricing)
	return settingsSet("pricing", string(b))
}

func getSettings() AppSettings {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return settingsCache
}

// ---------- random ----------

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
