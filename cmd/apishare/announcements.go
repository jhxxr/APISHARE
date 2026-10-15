package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

const publicAnnouncementLimit = 5

type Announcement struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Content     string `json:"content"`
	Published   bool   `json:"published"`
	Pinned      bool   `json:"pinned"`
	PublishedAt string `json:"published_at"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// Public announcements expose only information explicitly published by an admin.
type PublicAnnouncement struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Content     string `json:"content"`
	Pinned      bool   `json:"pinned"`
	PublishedAt string `json:"published_at"`
}

type announcementInput struct {
	Title     *string `json:"title"`
	Content   *string `json:"content"`
	Published *bool   `json:"published"`
	Pinned    *bool   `json:"pinned"`
}

func bindAnnouncement(c *gin.Context, create bool) (*announcementInput, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	var input announcementInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "公告数据无效或内容过长"})
		return nil, false
	}
	for _, field := range []struct {
		name, label string
		value       *string
		limit       int
	}{{"title", "标题", input.Title, 120}, {"content", "正文", input.Content, 10000}} {
		if field.value == nil && !create {
			continue
		}
		if field.value != nil {
			*field.value = strings.TrimSpace(*field.value)
		}
		if field.value == nil || *field.value == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请填写公告" + field.label, "field": field.name})
			return nil, false
		}
		if utf8.RuneCountInString(*field.value) > field.limit {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("公告%s最多 %d 个字符", field.label, field.limit), "field": field.name})
			return nil, false
		}
	}
	return &input, true
}

const announcementColumns = `id,title,content,published,pinned,published_at,created_at,updated_at`

func scanAnnouncement(row interface{ Scan(...any) error }) (*Announcement, error) {
	var item Announcement
	err := row.Scan(&item.ID, &item.Title, &item.Content, &item.Published, &item.Pinned, &item.PublishedAt, &item.CreatedAt, &item.UpdatedAt)
	return &item, err
}

func announcementID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的公告 ID"})
		return 0, false
	}
	return id, true
}

func announcementResult(c *gin.Context, item *Announcement, err error, status int) {
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "公告不存在，请刷新列表"})
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存公告失败，请稍后重试"})
	} else {
		c.JSON(status, item)
	}
}

func registerAnnouncementRoutes(auth *gin.RouterGroup) {
	auth.GET("/announcements", func(c *gin.Context) {
		rows, err := db.Query(`SELECT ` + announcementColumns + ` FROM announcements ORDER BY pinned DESC,created_at DESC,id DESC`)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "无法读取公告列表"})
			return
		}
		defer rows.Close()
		items := make([]Announcement, 0)
		for rows.Next() {
			item, err := scanAnnouncement(rows)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "无法读取公告列表"})
				return
			}
			items = append(items, *item)
		}
		if rows.Err() != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "无法读取公告列表"})
			return
		}
		c.JSON(http.StatusOK, items)
	})
	auth.POST("/announcements", func(c *gin.Context) {
		input, ok := bindAnnouncement(c, true)
		if !ok {
			return
		}
		published, pinned := input.Published != nil && *input.Published, input.Pinned != nil && *input.Pinned
		now, publishedAt := beijingNow().Format(time.RFC3339), ""
		if published {
			publishedAt = now
		}
		item, err := scanAnnouncement(db.QueryRow(`INSERT INTO announcements(title,content,published,pinned,published_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?) RETURNING `+announcementColumns,
			*input.Title, *input.Content, published, pinned, publishedAt, now, now))
		announcementResult(c, item, err, http.StatusCreated)
	})
	auth.PATCH("/announcements/:id", func(c *gin.Context) {
		id, ok := announcementID(c)
		if !ok {
			return
		}
		input, ok := bindAnnouncement(c, false)
		if !ok {
			return
		}
		if input.Title == nil && input.Content == nil && input.Published == nil && input.Pinned == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请选择需要修改的公告内容或状态"})
			return
		}
		now := beijingNow().Format(time.RFC3339)
		// Apply partial updates atomically; editing or pinning preserves publication time.
		item, err := scanAnnouncement(db.QueryRow(`UPDATE announcements SET title=COALESCE(?,title),content=COALESCE(?,content),pinned=COALESCE(?,pinned),
			published_at=CASE WHEN ?=0 THEN '' WHEN ?=1 AND published=0 THEN ? ELSE published_at END,
			published=COALESCE(?,published),updated_at=? WHERE id=? RETURNING `+announcementColumns,
			input.Title, input.Content, input.Pinned, input.Published, input.Published, now, input.Published, now, id))
		announcementResult(c, item, err, http.StatusOK)
	})
	auth.DELETE("/announcements/:id", func(c *gin.Context) {
		id, ok := announcementID(c)
		if !ok {
			return
		}
		result, err := db.Exec(`DELETE FROM announcements WHERE id=?`, id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "删除公告失败，请稍后重试"})
			return
		}
		count, err := result.RowsAffected()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "删除公告失败，请稍后重试"})
			return
		}
		if count == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "公告不存在，请刷新列表"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
}

func publicAnnouncementsHandler(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	rows, err := db.Query(`SELECT id,title,content,pinned,published_at FROM announcements WHERE published=1 ORDER BY pinned DESC,published_at DESC,id DESC LIMIT ?`, publicAnnouncementLimit)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "暂时无法读取公告，请稍后重试"})
		return
	}
	defer rows.Close()
	items := make([]PublicAnnouncement, 0)
	for rows.Next() {
		var item PublicAnnouncement
		if err := rows.Scan(&item.ID, &item.Title, &item.Content, &item.Pinned, &item.PublishedAt); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "暂时无法读取公告，请稍后重试"})
			return
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "暂时无法读取公告，请稍后重试"})
		return
	}
	c.JSON(http.StatusOK, items)
}
