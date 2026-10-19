package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// Two users whose conversations legitimately begin with the same text, or
// even send the same client session id, must not share routing affinity. A
// shared key would pin both to one upstream and therefore share its upstream
// credential and proxy egress.
func TestSessionAffinityIsolatedByGatewayKey(t *testing.T) {
	modelsTestDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, fixtureSSE(FormatOpenAIChat))
	}))
	defer upstream.Close()
	up := fixtureUpstream(FormatOpenAIChat, upstream.URL)
	up.Enabled, up.Weight, up.Models = true, 1, "alias"
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	keyA := &ApiKey{Name: "user-a", Key: "sk-affinity-a", Enabled: true, QPS: 100, Concurrency: 10, Models: "alias"}
	keyB := &ApiKey{Name: "user-b", Key: "sk-affinity-b", Enabled: true, QPS: 100, Concurrency: 10, Models: "alias"}
	for _, key := range []*ApiKey{keyA, keyB} {
		if err := dbInsertKey(key); err != nil {
			t.Fatal(err)
		}
	}
	router := gin.New()
	router.Group("/v1", keyAuth()).POST("/chat/completions", relayHandler(FormatOpenAIChat, false))
	for _, key := range []*ApiKey{keyA, keyB} {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"alias","stream":true,"messages":[{"role":"user","content":"same opening"}]}`))
		req.Header.Set("Authorization", "Bearer "+key.Key)
		req.Header.Set("X-Session-Id", "shared-opening")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s relay failed: %d %s", key.Name, rec.Code, rec.Body.String())
		}
	}
	rows, err := db.Query(`SELECT skey FROM session_affinity ORDER BY skey`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var skey string
		if err := rows.Scan(&skey); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, skey)
	}
	want := []string{
		fmt.Sprintf("key:%d:sid:shared-opening", keyA.ID),
		fmt.Sprintf("key:%d:sid:shared-opening", keyB.ID),
	}
	if len(keys) != len(want) || keys[0] != want[0] || keys[1] != want[1] {
		t.Fatalf("affinity keys were shared: %v, want %v", keys, want)
	}
	if keyA.ID != keyB.ID && (keys[0] == keys[1]) {
		t.Fatal("identical conversations still shared affinity")
	}
}
