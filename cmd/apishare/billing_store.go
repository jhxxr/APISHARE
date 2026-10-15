package main

import (
	"fmt"
	"math"
)

// Existing logs retain their original totals and cost. New snapshots are
// nullable at the API level so historical rows never acquire invented detail.
func migrateBillingLogSchema() error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('call_logs') WHERE name='billing_json'`)
	if err != nil {
		return err
	}
	exists := rows.Next()
	err = rows.Err()
	rows.Close()
	if err != nil || exists {
		return err
	}
	_, err = db.Exec(`ALTER TABLE call_logs ADD COLUMN billing_json TEXT NOT NULL DEFAULT ''`)
	return err
}

// Speed columns stay zero for historical rows, which is rendered as "—".
func migrateSpeedLogSchema() error {
	for _, column := range []string{"first_token_ms", "duration_ms"} {
		rows, err := db.Query(`SELECT name FROM pragma_table_info('call_logs') WHERE name=?`, column)
		if err != nil {
			return err
		}
		exists := rows.Next()
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE call_logs ADD COLUMN ` + column + ` INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	return nil
}

// The receipt and quota deduction commit together. A failed log insert cannot
// leave an unexplained debit in either the shared or model-specific pool.
func dbRecordBilledCall(entry *CallLog, independent bool) error {
	if math.IsNaN(entry.CostUSD) || math.IsInf(entry.CostUSD, 0) || entry.CostUSD < 0 {
		return fmt.Errorf("invalid billing receipt cost")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if entry.CostUSD > 0 {
		if err := dbChargeKeyTx(tx, entry.KeyID, entry.Model, entry.CostUSD, independent); err != nil {
			return err
		}
	}
	if err := insertCallLog(tx, entry); err != nil {
		return err
	}
	return tx.Commit()
}
