package main

import (
	"context"
	"database/sql"
	"sync"
	"time"
)

const publicOverviewTTL = 5 * time.Second

type publicOverviewEntry struct {
	database *sql.DB
	done     chan struct{}
	body     []byte
	err      error
	expires  time.Time
}

// Cache encoded anonymous snapshots by resolution. Only the builder uses the
// database; simultaneous visitors share its result and JSON serialization.
type publicOverviewCache struct {
	sync.Mutex
	entries map[string]*publicOverviewEntry
}

var publicOverviews publicOverviewCache

func (cache *publicOverviewCache) get(ctx context.Context, database *sql.DB, interval string, build func() ([]byte, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cache.Lock()
	if entry := cache.entries[interval]; entry != nil && entry.database == database {
		if entry.expires.IsZero() {
			cache.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-entry.done:
				return entry.body, entry.err
			}
		}
		if time.Now().Before(entry.expires) {
			cache.Unlock()
			return entry.body, entry.err
		}
	}
	entry := &publicOverviewEntry{database: database, done: make(chan struct{})}
	if cache.entries == nil {
		cache.entries = make(map[string]*publicOverviewEntry)
	}
	cache.entries[interval] = entry
	cache.Unlock()

	body, err := build()
	cache.Lock()
	entry.body, entry.err = body, err
	entry.expires = time.Now().Add(publicOverviewTTL)
	if err != nil && cache.entries[interval] == entry {
		delete(cache.entries, interval) // Failed snapshots never become cache hits.
	}
	close(entry.done)
	cache.Unlock()
	return body, err
}

func invalidatePublicOverviews() {
	publicOverviews.Lock()
	publicOverviews.entries = nil
	publicOverviews.Unlock()
}
