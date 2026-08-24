// Package watchdog drives hack/lambda-watchdog.sh against a stand-in for
// Lambda's API.
//
// The guard exists because a paid run's teardown cannot live in the process
// that can be killed, which means the guard itself is the last thing between
// a dead script and an open meter. Every branch in it is therefore either a
// termination nobody asked for or a rental nobody hands back, and neither
// shows up until it has already cost something.
//
// Assertions live here rather than in the script, per the repo's rule that
// shell orchestrates and Go asserts. The script's LAMBDA_API_BASE seam is
// what lets the test point it somewhere free.
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

type lambdaTag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type lambdaInstance struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Status string      `json:"status"`
	Tags   []lambdaTag `json:"tags"`
}

// fakeLambda serves the two endpoints the guard calls and records every
// terminate it was asked for.
type fakeLambda struct {
	mu         sync.Mutex
	instances  []lambdaInstance
	terminated []string
	listStatus int
}

func (f *fakeLambda) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/instances", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.listStatus != 0 {
			http.Error(w, `{"error":{"code":"global/internal","message":"boom"}}`, f.listStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": f.instances})
	})
	mux.HandleFunc("/instance-operations/terminate", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			InstanceIDs []string `json:"instance_ids"`
		}
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		f.terminated = append(f.terminated, req.InstanceIDs...)
		remaining := f.instances[:0]
		for _, inst := range f.instances {
			if !contains(req.InstanceIDs, inst.ID) {
				remaining = append(remaining, inst)
			}
		}
		f.instances = remaining
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"terminated_instances": req.InstanceIDs},
		})
	})
	return mux
}

func (f *fakeLambda) terminations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.terminated...)
}

func contains(hay []string, needle string) bool { return slices.Contains(hay, needle) }

// runGuard runs the watchdog for one pass against the fake and returns its
// combined output. --max-runtime 1 makes it exit after a single sweep.
func runGuard(t *testing.T, f *fakeLambda, heartbeatAge time.Duration, extra ...string) (*fakeLambda, string) {
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

	script := filepath.Join(repoRoot(t), "hack", "lambda-watchdog.sh")
	args := append([]string{
		"--heartbeat", hb,
		"--state", filepath.Join(dir, "seen"),
		"--max-stale", "60",
		"--interval", "1",
		"--max-runtime", "1",
	}, extra...)
	cmd := exec.Command(script, args...)
	cmd.Env = append(os.Environ(),
		"LAMBDA_API_KEY=test-key",
		"LAMBDA_API_BASE="+srv.URL,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("watchdog exited %v\n%s", err, out)
	}
	return f, string(out)
}

// requireTools skips rather than fails on a machine without the guard's own
// dependencies. The script is only ever run where they exist.
func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "curl", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH; the watchdog needs it", tool)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

// The whole point: the creator died, so the machine it left behind goes.
func TestGuardTerminatesAnOwnedInstanceWhenTheHeartbeatGoesStale(t *testing.T) {
	f, out := runGuard(t, &fakeLambda{instances: []lambdaInstance{
		{ID: "i-1", Name: "iplane-lambda-probe", Status: "active"},
	}}, 5*time.Minute)
	if got := f.terminations(); len(got) != 1 || got[0] != "i-1" {
		t.Errorf("terminated = %v, want [i-1]\n%s", got, out)
	}
}

// The account holds boxes this project did not create. Terminating one of
// those is worse than leaking one of ours, so ownership is positive-only and
// an unmatched name survives a stale heartbeat.
func TestGuardLeavesSomebodyElsesInstanceAlone(t *testing.T) {
	f, out := runGuard(t, &fakeLambda{instances: []lambdaInstance{
		{ID: "i-mine", Name: "iplane-lambda-probe", Status: "active"},
		{ID: "i-theirs", Name: "research-cluster", Status: "active"},
	}}, 5*time.Minute)
	got := f.terminations()
	if contains(got, "i-theirs") {
		t.Errorf("terminated somebody else's instance: %v\n%s", got, out)
	}
	if !contains(got, "i-mine") {
		t.Errorf("did not terminate our own instance: %v\n%s", got, out)
	}
}

// A script that calls Lambda directly never gets a name stamped for it, so
// the registry file is the other way to claim an id.
func TestGuardTerminatesARegisteredInstanceWithoutTheNamePrefix(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "registry")
	if err := os.WriteFile(registry, []byte("i-registered\n"), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	f, out := runGuard(t, &fakeLambda{instances: []lambdaInstance{
		{ID: "i-registered", Name: "hand-launched", Status: "active"},
	}}, 5*time.Minute, "--registry", registry)
	if got := f.terminations(); len(got) != 1 || got[0] != "i-registered" {
		t.Errorf("terminated = %v, want [i-registered]\n%s", got, out)
	}
}

// A fresh heartbeat means the run is alive and the machine is meant to be
// there. Terminating it would kill the thing the guard is protecting.
func TestGuardLeavesEverythingAloneWhileTheHeartbeatIsFresh(t *testing.T) {
	f, out := runGuard(t, &fakeLambda{instances: []lambdaInstance{
		{ID: "i-1", Name: "iplane-lambda-probe", Status: "active"},
	}}, 0)
	if got := f.terminations(); len(got) != 0 {
		t.Errorf("terminated %v while the run was alive\n%s", got, out)
	}
}

// An API that will not answer is not evidence of anything. Reading it as
// "nothing is running" would terminate on an inference, and reading it as
// "everything is orphaned" would terminate on a worse one.
func TestGuardTerminatesNothingWhenTheAPIFails(t *testing.T) {
	f, out := runGuard(t, &fakeLambda{
		instances:  []lambdaInstance{{ID: "i-1", Name: "iplane-lambda-probe", Status: "active"}},
		listStatus: http.StatusInternalServerError,
	}, 5*time.Minute)
	if got := f.terminations(); len(got) != 0 {
		t.Errorf("terminated %v on an unreadable API\n%s", got, out)
	}
	if !strings.Contains(out, "leaving everything running") {
		t.Errorf("output should say it could not tell:\n%s", out)
	}
}

// --dry-run has to be believable before anyone arms this for real.
func TestGuardDryRunTerminatesNothing(t *testing.T) {
	f, out := runGuard(t, &fakeLambda{instances: []lambdaInstance{
		{ID: "i-1", Name: "iplane-lambda-probe", Status: "active"},
	}}, 5*time.Minute, "--dry-run")
	if got := f.terminations(); len(got) != 0 {
		t.Errorf("dry run terminated %v\n%s", got, out)
	}
	if !strings.Contains(out, "DRY-RUN would terminate i-1") {
		t.Errorf("dry run should say what it would have done:\n%s", out)
	}
}

// Lambda publishes no launch timestamp, so age is measured from first sight.
// An instance the guard has only just met is young whatever the truth is,
// which keeps --max-lifetime from firing on a reading it does not have.
func TestGuardAgesFromFirstSightNotFromTheProvider(t *testing.T) {
	f, out := runGuard(t, &fakeLambda{instances: []lambdaInstance{
		{ID: "i-1", Name: "iplane-lambda-probe", Status: "active"},
	}}, 0, "--max-lifetime", "3600")
	if got := f.terminations(); len(got) != 0 {
		t.Errorf("terminated %v on its first sighting\n%s", got, out)
	}
	if !strings.Contains(out, "age=0s") {
		t.Errorf("first sighting should report age 0:\n%s", out)
	}
}

// The state file is what makes first-sight ageing survive a sweep, so an
// instance seen long enough ago goes even while the creator is healthy. This
// is the "creator is alive and wrong" case.
func TestGuardTerminatesPastMaxLifetime(t *testing.T) {
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	long := time.Now().Add(-2 * time.Hour).Unix()
	if err := os.WriteFile(seen, fmt.Appendf(nil, "i-1 %d\n", long), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	requireTools(t)
	f := &fakeLambda{instances: []lambdaInstance{
		{ID: "i-1", Name: "iplane-lambda-probe", Status: "active"},
	}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	hb := filepath.Join(dir, "run.hb")
	if err := os.WriteFile(hb, []byte("beat\n"), 0o600); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}
	cmd := exec.Command(filepath.Join(repoRoot(t), "hack", "lambda-watchdog.sh"),
		"--heartbeat", hb, "--state", seen,
		"--max-stale", "600", "--max-lifetime", "3600",
		"--interval", "1", "--max-runtime", "1")
	cmd.Env = append(os.Environ(), "LAMBDA_API_KEY=test-key", "LAMBDA_API_BASE="+srv.URL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("watchdog exited %v\n%s", err, out)
	}
	if got := f.terminations(); len(got) != 1 || got[0] != "i-1" {
		t.Errorf("terminated = %v, want [i-1]\n%s", got, out)
	}
}

// The instance name is a display field an operator can change from the
// console. A box renamed after it was rented is still ours, and the tag is
// what says so.
func TestGuardClaimsARenamedInstanceByItsTag(t *testing.T) {
	f, out := runGuard(t, &fakeLambda{instances: []lambdaInstance{
		{ID: "i-1", Name: "renamed-by-hand", Status: "active", Tags: []lambdaTag{
			{Key: "iplane-id", Value: "lambda-probe"},
		}},
	}}, 5*time.Minute)
	if got := f.terminations(); len(got) != 1 || got[0] != "i-1" {
		t.Errorf("terminated = %v, want [i-1]\n%s", got, out)
	}
}

// A tag that is not ours claims nothing, and neither does an iplane-id tag
// with an empty value. Positive-only cuts both ways.
func TestGuardDoesNotClaimAForeignTag(t *testing.T) {
	f, out := runGuard(t, &fakeLambda{instances: []lambdaInstance{
		{ID: "i-theirs", Name: "research-cluster", Status: "active", Tags: []lambdaTag{
			{Key: "team", Value: "platform"},
			{Key: "iplane-id", Value: ""},
		}},
	}}, 5*time.Minute)
	if got := f.terminations(); len(got) != 0 {
		t.Errorf("terminated %v on a tag that is not ours\n%s", got, out)
	}
}
