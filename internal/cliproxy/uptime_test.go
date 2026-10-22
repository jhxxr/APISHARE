package cliproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestAccountUptimeRollingWindowAndPersistence(t *testing.T) {
	root := t.TempDir()
	recorder, err := openAccountUptimeRecorder(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(time.Now().UnixMilli()).UTC()
	for i := 0; i < 9; i++ {
		if err := recorder.record(now.Add(-23*time.Hour), "a.json", true); err != nil {
			t.Fatal(err)
		}
	}
	for _, sample := range []struct {
		at      time.Time
		id      string
		success bool
	}{
		{now, "a.json", false},
		{now.Add(-24*time.Hour - time.Millisecond), "a.json", false},
		{now.Add(time.Millisecond), "a.json", false},
		{now.Add(-time.Hour), "b.json", false},
	} {
		if err := recorder.record(sample.at, sample.id, sample.success); err != nil {
			t.Fatal(err)
		}
	}
	if err := recorder.db.Close(); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(root, 18317)
	if err != nil {
		t.Fatal(err)
	}
	history, err := manager.AccountUptimes(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	a, b := history["a.json"], history["b.json"]
	if a == nil || a.Calls != 10 || a.Successes != 9 || a.Failures != 1 || a.UptimePercent == nil || *a.UptimePercent != 90 {
		t.Fatalf("request-weighted 24h uptime = %+v", a)
	}
	if b == nil || b.Calls != 1 || b.UptimePercent == nil || *b.UptimePercent != 0 {
		t.Fatalf("another account's failures were lost: %+v", b)
	}
	var calls, successes int64
	for _, hour := range a.History {
		calls += hour.Calls
		successes += hour.Successes
		if hour.Calls == 0 && hour.UptimePercent != nil {
			t.Fatal("an idle hour was represented as uptime")
		}
	}
	if len(a.History) != 24 || calls != a.Calls || successes != a.Successes || a.History[23].Calls != 1 {
		t.Fatalf("hourly timeline dropped the latest outcome: %+v", a)
	}
	credentials := []Credential{{ID: "a.json", Name: "a.json"}, {ID: "idle.json", Name: "idle.json"}}
	if err := manager.AttachAccountUptimes(context.Background(), credentials, now); err != nil {
		t.Fatal(err)
	}
	idle := credentials[1].Uptime
	if idle == nil || idle.UptimePercent != nil || idle.Calls != 0 || len(idle.History) != 24 {
		t.Fatalf("idle account = %+v", idle)
	}
	other, _ := NewManager(t.TempDir(), 18318)
	if err := other.AttachAccountUptimes(context.Background(), credentials, now); err != nil {
		t.Fatal(err)
	}
	if credentials[0].Uptime.Calls != 0 {
		t.Fatal("same credential filename inherited another pool's history")
	}
	// A stopped pool can still expose safe account fields and stored history.
	if err := manager.SeedCredentials(map[string]json.RawMessage{"a.json": json.RawMessage(`{"type":"codex","email":"a@example.invalid","access_token":"PRIVATE-TOKEN","disabled":true}`)}); err != nil {
		t.Fatal(err)
	}
	offline, err := manager.Credentials(context.Background())
	if err != nil || len(offline) != 1 {
		t.Fatalf("offline accounts: %+v %v", offline, err)
	}
	if err := manager.AttachAccountUptimes(context.Background(), offline, now); err != nil || offline[0].Uptime.Calls != 10 {
		t.Fatalf("offline history: %+v %v", offline, err)
	}
	raw, _ := json.Marshal(offline)
	if bytes.Contains(raw, []byte("PRIVATE-TOKEN")) {
		t.Fatal("offline account history exposed credential secrets")
	}
}

func TestAccountUptimeIgnoresNonGenerationAndCanceledCalls(t *testing.T) {
	root := t.TempDir()
	recorder, err := openAccountUptimeRecorder(root)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.db.Close()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, fixture := range []struct {
		ctx    context.Context
		result coreauth.Result
	}{
		{context.Background(), coreauth.Result{AuthID: "a.json", Success: true, SkipQuotaObservation: true}},
		{canceled, coreauth.Result{AuthID: "a.json", Success: false}},
		{context.Background(), coreauth.Result{AuthID: "a.json", Error: &coreauth.Error{HTTPStatus: 499}}},
		{context.Background(), coreauth.Result{AuthID: "a.json", Error: &coreauth.Error{Message: `Post "https://provider.invalid": context canceled`}}},
		{context.Background(), coreauth.Result{Success: true}},
	} {
		if got := recorder.ApplyResultPolicy(fixture.ctx, fixture.result); !reflect.DeepEqual(got, fixture.result) {
			t.Fatal("collecting uptime modified the SDK result")
		}
	}
	recorder.ApplyResultPolicy(context.Background(), coreauth.Result{AuthID: "a.json", Success: true, AccessToken: "PRIVATE-ACCESS"})
	manager, _ := NewManager(root, 18317)
	history, err := manager.AccountUptimes(context.Background(), time.Now())
	if err != nil || len(history) != 1 || history["a.json"].Calls != 1 || *history["a.json"].UptimePercent != 100 {
		t.Fatalf("non-generation events affected uptime: %+v %v", history, err)
	}
	var columns int
	if err := recorder.db.QueryRow(`SELECT count(*) FROM pragma_table_info('account_results')`).Scan(&columns); err != nil || columns != 4 {
		t.Fatalf("unexpected outcome data stored: %d %v", columns, err)
	}
}

func TestAccountUptimeRetainsOnlyRecentOutcomes(t *testing.T) {
	root := t.TempDir()
	recorder, err := openAccountUptimeRecorder(root)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.db.Close()
	now := time.Now().UTC()
	for _, at := range []time.Time{now.Add(-49 * time.Hour), now.Add(-47 * time.Hour), now.Add(-time.Minute)} {
		if err := recorder.record(at, "a.json", true); err != nil {
			t.Fatal(err)
		}
	}
	if err := recorder.prune(now); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := recorder.db.QueryRow(`SELECT count(*) FROM account_results`).Scan(&remaining); err != nil || remaining != 2 {
		t.Fatalf("retention: %d %v", remaining, err)
	}
	manager, _ := NewManager(root, 18317)
	history, err := manager.AccountUptimes(context.Background(), now)
	if err != nil || history["a.json"].Calls != 1 || math.Abs(*history["a.json"].UptimePercent-100) > 1e-9 {
		t.Fatalf("older retained samples entered the 24h window: %+v %v", history, err)
	}
}
