package main

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

var adminConfigMu sync.RWMutex
var adminPathPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{2,63}$`)

type adminRequestPathKey struct{}

func normalizeAdminPath(value string) (string, error) {
	value = strings.Trim(strings.TrimSpace(value), "/")
	if !adminPathPattern.MatchString(value) {
		return "", fmt.Errorf("后台路径需为 3–64 位字母、数字、短横线或下划线，并以字母或数字开头")
	}
	switch strings.ToLower(value) {
	case "v1", "v1beta", "api", "public", "healthz", "static", "setup":
		return "", fmt.Errorf("此路径由系统使用，请选择其他后台路径")
	}
	return "/" + value, nil
}

func currentAdminPath() string {
	adminConfigMu.RLock()
	defer adminConfigMu.RUnlock()
	return adminPath
}

func requestAdminPath(c *gin.Context) string {
	if path, ok := c.Request.Context().Value(adminRequestPathKey{}).(string); ok {
		return path
	}
	return currentAdminPath()
}

func adminSetupRequired() bool {
	value, err := settingsGet("admin_setup_completed")
	return err == nil && value == "0"
}

// Persist security changes together, so a failed write never leaves half a setup.
func settingsSetBatch(values map[string]string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, value := range values {
		if _, err := tx.Exec(`INSERT INTO settings(k,v) VALUES(?,?)
			ON CONFLICT(k) DO UPDATE SET v=excluded.v`, key, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func bootstrapAdmin() error {
	adminConfigMu.Lock()
	defer adminConfigMu.Unlock()
	path, err := settingsGet("admin_path")
	if err != nil {
		return err
	}
	if path == "" {
		path = envOr("ADMIN_PATH", defaultAdminPath)
	}
	path, err = normalizeAdminPath(path)
	if err != nil {
		return err
	}
	hash, err := settingsGet("admin_pass_hash")
	if err != nil {
		return err
	}
	completed, err := settingsGet("admin_setup_completed")
	if err != nil {
		return err
	}
	values := map[string]string{}
	if hash == "" {
		// A new database stays unconfigured until the first-run form is saved.
		values["admin_setup_completed"] = "0"
	} else if completed == "" {
		// Existing installations can edit deployment settings without onboarding again.
		values["admin_path"] = path
		values["admin_setup_completed"] = "1"
	}
	secret, err := settingsGet("session_secret")
	if err != nil {
		return err
	}
	if secret == "" {
		values["session_secret"] = randHex(32)
	}
	if err := settingsSetBatch(values); err != nil {
		return err
	}
	adminPath = path
	return nil
}

func registerSetupRoutes(r *gin.Engine) {
	sub, _ := fs.Sub(webFS, "web")
	setup := r.Group("/setup", func(c *gin.Context) { c.Header("Cache-Control", "no-store") })
	setup.GET("", func(c *gin.Context) { c.Redirect(http.StatusTemporaryRedirect, "/setup/") })
	setup.GET("/", func(c *gin.Context) {
		if !adminSetupRequired() {
			c.Redirect(http.StatusTemporaryRedirect, "/")
			return
		}
		serveAdminFile(c, sub, "setup.html")
	})
	// Only the assets needed by onboarding are exposed under this public prefix.
	for _, name := range []string{"style.css", "deployment.js", "setup.js"} {
		setup.GET("/static/"+name, func(c *gin.Context) {
			if !adminSetupRequired() {
				c.Status(http.StatusNotFound)
				return
			}
			serveAdminFile(c, sub, name)
		})
	}
	setup.GET("/api/deployment", func(c *gin.Context) {
		adminConfigMu.RLock()
		defer adminConfigMu.RUnlock()
		if !adminSetupRequired() {
			c.Status(http.StatusNotFound)
			return
		}
		c.JSON(http.StatusOK, gin.H{"admin_path": adminPath, "setup_required": true})
	})
	setup.PUT("/api/deployment", saveDeploymentConfig(true))
}

// A private router lets the configured prefix change without mutating Gin's route tree.
func registerAdmin(r *gin.Engine) {
	admin := gin.New()
	admin.Use(gin.Recovery(), func(c *gin.Context) { c.Header("Cache-Control", "no-store") })
	admin.SetTrustedProxies(nil)
	registerAdminRoutes(admin)
	r.Use(func(c *gin.Context) {
		prefix := currentAdminPath()
		if c.Request.URL.Path == prefix && (c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead) {
			destination := prefix + "/"
			if c.Request.URL.RawQuery != "" {
				destination += "?" + c.Request.URL.RawQuery
			}
			c.Redirect(http.StatusMovedPermanently, destination)
			c.Abort()
			return
		}
		if !strings.HasPrefix(c.Request.URL.Path, prefix+"/") {
			return
		}
		request := c.Request.Clone(context.WithValue(c.Request.Context(), adminRequestPathKey{}, prefix))
		request.URL.Path = strings.TrimPrefix(request.URL.Path, prefix)
		request.URL.RawPath = ""
		admin.ServeHTTP(c.Writer, request)
		c.Abort()
	})
}

func deploymentError(c *gin.Context, status int, field, message string) {
	c.JSON(status, gin.H{"error": message, "field": field})
}

func registerDeploymentRoutes(auth *gin.RouterGroup) {
	auth.GET("/deployment", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"admin_path": currentAdminPath(), "setup_required": adminSetupRequired()})
	})
	auth.PUT("/deployment", saveDeploymentConfig(false))
}

func saveDeploymentConfig(initialOnly bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		adminConfigMu.Lock()
		defer adminConfigMu.Unlock()
		// Check under the same lock as the write so first-run setup can succeed only once.
		initial := adminSetupRequired()
		if initialOnly && !initial {
			c.Status(http.StatusNotFound)
			return
		}
		if !initialOnly && !validAdminToken(adminRequestToken(c)) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		var req struct {
			CurrentPassword string `json:"current_password"`
			NewPassword     string `json:"new_password"`
			ConfirmPassword string `json:"confirm_password"`
			AdminPath       string `json:"admin_path"`
		}
		if c.ShouldBindJSON(&req) != nil {
			deploymentError(c, http.StatusBadRequest, "", "无法读取配置，请检查填写内容")
			return
		}
		path, err := normalizeAdminPath(req.AdminPath)
		if err != nil {
			deploymentError(c, http.StatusBadRequest, "admin_path", err.Error())
			return
		}
		if req.NewPassword != "" {
			if utf8.RuneCountInString(req.NewPassword) < 12 || len(req.NewPassword) > 72 || strings.TrimSpace(req.NewPassword) == "" {
				deploymentError(c, http.StatusBadRequest, "new_password", "新密码至少 12 个字符，最多 72 字节")
				return
			}
			if req.NewPassword != req.ConfirmPassword {
				deploymentError(c, http.StatusBadRequest, "confirm_password", "两次输入的密码不一致")
				return
			}
		} else if req.ConfirmPassword != "" {
			deploymentError(c, http.StatusBadRequest, "new_password", "请先填写新管理员密码")
			return
		}
		if !initialOnly {
			hash, err := settingsGet("admin_pass_hash")
			if err != nil {
				deploymentError(c, http.StatusInternalServerError, "", "暂时无法读取管理员配置，请稍后重试")
				return
			}
			if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.CurrentPassword)) != nil {
				deploymentError(c, http.StatusBadRequest, "current_password", "当前管理员密码不正确")
				return
			}
		}
		if initial && req.NewPassword == "" {
			deploymentError(c, http.StatusBadRequest, "new_password", "首次配置请设置新的管理员密码")
			return
		}
		values := map[string]string{"admin_path": path, "admin_setup_completed": "1", "session_secret": randHex(32)}
		if req.NewPassword != "" {
			encoded, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
			if err != nil {
				deploymentError(c, http.StatusInternalServerError, "", "密码保存失败，请稍后重试")
				return
			}
			values["admin_pass_hash"] = string(encoded)
		}
		if err := settingsSetBatch(values); err != nil {
			deploymentError(c, http.StatusInternalServerError, "", "配置保存失败，请稍后重试")
			return
		}
		previousPath := adminPath
		adminPath = path
		tok := makeAdminToken()
		if previousPath != path {
			setAdminCookie(c, "", -1, previousPath)
		}
		setAdminCookie(c, tok, 7*24*3600, path)
		c.JSON(http.StatusOK, gin.H{"ok": true, "admin_path": path, "setup_required": false, "token": tok})
	}
}
