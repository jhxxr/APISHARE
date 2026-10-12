package main

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"golang.org/x/net/html"
)

func pageAssetURLs(page string) []string {
	var urls []string
	tokens := html.NewTokenizer(strings.NewReader(page))
	for {
		kind := tokens.Next()
		if kind == html.ErrorToken {
			return urls
		}
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			continue
		}
		tag := tokens.Token()
		for _, attr := range tag.Attr {
			if (tag.Data == "script" && attr.Key == "src") || (tag.Data == "link" && attr.Key == "href" && (strings.Contains(attr.Val, ".js") || strings.Contains(attr.Val, ".css"))) {
				urls = append(urls, attr.Val)
			}
		}
	}
}

func TestHTMLAssetsChangeURLsWhenContentChanges(t *testing.T) {
	assets := fstest.MapFS{
		"web/public/home.js":  &fstest.MapFile{Data: []byte("old frontend without announcements or model quotas")},
		"web/public/home.css": &fstest.MapFile{Data: []byte("old styles")},
		"web/app.js":          &fstest.MapFile{Data: []byte("admin frontend")},
	}
	page := []byte(`<link rel="stylesheet" href="/public/static/home.css"><script defer src="/public/static/home.js"></script><script src='static/app.js?lang=zh&amp;v=obsolete#start'></script>`)
	old := pageAssetURLs(string(versionHTMLAssets(page, assets)))
	if stable := pageAssetURLs(string(versionHTMLAssets(page, assets))); strings.Join(stable, "\n") != strings.Join(old, "\n") {
		t.Fatal("identical content must reuse its version across reloads/restarts")
	}
	assets["web/public/home.js"].Data = []byte("new frontend with announcements and model quotas")
	assets["web/public/home.css"].Data = []byte("new styles with announcement and quota panels")
	updated := pageAssetURLs(string(versionHTMLAssets(page, assets)))
	if len(old) != 3 || len(updated) != 3 || old[0] == updated[0] || old[1] == updated[1] || old[2] != updated[2] {
		t.Fatalf("an upgrade must bypass cached changed assets: old=%v new=%v", old, updated)
	}
	adminURL, err := url.Parse(updated[2])
	if err != nil || adminURL.Query().Get("lang") != "zh" || adminURL.Fragment != "start" || adminURL.Query().Get("v") == "obsolete" || len(adminURL.Query()["v"]) != 1 {
		t.Fatalf("existing URL options must survive version replacement: %s %v", updated[2], err)
	}
	untouched := []byte(`<script type="importmap">{"imports":{"three":"/public/static/vendor/three-0.186.1/three.module.js"}}</script><link rel="modulepreload" href="/public/static/vendor/three-0.186.1/three.module.js"><script>const sample='<script src="/public/static/home.js">';</script><script src="https://example.invalid/static/app.js"></script><link href="data:text/css,body{}"><script src="static/missing.js"></script>`)
	if got := versionHTMLAssets(untouched, assets); string(got) != string(untouched) {
		t.Fatalf("vendor/import maps, inline scripts and external URLs must stay intact: %s", got)
	}
}

func TestPagesServeMatchingVersionedAssetsAndCachePolicy(t *testing.T) {
	r := deploymentTestRouter(t)
	// The first-run page also needs versioned scripts and styles.
	setup := deploymentRequest(r, "GET", "/setup/", "", "")
	checkPage := func(pagePath string, page *httptest.ResponseRecorder) {
		t.Helper()
		if page.Code != http.StatusOK || page.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("page %s must stay fresh: %d %v", pagePath, page.Code, page.Header())
		}
		urls := pageAssetURLs(page.Body.String())
		if len(urls) == 0 {
			t.Fatalf("page %s has no frontend assets", pagePath)
		}
		base, _ := url.Parse(pagePath)
		for _, link := range urls {
			asset, err := url.Parse(link)
			if err != nil {
				t.Fatal(err)
			}
			asset = base.ResolveReference(asset)
			vendor := strings.HasPrefix(asset.Path, "/public/static/vendor/")
			if !vendor && asset.Query().Get("v") == "" {
				t.Fatalf("%s references an unversioned resource: %s", pagePath, link)
			}
			w := deploymentRequest(r, "GET", asset.String(), "", "")
			if w.Code != http.StatusOK {
				t.Fatalf("%s resource %s returned %d", pagePath, link, w.Code)
			}
			if !vendor && asset.Query().Get("v") != webAssetVersion(w.Body.Bytes()) {
				t.Fatalf("HTML and resource bytes have different versions: %s", link)
			}
			if strings.HasPrefix(asset.Path, "/public/static/") {
				if !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
					t.Fatalf("versioned public resource is not cached: %s %v", link, w.Header())
				}
			} else if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("private/setup resources must retain no-store: %s", link)
			}
		}
	}
	checkPage("/setup/", setup)
	deploymentJSON(t, deploymentRequest(r, "PUT", "/setup/api/deployment", "", `{"new_password":"asset-test-password-123","confirm_password":"asset-test-password-123","admin_path":"asset-console"}`), 200)
	for _, pagePath := range []string{"/", "/asset-console/"} {
		checkPage(pagePath, deploymentRequest(r, "GET", pagePath, "", ""))
	}
	for _, assetPath := range []string{"/public/static/home.js", "/public/static/home.css", "/public/static/home.js?v=old-release", "/public/static/home.css?v=old-release"} {
		w := deploymentRequest(r, "GET", assetPath, "", "")
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("legacy/stale resource URL may be cached: %s %v", assetPath, w.Header())
		}
	}
	// The version is based on source content, independent of gzip negotiation.
	plain := deploymentRequest(r, "GET", "/public/static/home.js", "", "")
	request := httptest.NewRequest("GET", "/public/static/home.js?v="+webAssetVersion(plain.Body.Bytes()), nil)
	request.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, request)
	if w.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("versioned gzip policy = %v", w.Header())
	}
	reader, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil || string(body) != plain.Body.String() {
		t.Fatalf("versioned compression changed resource bytes: %v", err)
	}
}
