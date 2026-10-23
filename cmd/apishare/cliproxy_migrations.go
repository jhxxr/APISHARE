package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"apishare/internal/cliproxy"
)

const migrationMarker = "apishare_migration"

type poolMigration struct {
	ID                 string
	SourceID, TargetID int64
	Name               string
	Disabled           bool
	ProxyID            int64
	Phase              string
	Message            string
}

func readPoolMigration(id string) (poolMigration, error) {
	var item poolMigration
	err := db.QueryRow(`SELECT id,source_id,target_id,name,disabled,proxy_id,phase,message FROM cliproxy_migrations WHERE id=?`, id).
		Scan(&item.ID, &item.SourceID, &item.TargetID, &item.Name, &item.Disabled, &item.ProxyID, &item.Phase, &item.Message)
	return item, err
}
func (m *poolMigration) save(phase, message string) error {
	_, err := db.Exec(`UPDATE cliproxy_migrations SET phase=?,message=?,updated_at=? WHERE id=?`, phase, message, nowStr(), m.ID)
	if err == nil {
		m.Phase = phase
		m.Message = message
	}
	return err
}
func credentialDocument(manager *cliproxy.Manager, name string) (map[string]any, bool, error) {
	files, err := manager.ReadCredentials()
	if err != nil {
		return nil, false, err
	}
	raw, exists := files[name]
	if !exists {
		return nil, false, nil
	}
	var document map[string]any
	err = json.Unmarshal(raw, &document)
	return document, true, err
}
func ownsMigration(document map[string]any, id string) bool { return document[migrationMarker] == id }
func migrationDisabled(document map[string]any) bool {
	value, _ := document["disabled"].(bool)
	return value
}

func movePoolCredential(ctx context.Context, source *cliproxy.Manager, sourceID int64, target *cliproxy.Manager, targetID int64, name string) error {
	return movePoolCredentialWithID(ctx, source, sourceID, target, targetID, name, randHex(16))
}
func movePoolCredentialWithID(ctx context.Context, source *cliproxy.Manager, sourceID int64, target *cliproxy.Manager, targetID int64, name, id string) error {
	if previous, err := readPoolMigration(id); err == nil {
		if previous.Phase == "done" {
			return nil
		}
		if previous.Phase == "rolled_back" {
			return errors.New(previous.Message)
		}
		return reconcilePoolMigration(previous)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	document, exists, err := credentialDocument(source, name)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("账号已不在源池中，请刷新列表")
	}
	if !source.Running() || !target.Running() {
		return errors.New("迁移前请先启动源池和目标池")
	}
	preview := make(map[string]any, len(document))
	for key, value := range document {
		preview[key] = value
	}
	if _, _, err := prepareCredentialReplacement(target, name, preview, false); err != nil {
		return errors.New("目标池已有重复账号或同名文件，源账号已保留")
	}
	item := poolMigration{ID: id, SourceID: sourceID, TargetID: targetID, Name: name, Disabled: migrationDisabled(document), ProxyID: accountProxyID(sourceID, name), Phase: "started"}
	if _, err := db.Exec(`INSERT INTO cliproxy_migrations(id,source_id,target_id,name,disabled,proxy_id,phase,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		id, sourceID, targetID, name, item.Disabled, item.ProxyID, item.Phase, nowStr()); err != nil {
		return err
	}
	if err := source.PatchCredential(ctx, name, map[string]any{"disabled": true, migrationMarker: id}); err != nil {
		return reconcilePoolMigration(item)
	}
	document, exists, err = credentialDocument(source, name)
	if err != nil || !exists || !ownsMigration(document, id) || !migrationDisabled(document) {
		return reconcilePoolMigration(item)
	}
	document["disabled"] = true
	raw, _ := json.Marshal(document)
	if err := item.save("copying", ""); err != nil {
		return reconcilePoolMigration(item)
	}
	// A timeout can mean the copy landed. Reconciliation reads both stores,
	// never treats an HTTP error as proof that a write did not happen.
	_ = importPoolCredential(ctx, target, targetID, name, raw, item.ProxyID)
	return reconcilePoolMigration(item)
}

// Every reconciliation gets a fresh budget, including rollback after a slow
// transfer. The journal contains no tokens or raw credential documents.
func reconcilePoolMigration(item poolMigration) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	source, err := cliProxyPool(item.SourceID)
	if err != nil {
		return errors.New("源池暂不可访问，迁移记录已保留")
	}
	target, err := cliProxyPool(item.TargetID)
	if err != nil {
		return errors.New("目标池暂不可访问，迁移记录已保留")
	}
	src, sourceExists, err := credentialDocument(source, item.Name)
	if err != nil {
		return errors.New("无法核对源凭据，稍后自动恢复")
	}
	dst, targetExists, err := credentialDocument(target, item.Name)
	if err != nil {
		return errors.New("无法核对目标凭据，稍后自动恢复")
	}
	targetOwned := targetExists && ownsMigration(dst, item.ID)
	sourceOwned := sourceExists && ownsMigration(src, item.ID)
	if !targetOwned {
		if !sourceExists {
			return errors.New("两侧缺少可确认的迁移凭据，需要人工检查；未执行清理")
		}
		if sourceOwned {
			_ = source.PatchCredential(ctx, item.Name, map[string]any{"disabled": item.Disabled})
			restored, exists, readErr := credentialDocument(source, item.Name)
			if readErr != nil || !exists || migrationDisabled(restored) != item.Disabled {
				return errors.New("源账号恢复未确认，稍后自动重试")
			}
		}
		message := "目标凭据未写入，源账号已保留并恢复原状态"
		if err := item.save("rolled_back", message); err != nil {
			return err
		}
		return errors.New(message)
	}
	if sourceExists && !sourceOwned {
		// Another operator changed the source. Never delete that credential or
		// enable a second live copy based on an old journal entry.
		return errors.New("源账号已发生变化，目标副本保持停用，请检查后重试")
	}
	if sourceExists && item.Phase == "rolling_back" {
		return rollbackPoolMigration(item, source, target)
	}
	if sourceExists {
		if !migrationDisabled(src) {
			_ = source.PatchCredential(ctx, item.Name, map[string]any{"disabled": true})
			src, sourceExists, err = credentialDocument(source, item.Name)
			if err != nil || !sourceExists || !migrationDisabled(src) {
				return errors.New("源账号停用未确认，未执行移除")
			}
		}
		if err := item.save("deleting", ""); err != nil {
			return err
		}
		_ = source.DeleteCredential(ctx, item.Name)
		src, sourceExists, err = credentialDocument(source, item.Name)
		if err != nil {
			return errors.New("源账号移除结果待核对，目标副本保持停用")
		}
		if sourceExists {
			// Restore and verify the authoritative source BEFORE removing our
			// disabled copy. A lost DELETE response must never lose both sides.
			if !ownsMigration(src, item.ID) {
				return errors.New("源账号已变化，迁移未继续")
			}
			if err := item.save("rolling_back", "源账号移除未完成，正在恢复"); err != nil {
				return err
			}
			return rollbackPoolMigration(item, source, target)
		}
	}
	if err := item.save("finishing", ""); err != nil {
		return err
	}
	// Source is confirmed absent, so only the destination may be activated.
	_ = target.PatchCredential(ctx, item.Name, map[string]any{"disabled": item.Disabled})
	dst, targetExists, err = credentialDocument(target, item.Name)
	if err != nil || !targetExists || !ownsMigration(dst, item.ID) || migrationDisabled(dst) != item.Disabled {
		return errors.New("凭据已移入目标池，启停状态待恢复")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO cliproxy_account_proxies(upstream_id,name,proxy_id) VALUES(?,?,?) ON CONFLICT(upstream_id,name) DO UPDATE SET proxy_id=excluded.proxy_id`, item.TargetID, item.Name, item.ProxyID); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM cliproxy_account_proxies WHERE upstream_id=? AND name=?`, item.SourceID, item.Name); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE cliproxy_migrations SET phase='done',message='',updated_at=? WHERE id=?`, nowStr(), item.ID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	source.Touch()
	target.Touch()
	_ = source.RefreshRuntime(ctx)
	_ = target.RefreshRuntime(ctx)
	invalidatePublicOverviews()
	return nil
}

func rollbackPoolMigration(item poolMigration, source, target *cliproxy.Manager) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = source.PatchCredential(ctx, item.Name, map[string]any{"disabled": item.Disabled})
	src, exists, err := credentialDocument(source, item.Name)
	if err != nil || !exists || !ownsMigration(src, item.ID) || migrationDisabled(src) != item.Disabled {
		return errors.New("源账号恢复待确认，未清理目标副本")
	}
	dst, exists, err := credentialDocument(target, item.Name)
	if err != nil {
		return errors.New("目标副本状态待核对")
	}
	if exists {
		if !ownsMigration(dst, item.ID) {
			return errors.New("目标账号已变化，未执行清理")
		}
		if !migrationDisabled(dst) {
			_ = target.PatchCredential(ctx, item.Name, map[string]any{"disabled": true})
			dst, exists, err = credentialDocument(target, item.Name)
			if err != nil || !exists || !migrationDisabled(dst) {
				return errors.New("目标副本停用待确认，未执行清理")
			}
		}
		_ = target.DeleteCredential(ctx, item.Name)
		_, exists, err = credentialDocument(target, item.Name)
		if err != nil || exists {
			return errors.New("源账号已恢复，停用的目标副本将在稍后清理")
		}
	}
	if _, err := db.Exec(`DELETE FROM cliproxy_account_proxies WHERE upstream_id=? AND name=?`, item.TargetID, item.Name); err != nil {
		return err
	}
	message := "迁移未完成，已核对并恢复源账号，目标副本已清理"
	if err := item.save("rolled_back", message); err != nil {
		return err
	}
	return errors.New(message)
}
