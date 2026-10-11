package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func announcementTestCreate(t *testing.T, router http.Handler, token, title, content string, published, pinned bool) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"title": title, "content": content, "published": published, "pinned": pinned})
	if err != nil {
		t.Fatal(err)
	}
	return deploymentJSON(t, deploymentRequest(router, "POST", "/admin/api/announcements", token, string(body)), http.StatusCreated)
}

func announcementTestPublic(t *testing.T, router http.Handler) []PublicAnnouncement {
	t.Helper()
	w := deploymentRequest(router, "GET", "/api/public/announcements", "", "")
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("public announcements = %d %s", w.Code, w.Body.String())
	}
	var items []PublicAnnouncement
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil || items == nil {
		t.Fatalf("expected an array, got %s: %v", w.Body.String(), err)
	}
	return items
}

func TestAnnouncementsPublishingLifecycleAndPrivacy(t *testing.T) {
	r := deploymentTestRouter(t)
	token := makeAdminToken()
	if items := announcementTestPublic(t, r); len(items) != 0 {
		t.Fatal("a new database must not contain placeholder announcements")
	}
	for _, request := range []struct{ method, path, body string }{
		{"GET", "/admin/api/announcements", ""},
		{"POST", "/admin/api/announcements", `{"title":"unauthorized","content":"body","published":true}`},
		{"PATCH", "/admin/api/announcements/1", `{"published":true}`},
		{"DELETE", "/admin/api/announcements/1", ""},
	} {
		if w := deploymentRequest(r, request.method, request.path, "", request.body); w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s %s = %d", request.method, request.path, w.Code)
		}
	}
	draft := announcementTestCreate(t, r, token, "  private draft  ", "  private content\nsecond line  ", false, true)
	if draft["title"] != "private draft" || draft["published_at"] != "" || draft["content"] != "private content\nsecond line" {
		t.Fatalf("draft normalization = %+v", draft)
	}
	if items := announcementTestPublic(t, r); len(items) != 0 {
		t.Fatal("pinned drafts must remain private")
	}
	id := int64(draft["id"].(float64))
	path := fmt.Sprintf("/admin/api/announcements/%d", id)
	published := deploymentJSON(t, deploymentRequest(r, "PATCH", path, token, `{"published":true}`), 200)
	if published["published_at"] == "" || !strings.HasSuffix(published["published_at"].(string), "+08:00") {
		t.Fatalf("publication time = %+v", published)
	}
	if items := announcementTestPublic(t, r); len(items) != 1 || items[0].ID != id || !items[0].Pinned {
		t.Fatalf("published announcement not visible: %+v", items)
	}
	w := deploymentRequest(r, "GET", "/api/public/announcements", "", "")
	for _, privateField := range []string{`"created_at"`, `"updated_at"`, `"published"`} {
		if strings.Contains(w.Body.String(), privateField) {
			t.Fatalf("public response contains admin field %s", privateField)
		}
	}
	edited := deploymentJSON(t, deploymentRequest(r, "PATCH", path, token, `{"title":"updated title","content":"<script>alert(1)</script>\nnew line","pinned":false}`), 200)
	if edited["published_at"] != published["published_at"] || edited["published"] != true {
		t.Fatal("editing must preserve publication state and time")
	}
	if items := announcementTestPublic(t, r); len(items) != 1 || items[0].Title != "updated title" || items[0].Pinned || items[0].Content != "<script>alert(1)</script>\nnew line" {
		t.Fatalf("content must be round-tripped as text: %+v", items)
	}
	withdrawn := deploymentJSON(t, deploymentRequest(r, "PATCH", path, token, `{"published":false}`), 200)
	if withdrawn["published_at"] != "" || withdrawn["title"] != "updated title" {
		t.Fatalf("withdrawal must retain the draft: %+v", withdrawn)
	}
	if items := announcementTestPublic(t, r); len(items) != 0 {
		t.Fatal("withdrawn announcement remains visible")
	}
	deploymentJSON(t, deploymentRequest(r, "DELETE", path, token, ""), 200)
	deploymentJSON(t, deploymentRequest(r, "DELETE", path, token, ""), 404)
	deploymentJSON(t, deploymentRequest(r, "PATCH", path, token, `{"published":true}`), 404)
}

func TestAnnouncementsOrderingValidationAndMigration(t *testing.T) {
	r := deploymentTestRouter(t)
	token := makeAdminToken()
	for i := 0; i < 7; i++ {
		announcementTestCreate(t, r, token, fmt.Sprintf("update %d", i), "body", true, false)
	}
	// An older pinned publication takes priority over newer ordinary publications.
	if _, err := db.Exec(`UPDATE announcements SET pinned=1,published_at='2020-01-01T08:00:00+08:00' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	announcementTestCreate(t, r, token, "private pinned draft", "never public", false, true)
	items := announcementTestPublic(t, r)
	if len(items) != publicAnnouncementLimit || items[0].ID != 1 || items[1].ID != 7 || items[4].ID != 4 {
		t.Fatalf("expected pinned then newest publications: %+v", items)
	}
	for _, body := range []string{
		`{}`, `null`, `{"title":"ok"}`, `{"title":" ","content":"body"}`, `{"title":"ok","content":"\n"}`,
		`{"title":"ok","content":"body","published":"true"}`, `{"title":`,
		`{"title":"` + strings.Repeat("中", 121) + `","content":"body"}`,
		`{"title":"ok","content":"` + strings.Repeat("中", 10001) + `"}`,
		`{"title":"ok","content":"` + strings.Repeat("x", 65536) + `"}`,
	} {
		deploymentJSON(t, deploymentRequest(r, "POST", "/admin/api/announcements", token, body), 400)
	}
	for _, body := range []string{`{}`, `{"title":""}`, `{"content":" "}`, `{"published":null}`} {
		deploymentJSON(t, deploymentRequest(r, "PATCH", "/admin/api/announcements/1", token, body), 400)
	}
	for _, id := range []string{"bad", "0", "-1", "9999999999999999999999999"} {
		deploymentJSON(t, deploymentRequest(r, "PATCH", "/admin/api/announcements/"+id, token, `{"pinned":true}`), 400)
		deploymentJSON(t, deploymentRequest(r, "DELETE", "/admin/api/announcements/"+id, token, ""), 400)
	}
	announcementTestCreate(t, r, token, strings.Repeat("中", 120), strings.Repeat("中", 10000), false, false)
	// Existing deployments can run the migration repeatedly without losing posts.
	if err := migrate(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM announcements`).Scan(&count); err != nil || count != 9 {
		t.Fatalf("migration retained %d announcements, err=%v", count, err)
	}
	// Content survives closing and reopening the database.
	var file string
	var seq int
	var name string
	if err := db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &file); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := openDB(file); err != nil {
		t.Fatal(err)
	}
	if items := announcementTestPublic(t, r); len(items) != publicAnnouncementLimit || items[0].ID != 1 {
		t.Fatalf("announcements did not survive restart: %+v", items)
	}
}

func TestAnnouncementRoutesFollowCustomAdminPath(t *testing.T) {
	r := deploymentTestRouter(t)
	result := deploymentJSON(t, deploymentRequest(r, "PUT", "/setup/api/deployment", "", `{"new_password":"announcement-test-password","confirm_password":"announcement-test-password","admin_path":"news-console"}`), 200)
	token := result["token"].(string)
	deploymentJSON(t, deploymentRequest(r, "POST", "/news-console/api/announcements", token, `{"title":"service news","content":"new model available","published":true}`), 201)
	if w := deploymentRequest(r, "GET", "/admin/api/announcements", token, ""); w.Code != 404 {
		t.Fatalf("old admin path remained exposed: %d", w.Code)
	}
	if items := announcementTestPublic(t, r); len(items) != 1 || items[0].Title != "service news" {
		t.Fatalf("custom admin publication missing: %+v", items)
	}
}
