package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"apishare/internal/cliproxy"
	"github.com/gin-gonic/gin"
)

type jobRoundTripper func(*http.Request) (*http.Response, error)

func (f jobRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func jobTestPool(t *testing.T, router *gin.Engine, name string) (int64, *cliproxy.Manager) {
	t.Helper()
	response := proxyAdminRequest(router, "POST", "upstreams", fmt.Sprintf(`{"name":%q,"type":"cliproxy"}`, name), true)
	var result struct{ ID int64 }
	json.Unmarshal(response.Body.Bytes(), &result)
	if response.Code != 200 || result.ID == 0 {
		t.Fatalf("create pool: %d %s", response.Code, response.Body.String())
	}
	manager, err := cliProxyPool(result.ID)
	if err != nil {
		t.Fatal(err)
	}
	return result.ID, manager
}
func waitPoolJob(t *testing.T, id string, check func(poolJob) bool) poolJob {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		job, err := readPoolJob(id)
		if err == nil && check(job) {
			return job
		}
		time.Sleep(25 * time.Millisecond)
	}
	job, _ := readPoolJob(id)
	t.Fatalf("job did not settle: %+v", job)
	return job
}
func TestPoolBackgroundJobsSurviveRequestAndWorkerRestart(t *testing.T) {
	router := testProxyAdminRouter(t)
	withTestCLIProxyPools(t)
	id, manager := jobTestPool(t, router, "background source")
	other, _ := jobTestPool(t, router, "other")
	ctx := context.Background()
	for _, name := range []string{"one.json", "two.json", "three.json"} {
		raw := json.RawMessage(fmt.Sprintf(`{"type":"codex","email":%q,"access_token":"PRIVATE-TOKEN","disabled":true}`, name+"@example.invalid"))
		if err := importPoolCredential(ctx, manager, id, name, raw, 0); err != nil {
			t.Fatal(err)
		}
	}
	original := http.DefaultTransport
	defer func() { stopPoolJobs(); http.DefaultTransport = original }()
	entered := make(chan struct{})
	transportCanceled := make(chan struct{})
	var once sync.Once
	http.DefaultTransport = jobRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == "PATCH" && strings.HasSuffix(r.URL.Path, "/credentials/fields") {
			once.Do(func() { close(entered) })
			<-r.Context().Done()
			select {
			case <-transportCanceled:
			default:
				close(transportCanceled)
			}
			return nil, r.Context().Err()
		}
		return original.RoundTrip(r)
	})
	base := fmt.Sprintf("upstreams/%d/cliproxy/", id)
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	request := httptest.NewRequest("POST", adminPath+"/api/"+base+"jobs", strings.NewReader(`{"action":"enable","names":["one.json","two.json","three.json"]}`)).WithContext(requestCtx)
	request.Header.Set("Authorization", "Bearer "+makeAdminToken())
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var created poolJob
	if response.Code != 202 || json.Unmarshal(response.Body.Bytes(), &created) != nil {
		t.Fatalf("queue: %d %s", response.Code, response.Body.String())
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not begin independently")
	}
	cancelRequest() // Closing the browser request must not cancel the worker.
	select {
	case <-transportCanceled:
		t.Fatal("browser request cancellation aborted the background worker")
	case <-time.After(100 * time.Millisecond):
	}
	if result := proxyAdminRequest(router, "GET", fmt.Sprintf("upstreams/%d/cliproxy/jobs/%s", other, created.ID), "", true); result.Code != 404 {
		t.Fatal("cross-pool task exposed")
	}
	list := proxyAdminRequest(router, "GET", base+"jobs", "", true)
	if list.Code != 200 || strings.Contains(list.Body.String(), "PRIVATE") {
		t.Fatal("job view leaked secret input")
	}
	stopPoolJobs()
	http.DefaultTransport = original
	resumePoolJobs(context.Background())
	job := waitPoolJob(t, created.ID, func(job poolJob) bool { return job.Status == "completed" })
	if len(job.Items) != 3 {
		t.Fatal("background job lost items")
	}
	files, _ := manager.ReadCredentials()
	for _, raw := range files {
		var doc map[string]any
		json.Unmarshal(raw, &doc)
		if doc["disabled"] != false {
			t.Fatal("restart did not finish all items")
		}
	}
	stopPoolJobs()
	// The durable job remains queryable after the runner is stopped.
	if result := proxyAdminRequest(router, "GET", base+"jobs/"+created.ID, "", true); result.Code != 200 {
		t.Fatal("job history was volatile")
	}
}

func TestPoolMigrationReconcilesLostDeleteResponse(t *testing.T) {
	router := testProxyAdminRouter(t)
	withTestCLIProxyPools(t)
	a, source := jobTestPool(t, router, "source")
	b, target := jobTestPool(t, router, "target")
	for _, test := range []struct {
		name   string
		landed bool
	}{{"landed.json", true}, {"not-landed.json", false}} {
		t.Run(test.name, func(t *testing.T) {
			raw := json.RawMessage(fmt.Sprintf(`{"type":"kimi","email":%q,"access_token":"local","expired":"2099-01-01T00:00:00Z","disabled":false}`, test.name+"@example.invalid"))
			if err := importPoolCredential(context.Background(), source, a, test.name, raw, 0); err != nil {
				t.Fatal(err)
			}
			original := http.DefaultTransport
			sourceBase, _ := source.Connection()
			http.DefaultTransport = jobRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.Method == "DELETE" && strings.HasPrefix(r.URL.String(), sourceBase) && strings.Contains(r.URL.RawQuery, test.name) {
					if test.landed {
						res, err := original.RoundTrip(r)
						if err != nil {
							return nil, err
						}
						io.Copy(io.Discard, res.Body)
						res.Body.Close()
					}
					return nil, errors.New("simulated lost management response")
				}
				return original.RoundTrip(r)
			})
			defer func() { http.DefaultTransport = original }()
			err := movePoolCredential(context.Background(), source, a, target, b, test.name)
			src, _ := source.ReadCredentials()
			dst, _ := target.ReadCredentials()
			if test.landed {
				if err != nil || src[test.name] != nil || dst[test.name] == nil {
					t.Fatalf("lost DELETE removed authoritative copy: %v", err)
				}
				var doc map[string]any
				json.Unmarshal(dst[test.name], &doc)
				if doc["disabled"] != false {
					t.Fatal("destination was left disabled")
				}
			} else {
				if err == nil || src[test.name] == nil || dst[test.name] != nil {
					t.Fatalf("failed DELETE did not roll back: %v", err)
				}
				var doc map[string]any
				json.Unmarshal(src[test.name], &doc)
				if doc["disabled"] != false {
					t.Fatal("source was left disabled")
				}
			}
		})
	}
}

func TestPoolImportJobConfirmationAndRetry(t *testing.T) {
	router := testProxyAdminRouter(t)
	withTestCLIProxyPools(t)
	id, manager := jobTestPool(t, router, "imports")
	defer stopPoolJobs()
	doc := json.RawMessage(`{"type":"codex","email":"one@example.invalid","access_token":"PRIVATE-OLD","disabled":true,"note":"preserve this","priority":12}`)
	if err := importPoolCredential(context.Background(), manager, id, "original.json", doc, 0); err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("upstreams/%d/cliproxy/", id)
	body := `{"action":"import","items":[{"name":"new-name.json","proxy_id":-1,"credential":{"type":"codex","email":"one@example.invalid","access_token":"PRIVATE-NEW"}},{"name":"bad.json","validation_error":"invalid"}]}`
	result := proxyAdminRequest(router, "POST", base+"jobs", body, true)
	var job poolJob
	if result.Code != 202 || json.Unmarshal(result.Body.Bytes(), &job) != nil {
		t.Fatal(result.Body.String())
	}
	job = waitPoolJob(t, job.ID, func(job poolJob) bool { return job.Status == "needs_confirmation" })
	if job.Items[0].Status != "confirmation" || job.Items[1].Status != "error" {
		t.Fatalf("import results: %+v", job)
	}
	if strings.Contains(result.Body.String(), "PRIVATE") {
		t.Fatal("job returned raw input")
	}
	stopPoolJobs()
	resumePoolJobs(context.Background())
	result = proxyAdminRequest(router, "POST", base+"jobs/"+job.ID+"/retry", `{"confirm":[0]}`, true)
	if result.Code != 202 {
		t.Fatalf("confirm: %d %s", result.Code, result.Body.String())
	}
	job = waitPoolJob(t, job.ID, func(job poolJob) bool { return job.Status == "completed_with_errors" })
	files, _ := manager.ReadCredentials()
	var saved map[string]any
	json.Unmarshal(files["original.json"], &saved)
	if saved["access_token"] != "PRIVATE-NEW" || saved["disabled"] != true || saved["note"] != "preserve this" || saved["priority"] != float64(12) || files["new-name.json"] != nil {
		t.Fatal("reauth lost account settings")
	}
}

func TestPoolMigrationResumesJournalWithStoppedDestination(t *testing.T) {
	router := testProxyAdminRouter(t)
	withTestCLIProxyPools(t)
	a, source := jobTestPool(t, router, "recover source")
	b, target := jobTestPool(t, router, "recover target")
	for _, test := range []struct {
		name, phase  string
		sourceAbsent bool
	}{{"forward.json", "finishing", true}, {"rollback.json", "rolling_back", false}} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if !target.Running() {
				if err := target.Start(ctx); err != nil {
					t.Fatal(err)
				}
			}
			id := randHex(16)
			document := map[string]any{"type": "kimi", "email": test.name + "@example.invalid", "access_token": "PRIVATE", "disabled": true, "expired": "2099-01-01T00:00:00Z", migrationMarker: id}
			raw, _ := json.Marshal(document)
			if err := importPoolCredential(ctx, source, a, test.name, raw, 0); err != nil {
				t.Fatal(err)
			}
			if err := importPoolCredential(ctx, target, b, test.name, raw, 0); err != nil {
				t.Fatal(err)
			}
			if test.sourceAbsent {
				if err := source.DeleteCredential(ctx, test.name); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`INSERT INTO cliproxy_migrations(id,source_id,target_id,name,disabled,proxy_id,phase,updated_at) VALUES(?,?,?,?,0,0,?,?)`, id, a, b, test.name, test.phase, nowStr()); err != nil {
				t.Fatal(err)
			}
			if err := target.Stop(); err != nil {
				t.Fatal(err)
			}
			journal, err := readPoolMigration(id)
			if err != nil {
				t.Fatal(err)
			}
			err = reconcilePoolMigration(journal)
			recovered, _ := readPoolMigration(id)
			src, _ := source.ReadCredentials()
			dst, _ := target.ReadCredentials()
			if test.sourceAbsent {
				if err != nil || recovered.Phase != "done" || src[test.name] != nil || dst[test.name] == nil {
					t.Fatalf("forward recovery: %+v %v", recovered, err)
				}
				var doc map[string]any
				json.Unmarshal(dst[test.name], &doc)
				if doc["disabled"] != false {
					t.Fatal("stopped destination not restored")
				}
			} else {
				if err == nil || recovered.Phase != "rolled_back" || src[test.name] == nil || dst[test.name] != nil {
					t.Fatalf("rollback recovery: %+v %v", recovered, err)
				}
				var doc map[string]any
				json.Unmarshal(src[test.name], &doc)
				if doc["disabled"] != false {
					t.Fatal("source remained disabled after rollback")
				}
			}
		})
	}
}
