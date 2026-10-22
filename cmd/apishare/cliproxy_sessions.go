package main

import "time"

func expireCoreLogin(state string, ttl time.Duration) {
	time.AfterFunc(ttl, func() {
		coreLoginSessions.Lock()
		session, exists := coreLoginSessions.items[state]
		if exists && !time.Now().Before(session.expires) {
			delete(coreLoginSessions.items, state)
		}
		coreLoginSessions.Unlock()
		if exists && session.worker != nil && !time.Now().Before(session.expires) {
			session.worker.DiscardLogin()
		}
	})
}
