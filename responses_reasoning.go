package main

import (
	"container/list"
	"sync"
	"time"
)

// Replay is private to the gateway key, model and selected upstream. Bound both
// memory and lifetime; opaque reasoning from another provider is never decoded.
type responseReplayScope struct {
	KeyID, UpstreamID    int64
	Model, UpstreamModel string
}

type responseReasoningKey struct {
	Scope responseReplayScope
	ID    string
}

type responseReasoningEntry struct {
	Key     responseReasoningKey
	Text    string
	Expires time.Time
}

const responseReasoningTTL = 7 * 24 * time.Hour
const responseReasoningMaxBytes = 8 << 20
const responseReasoningMaxItems = 1024

var responseReasoningCache = struct {
	sync.Mutex
	Items map[responseReasoningKey]*list.Element
	Order *list.List
	Bytes int
}{Items: make(map[responseReasoningKey]*list.Element), Order: list.New()}

func pruneResponseReasoning(now time.Time) {
	cache := &responseReasoningCache
	for cache.Order.Len() > 0 {
		e := cache.Order.Front()
		entry := e.Value.(responseReasoningEntry)
		if now.Before(entry.Expires) && cache.Order.Len() <= responseReasoningMaxItems && cache.Bytes <= responseReasoningMaxBytes {
			break
		}
		delete(cache.Items, entry.Key)
		cache.Bytes -= len(entry.Text)
		cache.Order.Remove(e)
	}
}

func cacheResponseReasoning(scope responseReplayScope, id, text string) {
	if id == "" || len(id) > 256 || len(scope.Model) > 512 || len(scope.UpstreamModel) > 512 || text == "" || len(text) > 1<<20 {
		return
	}
	cache := &responseReasoningCache
	cache.Lock()
	defer cache.Unlock()
	key := responseReasoningKey{scope, id}
	if e := cache.Items[key]; e != nil {
		cache.Bytes -= len(e.Value.(responseReasoningEntry).Text)
		cache.Order.Remove(e)
	}
	now := time.Now()
	cache.Items[key] = cache.Order.PushBack(responseReasoningEntry{key, text, now.Add(responseReasoningTTL)})
	cache.Bytes += len(text)
	pruneResponseReasoning(now)
}

func lookupResponseReasoning(scope responseReplayScope, id string) string {
	cache := &responseReasoningCache
	cache.Lock()
	defer cache.Unlock()
	pruneResponseReasoning(time.Now())
	if e := cache.Items[responseReasoningKey{scope, id}]; e != nil {
		entry := e.Value.(responseReasoningEntry)
		entry.Expires = time.Now().Add(responseReasoningTTL)
		e.Value = entry
		cache.Order.MoveToBack(e)
		return entry.Text
	}
	return ""
}

func replayResponseReasoning(messages []canonMsg, scope responseReplayScope) []canonMsg {
	// A retry must not reuse reasoning fetched for the previous upstream.
	out := append([]canonMsg(nil), messages...)
	lastTurnReasoning := ""
	for i := range out {
		msg := &out[i]
		if msg.Role == "user" {
			lastTurnReasoning = ""
		}
		if msg.Role != "assistant" {
			continue
		}
		for _, id := range msg.ReasoningIDs {
			if msg.Reasoning != "" {
				cacheResponseReasoning(scope, id, msg.Reasoning)
			} else {
				msg.Reasoning = lookupResponseReasoning(scope, id)
			}
		}
		if msg.Reasoning == "" {
			// Cache lookup takes precedence over the previous span. A miss may
			// reuse reasoning from this user turn, but must not cache it under a
			// different opaque item's ID as though it were that item's plaintext.
			msg.Reasoning = lastTurnReasoning
		} else {
			lastTurnReasoning = msg.Reasoning
		}
	}
	return out
}

func cacheResponseOutputReasoning(scope responseReplayScope, response map[string]any) {
	for _, raw := range array(response["output"]) {
		item := object(raw)
		if str(item["type"]) == "reasoning" {
			cacheResponseReasoning(scope, str(item["id"]), contentText(item["summary"]))
		}
	}
}
