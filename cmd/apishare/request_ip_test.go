package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequestIPModes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mode          string
		proxies       string
		customHeaders string
		headers       map[string][]string
		remoteAddr    string
		wantLog       string
		wantTrusted   string
	}{
		{
			name: "Docker private X-Real-IP does not hide public XFF",
			headers: map[string][]string{
				"X-Real-IP": {"172.18.0.1"}, "X-Forwarded-For": {"203.0.113.42, 172.18.0.2"},
			},
			remoteAddr: "172.18.0.1:54321", wantLog: "203.0.113.42", wantTrusted: "172.18.0.1",
		},
		{
			name: "Cloudflare public IP takes precedence",
			headers: map[string][]string{
				"CF-Connecting-IP": {"198.51.100.11"}, "X-Real-IP": {"203.0.113.42"}, "X-Forwarded-For": {"203.0.113.43"},
			},
			remoteAddr: "172.18.0.1:54321", wantLog: "198.51.100.11", wantTrusted: "172.18.0.1",
		},
		{
			name: "True-Client-IP supports CDN forwarding",
			headers: map[string][]string{
				"CF-Connecting-IP": {"10.0.0.1"}, "True-Client-IP": {"198.51.100.12"}, "X-Real-IP": {"203.0.113.42"},
			},
			remoteAddr: "172.18.0.1:54321", wantLog: "198.51.100.12", wantTrusted: "172.18.0.1",
		},
		{
			name:          "custom headers take priority and ignore invalid names",
			customHeaders: "Bad Header, X-Client-IP, X-Edge-IP",
			headers: map[string][]string{
				"X-Client-IP": {"unknown, 10.0.0.1"}, "X-Edge-IP": {"198.51.100.15"},
				"CF-Connecting-IP": {"198.51.100.11"},
			},
			remoteAddr: "172.18.0.1:54321", wantLog: "198.51.100.15", wantTrusted: "172.18.0.1",
		},
		{
			name:          "private custom header yields to public standard header",
			customHeaders: "X-Client-IP",
			headers: map[string][]string{
				"X-Client-IP": {"10.0.0.1"}, "X-Forwarded-For": {"203.0.113.42"},
			},
			remoteAddr: "172.18.0.1:54321", wantLog: "203.0.113.42", wantTrusted: "172.18.0.1",
		},
		{
			name: "multiple header lines and mapped IPv4 with port",
			headers: map[string][]string{
				"X-Forwarded-For": {"unknown, 10.0.0.1", "[::ffff:203.0.113.99]:8080, 172.18.0.2"},
			},
			remoteAddr: "172.18.0.1:54321", wantLog: "203.0.113.99", wantTrusted: "172.18.0.1",
		},
		{
			name: "IPv6 public address beats private and link-local addresses",
			headers: map[string][]string{
				"CF-Connecting-IP": {"fd00::1"}, "X-Real-IP": {"fe80::1"},
				"X-Forwarded-For": {"2001:db8::1, ::1"},
			},
			remoteAddr: "[fd00::2]:54321", wantLog: "2001:db8::1", wantTrusted: "fd00::2",
		},
		{
			name:          "only private headers retain first custom fallback",
			customHeaders: "X-Client-IP",
			headers: map[string][]string{
				"X-Client-IP": {"192.168.1.8"}, "CF-Connecting-IP": {"10.0.0.1"},
			},
			remoteAddr: "172.18.0.1:54321", wantLog: "192.168.1.8", wantTrusted: "172.18.0.1",
		},
		{
			name: "malformed and unusable headers fall back to peer",
			headers: map[string][]string{
				"CF-Connecting-IP": {"unknown"}, "X-Real-IP": {"0.0.0.0"},
				"X-Forwarded-For": {"not-an-ip, ::, 224.0.0.1, ff02::1"},
			},
			remoteAddr: "172.18.0.1:54321", wantLog: "172.18.0.1", wantTrusted: "172.18.0.1",
		},
		{
			name:       "no headers use normalized peer",
			remoteAddr: "[::ffff:203.0.113.42]:54321", wantLog: "203.0.113.42", wantTrusted: "203.0.113.42",
		},
		{
			name: "strict mode ignores untrusted forwarding",
			mode: "false",
			headers: map[string][]string{
				"CF-Connecting-IP": {"198.51.100.11"}, "True-Client-IP": {"198.51.100.12"},
				"X-Real-IP": {"203.0.113.42"}, "X-Forwarded-For": {"203.0.113.43"},
			},
			remoteAddr: "172.18.0.1:54321", wantLog: "172.18.0.1", wantTrusted: "172.18.0.1",
		},
		{
			name: "strict mode validates proxy chain from the right",
			mode: "false", proxies: "172.18.0.0/16, 127.0.0.1\n::1",
			customHeaders: "X-Client-IP",
			headers: map[string][]string{
				"X-Client-IP": {"198.51.100.11"}, "CF-Connecting-IP": {"198.51.100.12"},
				"X-Forwarded-For": {"198.51.100.99, 203.0.113.42, 172.18.0.2"},
			},
			remoteAddr: "172.18.0.1:54321", wantLog: "203.0.113.42", wantTrusted: "203.0.113.42",
		},
		{
			name: "strict mode rejects peer outside proxy CIDR",
			mode: "false", proxies: "10.0.0.0/8",
			headers:    map[string][]string{"X-Forwarded-For": {"203.0.113.42"}},
			remoteAddr: "172.18.0.1:54321", wantLog: "172.18.0.1", wantTrusted: "172.18.0.1",
		},
		{
			name:       "invalid mode fails closed",
			mode:       "invalid",
			headers:    map[string][]string{"X-Forwarded-For": {"203.0.113.42"}},
			remoteAddr: "172.18.0.1:54321", wantLog: "172.18.0.1", wantTrusted: "172.18.0.1",
		},
		{
			name: "invalid proxy config discards partial trusted entries",
			mode: "false", proxies: "172.18.0.0/16,invalid",
			headers:    map[string][]string{"X-Forwarded-For": {"203.0.113.42"}},
			remoteAddr: "172.18.0.1:54321", wantLog: "172.18.0.1", wantTrusted: "172.18.0.1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOG_TRUST_FORWARDED_IP", tc.mode)
			t.Setenv("TRUSTED_PROXIES", tc.proxies)
			t.Setenv("LOG_FORWARDED_IP_HEADERS", tc.customHeaders)
			router := gin.New()
			configureRequestIP(router)
			router.GET("/", func(c *gin.Context) {
				if got := requestClientFor(c).identity.IP; got != tc.wantLog {
					t.Errorf("log IP = %q, want %q", got, tc.wantLog)
				}
				if got := parseCleanIP(c.ClientIP()); got != tc.wantTrusted {
					t.Errorf("trusted IP = %q, want %q", got, tc.wantTrusted)
				}
				c.Status(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remoteAddr
			for header, values := range tc.headers {
				for _, value := range values {
					req.Header.Add(header, value)
				}
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != http.StatusNoContent {
				t.Fatalf("response status = %d", response.Code)
			}
		})
	}
}

func TestRequestIPPolicyIsCapturedAtRouterStartup(t *testing.T) {
	t.Setenv("LOG_TRUST_FORWARDED_IP", "true")
	t.Setenv("LOG_FORWARDED_IP_HEADERS", "X-Client-IP")
	t.Setenv("TRUSTED_PROXIES", "")
	router := gin.New()
	configureRequestIP(router)
	router.GET("/", func(c *gin.Context) {
		if got := extractRequestIP(c); got != "203.0.113.42" {
			t.Errorf("captured IP policy returned %q", got)
		}
	})
	t.Setenv("LOG_TRUST_FORWARDED_IP", "false")
	t.Setenv("LOG_FORWARDED_IP_HEADERS", "X-Other-IP")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "172.18.0.1:54321"
	req.Header.Set("X-Client-IP", "203.0.113.42")
	router.ServeHTTP(httptest.NewRecorder(), req)
}

func TestRequestClientPersistsForwardedIPBehindDocker(t *testing.T) {
	t.Setenv("LOG_TRUST_FORWARDED_IP", "")
	t.Setenv("LOG_FORWARDED_IP_HEADERS", "")
	t.Setenv("TRUSTED_PROXIES", "")
	modelsTestDB(t)
	if err := settingsSet("session_secret", "docker-ip-test-secret"); err != nil {
		t.Fatal(err)
	}
	key := &ApiKey{Name: "docker-client", Key: "sk-docker-client", Enabled: true, QPS: 1000000, Concurrency: 10}
	if err := dbInsertKey(key); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{"))
	req.RemoteAddr = "172.18.0.1:54321"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key.Key)
	req.Header.Set("X-Real-IP", "172.18.0.2")
	req.Header.Set("X-Forwarded-For", "203.0.113.42, 172.18.0.2")
	response := httptest.NewRecorder()
	newRouter().ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("bad request status = %d", response.Code)
	}
	logs, err := dbListLogs(10, 0)
	if err != nil || len(logs) != 1 {
		t.Fatalf("logs = %+v / %v", logs, err)
	}
	if logs[0].ClientIP != "203.0.113.42" || logs[0].Client.IP != "203.0.113.42" {
		t.Fatalf("forwarded IP lost on persistence: %+v", logs[0])
	}
	query := httptest.NewRequest(http.MethodGet, currentAdminPath()+"/api/logs?ip=203.0.113.42", nil)
	query.Header.Set("Authorization", "Bearer "+makeAdminToken())
	response = httptest.NewRecorder()
	newRouter().ServeHTTP(response, query)
	var result logQueryResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != http.StatusOK || result.Total != 1 {
		t.Fatalf("forwarded IP filter = %d %s / %v", response.Code, response.Body.String(), err)
	}
}
