package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

type poolJobImport struct {
	Name            string          `json:"name"`
	Credential      json.RawMessage `json:"credential,omitempty"`
	ProxyID         int64           `json:"proxy_id"`
	ValidationError string          `json:"validation_error,omitempty"`
}
type poolJobItem struct {
	Position     int    `json:"position"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	Message      string `json:"message,omitempty"`
	ExistingName string `json:"existing_name,omitempty"`
	Confirmed    bool   `json:"-"`
}
type poolJob struct {
	ID        string        `json:"id"`
	PoolID    int64         `json:"pool_id"`
	Action    string        `json:"action"`
	ProxyID   int64         `json:"proxy_id"`
	TargetID  int64         `json:"target_pool_id"`
	Status    string        `json:"status"`
	Canceled  bool          `json:"cancel_requested"`
	CreatedAt string        `json:"created_at"`
	UpdatedAt string        `json:"updated_at"`
	Items     []poolJobItem `json:"items"`
}

func readPoolJob(id string) (poolJob, error) {
	var job poolJob
	err := db.QueryRow(`SELECT id,pool_id,action,proxy_id,target_id,status,canceled,created_at,updated_at FROM cliproxy_jobs WHERE id=?`, id).
		Scan(&job.ID, &job.PoolID, &job.Action, &job.ProxyID, &job.TargetID, &job.Status, &job.Canceled, &job.CreatedAt, &job.UpdatedAt)
	if err != nil {
		return job, err
	}
	rows, err := db.Query(`SELECT position,name,status,message,existing_name,confirmed FROM cliproxy_job_items WHERE job_id=? ORDER BY position`, id)
	if err != nil {
		return job, err
	}
	defer rows.Close()
	job.Items = make([]poolJobItem, 0)
	for rows.Next() {
		var item poolJobItem
		if err := rows.Scan(&item.Position, &item.Name, &item.Status, &item.Message, &item.ExistingName, &item.Confirmed); err != nil {
			return job, err
		}
		job.Items = append(job.Items, item)
	}
	return job, rows.Err()
}

var poolJobRunner = struct {
	sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	database    *sql.DB
	running     map[string]bool
	activePools map[int64]bool
	wg          sync.WaitGroup
}{}

func stopPoolJobs() {
	poolJobRunner.Lock()
	if poolJobRunner.cancel != nil {
		poolJobRunner.cancel()
	}
	poolJobRunner.Unlock()
	poolJobRunner.wg.Wait()
	poolJobRunner.Lock()
	poolJobRunner.ctx = nil
	poolJobRunner.cancel = nil
	poolJobRunner.database = nil
	poolJobRunner.running = nil
	poolJobRunner.activePools = nil
	poolJobRunner.Unlock()
}

func resumePoolJobs(parent context.Context) {
	poolJobRunner.Lock()
	if poolJobRunner.ctx != nil {
		poolJobRunner.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	poolJobRunner.ctx = ctx
	poolJobRunner.cancel = cancel
	poolJobRunner.database = db
	poolJobRunner.running = make(map[string]bool)
	poolJobRunner.activePools = make(map[int64]bool)
	poolJobRunner.wg.Add(1)
	poolJobRunner.Unlock()
	// A previously running refresh may already have reached the provider.
	// Do not repeat it automatically after a gateway restart.
	_, _ = db.Exec(`UPDATE cliproxy_job_items SET status='error',message='网关重启时刷新结果未确认，可手动重试' WHERE status='running' AND job_id IN(SELECT id FROM cliproxy_jobs WHERE action='refresh')`)
	_, _ = db.Exec(`UPDATE cliproxy_job_items SET status='pending' WHERE status='running'`)
	go func() {
		defer poolJobRunner.wg.Done()
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			dispatchPoolJobs()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
func dispatchPoolJobs() {
	rows, err := db.Query(`SELECT id FROM cliproxy_jobs WHERE status IN('queued','running','recovering') ORDER BY created_at LIMIT 50`)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		launchPoolJob(id)
	}
	// Legacy synchronous migrations also get crash/timeout reconciliation.
	rows, err = db.Query(`SELECT id FROM cliproxy_migrations WHERE phase NOT IN('done','rolled_back') LIMIT 100`)
	if err != nil {
		return
	}
	var migrations []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			migrations = append(migrations, id)
		}
	}
	rows.Close()
	for _, id := range migrations {
		poolJobRunner.Lock()
		ctx := poolJobRunner.ctx
		poolJobRunner.Unlock()
		if ctx == nil || ctx.Err() != nil {
			return
		}
		proxyConfigMu.Lock()
		item, err := readPoolMigration(id)
		if err == nil && item.Phase != "done" && item.Phase != "rolled_back" {
			_ = reconcilePoolMigration(item)
		}
		proxyConfigMu.Unlock()
	}
}
func launchPoolJob(id string) {
	job, err := readPoolJob(id)
	if err != nil {
		return
	}
	poolJobRunner.Lock()
	if poolJobRunner.ctx == nil || poolJobRunner.ctx.Err() != nil || poolJobRunner.running[id] || poolJobRunner.activePools[job.PoolID] {
		poolJobRunner.Unlock()
		return
	}
	poolJobRunner.running[id] = true
	poolJobRunner.activePools[job.PoolID] = true
	ctx := poolJobRunner.ctx
	poolJobRunner.wg.Add(1)
	poolJobRunner.Unlock()
	go func() {
		defer poolJobRunner.wg.Done()
		defer func() {
			poolJobRunner.Lock()
			delete(poolJobRunner.running, id)
			delete(poolJobRunner.activePools, job.PoolID)
			poolJobRunner.Unlock()
		}()
		runPoolJob(ctx, id)
	}()
}
func updatePoolJobItem(id string, item poolJobItem) error {
	_, err := db.Exec(`UPDATE cliproxy_job_items SET status=?,message=?,existing_name=? WHERE job_id=? AND position=?`, item.Status, item.Message, item.ExistingName, id, item.Position)
	return err
}
func runPoolJob(ctx context.Context, id string) {
	job, err := readPoolJob(id)
	if err != nil {
		return
	}
	_, _ = db.Exec(`UPDATE cliproxy_jobs SET status='running',updated_at=? WHERE id=?`, nowStr(), id)
	var imports []poolJobImport
	if job.Action == "import" {
		manager, e := cliProxyPool(job.PoolID)
		if e == nil {
			var raw []byte
			raw, e = manager.ReadJobInput(id)
			if e == nil {
				e = json.Unmarshal(raw, &imports)
			}
		}
		if e != nil {
			for _, item := range job.Items {
				if item.Status == "pending" || item.Status == "running" {
					item.Status = "error"
					item.Message = "导入任务文件不可读取，请重新上传"
					_ = updatePoolJobItem(id, item)
				}
			}
		}
	}
	for _, item := range job.Items {
		if ctx.Err() != nil {
			return
		}
		if item.Status != "pending" && item.Status != "running" && item.Status != "recovering" {
			continue
		}
		var canceled bool
		if db.QueryRow(`SELECT canceled FROM cliproxy_jobs WHERE id=?`, id).Scan(&canceled) != nil {
			return
		}
		if canceled && item.Status != "recovering" {
			item.Status = "canceled"
			item.Message = "尚未执行，已取消"
			_ = updatePoolJobItem(id, item)
			continue
		}
		item.Status = "running"
		item.Message = ""
		_ = updatePoolJobItem(id, item)
		opCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		proxyConfigMu.Lock()
		err := executePoolJobItem(opCtx, job, item, imports)
		proxyConfigMu.Unlock()
		cancel()
		item.Status = "ok"
		if err != nil {
			item.Status = "error"
			item.Message = err.Error()
			var duplicate *duplicatePoolCredential
			if errors.As(err, &duplicate) {
				item.Status = "confirmation"
				item.Message = "当前池已有此账号，确认后更新凭据并保留原配置"
				item.ExistingName = duplicate.Name
			}
			if job.Action == "move" {
				migration, e := readPoolMigration(poolJobMigrationID(job.ID, item.Position))
				if e == nil && migration.Phase != "done" && migration.Phase != "rolled_back" {
					item.Status = "recovering"
					item.Message = "迁移状态正在核对：" + err.Error()
				}
			}
		}
		if ctx.Err() != nil {
			return
		} // Keep running state for restart reconciliation.
		_ = updatePoolJobItem(id, item)
		_, _ = db.Exec(`UPDATE cliproxy_jobs SET updated_at=? WHERE id=?`, nowStr(), id)
	}
	finishPoolJob(id)
}
func poolJobMigrationID(id string, position int) string { return id + "-" + strconv.Itoa(position) }
func executePoolJobItem(ctx context.Context, job poolJob, item poolJobItem, imports []poolJobImport) error {
	manager, err := cliProxyPool(job.PoolID)
	if err != nil {
		return errors.New("当前账号池已不存在")
	}
	if job.Action == "import" {
		if item.Position >= len(imports) {
			return errors.New("导入任务文件不可读取")
		}
		input := imports[item.Position]
		if !validCoreCredentialName(input.Name) {
			return errors.New("请使用有效的 .json 文件名")
		}
		if len(input.Credential) > 1<<20 {
			return errors.New("单个凭据文件不能超过 1 MB")
		}
		if input.ProxyID < -1 {
			return errors.New("请选择有效的代理")
		}
		return importPoolCredential(ctx, manager, job.PoolID, input.Name, input.Credential, input.ProxyID, item.Confirmed)
	}
	if job.Action == "move" {
		target, err := cliProxyPool(job.TargetID)
		if err != nil {
			return errors.New("目标账号池已不存在")
		}
		return movePoolCredentialWithID(ctx, manager, job.PoolID, target, job.TargetID, item.Name, poolJobMigrationID(job.ID, item.Position))
	}
	files, err := manager.ReadCredentials()
	if err != nil {
		return errors.New("无法读取账号凭据")
	}
	if !validCoreCredentialName(item.Name) || files[item.Name] == nil {
		return errors.New("账号不属于当前池")
	}
	switch job.Action {
	case "enable", "disable":
		err = manager.PatchCredential(ctx, item.Name, map[string]any{"disabled": job.Action == "disable"})
	case "refresh":
		err = manager.Client().Do(ctx, "POST", "/credentials/refresh", map[string]any{"name": item.Name}, nil)
	case "proxy":
		err = saveAccountProxy(ctx, manager, job.PoolID, item.Name, job.ProxyID)
	default:
		return errors.New("不支持的任务")
	}
	manager.Touch()
	_ = manager.RefreshRuntime(ctx)
	invalidatePublicOverviews()
	return err
}
func finishPoolJob(id string) {
	job, err := readPoolJob(id)
	if err != nil {
		return
	}
	status := "completed"
	hasErrors, hasConfirm, hasPending := false, false, false
	for _, item := range job.Items {
		hasConfirm = hasConfirm || item.Status == "confirmation"
		hasErrors = hasErrors || item.Status == "error"
		hasPending = hasPending || item.Status == "pending" || item.Status == "running" || item.Status == "recovering"
	}
	if hasErrors {
		status = "completed_with_errors"
	}
	if hasConfirm {
		status = "needs_confirmation"
	}
	if hasPending {
		status = "recovering"
	}
	if job.Canceled && status == "completed" {
		status = "canceled"
	}
	_, _ = db.Exec(`UPDATE cliproxy_jobs SET status=?,updated_at=? WHERE id=?`, status, nowStr(), id)
	if status == "completed" {
		if manager, err := cliProxyPool(job.PoolID); err == nil {
			_ = manager.RemoveJobInput(id)
		}
	}
}

func registerCLIProxyJobRoutes(group *gin.RouterGroup) {
	group.GET("/jobs", func(c *gin.Context) {
		rows, err := db.Query(`SELECT id FROM cliproxy_jobs WHERE pool_id=? ORDER BY CASE WHEN status IN('queued','running','recovering','needs_confirmation') THEN 0 ELSE 1 END,rowid DESC LIMIT 20`, requestCLIProxyPoolID(c))
		if err != nil {
			c.JSON(500, gin.H{"error": "无法读取任务列表"})
			return
		}
		var ids []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		jobs := make([]poolJob, 0, len(ids))
		for _, id := range ids {
			if job, err := readPoolJob(id); err == nil {
				jobs = append(jobs, job)
			}
		}
		c.JSON(200, gin.H{"jobs": jobs})
	})
	requireJob := func(c *gin.Context) (poolJob, bool) {
		job, err := readPoolJob(c.Param("job"))
		if err != nil || job.PoolID != requestCLIProxyPoolID(c) {
			c.JSON(404, gin.H{"error": "任务不属于当前账号池"})
			return job, false
		}
		return job, true
	}
	group.GET("/jobs/:job", func(c *gin.Context) {
		if job, ok := requireJob(c); ok {
			c.JSON(200, job)
		}
	})
	group.POST("/jobs", func(c *gin.Context) {
		var request struct {
			poolAccountOperation
			Items []poolJobImport `json:"items"`
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 24<<20)
		if c.ShouldBindJSON(&request) != nil {
			c.JSON(400, gin.H{"error": "任务参数无效，导入总大小不可超过 24 MB"})
			return
		}
		if request.Action != "import" && len(request.Items) > 0 {
			c.JSON(400, gin.H{"error": "任务参数无效"})
			return
		}
		switch request.Action {
		case "enable", "disable", "refresh", "proxy", "move", "import":
		default:
			c.JSON(400, gin.H{"error": "不支持的批量操作"})
			return
		}
		if request.Action == "import" {
			request.Names = nil
			for _, item := range request.Items {
				request.Names = append(request.Names, item.Name)
			}
		}
		if len(request.Names) == 0 || len(request.Names) > 100 {
			c.JSON(400, gin.H{"error": "每次请选择 1–100 项"})
			return
		}
		proxyConfigMu.Lock()
		defer proxyConfigMu.Unlock()
		poolID := requestCLIProxyPoolID(c)
		if poolID == 0 {
			c.JSON(400, gin.H{"error": "请在具体账号池中创建任务"})
			return
		}
		var active int
		if db.QueryRow(`SELECT count(*) FROM cliproxy_jobs WHERE pool_id=? AND status IN('queued','running','recovering','needs_confirmation')`, poolID).Scan(&active) != nil || active >= 5 {
			c.JSON(409, gin.H{"error": "当前池任务队列已满，请等待现有任务完成"})
			return
		}
		if request.Action == "proxy" && (request.ProxyID < -1 || request.ProxyID > 0 && validateUpstreamProxy(request.ProxyID) != nil) {
			c.JSON(400, gin.H{"error": "请选择有效代理"})
			return
		}
		if request.Action == "move" {
			if request.TargetPoolID == poolID {
				c.JSON(400, gin.H{"error": "请选择其他账号池"})
				return
			}
			if _, err := cliProxyPool(request.TargetPoolID); err != nil {
				c.JSON(400, gin.H{"error": "目标池不存在"})
				return
			}
		}
		id := randHex(16)
		if request.Action == "import" {
			raw, _ := json.Marshal(request.Items)
			if err := requestCLIProxyCore(c).SaveJobInput(id, raw); err != nil {
				c.JSON(500, gin.H{"error": "导入文件保存失败"})
				return
			}
		}
		tx, err := db.Begin()
		if err != nil {
			c.JSON(500, gin.H{"error": "任务保存失败"})
			return
		}
		defer tx.Rollback()
		_, err = tx.Exec(`INSERT INTO cliproxy_jobs(id,pool_id,action,proxy_id,target_id,status,created_at,updated_at) VALUES(?,?,?,?,?,'queued',?,?)`, id, poolID, request.Action, request.ProxyID, request.TargetPoolID, nowStr(), nowStr())
		if err == nil {
			for index, name := range request.Names {
				status, message := "pending", ""
				if !validCoreCredentialName(name) {
					status = "error"
					message = "凭据文件名无效"
				}
				if request.Action == "import" && request.Items[index].ValidationError != "" {
					status = "error"
					message = "文件格式或大小无效，请重新选择文件"
				}
				if request.Action == "import" && len(request.Items[index].Credential) > 1<<20 {
					status = "error"
					message = "单个凭据文件不能超过 1 MB"
				}
				_, err = tx.Exec(`INSERT INTO cliproxy_job_items(job_id,position,name,status,message) VALUES(?,?,?,?,?)`, id, index, name, status, message)
				if err != nil {
					break
				}
			}
		}
		if err != nil || tx.Commit() != nil {
			_ = requestCLIProxyCore(c).RemoveJobInput(id)
			c.JSON(500, gin.H{"error": "任务保存失败"})
			return
		}
		resumePoolJobs(context.Background())
		job, _ := readPoolJob(id)
		c.JSON(202, job)
		launchPoolJob(id)
	})
	group.POST("/jobs/:job/cancel", func(c *gin.Context) {
		job, ok := requireJob(c)
		if !ok {
			return
		}
		_, err := db.Exec(`UPDATE cliproxy_jobs SET canceled=1,updated_at=? WHERE id=?`, nowStr(), job.ID)
		if err != nil {
			c.JSON(500, gin.H{"error": "取消请求保存失败"})
			return
		}
		if _, err = db.Exec(`UPDATE cliproxy_job_items SET status='canceled',message='尚未执行，已取消' WHERE job_id=? AND status IN('pending','confirmation')`, job.ID); err != nil {
			c.JSON(500, gin.H{"error": "取消结果保存失败"})
			return
		}
		c.JSON(200, gin.H{"ok": true})
		launchPoolJob(job.ID)
	})
	group.POST("/jobs/:job/retry", func(c *gin.Context) {
		job, ok := requireJob(c)
		if !ok {
			return
		}
		poolJobRunner.Lock()
		busy := poolJobRunner.running[job.ID]
		poolJobRunner.Unlock()
		if busy || job.Status == "queued" || job.Status == "running" || job.Status == "recovering" {
			c.JSON(409, gin.H{"error": "任务仍在执行或恢复中"})
			return
		}
		var request struct {
			Confirm []int `json:"confirm"`
		}
		if c.ShouldBindJSON(&request) != nil {
			c.JSON(400, gin.H{"error": "重试参数无效"})
			return
		}
		confirmed := make(map[int]bool)
		for _, position := range request.Confirm {
			confirmed[position] = true
		}
		if job.Action == "import" {
			manager, e := cliProxyPool(job.PoolID)
			if e != nil {
				c.JSON(400, gin.H{"error": "当前池不可读取"})
				return
			}
			if _, e = manager.ReadJobInput(job.ID); e != nil {
				c.JSON(400, gin.H{"error": "任务输入已清理，请重新上传"})
				return
			}
		}
		tx, err := db.Begin()
		if err != nil {
			c.JSON(500, gin.H{"error": "无法保存重试"})
			return
		}
		defer tx.Rollback()
		count := 0
		for _, item := range job.Items {
			if item.Status == "confirmation" && !confirmed[item.Position] {
				continue
			}
			if item.Status != "error" && item.Status != "canceled" && item.Status != "confirmation" {
				continue
			}
			if job.Action == "move" {
				// A completed rollback can be retried as a fresh operation. Its
				// private ownership marker is replaced on the next source patch.
				if _, err = tx.Exec(`DELETE FROM cliproxy_migrations WHERE id=? AND phase='rolled_back'`, poolJobMigrationID(job.ID, item.Position)); err != nil {
					break
				}
			}
			_, err = tx.Exec(`UPDATE cliproxy_job_items SET status='pending',message='',confirmed=? WHERE job_id=? AND position=?`, confirmed[item.Position] || item.Confirmed, job.ID, item.Position)
			if err != nil {
				break
			}
			count++
		}
		if err != nil || count == 0 {
			c.JSON(400, gin.H{"error": "没有可重试项；无效文件请重新上传"})
			return
		}
		if _, err = tx.Exec(`UPDATE cliproxy_jobs SET status='queued',canceled=0,updated_at=? WHERE id=?`, nowStr(), job.ID); err != nil {
			c.JSON(500, gin.H{"error": "无法保存重试"})
			return
		}
		if err = tx.Commit(); err != nil {
			c.JSON(500, gin.H{"error": "无法保存重试"})
			return
		}
		c.JSON(202, gin.H{"ok": true})
		launchPoolJob(job.ID)
	})
}
