package watchdog

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type vastInstance struct {
	ID        int     `json:"id"`
	Label     string  `json:"label"`
	StartDate float64 `json:"start_date"`
	DPHTotal  float64 `json:"dph_total"`
}

// fakeVast serves the two endpoints the Vast guard calls. The list lives on
// /api/v1 and the destroy on /api/v0, which is not tidiness: v0's list is
// deprecated and answers with an error object that parses as an empty list,
// so the guard reads v1 and destroys through v0 deliberately.
type fakeVast struct {
	mu         sync.Mutex
	instances  []vastInstance
	destroyed  []string
	listStatus int
	// bearerSeen records the auth header on the list call. Vast is a Bearer
	// provider and Lambda is HTTP Basic, so a guard copied from one to the
	// other 401s on every call and reports a teardown failure it cannot act
	// on.
	bearerSeen string
}

func (f *fakeVast) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/instances/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.bearerSeen = r.Header.Get("Authorization")
		if f.listStatus != 0 {
			http.Error(w, `{"msg":"boom"}`, f.listStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"instances": f.instances})
	})
	mux.HandleFunc("/api/v0/instances/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, `{"msg":"method"}`, http.StatusMethodNotAllowed)
			return
		}
		_, _ = io.ReadAll(r.Body)
		id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v0/instances/"), "/")
		f.mu.Lock()
		f.destroyed = append(f.destroyed, id)
		f.instances = slices.DeleteFunc(f.instances, func(i vastInstance) bool {
			return fmt.Sprint(i.ID) == id
		})
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	})
	return mux
}

func (f *fakeVast) destructions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.destroyed...)
}

func (f *fakeVast) auth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bearerSeen
}

// runVastGuard runs the Vast watchdog for one sweep against the fake.
func runVastGuard(t *testing.T, f *fakeVast, heartbeatAge time.Duration, extra ...string) (*fakeVast, string) {
	t.Helper()
	requireTools(t)
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	hb := filepath.Join(dir, "run.hb")
	if err := os.WriteFile(hb, []byte("beat\n"), 0o600); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}
	stamp := time.Now().Add(-heartbeatAge)
	if err := os.Chtimes(hb, stamp, stamp); err != nil {
		t.Fatalf("age heartbeat: %v", err)
	}

	args := append([]string{
		"--heartbeat", hb,
		"--max-stale", "60",
		"--interval", "1",
		"--max-runtime", "1",
	}, extra...)
	cmd := exec.Command(filepath.Join(repoRoot(t), "hack", "vast-watchdog.sh"), args...)
	cmd.Env = append(os.Environ(),
		"VAST_API_KEY=test-key",
		"VAST_API_BASE="+srv.URL+"/api/v0",
		"VAST_API_V1_BASE="+srv.URL+"/api/v1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("watchdog exited %v\n%s", err, out)
	}
	return f, string(out)
}

func recentlyStarted(age time.Duration) float64 {
	return float64(time.Now().Add(-age).Unix())
}

// The whole point: the creator died, so the rental it left behind goes.
func TestVastGuardDestroysAnOwnedInstanceWhenTheHeartbeatGoesStale(t *testing.T) {
	f, out := runVastGuard(t, &fakeVast{instances: []vastInstance{
		{ID: 1, Label: "iplane-my-llama", StartDate: recentlyStarted(time.Minute), DPHTotal: 0.64},
	}}, 5*time.Minute)
	if got := f.destructions(); len(got) != 1 || got[0] != "1" {
		t.Errorf("destroyed = %v, want [1]\n%s", got, out)
	}
}

// The account holds boxes this project did not create. Destroying one of
// those is worse than leaking one of ours, so ownership is positive-only and
// an unmatched label survives a stale heartbeat.
func TestVastGuardLeavesSomebodyElsesInstanceAlone(t *testing.T) {
	f, out := runVastGuard(t, &fakeVast{instances: []vastInstance{
		{ID: 1, Label: "iplane-my-llama", StartDate: recentlyStarted(time.Minute)},
		{ID: 2, Label: "research-cluster", StartDate: recentlyStarted(time.Minute)},
	}}, 5*time.Minute)
	got := f.destructions()
	if slices.Contains(got, "2") {
		t.Errorf("destroyed somebody else's instance: %v\n%s", got, out)
	}
	if !slices.Contains(got, "1") {
		t.Errorf("did not destroy our own instance: %v\n%s", got, out)
	}
}

// A script that calls the Vast API directly never gets a label stamped for
// it, so the registry file is the other way to claim an id.
func TestVastGuardDestroysARegisteredInstanceWithoutTheLabelPrefix(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "registry")
	if err := os.WriteFile(registry, []byte("7\n"), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	f, out := runVastGuard(t, &fakeVast{instances: []vastInstance{
		{ID: 7, Label: "hand-launched", StartDate: recentlyStarted(time.Minute)},
	}}, 5*time.Minute, "--registry", registry)
	if got := f.destructions(); len(got) != 1 || got[0] != "7" {
		t.Errorf("destroyed = %v, want [7]\n%s", got, out)
	}
}

// A fresh heartbeat means the run is alive and the rental is meant to be
// there. Destroying it would kill the thing the guard is protecting.
func TestVastGuardLeavesEverythingAloneWhileTheHeartbeatIsFresh(t *testing.T) {
	f, out := runVastGuard(t, &fakeVast{instances: []vastInstance{
		{ID: 1, Label: "iplane-my-llama", StartDate: recentlyStarted(time.Minute)},
	}}, 0)
	if got := f.destructions(); len(got) != 0 {
		t.Errorf("destroyed %v while the run was alive\n%s", got, out)
	}
}

// An API that will not answer is not evidence of anything. Reading it as
// "nothing is running" would destroy on an inference.
func TestVastGuardDestroysNothingWhenTheAPIFails(t *testing.T) {
	f, out := runVastGuard(t, &fakeVast{
		instances:  []vastInstance{{ID: 1, Label: "iplane-my-llama", StartDate: recentlyStarted(time.Minute)}},
		listStatus: http.StatusInternalServerError,
	}, 5*time.Minute)
	if got := f.destructions(); len(got) != 0 {
		t.Errorf("destroyed %v on an unreadable API\n%s", got, out)
	}
	if !strings.Contains(out, "leaving everything running") {
		t.Errorf("output should say it could not tell:\n%s", out)
	}
}

// --dry-run has to be believable before anyone arms this for real.
func TestVastGuardDryRunDestroysNothing(t *testing.T) {
	f, out := runVastGuard(t, &fakeVast{instances: []vastInstance{
		{ID: 1, Label: "iplane-my-llama", StartDate: recentlyStarted(time.Minute)},
	}}, 5*time.Minute, "--dry-run")
	if got := f.destructions(); len(got) != 0 {
		t.Errorf("dry run destroyed %v\n%s", got, out)
	}
	if !strings.Contains(out, "DRY-RUN would destroy 1") {
		t.Errorf("dry run should say what it would have done:\n%s", out)
	}
}

// Vast publishes start_date, so --max-lifetime is provider-reported age
// rather than the first-sight ageing Lambda needs. This is the "creator is
// alive and wrong" case: the heartbeat is fresh and the box goes anyway.
func TestVastGuardDestroysPastMaxLifetimeUsingTheProvidersStartDate(t *testing.T) {
	f, out := runVastGuard(t, &fakeVast{instances: []vastInstance{
		{ID: 1, Label: "iplane-my-llama", StartDate: recentlyStarted(2 * time.Hour)},
	}}, 0, "--max-lifetime", "3600")
	if got := f.destructions(); len(got) != 1 || got[0] != "1" {
		t.Errorf("destroyed = %v, want [1]\n%s", got, out)
	}
	if !strings.Contains(out, "exceeds max-lifetime") {
		t.Errorf("output should name the deadline it passed:\n%s", out)
	}
}

// An instance the API reports without a start_date has no age, and no age is
// not a young age. --max-lifetime cannot fire on a reading it does not have.
func TestVastGuardLeavesAnInstanceWithNoStartDateAlone(t *testing.T) {
	f, out := runVastGuard(t, &fakeVast{instances: []vastInstance{
		{ID: 1, Label: "iplane-my-llama"},
	}}, 0, "--max-lifetime", "1")
	if got := f.destructions(); len(got) != 0 {
		t.Errorf("destroyed %v on an instance with no start_date\n%s", got, out)
	}
}

// Vast authenticates with a Bearer token and Lambda with HTTP Basic. A guard
// copied across and edited 401s on every call, which the guard would report
// as a teardown failure it cannot act on.
func TestVastGuardSendsABearerToken(t *testing.T) {
	f, out := runVastGuard(t, &fakeVast{instances: []vastInstance{
		{ID: 1, Label: "iplane-my-llama", StartDate: recentlyStarted(time.Minute)},
	}}, 0)
	if got := f.auth(); got != "Bearer test-key" {
		t.Errorf("Authorization = %q, want a Bearer token\n%s", got, out)
	}
}
