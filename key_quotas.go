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
