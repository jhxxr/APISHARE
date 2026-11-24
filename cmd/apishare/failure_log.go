package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

var failureLogs *failureLogStore // Set at startup and swapped when settings change.

// Failure diagnostics are configured from the admin console, never from the
// environment, so operators can change retention and redaction without a restart.
const failureLogSettingsKey = "failure_logs"

const (
	failureLogDefaultMaxMB    = 32
	failureLogDefaultEntryKiB = 64
	failureLogMaxMB           = 16384
)

type failureLogSettings struct {
	Enabled     bool `json:"enabled"`
	MaxMB       int  `json:"max_mb"`
	EntryKiB    int  `json:"entry_kib"`
	FullContent bool `json:"full_content"` // Store raw request/response text instead of redacting.
}

func defaultFailureLogSettings() failureLogSettings {
	return failureLogSettings{Enabled: true, MaxMB: failureLogDefaultMaxMB, EntryKiB: failureLogDefaultEntryKiB}
}

// failureLogSettingsMu serializes settings writes with the store swap so two
// admins saving at once cannot interleave a save and an apply.
var failureLogSettingsMu sync.Mutex

func (s failureLogSettings) normalized() failureLogSettings {
	if s.MaxMB <= 0 || s.MaxMB > failureLogMaxMB {
		s.MaxMB = failureLogDefaultMaxMB
	}
	if s.EntryKiB < 1 || s.EntryKiB > 1024 {
		s.EntryKiB = failureLogDefaultEntryKiB
	}
	return s
}

func readFailureLogSettings() failureLogSettings {
	raw, err := settingsGet(failureLogSettingsKey)
	if err != nil || strings.TrimSpace(raw) == "" {
		return defaultFailureLogSettings()
	}
	var settings failureLogSettings
	if json.Unmarshal([]byte(raw), &settings) != nil {
		return defaultFailureLogSettings()
	}
	return settings.normalized()
}

func saveFailureLogSettings(settings failureLogSettings) error {
	encoded, err := json.Marshal(settings.normalized())
	if err != nil {
		return err
	}
	return settingsSet(failureLogSettingsKey, string(encoded))
}

// activeFailureLogs returns the live store. The handle is replaced when the
// settings change, so never cache it for the lifetime of a request.
func activeFailureLogs() *failureLogStore { return failureLogs }

// applyFailureLogSettings rebuilds the store so a retention change takes effect
// immediately. Disabling only stops new records; existing files stay readable.
func applyFailureLogSettings(settings failureLogSettings) error {
	settings = settings.normalized()
	if !settings.Enabled {
		failureLogs = nil
		return nil
	}
	s, err := openFailureLogStore(failureLogDir(), int64(settings.MaxMB)<<20, settings.EntryKiB<<10)
	if err != nil {
		return err
	}
	s.fullContent = settings.FullContent
	failureLogs = s
	return nil
}

// failureLogDBPath is captured at startup so diagnostics live next to the
// database without any extra configuration.
var failureLogDBPath = "apishare.db"

// failureLogDir keeps diagnostics next to the database.
func failureLogDir() string {
	return failureLogDBPath + ".failure-logs"
}

const failureLogRequestHeader = "X-Apishare-Request-Id"

var failureLogFilename = regexp.MustCompile(`^[0-9]{19}-[0-9a-f]{32}\.json$`)
var failureLogCode = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,80}$`)

type failureLogFile struct {
	name string
	size int64
}

type failureLogStore struct {
	dir         string
	maxBytes    int64
	entryBytes  int
	fullContent bool
	mu          sync.Mutex
	files       []failureLogFile // Oldest first; one writer per directory.
	used        int64
}

func openFailureLogStore(dir string, maxBytes int64, entryBytes int) (*failureLogStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir) // Sorted by name (UTC Unix nanoseconds first).
	if err != nil {
		return nil, err
	}
	s := &failureLogStore{dir: dir, maxBytes: maxBytes, entryBytes: entryBytes}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		if strings.HasPrefix(entry.Name(), ".apishare-failure-") && strings.HasSuffix(entry.Name(), ".tmp") {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return nil, err
			}
			continue
		}
		if !failureLogFilename.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		s.files = append(s.files, failureLogFile{entry.Name(), info.Size()})
		s.used += info.Size()
	}
	if err := s.makeRoom(0); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *failureLogStore) makeRoom(size int64) error {
	for s.used+size > s.maxBytes && len(s.files) != 0 {
		oldest := s.files[0]
		if err := os.Remove(filepath.Join(s.dir, oldest.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		s.files = s.files[1:]
		s.used -= oldest.size
	}
	return nil
}

func (s *failureLogStore) write(entry failureLogEntry) error {
	data, err := entry.marshal(s.entryBytes)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	size := int64(len(data))
	if size > s.maxBytes {
		return fmt.Errorf("failure record exceeds cache capacity")
	}
	if err := s.makeRoom(size); err != nil {
		return err
	}
	file, err := os.CreateTemp(s.dir, ".apishare-failure-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(data)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	name := fmt.Sprintf("%019d-%s.json", time.Now().UnixNano(), entry.RequestID)
	if err := os.Rename(file.Name(), filepath.Join(s.dir, name)); err != nil {
		return err
	}
	s.files = append(s.files, failureLogFile{name, size})
	s.used += size
	return nil
}

// failureLogRecord is the list-row view shown in the console.
type failureLogRecord struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	Time      time.Time `json:"time"`
	RequestID string    `json:"request_id"`
	Duration  int64     `json:"duration_ms"`
	Status    int       `json:"gateway_status"`
	Model     string    `json:"model"`
	Route     string    `json:"route"`
	Channel   string    `json:"channel"`
	Kind      string    `json:"failure_kind"`
	Summary   string    `json:"summary"`
}

// list returns records newest first. Entries that can no longer be read (for
// example removed between listing and parsing) are skipped rather than failing
// the whole page.
func (s *failureLogStore) list(offset, limit int) ([]failureLogRecord, int) {
	if s == nil {
		return []failureLogRecord{}, 0
	}
	s.mu.Lock()
	files := make([]failureLogFile, len(s.files))
	copy(files, s.files)
	s.mu.Unlock()
	total := len(files)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	ordered := make([]failureLogFile, 0, limit)
	for i := total - 1 - offset; i >= 0 && len(ordered) < limit; i-- {
		ordered = append(ordered, files[i])
	}
	records := make([]failureLogRecord, 0, len(ordered))
	for _, file := range ordered {
		entry, err := s.entry(file.name)
		if err != nil {
			continue
		}
		records = append(records, failureLogRecord{Name: file.name, Size: file.size, Time: entry.Time, RequestID: entry.RequestID,
			Duration: entry.Duration, Status: entry.Status, Model: entry.Request.Model, Route: entry.Request.Route,
			Channel: entry.Upstream.Channel, Kind: entry.Upstream.Kind, Summary: entry.Upstream.Summary})
	}
	return records, total
}

// entry loads one full record, including every request and response detail the
// detail view renders.
func (s *failureLogStore) entry(name string) (failureLogEntry, error) {
	var entry failureLogEntry
	// Only names this store produced may be read back; never join a client value.
	if s == nil || !failureLogFilename.MatchString(name) {
		return entry, os.ErrNotExist
	}
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		return entry, err
	}
	return entry, json.Unmarshal(data, &entry)
}

func (s *failureLogStore) remove(name string) error {
	if s == nil || !failureLogFilename.MatchString(name) {
		return os.ErrNotExist
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index := -1
	for i, file := range s.files {
		if file.name == name {
			index = i
			break
		}
	}
	if index < 0 {
		return os.ErrNotExist
	}
	if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.used -= s.files[index].size
	s.files = append(s.files[:index], s.files[index+1:]...)
	return nil
}

func (s *failureLogStore) clear() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for _, file := range s.files {
		if err := os.Remove(filepath.Join(s.dir, file.name)); err == nil || errors.Is(err, os.ErrNotExist) {
			removed++
		}
	}
	s.files = nil
	s.used = 0
	return removed
}

type failureLogEntry struct {
	Time      time.Time       `json:"time"`
	RequestID string          `json:"request_id"`
	Duration  int64           `json:"duration_ms"`
	Status    int             `json:"gateway_status"`
	Request   failureRequest  `json:"request"`
	Upstream  failureResponse `json:"upstream"`
	Truncated bool            `json:"truncated,omitempty"`
}

type failureRequest struct {
	Method    string            `json:"method"`
	Route     string            `json:"route"`
	Model     string            `json:"model"`
	Bytes     int               `json:"body_bytes"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      map[string]any    `json:"body,omitempty"`
	Raw       string            `json:"raw_body,omitempty"` // Set only when full content capture is on.
	Redacted  bool              `json:"redacted,omitempty"`
	Truncated bool              `json:"truncated,omitempty"`
}

type failureResponse struct {
	ChannelID int64  `json:"channel_id,omitempty"`
	Channel   string `json:"channel,omitempty"`
	Model     string `json:"model,omitempty"`
	Status    int    `json:"http_status,omitempty"`
	Kind      string `json:"failure_kind"`
	ErrorType string `json:"error_type"`
	Summary   string `json:"summary"`
	Body      string `json:"body,omitempty"`
	Truncated bool   `json:"body_truncated,omitempty"`
	Redacted  bool   `json:"body_redacted,omitempty"`
}

func (e failureLogEntry) marshal(limit int) ([]byte, error) {
	encode := func() ([]byte, bool) {
		b, err := json.Marshal(e)
		return b, err == nil && len(b) <= limit
	}
	if b, ok := encode(); ok {
		return b, nil
	}
	e.Truncated = true
	body := e.Upstream.Body
	e.Upstream.Body = ""
	e.Upstream.Truncated = e.Upstream.Truncated || body != ""
	// Keep both envelopes even at the smallest configured limit.
	if _, ok := encode(); !ok {
		e.Request.Body, e.Request.Headers = nil, nil
		e.Request.Truncated = true
	}
	best, ok := encode()
	if !ok {
		e.Request.Model = failurePrefix(e.Request.Model, 24)
		e.Upstream.Model = failurePrefix(e.Upstream.Model, 24)
		e.Upstream.Channel = failurePrefix(e.Upstream.Channel, 24)
		best, ok = encode()
		if !ok {
			return nil, fmt.Errorf("failure log metadata exceeds entry limit")
		}
	}
	// Account for actual JSON-escaped size rather than guessing from input bytes.
	for lo, hi := 1, len(body); lo <= hi; {
		mid := (lo + hi) / 2
		e.Upstream.Body = failurePrefix(body, mid)
		if b, fits := encode(); fits {
			best = b
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return best, nil
}

func failurePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Keep the unmodified client body only for the life of the relay. Parsing and
// redaction are deferred until failure, not performed on every successful call.
func (t *relayTask) initFailureLog(c *gin.Context, body []byte) {
	if failureLogs != nil {
		t.failureBody = body
		t.failureStartedAt = time.Now()
		t.traceID = randHex(16)
		c.Header(failureLogRequestHeader, t.traceID)
	}
}

func recordRelayFailure(c *gin.Context, task *relayTask, info relayInfo, status int, err error, key string) {
	s := failureLogs
	if s == nil || task.failureBody == nil {
		return
	}
	body := info.failureReply
	var payload *upstreamFailurePayload
	if body.data == nil && errors.As(err, &payload) {
		body = payload.body
	}
	kind := relayFailureKind(info, err)
	entry := failureLogEntry{
		Time: time.Now().UTC(), RequestID: task.traceID, Status: status, Duration: time.Since(task.failureStartedAt).Milliseconds(),
		Request: failureRequest{Method: c.Request.Method, Route: failurePrefix(c.FullPath(), 96), Model: failurePrefix(task.modelRequest(), 96), Bytes: len(task.failureBody)},
		Upstream: failureResponse{ChannelID: info.UpstreamID, Channel: failurePrefix(info.UpstreamName, 96), Model: failurePrefix(info.UpstreamModel, 96),
			Status: info.failureHTTPStatus, Kind: kind, ErrorType: kind, Summary: failureSummaryLine(info, kind, err)},
	}
	entry.Request.Headers = failureRequestHeaders(c.Request.Header)
	root, _ := decodeProtocolObject(task.failureBody)
	if s.fullContent {
		// Explicit admin opt-in: keep the diagnostics usable for real debugging.
		// The files are 0600 inside a 0700 directory; the UI warns about scope.
		entry.Request.Headers = failureRawHeaders(c.Request.Header)
		entry.Request.Body, entry.Request.Raw = failureRawBody(task.failureBody, s.entryBytes)
	} else {
		entry.Request.Body, entry.Request.Truncated = summarizeFailureRequest(root)
	}
	secrets := append([]string{key}, failureHeaderSecrets(c.Request.Header)...)
	secrets = append(secrets, info.failureSecrets...)
	// The summary carries a raw error string, which may quote a credential;
	// scrub the same literals before it reaches disk.
	if scrubbed := failureScrubText(entry.Upstream.Summary, secrets); scrubbed != "" {
		entry.Upstream.Summary = scrubbed
	}
	if s.fullContent {
		entry.Request.Redacted = false
		reply := body
		if reply.truncated || len(reply.data) > s.entryBytes {
			reply = failureReply{data: reply.data[:min(len(reply.data), s.entryBytes)], truncated: true}
		}
		entry.Upstream.Body = strings.ToValidUTF8(string(reply.data), "\uFFFD")
		entry.Upstream.Truncated = reply.truncated
	} else {
		entry.Upstream.setBody(body, root, secrets)
	}
	if len(entry.Upstream.Body) > s.entryBytes {
		entry.Upstream.Body = failurePrefix(entry.Upstream.Body, s.entryBytes)
		entry.Upstream.Truncated = true
	}
	if err := s.write(entry); err != nil {
		log.Printf("[failure-logs] request_id=%s: %v", task.traceID, err)
	}
}

func relayFailureKind(info relayInfo, err error) string {
	var conversion *protocolConversionError
	var toolLimit *upstreamToolLimitError
	var rateLimit *openCodeRateLimitError
	var timeout net.Error
	switch {
	case errors.Is(err, errDownstreamWrite), errors.Is(err, context.Canceled):
		return "client_disconnected"
	case errors.Is(err, errUpstreamFirstOutputTimeout), errors.Is(err, errUpstreamIdleTimeout), errors.As(err, &timeout) && timeout.Timeout():
		return "upstream_timeout"
	case errors.As(err, &conversion):
		return "protocol_conversion"
	case errors.As(err, &toolLimit):
		return "upstream_tool_limit"
	case errors.As(err, &rateLimit):
		return "upstream_rate_limit"
	case info.Status == http.StatusTooManyRequests:
		return "upstream_rate_limit"
	case info.Status != 0 && (info.Status < 200 || info.Status >= 300):
		return "upstream_http_error"
	case info.Status != 0:
		return "upstream_response_error"
	case info.UpstreamID != 0:
		return "upstream_unreachable"
	default:
		return "routing_failure"
	}
}

func relayFailureSummary(info relayInfo, kind string) string {
	if info.Status != 0 && (info.Status < 200 || info.Status >= 300) {
		return fmt.Sprintf("upstream returned %d", info.Status)
	}
	return kind
}

// failureSummaryLine is the human-readable one-liner shown in the console list.
// The raw error is kept because network failures (DNS, refused, TLS) are only
// diagnosable from its text; it is bounded and stripped of line breaks.
func failureSummaryLine(info relayInfo, kind string, err error) string {
	summary := relayFailureSummary(info, kind)
	if err == nil {
		return summary
	}
	detail := strings.Join(strings.Fields(err.Error()), " ")
	if detail == "" {
		return summary
	}
	if len(detail) > failureSummaryLimit {
		detail = failurePrefix(detail, failureSummaryLimit)
	}
	if summary == kind {
		return kind + ": " + detail
	}
	return summary + ": " + detail
}

const failureSummaryLimit = 400

// Read the original relay prefix first, then attempt a bounded diagnostic
// read. A provider that stalls after the prefix must not delay failover by a
// second full idle timeout (or indefinitely when idle timeouts are disabled).
func readFailureReply(r io.Reader, relayLimit int, cancel context.CancelFunc) ([]byte, failureReply) {
	b, err := io.ReadAll(io.LimitReader(r, int64(relayLimit)))
	if failureLogs == nil {
		return b, failureReply{}
	}
	if err != nil || len(b) < relayLimit {
		return b, capFailureReply(b, err != nil)
	}
	limit := failureCaptureLimit()
	timerDone := make(chan struct{})
	timer := time.AfterFunc(time.Second, func() {
		defer close(timerDone)
		cancel()
	})
	tail, err := io.ReadAll(io.LimitReader(r, int64(limit+1-len(b))))
	timedOut := !timer.Stop()
	if timedOut {
		<-timerDone
	}
	reply := append(b, tail...)
	return b, capFailureReply(reply, timedOut || err != nil)
}

// Allow small entries to sanitize a complete JSON error before clipping the
// serialized record. The extra diagnostic reads remain bounded.
func failureCaptureLimit() int { return max(64<<10, failureLogs.entryBytes) }

type failureReply struct {
	data      []byte
	truncated bool
}

func capFailureReply(body []byte, truncated bool) failureReply {
	limit := min(failureCaptureLimit(), len(body))
	return failureReply{bytes.Clone(body[:limit]), truncated || len(body) > limit}
}

// Capture only bytes the audio relay actually reads, without altering its
// downstream status, body, or buffering behavior.
type failureReplyReader struct {
	io.ReadCloser
	limit int
	reply failureReply
}

func (r *failureReplyReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	keep := min(n, r.limit-len(r.reply.data))
	r.reply.data = append(r.reply.data, p[:keep]...)
	r.reply.truncated = r.reply.truncated || keep < n
	return n, err
}

type upstreamFailurePayload struct {
	cause error
	body  failureReply
}

func (p *upstreamFailurePayload) Error() string { return p.cause.Error() }
func (p *upstreamFailurePayload) Unwrap() error { return p.cause }

func withFailurePayload(err error, payload string) error {
	if failureLogs == nil {
		return err
	}
	limit := min(len(payload), failureCaptureLimit())
	return &upstreamFailurePayload{err, failureReply{[]byte(payload[:limit]), limit < len(payload)}}
}

func upstreamFailureSecrets(up *Upstream, req *http.Request) []string {
	secrets := []string{up.APIKey}
	for _, key := range up.APIKeys {
		secrets = append(secrets, key.APIKey)
	}
	return append(secrets, failureHeaderSecrets(req.Header)...)
}

func failureHeaderSecrets(headers http.Header) []string {
	var secrets []string
	for _, name := range []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "X-Goog-Api-Key", "Cookie"} {
		secrets = append(secrets, headers.Values(name)...)
	}
	return secrets
}
