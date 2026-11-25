package main

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// Admin API for the failure diagnostics console. Records are produced by the
// relay and stored as paired JSON files next to the database.
func registerFailureLogAdminRoutes(auth *gin.RouterGroup) {
	auth.GET("/failure-logs", func(c *gin.Context) {
		offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
		limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
		if offset < 0 {
			offset = 0
		}
		if limit <= 0 || limit > 200 {
			limit = 50
		}
		// Reading keeps working after recording is disabled, so browse the
		// directory even when there is no live write target.
		store := failureLogStoreForReading()
		items, total := store.list(offset, limit)
		settings := readFailureLogSettings()
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{
			"items": items, "total": total,
			"enabled":  settings.Enabled && store != nil,
			"settings": failureLogSettingsView(settings, store != nil),
		})
	})

	auth.GET("/failure-logs/settings", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, failureLogSettingsView(readFailureLogSettings(), activeFailureLogs() != nil))
	})

	auth.PUT("/failure-logs/settings", func(c *gin.Context) {
		var incoming struct {
			Enabled     *bool `json:"enabled"`
			MaxMB       *int  `json:"max_mb"`
			EntryKiB    *int  `json:"entry_kib"`
			FullContent *bool `json:"full_content"`
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
		if err := c.ShouldBindJSON(&incoming); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的失败诊断设置"})
			return
		}
		settings := readFailureLogSettings()
		if incoming.MaxMB != nil {
			if *incoming.MaxMB < 1 || *incoming.MaxMB > failureLogMaxMB {
				c.JSON(http.StatusBadRequest, gin.H{"error": "最多保存容量需在 1 到 16384 MB 之间"})
				return
			}
			settings.MaxMB = *incoming.MaxMB
		}
		if incoming.EntryKiB != nil {
			if *incoming.EntryKiB < 1 || *incoming.EntryKiB > 1024 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "单条记录上限需在 1 到 1024 KiB 之间"})
				return
			}
			settings.EntryKiB = *incoming.EntryKiB
		}
		if incoming.Enabled != nil {
			settings.Enabled = *incoming.Enabled
		}
		if incoming.FullContent != nil {
			settings.FullContent = *incoming.FullContent
		}
		failureLogSettingsMu.Lock()
		defer failureLogSettingsMu.Unlock()
		if err := saveFailureLogSettings(settings); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "无法保存失败诊断设置"})
			return
		}
		if err := applyFailureLogSettings(settings); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "无法应用失败诊断设置：" + err.Error()})
			return
		}
		c.Header("Cache-Control", "no-store")
		view := failureLogSettingsView(settings, activeFailureLogs() != nil)
		view["ok"] = true
		c.JSON(http.StatusOK, view)
	})

	auth.GET("/failure-logs/:name", func(c *gin.Context) {
		name := c.Param("name")
		entry, err := failureLogStoreForReading().entry(name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				c.JSON(http.StatusNotFound, gin.H{"error": "该失败记录不存在或已被清理"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "无法读取失败记录"})
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, failureLogDetailView{
			Name: name, Time: entry.Time, RequestID: entry.RequestID, Duration: entry.Duration,
			Status: entry.Status, Request: entry.Request, Upstream: entry.Upstream, Truncated: entry.Truncated,
		})
	})

	auth.DELETE("/failure-logs/:name", func(c *gin.Context) {
		if err := failureLogStoreForReading().remove(c.Param("name")); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "该失败记录不存在或已被清理"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	auth.DELETE("/failure-logs", func(c *gin.Context) {
		removed := failureLogStoreForReading().clear()
		c.JSON(http.StatusOK, gin.H{"ok": true, "removed": removed})
	})
}

// failureLogDetailView is the stored entry plus the file name the console needs
// to address it (delete, refresh). The name is not part of the record body.
type failureLogDetailView struct {
	Name      string          `json:"name"`
	Time      time.Time       `json:"time"`
	RequestID string          `json:"request_id"`
	Duration  int64           `json:"duration_ms"`
	Status    int             `json:"gateway_status"`
	Request   failureRequest  `json:"request"`
	Upstream  failureResponse `json:"upstream"`
	Truncated bool            `json:"truncated,omitempty"`
}

// failureLogSettingsView keeps one shape for every settings response so the
// console never has to guess where `enabled` lives.
func failureLogSettingsView(settings failureLogSettings, active bool) gin.H {
	return gin.H{"enabled": settings.Enabled && active, "max_mb": settings.MaxMB,
		"entry_kib": settings.EntryKiB, "full_content": settings.FullContent}
}
