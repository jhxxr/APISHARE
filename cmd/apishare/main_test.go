package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestProductionRouterStartupAndPreflight(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := newRouter() // Exercise every production route, including OPTIONS conflicts.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("health check: %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{
		"/v1/models", "/v1/models/demo", "/v1/chat/completions",
		"/v1/responses", "/v1/responses/compact", "/v1/messages", "/v1/embeddings",
		"/v1/audio/speech", "/v1/audio/transcriptions", "/v1/audio/translations",
		"/v1/realtime", "/v1/realtime/calls",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodOptions, path, nil)
			req.Header.Set("Origin", "https://client.example")
			req.Header.Set("Access-Control-Request-Method", "POST")
			req.Header.Set("Access-Control-Request-Headers", "authorization,x-custom-header")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != "*" || w.Header().Get("Access-Control-Allow-Headers") != "authorization,x-custom-header" {
				t.Fatalf("preflight: %d %v", w.Code, w.Header())
			}
		})
	}
	for _, path := range []string{"/v1/models", "/v1/realtime", "/v1/responses"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated GET %s: %d", path, w.Code)
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodOptions} {
		for _, path := range []string{"/unregistered", "/v10/unregistered"} {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(method, path, nil))
			if w.Code != http.StatusNotFound || w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatalf("unregistered route %s %s: %d %v", method, path, w.Code, w.Header())
			}
		}
	}
}
