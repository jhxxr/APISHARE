package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/sergeymakinen/go-ico"
	"golang.org/x/image/webp"
)

const (
	defaultSiteTitle       = "API Share"
	defaultPublicSiteTitle = "API 分发"
	defaultFaviconPath     = "/public/static/brand/apishare-favicon-v2.png"
	defaultTouchIconPath   = "/public/static/brand/apishare-touch-icon-v2.png"
	defaultSiteLogoPath    = "/public/static/brand/apishare-icon-v2.png"
	maxSiteTitleRunes      = 40
	maxSiteIconBytes       = 512 << 10
	maxSiteIconPixels      = 16_000_000
	siteIconPath           = "/api/public/branding/icon"
)

// normalizeSiteTitle trims a submitted title and returns the empty string when
// it should fall back to the built-in branding. Control characters are refused
// so the title cannot break document titles, attributes or server logs.
func normalizeSiteTitle(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if utf8.RuneCountInString(value) > maxSiteTitleRunes {
		return "", fmt.Errorf("网站标题最多 %d 个字符", maxSiteTitleRunes)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("网站标题不能包含控制字符")
		}
	}
	return value, nil
}

func customSiteTitle() string {
	return strings.TrimSpace(getSettings().SiteTitle)
}

func effectiveSiteTitle() string {
	if title := customSiteTitle(); title != "" {
		return title
	}
	return defaultSiteTitle
}

func publicSiteTitle() string {
	if title := customSiteTitle(); title != "" {
		return title
	}
	return defaultPublicSiteTitle
}

func validSiteIconMIME(value string) bool {
	switch value {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/x-icon":
		return true
	}
	return false
}

// Decode the image before replacing the current icon. Check dimensions first
// so a small compressed upload cannot cause an unbounded pixel allocation.
func validateSiteIcon(data []byte, mime string) error {
	var decodeConfig func(io.Reader) (image.Config, error)
	var decode func(io.Reader) (image.Image, error)
	switch mime {
	case "image/png":
		decodeConfig, decode = png.DecodeConfig, png.Decode
	case "image/jpeg":
		decodeConfig, decode = jpeg.DecodeConfig, jpeg.Decode
	case "image/gif":
		decodeConfig, decode = gif.DecodeConfig, gif.Decode
	case "image/webp":
		decodeConfig, decode = webp.DecodeConfig, webp.Decode
	case "image/x-icon":
		// Call the ICO decoder directly to preserve bytes.Reader's seeking.
		decodeConfig, decode = ico.DecodeConfig, ico.Decode
	default:
		return fmt.Errorf("网站图标仅支持 PNG、JPEG、GIF、WebP 或 ICO 图片")
	}
	config, err := decodeConfig(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("网站图标已损坏或无法解码，请选择有效图片")
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > maxSiteIconPixels/config.Height {
		return fmt.Errorf("网站图标最多支持 1600 万像素")
	}
	if _, err := decode(bytes.NewReader(data)); err != nil {
		return fmt.Errorf("网站图标已损坏或无法解码，请选择有效图片")
	}
	return nil
}

// currentSiteIcon only reads the small metadata rows. The base64 payload is
// loaded when the icon is requested, not on every HTML page render.
func currentSiteIcon() (string, string) {
	updated, err := settingsGet("site_icon_updated")
	if err != nil {
		return "", ""
	}
	version, err := strconv.ParseInt(strings.TrimSpace(updated), 10, 64)
	if err != nil || version <= 0 {
		return "", ""
	}
	mime, err := settingsGet("site_icon_type")
	if err != nil || !validSiteIconMIME(mime) {
		return "", ""
	}
	return siteIconPath + "?v=" + strconv.FormatInt(version, 10), mime
}

// injectSiteBranding replaces the static placeholders in the embedded HTML
// pages. Doing this on the server keeps the document title, favicon and logo
// correct on first paint, including when JavaScript is unavailable.
func injectSiteBranding(page []byte) []byte {
	title, publicTitle := effectiveSiteTitle(), publicSiteTitle()
	favicon, touchIcon, logo := defaultFaviconPath, defaultTouchIconPath, defaultSiteLogoPath
	iconType := "image/png"
	if iconURL, mime := currentSiteIcon(); iconURL != "" {
		favicon, touchIcon, logo = iconURL, iconURL, iconURL
		iconType = mime
	}
	replacer := strings.NewReplacer(
		"{{SITE_TITLE}}", html.EscapeString(title),
		"{{PUBLIC_TITLE}}", html.EscapeString(publicTitle),
		"{{SITE_ICON_FAVICON}}", html.EscapeString(favicon),
		"{{SITE_ICON_TOUCH}}", html.EscapeString(touchIcon),
		"{{SITE_ICON_LOGO}}", html.EscapeString(logo),
		"{{SITE_ICON_TYPE}}", html.EscapeString(iconType),
	)
	return []byte(replacer.Replace(string(page)))
}

func registerSiteBrandingPublic(r *gin.Engine) {
	r.GET("/api/public/branding", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		title := effectiveSiteTitle()
		iconURL, iconType := currentSiteIcon()
		c.JSON(http.StatusOK, gin.H{
			"title":        title,
			"custom_title": customSiteTitle() != "",
			"icon_url":     iconURL,
			"icon_type":    iconType,
			"custom_icon":  iconURL != "",
		})
	})

	r.GET(siteIconPath, func(c *gin.Context) {
		updated, _ := settingsGet("site_icon_updated")
		version, err := strconv.ParseInt(strings.TrimSpace(updated), 10, 64)
		if err != nil || version <= 0 {
			c.Header("Cache-Control", "no-store")
			c.Header("X-Content-Type-Options", "nosniff")
			c.Status(http.StatusNotFound)
			return
		}
		encoded, _ := settingsGet("site_icon")
		mime, _ := settingsGet("site_icon_type")
		if encoded == "" || !validSiteIconMIME(mime) {
			c.Header("Cache-Control", "no-store")
			c.Header("X-Content-Type-Options", "nosniff")
			c.Status(http.StatusNotFound)
			return
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(data) == 0 || len(data) > maxSiteIconBytes {
			c.Header("Cache-Control", "no-store")
			c.Header("X-Content-Type-Options", "nosniff")
			c.Status(http.StatusNotFound)
			return
		}
		c.Header("Cache-Control", "no-store")
		if c.Query("v") == strconv.FormatInt(version, 10) {
			c.Header("Cache-Control", "public, max-age=31536000, immutable")
		}
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Data(http.StatusOK, mime, data)
	})
}

func registerSiteBrandingAdmin(auth *gin.RouterGroup) {
	upload := func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxSiteIconBytes+64<<10)
		if err := c.Request.ParseMultipartForm(maxSiteIconBytes); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "网站图标需以表单上传，且不能超过 512 KB"})
			return
		}
		defer c.Request.MultipartForm.RemoveAll()
		file, _, err := c.Request.FormFile("icon")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请选择网站图标文件"})
			return
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, maxSiteIconBytes+1))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无法读取网站图标"})
			return
		}
		if len(data) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "网站图标不能为空"})
			return
		}
		if len(data) > maxSiteIconBytes {
			c.JSON(http.StatusBadRequest, gin.H{"error": "网站图标不能超过 512 KB"})
			return
		}
		mime := http.DetectContentType(data)
		if err := validateSiteIcon(data, mime); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		version := strconv.FormatInt(time.Now().UnixNano(), 10)
		if err := settingsSetBatch(map[string]string{
			"site_icon":         base64.StdEncoding.EncodeToString(data),
			"site_icon_type":    mime,
			"site_icon_updated": version,
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存网站图标失败，请稍后重试"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"ok":               true,
			"site_icon_url":    siteIconPath + "?v=" + version,
			"site_icon_type":   mime,
			"site_icon_custom": true,
		})
	}
	auth.POST("/settings/icon", upload)

	auth.DELETE("/settings/icon", func(c *gin.Context) {
		if err := settingsSetBatch(map[string]string{
			"site_icon":         "",
			"site_icon_type":    "",
			"site_icon_updated": "",
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "恢复默认图标失败，请稍后重试"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"ok":               true,
			"site_icon_url":    "",
			"site_icon_type":   "",
			"site_icon_custom": false,
		})
	})
}
