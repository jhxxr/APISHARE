package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/sergeymakinen/go-ico"
)

func siteBrandingTestRouter(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	r := deploymentTestRouter(t)
	previous := getSettings()
	t.Cleanup(func() {
		settingsMu.Lock()
		settingsCache = previous
		settingsMu.Unlock()
	})
	if err := settingsSet("admin_setup_completed", "1"); err != nil {
		t.Fatal(err)
	}
	return r, makeAdminToken()
}

func siteBrandingPage(t *testing.T, r http.Handler, path string) string {
	t.Helper()
	w := deploymentRequest(r, "GET", path, "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("page %s returned %d: %s", path, w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "{{") {
		t.Fatalf("page %s still contains an unresolved branding placeholder", path)
	}
	return body
}

func siteBrandingMultipart(t *testing.T, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("icon", "icon.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, writer.FormDataContentType()
}

func siteBrandingIconRequest(t *testing.T, r http.Handler, token string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := siteBrandingMultipart(t, data)
	req := httptest.NewRequest("POST", currentAdminPath()+"/api/settings/icon", body)
	req.Header.Set("Content-Type", contentType)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestSiteBrandingDefaultsAcrossPages(t *testing.T) {
	r := deploymentTestRouter(t)

	setup := siteBrandingPage(t, r, "/setup/")
	for _, want := range []string{
		`<title>API Share · 首次部署配置</title>`,
		`data-site-title="API Share"`,
		`/public/static/brand/apishare-favicon-v2.png`,
		`/public/static/brand/apishare-icon-v2.png`,
		`/public/static/brand/apishare-touch-icon-v2.png`,
		`<span data-site-brand-name>API Share</span>`,
	} {
		if !strings.Contains(setup, want) {
			t.Fatalf("default setup page is missing %q", want)
		}
	}

	branding := deploymentJSON(t, deploymentRequest(r, "GET", "/api/public/branding", "", ""), 200)
	if branding["title"] != "API Share" || branding["custom_title"] != false || branding["custom_icon"] != false || branding["icon_url"] != "" {
		t.Fatalf("default public branding = %+v", branding)
	}
	if w := deploymentRequest(r, "GET", "/api/public/branding", "", ""); w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("branding metadata must not be cached")
	}

	result := deploymentJSON(t, deploymentRequest(r, "PUT", "/setup/api/deployment", "",
		`{"new_password":"brand-test-password-123","confirm_password":"brand-test-password-123","admin_path":"brand-console"}`), 200)
	adminPage := result["admin_path"].(string) + "/"

	public := siteBrandingPage(t, r, "/")
	for _, want := range []string{
		`<title>API 分发 · 用量与模型状态</title>`,
		`data-site-title="API Share"`,
		`aria-label="API Share 首页"`,
		`/public/static/brand/apishare-favicon-v2.png`,
		`/public/static/brand/apishare-icon-v2.png`,
		`<span data-site-brand-name>API Share</span>`,
	} {
		if !strings.Contains(public, want) {
			t.Fatalf("default public page is missing %q", want)
		}
	}
	admin := siteBrandingPage(t, r, adminPage)
	for _, want := range []string{
		`<title>API Share · 工作空间</title>`,
		`data-site-title="API Share"`,
		`title="API Share"`,
		`/public/static/brand/apishare-favicon-v2.png`,
		`/public/static/brand/apishare-icon-v2.png`,
		`/public/static/brand/apishare-touch-icon-v2.png`,
		`<span class="brand-name" data-site-brand-name>API Share</span>`,
	} {
		if !strings.Contains(admin, want) {
			t.Fatalf("default admin page is missing %q", want)
		}
	}
	if w := deploymentRequest(r, "POST", strings.TrimSuffix(adminPage, "/")+"/api/settings/icon", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated icon upload returned %d", w.Code)
	}
}

func TestSiteTitleSettingsAndHTMLInjection(t *testing.T) {
	r, token := siteBrandingTestRouter(t)

	deploymentJSON(t, deploymentRequest(r, "PUT", "/admin/api/settings", token, `{"site_title":"  星河网关  "}`), 200)
	settings := deploymentJSON(t, deploymentRequest(r, "GET", "/admin/api/settings", token, ""), 200)
	if settings["site_title"] != "星河网关" || settings["site_icon_custom"] != false {
		t.Fatalf("saved title = %+v", settings)
	}
	if _, ok := settings["site_icon"]; ok {
		t.Fatal("settings endpoint must not expose the icon payload")
	}

	public := siteBrandingPage(t, r, "/")
	for _, want := range []string{
		`<title>星河网关 · 用量与模型状态</title>`,
		`aria-label="星河网关 首页"`,
		`<span data-site-brand-name>星河网关</span>`,
	} {
		if !strings.Contains(public, want) {
			t.Fatalf("custom public page is missing %q", want)
		}
	}
	admin := siteBrandingPage(t, r, "/admin/")
	for _, want := range []string{
		`<title>星河网关 · 工作空间</title>`,
		`data-site-title="星河网关"`,
		`title="星河网关"`,
		`<span class="brand-name" data-site-brand-name>星河网关</span>`,
		`<span><span data-site-brand-name>星河网关</span> · Gateway Console</span>`,
		`<span><span data-site-brand-name>星河网关</span> / Gateway Console</span>`,
	} {
		if !strings.Contains(admin, want) {
			t.Fatalf("custom admin page is missing %q", want)
		}
	}
	branding := deploymentJSON(t, deploymentRequest(r, "GET", "/api/public/branding", "", ""), 200)
	if branding["title"] != "星河网关" || branding["custom_title"] != true {
		t.Fatalf("public branding after title change = %+v", branding)
	}

	// A later settings write (without site_title) must preserve the name.
	deploymentJSON(t, deploymentRequest(r, "PUT", "/admin/api/settings", token, `{"log_retention_days":30}`), 200)
	settings = deploymentJSON(t, deploymentRequest(r, "GET", "/admin/api/settings", token, ""), 200)
	if settings["site_title"] != "星河网关" {
		t.Fatalf("unrelated settings write cleared the title: %+v", settings)
	}

	// The title survives a cache reload from SQLite.
	settingsMu.Lock()
	settingsCache.SiteTitle = ""
	settingsMu.Unlock()
	if err := loadSettingsCache(); err != nil {
		t.Fatal(err)
	}
	if getSettings().SiteTitle != "星河网关" {
		t.Fatalf("title did not reload from SQLite: %q", getSettings().SiteTitle)
	}

	// The first-run page uses the same custom title if it is ever shown again.
	if err := settingsSet("admin_setup_completed", "0"); err != nil {
		t.Fatal(err)
	}
	setup := siteBrandingPage(t, r, "/setup/")
	if !strings.Contains(setup, `<title>星河网关 · 首次部署配置</title>`) || !strings.Contains(setup, `<span data-site-brand-name>星河网关</span>`) {
		t.Fatal("setup page did not receive the custom title")
	}
	if err := settingsSet("admin_setup_completed", "1"); err != nil {
		t.Fatal(err)
	}

	for _, body := range []string{
		`{"site_title":"bad\u0000title"}`,
		`{"site_title":"bad\ntitle"}`,
		`{"site_title":"` + strings.Repeat("星", maxSiteTitleRunes+1) + `"}`,
	} {
		w := deploymentRequest(r, "PUT", "/admin/api/settings", token, body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "网站标题") {
			t.Fatalf("invalid title accepted: %d %s", w.Code, w.Body.String())
		}
	}
	if settings = deploymentJSON(t, deploymentRequest(r, "GET", "/admin/api/settings", token, ""), 200); settings["site_title"] != "星河网关" {
		t.Fatalf("invalid titles changed the saved value: %+v", settings)
	}

	deploymentJSON(t, deploymentRequest(r, "PUT", "/admin/api/settings", token, `{"site_title":""}`), 200)
	if public = siteBrandingPage(t, r, "/"); !strings.Contains(public, `<title>API 分发 · 用量与模型状态</title>`) || !strings.Contains(public, `aria-label="API Share 首页"`) {
		t.Fatal("empty title did not restore the default public branding")
	}
	if admin = siteBrandingPage(t, r, "/admin/"); !strings.Contains(admin, `<title>API Share · 工作空间</title>`) {
		t.Fatal("empty title did not restore the default admin title")
	}
}

func TestSiteTitleIsHTMLEscaped(t *testing.T) {
	r, token := siteBrandingTestRouter(t)
	title := `<b>网关 & "团队"</b>`
	body, err := json.Marshal(map[string]string{"site_title": title})
	if err != nil {
		t.Fatal(err)
	}
	deploymentJSON(t, deploymentRequest(r, "PUT", "/admin/api/settings", token, string(body)), 200)
	public := siteBrandingPage(t, r, "/")
	if strings.Contains(public, title) || !strings.Contains(public, `&lt;b&gt;网关 &amp; &#34;团队&#34;&lt;/b&gt;`) {
		t.Fatal("site title was not HTML-escaped in the document")
	}
}

func TestSiteIconUploadServeAndReset(t *testing.T) {
	r, token := siteBrandingTestRouter(t)
	png, err := fs.ReadFile(webFS, "web/public/brand/apishare-favicon-v2.png")
	if err != nil {
		t.Fatal(err)
	}

	deploymentJSON(t, deploymentRequest(r, "PUT", "/admin/api/settings", token, `{"site_title":"图标测试站"}`), 200)
	if w := siteBrandingIconRequest(t, r, "", png); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated icon upload returned %d", w.Code)
	}
	for _, invalid := range [][]byte{
		[]byte("not an image"),
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		bytes.Repeat([]byte{0}, maxSiteIconBytes+1),
	} {
		w := siteBrandingIconRequest(t, r, token, invalid)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid icon was accepted: %d %s", w.Code, w.Body.String())
		}
	}
	settings := deploymentJSON(t, deploymentRequest(r, "GET", "/admin/api/settings", token, ""), 200)
	if settings["site_icon_custom"] != false {
		t.Fatal("invalid uploads changed the site icon")
	}

	result := deploymentJSON(t, siteBrandingIconRequest(t, r, token, png), 200)
	iconURL, _ := result["site_icon_url"].(string)
	if !strings.HasPrefix(iconURL, "/api/public/branding/icon?v=") || result["site_icon_type"] != "image/png" || result["site_icon_custom"] != true {
		t.Fatalf("icon upload result = %+v", result)
	}

	icon := deploymentRequest(r, "GET", iconURL, "", "")
	if icon.Code != http.StatusOK || !bytes.Equal(icon.Body.Bytes(), png) {
		t.Fatalf("served icon = %d %d bytes", icon.Code, icon.Body.Len())
	}
	if icon.Header().Get("Content-Type") != "image/png" || icon.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(icon.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("icon headers = %v", icon.Header())
	}
	stale := deploymentRequest(r, "GET", "/api/public/branding/icon?v=stale", "", "")
	if stale.Code != http.StatusOK || stale.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("stale icon URL = %d %v", stale.Code, stale.Header())
	}

	for _, path := range []string{"/", "/admin/"} {
		page := siteBrandingPage(t, r, path)
		if !strings.Contains(page, `href="`+iconURL+`"`) || !strings.Contains(page, `src="`+iconURL+`"`) {
			t.Fatalf("%s does not reference the uploaded icon", path)
		}
	}
	settings = deploymentJSON(t, deploymentRequest(r, "GET", "/admin/api/settings", token, ""), 200)
	if settings["site_icon_url"] != iconURL || settings["site_icon_custom"] != true || settings["site_title"] != "图标测试站" {
		t.Fatalf("settings after icon upload = %+v", settings)
	}
	if _, ok := settings["site_icon"]; ok || strings.Contains(deploymentRequest(r, "GET", "/admin/api/settings", token, "").Body.String(), base64.StdEncoding.EncodeToString(png)) {
		t.Fatal("settings endpoint leaked the icon payload")
	}
	branding := deploymentJSON(t, deploymentRequest(r, "GET", "/api/public/branding", "", ""), 200)
	if branding["icon_url"] != iconURL || branding["icon_type"] != "image/png" || branding["custom_icon"] != true {
		t.Fatalf("public icon metadata = %+v", branding)
	}

	// Unrelated settings writes and rejected uploads keep the current icon.
	deploymentJSON(t, deploymentRequest(r, "PUT", "/admin/api/settings", token, `{"retry":1}`), 200)
	if w := siteBrandingIconRequest(t, r, token, []byte("<svg/>")); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid replacement icon returned %d", w.Code)
	}
	if w := deploymentRequest(r, "GET", iconURL, "", ""); w.Code != http.StatusOK {
		t.Fatalf("existing icon disappeared after unrelated writes: %d", w.Code)
	}

	if w := deploymentRequest(r, "DELETE", "/admin/api/settings/icon", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated icon reset returned %d", w.Code)
	}
	deploymentJSON(t, deploymentRequest(r, "DELETE", "/admin/api/settings/icon", token, ""), 200)
	if w := deploymentRequest(r, "GET", iconURL, "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("uploaded icon survived reset: %d", w.Code)
	}
	if w := deploymentRequest(r, "GET", "/api/public/branding/icon", "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("empty icon route returned %d", w.Code)
	}
	if page := siteBrandingPage(t, r, "/"); !strings.Contains(page, `/public/static/brand/apishare-favicon-v2.png`) || strings.Contains(page, iconURL) {
		t.Fatal("public page did not return to the default icon")
	}
	settings = deploymentJSON(t, deploymentRequest(r, "GET", "/admin/api/settings", token, ""), 200)
	if settings["site_icon_url"] != "" || settings["site_icon_custom"] != false || settings["site_title"] != "图标测试站" {
		t.Fatalf("settings after icon reset = %+v", settings)
	}
}

func TestSiteIconWithBrandingAdminPath(t *testing.T) {
	r := deploymentTestRouter(t)
	setup := deploymentJSON(t, deploymentRequest(r, "PUT", "/setup/api/deployment", "",
		`{"new_password":"brand-test-password-123","confirm_password":"brand-test-password-123","admin_path":"branding"}`), 200)
	data, err := fs.ReadFile(webFS, "web/public/brand/apishare-favicon-v2.png")
	if err != nil {
		t.Fatal(err)
	}
	upload := deploymentJSON(t, siteBrandingIconRequest(t, r, setup["token"].(string), data), 200)
	iconURL := upload["site_icon_url"].(string)
	icon := deploymentRequest(r, "GET", iconURL, "", "")
	if icon.Code != http.StatusOK || !bytes.Equal(icon.Body.Bytes(), data) {
		t.Fatalf("icon with /branding admin prefix = %d: %s", icon.Code, icon.Body.String())
	}
	branding := deploymentJSON(t, deploymentRequest(r, "GET", "/api/public/branding", "", ""), 200)
	if branding["icon_url"] != iconURL {
		t.Fatalf("public branding icon URL = %v, want %s", branding["icon_url"], iconURL)
	}
	for _, path := range []string{"/", "/branding/"} {
		if page := siteBrandingPage(t, r, path); !strings.Contains(page, `src="`+iconURL+`"`) {
			t.Fatalf("%s does not reference the working icon URL", path)
		}
	}
}

func TestSiteIconFormatsAndInvalidReplacement(t *testing.T) {
	r, token := siteBrandingTestRouter(t)
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.NRGBA{R: 255, A: 255})
	formats := []struct {
		name, mime string
		encode     func(io.Writer) error
	}{
		{"png", "image/png", func(w io.Writer) error { return png.Encode(w, img) }},
		{"jpeg", "image/jpeg", func(w io.Writer) error { return jpeg.Encode(w, img, nil) }},
		{"gif", "image/gif", func(w io.Writer) error { return gif.Encode(w, img, nil) }},
		{"ico", "image/x-icon", func(w io.Writer) error { return ico.Encode(w, img) }},
		{"ico-png", "image/x-icon", func(w io.Writer) error {
			return ico.Encode(w, image.NewNRGBA(image.Rect(0, 0, 256, 256)))
		}},
		// Lossless 1x1 WebP; keep this fixture local so tests need no encoder or network.
		{"webp", "image/webp", func(w io.Writer) error {
			data, err := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
			if err != nil {
				return err
			}
			_, err = w.Write(data)
			return err
		}},
	}
	for _, format := range formats {
		t.Run(format.name, func(t *testing.T) {
			var data bytes.Buffer
			if err := format.encode(&data); err != nil {
				t.Fatal(err)
			}
			upload := deploymentJSON(t, siteBrandingIconRequest(t, r, token, data.Bytes()), 200)
			iconURL := upload["site_icon_url"].(string)
			if upload["site_icon_type"] != format.mime {
				t.Fatalf("icon MIME = %v", upload["site_icon_type"])
			}
			// A valid header alone must not replace the uploaded image. Also cut
			// the body in half to exercise failures after format detection.
			for _, broken := range [][]byte{data.Bytes()[:8], data.Bytes()[:data.Len()/2]} {
				if w := siteBrandingIconRequest(t, r, token, broken); w.Code != http.StatusBadRequest {
					t.Fatalf("corrupt %s accepted: %d %s", format.name, w.Code, w.Body.String())
				}
				settings := deploymentJSON(t, deploymentRequest(r, "GET", "/admin/api/settings", token, ""), 200)
				icon := deploymentRequest(r, "GET", iconURL, "", "")
				if settings["site_icon_url"] != iconURL || icon.Code != http.StatusOK || !bytes.Equal(icon.Body.Bytes(), data.Bytes()) {
					t.Fatal("rejected upload changed the saved icon")
				}
			}
		})
	}
}

func TestSiteIconRejectsExcessivePixelCount(t *testing.T) {
	r, token := siteBrandingTestRouter(t)
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	// Alter only IHDR and its CRC, without allocating an oversized pixel buffer.
	header := bytes.Clone(data.Bytes()[:33])
	binary.BigEndian.PutUint32(header[16:20], maxSiteIconPixels+1)
	binary.BigEndian.PutUint32(header[29:33], crc32.ChecksumIEEE(header[12:29]))
	if _, err := png.DecodeConfig(bytes.NewReader(header)); err != nil {
		t.Fatalf("invalid oversized fixture: %v", err)
	}
	w := siteBrandingIconRequest(t, r, token, header)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "1600 万像素") {
		t.Fatalf("oversized image = %d %s", w.Code, w.Body.String())
	}
}
