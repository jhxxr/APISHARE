package main

// Keep existing channel and billing records, but never send traffic to removed types.
func disableRemovedDecisionUpstreams() error {
	_, err := db.Exec(`UPDATE upstreams SET enabled=0 WHERE enabled<>0 AND type IN ('openrouter','systemone','cloudflare')`)
	return err
}
