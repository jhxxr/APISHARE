package main

import (
	"database/sql"
	"net/http"
	"strings"
	"sync"
	"time"
)

const routingSnapshotTTL = 30 * time.Second

type upstreamConfig struct {
	modelsRaw, mapRaw, headersRaw string
	models                        map[string]bool
	modelMap                      map[string]string
	headerRules                   []headerRule
	headerError                   error
}

func compileUpstreamConfig(up *Upstream) {
	config := &upstreamConfig{modelsRaw: up.Models, mapRaw: up.ModelMap, headersRaw: up.HeaderOverrides}
	if strings.TrimSpace(up.Models) != "" {
		config.models = make(map[string]bool)
		for _, model := range strings.Split(up.Models, ",") {
			config.models[strings.TrimSpace(model)] = true
		}
	}
	config.modelMap, _ = parseModelMap(up.ModelMap)
	config.headerRules, config.headerError = parseHeaderOverrides(up.HeaderOverrides)
	up.config = config
}

func (up *Upstream) applyHeaders(req *http.Request, incoming http.Header) error {
	config := up.config
	var rules []headerRule
	if config == nil || config.headersRaw != up.HeaderOverrides {
		var err error
		rules, err = parseHeaderOverrides(up.HeaderOverrides)
		if err != nil {
			return err
		}
	} else {
		if config.headerError != nil {
			return config.headerError
		}
		rules = config.headerRules
	}
	if up.Type == "opencode" {
		return applyOpenCodeHeaderRules(req, rules, up.APIKey, incoming)
	}
	return applyHeaderRules(req, rules, up.APIKey, incoming)
}

type routingSnapshot struct {
	database  *sql.DB
	expires   time.Time
	upstreams []Upstream
	byID      map[int64]Upstream
}

var routingCache struct {
	sync.RWMutex
	snapshot *routingSnapshot
}

// Snapshots are immutable. A mutation invalidates the whole snapshot after its
// commit, so requests admitted after an admin save see the new configuration.
func getRoutingSnapshot() (*routingSnapshot, error) {
	routingCache.RLock()
	snapshot := routingCache.snapshot
	if snapshot != nil && snapshot.database == db && time.Now().Before(snapshot.expires) {
		routingCache.RUnlock()
		return snapshot, nil
	}
	routingCache.RUnlock()
	routingCache.Lock()
	defer routingCache.Unlock()
	snapshot = routingCache.snapshot
	if snapshot != nil && snapshot.database == db && time.Now().Before(snapshot.expires) {
		return snapshot, nil
	}
	ups, err := dbListUpstreams()
	if err != nil {
		return nil, err
	}
	snapshot = &routingSnapshot{database: db, expires: time.Now().Add(routingSnapshotTTL), upstreams: ups, byID: make(map[int64]Upstream, len(ups))}
	for i := range snapshot.upstreams {
		up := &snapshot.upstreams[i]
		compileUpstreamConfig(up)
		snapshot.byID[up.ID] = *up
	}
	routingCache.snapshot = snapshot
	return snapshot, nil
}

func cachedRoutingUpstream(id int64) (*Upstream, error) {
	snapshot, err := getRoutingSnapshot()
	if err != nil {
		return nil, err
	}
	up, ok := snapshot.byID[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return &up, nil // Return a value copy, keeping shared configuration immutable.
}

func invalidateRoutingSnapshot() {
	routingCache.Lock()
	routingCache.snapshot = nil
	routingCache.Unlock()
}
