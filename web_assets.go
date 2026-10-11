package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"net/url"
	"path"
	"strings"

	"golang.org/x/net/html"
)

func webAssetVersion(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:12])
}

// Version each local script and stylesheet by its embedded content. The HTML
// and its assets then stay in sync even when a browser/CDN cached an old URL.
func versionHTMLAssets(page []byte, assets fs.FS) []byte {
	var output bytes.Buffer
	tokens := html.NewTokenizer(bytes.NewReader(page))
	for {
		kind := tokens.Next()
		if kind == html.ErrorToken {
			if tokens.Err() != io.EOF {
				return page
			}
			return output.Bytes()
		}
		raw := tokens.Raw()
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			output.Write(raw)
			continue
		}
		tag := tokens.Token()
		if tag.Data != "script" && tag.Data != "link" {
			output.Write(raw)
			continue
		}
		changed := false
		for i := range tag.Attr {
			attr := &tag.Attr[i]
			if (tag.Data != "script" || attr.Key != "src") && (tag.Data != "link" || attr.Key != "href") {
				continue
			}
			assetURL, err := url.Parse(attr.Val)
			if err != nil || assetURL.Scheme != "" || assetURL.Host != "" {
				continue
			}
			var name string
			switch {
			case strings.HasPrefix(assetURL.Path, "/public/static/"):
				name = "web/public/" + strings.TrimPrefix(assetURL.Path, "/public/static/")
			case strings.HasPrefix(assetURL.Path, "static/"):
				name = "web/" + strings.TrimPrefix(assetURL.Path, "static/")
			default:
				continue
			}
			// Versioned vendor directories and import maps already share stable URLs.
			if !fs.ValidPath(name) || strings.HasPrefix(name, "web/public/vendor/") || (path.Ext(name) != ".js" && path.Ext(name) != ".css") {
				continue
			}
			content, err := fs.ReadFile(assets, name)
			if err != nil {
				continue
			}
			query := assetURL.Query()
			query.Set("v", webAssetVersion(content))
			assetURL.RawQuery = query.Encode()
			attr.Val = assetURL.String()
			changed = true
		}
		if changed {
			output.WriteString(tag.String())
		} else {
			output.Write(raw)
		}
	}
}
