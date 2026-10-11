package main

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
)

// ModelQuotas contains only independent budgets. Other bound models share QuotaUSD.
// Both kinds use 0 for unlimited, matching the existing key quota convention.
type ModelQuota struct {
	QuotaUSD float64 `json:"quota_usd"`
	UsedUSD  float64 `json:"used_usd"`
}

func validQuota(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validateKeyQuotas(ak *ApiKey) error {
	if !validQuota(ak.QuotaUSD) {
		return fmt.Errorf("共享额度必须是大于或等于 0 的有效金额")
	}
	for model, budget := range ak.ModelQuotas {
		if model == "" || model != strings.TrimSpace(model) || !ak.allowsModel(model) {
			return fmt.Errorf("独立额度的模型 %q 必须先绑定到该密钥", model)
		}
		if !validQuota(budget.QuotaUSD) {
			return fmt.Errorf("模型 %q 的独立额度必须是大于或等于 0 的有效金额", model)
		}
	}
	return nil
}

func (ak ApiKey) quotaExhausted(model string) bool {
	if budget, independent := ak.ModelQuotas[model]; independent {
		return budget.QuotaUSD > 0 && budget.UsedUSD >= budget.QuotaUSD
	}
	return ak.QuotaUSD > 0 && ak.SharedUsedUSD >= ak.QuotaUSD
}

// Read permissions and balances in the same snapshot so an edit cannot mix pool modes.
func dbLoadModelQuotas(tx *sql.Tx, ak *ApiKey) error {
	rows, err := tx.Query(`SELECT model,quota_usd,used_usd FROM api_key_model_quotas WHERE key_id=? AND quota_usd IS NOT NULL`, ak.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	ak.ModelQuotas = make(map[string]ModelQuota)
	for rows.Next() {
		var model string
		var budget ModelQuota
		if err := rows.Scan(&model, &budget.QuotaUSD, &budget.UsedUSD); err != nil {
			return err
		}
		ak.ModelQuotas[model] = budget
	}
	return rows.Err()
}

func dbSaveModelQuotas(tx *sql.Tx, ak *ApiKey) error {
	// Keep historical independent spend even when changing modes or unbinding a model.
	if _, err := tx.Exec(`UPDATE api_key_model_quotas SET quota_usd=NULL WHERE key_id=?`, ak.ID); err != nil {
		return err
	}
	for model, budget := range ak.ModelQuotas {
		if !ak.allowsModel(model) {
			continue
		}
		// Usage is server-owned; edits never reset it or overwrite concurrent billing.
		if _, err := tx.Exec(`INSERT INTO api_key_model_quotas(key_id,model,quota_usd) VALUES(?,?,?)
			ON CONFLICT(key_id,model) DO UPDATE SET quota_usd=excluded.quota_usd`, ak.ID, model, budget.QuotaUSD); err != nil {
			return err
		}
	}
	return nil
}

// Charge the pool chosen when the request was admitted, even if settings change in flight.
func dbChargeKey(keyID int64, model string, cost float64, independent bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	sharedCost := cost
	if independent {
		sharedCost = 0
	}
	res, err := tx.Exec(`UPDATE api_keys SET used_usd=used_usd+?,shared_used_usd=shared_used_usd+? WHERE id=?`, cost, sharedCost, keyID)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if independent && changed > 0 {
		if _, err := tx.Exec(`INSERT INTO api_key_model_quotas(key_id,model,quota_usd,used_usd) VALUES(?,?,NULL,?)
			ON CONFLICT(key_id,model) DO UPDATE SET used_usd=used_usd+excluded.used_usd`, keyID, model, cost); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func publicQuotaSummary(tx *sql.Tx, summary *PublicSummary, modelQuotas map[string]*PublicModelQuota) error {
	keys := make(map[int64]*ApiKey)
	if err := publicRows(tx, `SELECT id,models,quota_usd,shared_used_usd,used_usd FROM api_keys WHERE show_on_home=1`, func(rows *sql.Rows) error {
		ak := &ApiKey{ModelQuotas: make(map[string]ModelQuota)}
		if err := rows.Scan(&ak.ID, &ak.Models, &ak.QuotaUSD, &ak.SharedUsedUSD, &ak.UsedUSD); err != nil {
			return err
		}
		keys[ak.ID] = ak
		summary.UsedUSD += ak.UsedUSD
		return nil
	}); err != nil {
		return err
	}
	if err := publicRows(tx, `SELECT q.key_id,q.model,q.quota_usd,q.used_usd FROM api_key_model_quotas q
		JOIN api_keys k ON k.id=q.key_id WHERE k.show_on_home=1 AND q.quota_usd IS NOT NULL`, func(rows *sql.Rows) error {
		var id int64
		var model string
		var budget ModelQuota
		if err := rows.Scan(&id, &model, &budget.QuotaUSD, &budget.UsedUSD); err != nil {
			return err
		}
		keys[id].ModelQuotas[model] = budget
		return nil
	}); err != nil {
		return err
	}
	remaining := 0.0
	add := func(quota, used float64) {
		if quota == 0 {
			summary.Unlimited = true
			return
		}
		summary.QuotaUSD += quota
		remaining += math.Max(quota-used, 0)
	}
	for _, ak := range keys {
		hasShared := false
		for _, model := range keyModelIDs(ak.Models) {
			quota := modelQuotas[model]
			if quota == nil {
				quota = &PublicModelQuota{}
				modelQuotas[model] = quota
			}
			if budget, independent := ak.ModelQuotas[model]; independent {
				add(budget.QuotaUSD, budget.UsedUSD)
				if quota.Independent == nil {
					quota.Independent = &PublicQuotaPool{}
				}
				quota.Independent.add(budget.QuotaUSD, budget.UsedUSD)
			} else {
				hasShared = true
				if quota.Shared == nil {
					quota.Shared = &PublicQuotaPool{}
				}
				quota.Shared.add(ak.QuotaUSD, ak.SharedUsedUSD)
			}
		}
		if hasShared {
			add(ak.QuotaUSD, ak.SharedUsedUSD)
		}
	}
	for _, quota := range modelQuotas {
		switch {
		case quota.Shared != nil && quota.Independent != nil:
			quota.Mode = "mixed"
		case quota.Independent != nil:
			quota.Mode = "independent"
		default:
			quota.Mode = "shared"
		}
	}
	summary.Configured = len(keys) > 0
	if summary.Configured && !summary.Unlimited {
		summary.RemainingUSD = &remaining
	}
	return nil
}

func (pool *PublicQuotaPool) add(quota, used float64) {
	if quota == 0 {
		pool.Unlimited = true
		return
	}
	pool.QuotaUSD += quota
	pool.UsedUSD += used
	pool.RemainingUSD += math.Max(quota-used, 0)
}
