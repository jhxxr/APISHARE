package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestFirstOutputDeadlineReleasesBlockedRead(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	body := newFirstOutputDeadlineBody(reader, time.Now().Add(30*time.Millisecond))
	defer body.Close()
	result := make(chan error, 1)
	go func() {
		_, err := body.Read(make([]byte, 1))
		result <- err
	}()
	select {
	case err := <-result:
		if err != errUpstreamFirstOutputTimeout {
			t.Fatalf("unexpected first-output deadline result: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first-output deadline left an upstream reader blocked")
	}
}

func TestFirstOutputProgressDisarmsDeadline(t *testing.T) {
	reader, writer := io.Pipe()
	body := newFirstOutputDeadlineBody(reader, time.Now().Add(150*time.Millisecond))
	defer body.Close()
	go func() {
		_, _ = io.WriteString(writer, "first")
		time.Sleep(220 * time.Millisecond)
		_, _ = io.WriteString(writer, "next")
		_ = writer.Close()
	}()
	buffer := make([]byte, 5)
	if _, err := io.ReadFull(body, buffer); err != nil || string(buffer) != "first" {
		t.Fatalf("initial stream progress failed: %q / %v", buffer, err)
	}
	body.MarkOutput()
	buffer = make([]byte, 4)
	if _, err := io.ReadFull(body, buffer); err != nil || string(buffer) != "next" {
		t.Fatalf("deadline remained active after output: %q / %v", buffer, err)
	}
}

func TestPreOutputIdleTimeoutUsesBoundedFirstOutputSwitch(t *testing.T) {
	guard := newFirstOutputDeadlineBody(io.NopCloser(strings.NewReader("")), time.Now().Add(time.Minute))
	defer guard.Close()
	info := &relayInfo{firstOutputGuard: guard}
	if err := normalizePreOutputTimeout(errUpstreamIdleTimeout, info); err == nil || !errors.Is(err, errUpstreamFirstOutputTimeout) {
		t.Fatalf("pre-output idle timeout did not share the bounded switch budget: %v", err)
	}
	info.markFirstOutput()
	if err := normalizePreOutputTimeout(errUpstreamIdleTimeout, info); !errors.Is(err, errUpstreamIdleTimeout) {
		t.Fatalf("timeout after semantic output was incorrectly treated as retryable pre-output idle: %v", err)
	}
}

func TestFirstOutputTimeoutOnlySwitchesOnceEvenWhenNormalRetryIsZero(t *testing.T) {
	modelsTestDB(t)
	previousTimeout := relayNetwork.FirstOutputTimeout
	relayNetwork.FirstOutputTimeout = 150 * time.Millisecond
	t.Cleanup(func() { relayNetwork.FirstOutputTimeout = previousTimeout })
	previousSettings := getSettings()
	settingsMu.Lock()
	settingsCache.Retry = 0
	settingsMu.Unlock()
	t.Cleanup(func() {
		settingsMu.Lock()
		settingsCache = previousSettings
		settingsMu.Unlock()
	})

	var stalledCalls [3]atomic.Int32
	var servers [3]*httptest.Server
	for index := range servers {
		index := index
		servers[index] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stalledCalls[index].Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, nativePrelude(FormatOpenAIChat))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer servers[index].Close()
	}
	first, second, third := fixtureUpstream(FormatOpenAIChat, servers[0].URL), fixtureUpstream(FormatOpenAIChat, servers[1].URL), fixtureUpstream(FormatOpenAIChat, servers[2].URL)
	first.Priority, second.Priority, third.Priority = 0, 1, 2
	for _, upstream := range []*Upstream{first, second, third} {
		upstream.Enabled, upstream.Weight, upstream.Models = true, 1, "alias"
		if err := dbInsertUpstream(upstream); err != nil {
			t.Fatal(err)
		}
	}
	key := ApiKey{Name: "first-output-timeout", Key: "sk-first-output-timeout", Models: "alias", Enabled: true}
	if err := dbInsertKey(&key); err != nil {
		t.Fatal(err)
	}
	affinitySet(scopedSessionKey(key.ID, "sid:first-output-timeout"), first.ID)
	engine := gin.New()
	engine.POST("/v1/chat/completions", func(c *gin.Context) { c.Set("apiKey", key) }, relayHandler(FormatOpenAIChat, false))
	task := fixtureTask(t, FormatOpenAIChat, true)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(task.body)))
	request.Header.Set("X-Session-Id", "first-output-timeout")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	if stalledCalls[0].Load() != 1 || stalledCalls[1].Load() != 1 || stalledCalls[2].Load() != 0 {
		t.Fatalf("first-output failover exceeded its one-switch budget: calls=%d/%d/%d", stalledCalls[0].Load(), stalledCalls[1].Load(), stalledCalls[2].Load())
	}
	if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), "[DONE]") {
		t.Fatalf("pre-output timeout unexpectedly succeeded: status=%d body=%s", response.Code, response.Body.String())
	}
	logs, err := dbListLogs(10, 0)
	if err != nil || len(logs) != 1 || logs[0].Status != http.StatusBadGateway {
		t.Fatalf("timeout attempt was not logged exactly once: logs=%+v err=%v", logs, err)
	}
}
