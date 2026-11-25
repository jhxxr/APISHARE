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

// Failure diagnostics are configured from the admin console, never from the
// environment, so operators can change retention and redaction without a restart.
const failureLogSettingsKey = "failure_logs"

const (
	failureLogDefaultMaxMB    = 32
	failureLogDefaultEntryKiB = 64
	// Full-content capture keeps whole payloads, so it needs more room to be
	// useful: at 64 KiB a normal prompt alone fills the file.
	failureLogFullContentEntryKiB = 256
	failureLogMaxMB               = 16384
	failureLogMaxEntryKiB         = 1024
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
	if s.EntryKiB < 1 || s.EntryKiB > failureLogMaxEntryKiB {
		s.EntryKiB = s.defaultEntryKiB()
	}
	return s
}

// defaultEntryKiB is the per-record budget implied by the capture mode.
func (s failureLogSettings) defaultEntryKiB() int {
	if s.FullContent {
		return failureLogFullContentEntryKiB
	}
	return failureLogDefaultEntryKiB
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

var failureLogs *failureLogStore // Written only under failureLogsMu.
var failureLogsMu sync.RWMutex   // Guards failureLogs against settings swaps.

// activeFailureLogs returns the live store. The handle is replaced when the
// settings change, so never cache it for the lifetime of a request.
func activeFailureLogs() *failureLogStore {
	failureLogsMu.RLock()
	defer failureLogsMu.RUnlock()
	return failureLogs
}

// setFailureLogs swaps the store under the same lock that guards readers.
func setFailureLogs(s *failureLogStore) {
	failureLogsMu.Lock()
	defer failureLogsMu.Unlock()
	failureLogs = s
}

func failureLogsEnabled() bool { return activeFailureLogs() != nil }

// applyFailureLogSettings rebuilds the store so a retention change takes effect
// immediately. Disabling only stops new records: the live handle is cleared so
// the relay stops writing, but the existing files stay readable through the
// read-only path (see failureLogStoreForReading).
func applyFailureLogSettings(settings failureLogSettings) error {
	settings = settings.normalized()
	if !settings.Enabled {
		setFailureLogs(nil)
		return nil
	}
	s, err := openFailureLogStore(failureLogDir(), int64(settings.MaxMB)<<20, settings.EntryKiB<<10)
	if err != nil {
		return err
	}
	s.fullContent = settings.FullContent
	setFailureLogs(s)
	return nil
}

// failureLogStoreForReading resolves the store for read/delete APIs. When
// recording is disabled there is no live handle, so a store is opened over the
// existing directory without ever being published as the write target. Records
// stay browsable and deletable until retention removes them.
func failureLogStoreForReading() *failureLogStore {
	if s := activeFailureLogs(); s != nil {
		return s
	}
	settings := readFailureLogSettings()
	s, err := openFailureLogStore(failureLogDir(), int64(settings.MaxMB)<<20, settings.EntryKiB<<10)
	if err != nil {
		return nil
	}
	return s
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
	// TruncatedReason explains why details were shortened, so the console can
	// say "configured limit" instead of silently showing a partial record.
	TruncatedReason string `json:"truncated_reason,omitempty"`
}

// failureTruncatedEntryLimit marks records shrunk to fit EntryKiB.
const failureTruncatedEntryLimit = "entry_limit"

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
	// RawTruncated records that raw_body itself was cut to fit the entry limit.
	RawTruncated bool `json:"raw_body_truncated,omitempty"`
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

// marshal fits the record into one stored file.
//
// Bodies are the only compressible part, so they share the budget instead of
// competing: the request body (what the caller sent) gets requestBodyShare and
// the upstream reply gets the rest. Both are shortened by binary search over
// the real JSON size, because escaping inflates the encoded bytes. Metadata is
// never dropped silently: the last resort keeps the failure visible with only
// its envelope, so a large payload can no longer make a failure vanish.
func (e failureLogEntry) marshal(limit int) ([]byte, error) {
	encode := func() ([]byte, bool) {
		b, err := json.Marshal(e)
		return b, err == nil && len(b) <= limit
	}
	if b, ok := encode(); ok {
		return b, nil
	}

	// Everything except the bodies is fixed overhead; measure it once with the
	// bodies emptied so the split below works on the real remaining budget.
	requestBody, requestRaw := e.Request.Body, e.Request.Raw
	upstreamBody := e.Upstream.Body
	e.Request.Body, e.Request.Raw, e.Upstream.Body = nil, "", ""
	e.Truncated = true
	// Reserve the marker up front: it is written with the record, so leaving it
	// out of the fixed cost would make the final encode overflow by its length.
	e.TruncatedReason = failureTruncatedEntryLimit
	fixed, ok := encode()
	if !ok {
		// Metadata alone does not fit: shrink the free-text fields, then give up
		// on descriptions rather than on the record itself.
		e.Request.Model = failurePrefix(e.Request.Model, 24)
		e.Upstream.Model = failurePrefix(e.Upstream.Model, 24)
		e.Upstream.Channel = failurePrefix(e.Upstream.Channel, 24)
		e.Upstream.Summary = failurePrefix(e.Upstream.Summary, failureSummaryLimit)
		if fixed, ok = encode(); !ok {
			e.Request.Headers = nil
			e.Request.Method = failurePrefix(e.Request.Method, 8)
			e.Request.Route = failurePrefix(e.Request.Route, 48)
			e.Upstream.Summary = failurePrefix(e.Upstream.Summary, 120)
			e.Upstream.Kind = failurePrefix(e.Upstream.Kind, 32)
			e.Upstream.ErrorType = failurePrefix(e.Upstream.ErrorType, 32)
			e.Request.Model = failurePrefix(e.Request.Model, 12)
			e.Upstream.Model = failurePrefix(e.Upstream.Model, 12)
			e.Upstream.Channel = failurePrefix(e.Upstream.Channel, 12)
			if fixed, ok = encode(); !ok {
				return nil, fmt.Errorf("failure log metadata exceeds entry limit")
			}
		}
	}

	budget := limit - len(fixed)
	if budget <= 0 {
		// Not even a shortened body fits; keep the metadata-only record but
		// report every body as dropped instead of leaving stale flags.
		e.Upstream.Truncated = upstreamBody != ""
		e.Request.RawTruncated = requestRaw != ""
		e.Request.Truncated = requestBody != nil
		if b, ok := encode(); ok {
			return b, nil
		}
		return fixed, nil
	}
	requestBudget := shareFailureBudget(budget)
	// Headers were measured inside the fixed part; drop them when the bodies
	// need the room, since they are the least useful detail after a big body.
	if requestBudget <= 0 && e.Request.Headers != nil {
		e.Request.Headers = nil
		if b, ok := encode(); ok && len(b) < len(fixed) {
			fixed, budget = b, limit-len(b)
			requestBudget = shareFailureBudget(budget)
		}
	}

	best := fixed
	grew := false
	if requestRaw != "" {
		// Raw bodies are one opaque blob: binary search the longest prefix that
		// fits the request share, then record whether anything was cut.
		e.Request.RawTruncated = true
		lo, hi := 0, len(requestRaw)
		fit := 0
		for lo <= hi {
			mid := (lo + hi) / 2
			e.Request.Raw = failurePrefix(requestRaw, mid)
			e.Request.RawTruncated = len(e.Request.Raw) < len(requestRaw)
			if b, ok := encode(); ok {
				best, grew, fit = b, true, mid
				lo = mid + 1
			} else {
				hi = mid - 1
			}
		}
		e.Request.Raw = failurePrefix(requestRaw, fit)
		e.Request.RawTruncated = len(e.Request.Raw) < len(requestRaw)
	} else if requestBody != nil {
		// Keep the scaled body on the entry so the final encode emits it; the
		// fitted bytes are only a snapshot of one intermediate attempt. The
		// budget is the whole limit: fitJSONValue encodes the complete record, so
		// the request share alone would let it overrun the file size.
		var kept map[string]any
		if fitted, ok := fitJSONValue(requestBody, func(v map[string]any) ([]byte, bool) {
			kept = v
			e.Request.Body = v
			return encode()
		}, budget); ok {
			best, grew = fitted, true
			e.Request.Body = kept
		}
	}
	if upstreamBody != "" {
		// Assume truncation until proven otherwise, so the fallback snapshot
		// never claims a full body it does not hold.
		e.Upstream.Truncated = true
		lo, hi := 0, len(upstreamBody)
		fit := 0
		for lo <= hi {
			mid := (lo + hi) / 2
			e.Upstream.Body = failurePrefix(upstreamBody, mid)
			// Set the flag before encoding so the kept snapshot carries it; a
			// later assignment would only patch the final attempt.
			e.Upstream.Truncated = len(e.Upstream.Body) < len(upstreamBody)
			if b, ok := encode(); ok {
				best, grew, fit = b, true, mid
				lo = mid + 1
			} else {
				hi = mid - 1
			}
		}
		e.Upstream.Body = failurePrefix(upstreamBody, fit)
		e.Upstream.Truncated = len(e.Upstream.Body) < len(upstreamBody)
	}
	if !grew && (requestRaw != "" || requestBody != nil || upstreamBody != "") {
		e.Request.Body, e.Request.Raw, e.Upstream.Body = nil, "", ""
	}
	// Flags follow the bytes that actually survived, so the stored record never
	// claims a body it dropped.
	e.Request.RawTruncated = len(e.Request.Raw) < len(requestRaw)
	e.Request.Truncated = requestBody != nil && e.Request.Body == nil
	e.Upstream.Truncated = len(e.Upstream.Body) < len(upstreamBody)
	// The flags consume a few bytes that were not part of the split, so trim the
	// bodies until the annotated record fits instead of dropping to an older,
	// unflagged snapshot.
	for i := 0; i < 64; i++ {
		b, ok := encode()
		if ok {
			return b, nil
		}
		if !trimFailureBodies(&e) {
			break
		}
	}
	if b, ok := encode(); ok {
		return b, nil
	}
	return best, nil
}

// trimFailureBodies shaves the largest remaining body and reports whether
// anything could still be removed. The request side is trimmed last: it is the
// only place the caller's payload is recorded, so dropping it is a last resort.
func trimFailureBodies(e *failureLogEntry) bool {
	switch {
	case len(e.Upstream.Body) > 0:
		e.Upstream.Body = failurePrefix(e.Upstream.Body, len(e.Upstream.Body)*15/16)
		return true
	case len(e.Request.Raw) > 0:
		e.Request.Raw = failurePrefix(e.Request.Raw, len(e.Request.Raw)*15/16)
		return true
	case e.Request.Body != nil:
		if b, ok := shrinkJSONStrings(e.Request.Body, 15); ok {
			e.Request.Body = b
			return true
		}
		e.Request.Body = nil
		return true
	case e.Request.Headers != nil:
		e.Request.Headers = nil
		return true
	}
	return false
}

// failureShrinkMinChars is the shortest field worth shortening; below this the
// text is an identifier or enum that must stay readable.
const failureShrinkMinChars = 64

// shrinkJSONStrings cuts every string leaf in body to num/16 of its length and
// reports whether anything was actually shortened. It keeps the request's
// structure intact instead of discarding the whole payload at the limit.
func shrinkJSONStrings(body map[string]any, num int) (map[string]any, bool) {
	shrunk := false
	scaled := scaleJSONFraction(body, num, &shrunk)
	if result, ok := scaled.(map[string]any); ok && shrunk {
		return result, true
	}
	return body, false
}

// scaleJSONFraction recursively shortens string leaves by num/16.
func scaleJSONFraction(value any, num int, shrunk *bool) any {
	switch typed := value.(type) {
	case map[string]any:
		clone := make(map[string]any, len(typed))
		for k, v := range typed {
			clone[k] = scaleJSONFraction(v, num, shrunk)
		}
		return clone
	case []any:
		clone := make([]any, len(typed))
		for i, v := range typed {
			clone[i] = scaleJSONFraction(v, num, shrunk)
		}
		return clone
	case string:
		// Short leaves are field values (model name, role, flags). Shrinking
		// them to nothing destroys meaning for almost no space, so only
		// substantial text is reduced.
		if len(typed) < failureShrinkMinChars {
			return typed
		}
		*shrunk = true
		return failurePrefix(typed, len(typed)*num/16)
	default:
		return value
	}
}

// requestBodyShare keeps most of the budget for the caller's own payload: the
// upstream status is already visible in the call log, while the request body is
// only recorded here.
const requestBodyShare = 6 // out of 10

// shareFailureBudget returns the request body's share of the free space. The
// upstream reply is encoded after the request, so it takes whatever is left.
func shareFailureBudget(budget int) int {
	if budget <= 0 {
		return 0
	}
	return budget * requestBodyShare / 10
}

// fitJSONValue returns the encoded record with the largest prefix of value that
// fits. Payloads are nested (messages[].content, tool results, attachments), so
// a flat scan over top-level keys would keep nothing; instead every string leaf
// is scaled by one shared factor, which preserves the request's shape and keeps
// the beginning of each field. That is what a truncated record should look like:
// same structure, earlier content.
func fitJSONValue(value map[string]any, encode func(map[string]any) ([]byte, bool), budget int) ([]byte, bool) {
	if budget <= 0 {
		return nil, false
	}
	original := deepCopyJSON(value)
	var best []byte
	fits := false
	lo, hi := 0, 1000 // per-mille of every string leaf kept
	for lo <= hi {
		mid := (lo + hi) / 2
		scaled, _ := scaleJSON(value, original, mid).(map[string]any)
		if scaled == nil {
			break
		}
		if b, ok := encode(scaled); ok {
			best, fits = b, true
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return best, fits
}

// deepCopyJSON clones a decoded body so scaling never mutates the caller's copy.
func deepCopyJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		clone := make(map[string]any, len(typed))
		for k, v := range typed {
			clone[k] = deepCopyJSON(v)
		}
		return clone
	case []any:
		clone := make([]any, len(typed))
		for i, v := range typed {
			clone[i] = deepCopyJSON(v)
		}
		return clone
	default:
		return typed
	}
}

// scaleJSON returns a copy of value where every string leaf keeps the first
// permille of its original length. Numbers, booleans and nulls stay intact so
// the record still shows parameter values.
func scaleJSON(value any, original any, permille int) any {
	switch typed := value.(type) {
	case map[string]any:
		source, ok := original.(map[string]any)
		if !ok {
			return value
		}
		clone := make(map[string]any, len(typed))
		for k, v := range typed {
			clone[k] = scaleJSON(v, source[k], permille)
		}
		return clone
	case []any:
		source, ok := original.([]any)
		if !ok {
			return value
		}
		clone := make([]any, len(typed))
		for i, v := range typed {
			if i < len(source) {
				clone[i] = scaleJSON(v, source[i], permille)
			} else {
				clone[i] = v
			}
		}
		return clone
	case string:
		source, ok := original.(string)
		if !ok || permille >= 1000 {
			return typed
		}
		// Keep identifiers and enums (model names, roles, tool types): shortening
		// them to nothing saves a few bytes and loses what the record is about.
		if len(source) < failureShrinkMinChars {
			return typed
		}
		return failurePrefix(source, len(source)*permille/1000)
	default:
		return value
	}
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
	if failureLogsEnabled() {
		t.failureBody = body
		t.failureStartedAt = time.Now()
		t.traceID = randHex(16)
		c.Header(failureLogRequestHeader, t.traceID)
	}
}

func recordRelayFailure(c *gin.Context, task *relayTask, info relayInfo, status int, err error, key string) {
	s := activeFailureLogs()
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
	if !failureLogsEnabled() {
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
func failureCaptureLimit() int { return max(64<<10, failureLogEntryBytes()) }

// failureLogEntryBytes reads the per-entry limit from the live store, falling
// back to the default when recording is disabled mid-flight.
func failureLogEntryBytes() int {
	if s := activeFailureLogs(); s != nil {
		return s.entryBytes
	}
	return failureLogDefaultEntryKiB << 10
}

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
	if !failureLogsEnabled() {
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
