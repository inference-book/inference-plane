package healthcheck

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeQuarantiner records every Quarantine / Restore call so tests
// can assert what the runner decided.
type fakeQuarantiner struct {
	mu       sync.Mutex
	quars    []string
	restores []string
}

func (f *fakeQuarantiner) Quarantine(deployID, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quars = append(f.quars, fmt.Sprintf("%s/%s", deployID, instanceID))
	return nil
}

func (f *fakeQuarantiner) Restore(deployID, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restores = append(f.restores, fmt.Sprintf("%s/%s", deployID, instanceID))
	return nil
}

func (f *fakeQuarantiner) qCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.quars)
}

func (f *fakeQuarantiner) rCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.restores)
}

// fakeSource is a settable snapshot the test mutates between ticks
// to simulate the state-file picking up Quarantine writes.
type fakeSource struct {
	mu   sync.Mutex
	snap []DeploymentSnapshot
}

func (s *fakeSource) Snapshot() []DeploymentSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

func (s *fakeSource) set(snap []DeploymentSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = snap
}

// TestRunner_QuarantinesAfterKFailures: a healthy replica that
// starts failing /health is quarantined after exactly K=3 ticks of
// failures, no sooner.
func TestRunner_QuarantinesAfterKFailures(t *testing.T) {
	var dead atomic.Bool
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if dead.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer engine.Close()

	q := &fakeQuarantiner{}
	src := &fakeSource{snap: []DeploymentSnapshot{{
		DeployID: "d1",
		Replicas: []ReplicaSnapshot{{InstanceID: "a", Endpoint: engine.URL}},
	}}}
	r := New(Config{
		PollInterval:     time.Hour, // tick driven manually
		FailureThreshold: 3,
		SuccessThreshold: 3,
		ProbeTimeout:     500 * time.Millisecond,
		MaxConcurrent:    4,
	}, src, q, nil)

	ctx := context.Background()
	// Three healthy ticks -- streak fails=0; no quarantine.
	for i := range 3 {
		r.tick(ctx)
		if q.qCount() != 0 {
			t.Fatalf("tick %d: unexpected Quarantine call on healthy replica", i)
		}
	}
	// Kill the engine; ticks 1,2 are failures but below threshold.
	dead.Store(true)
	r.tick(ctx)
	if q.qCount() != 0 {
		t.Fatalf("after 1 failure: should not quarantine yet (K=3)")
	}
	r.tick(ctx)
	if q.qCount() != 0 {
		t.Fatalf("after 2 failures: should not quarantine yet (K=3)")
	}
	// Tick 3 trips the threshold.
	r.tick(ctx)
	if q.qCount() != 1 {
		t.Fatalf("after 3 failures: expected 1 Quarantine call, got %d", q.qCount())
	}

	// Further failure ticks: the source still reflects "not quarantined"
	// (we haven't updated it) so the runner would call Quarantine again
	// because shouldQuarantine = !quarantined && fails>=K. Update the
	// source to reflect the quarantine; subsequent failed ticks are no-op.
	src.set([]DeploymentSnapshot{{
		DeployID: "d1",
		Replicas: []ReplicaSnapshot{{InstanceID: "a", Endpoint: engine.URL, Quarantined: true}},
	}})
	r.tick(ctx)
	r.tick(ctx)
	if q.qCount() != 1 {
		t.Errorf("once quarantined, further failures should not re-call: got qCount=%d", q.qCount())
	}
}

// TestRunner_RestoresAfterKSuccesses: a quarantined replica that
// starts passing /health is restored after K=3 consecutive
// successful ticks.
func TestRunner_RestoresAfterKSuccesses(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer engine.Close()

	q := &fakeQuarantiner{}
	src := &fakeSource{snap: []DeploymentSnapshot{{
		DeployID: "d1",
		Replicas: []ReplicaSnapshot{{InstanceID: "a", Endpoint: engine.URL, Quarantined: true}},
	}}}
	r := New(Config{
		PollInterval:     time.Hour,
		FailureThreshold: 3,
		SuccessThreshold: 3,
		ProbeTimeout:     500 * time.Millisecond,
		MaxConcurrent:    4,
	}, src, q, nil)

	ctx := context.Background()
	// Two passing ticks below threshold -> no restore yet.
	r.tick(ctx)
	r.tick(ctx)
	if q.rCount() != 0 {
		t.Fatalf("under threshold: should not restore yet, got rCount=%d", q.rCount())
	}
	// Third success trips it.
	r.tick(ctx)
	if q.rCount() != 1 {
		t.Fatalf("expected 1 Restore call after 3 successes, got %d", q.rCount())
	}
}

// TestRunner_FlapResetsStreak: a single failed probe between
// successes resets the success streak; restore takes a fresh K
// consecutive passes from that point. Prevents flap from prematurely
// restoring an unstable replica.
func TestRunner_FlapResetsStreak(t *testing.T) {
	var failNext atomic.Bool
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failNext.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer engine.Close()

	q := &fakeQuarantiner{}
	src := &fakeSource{snap: []DeploymentSnapshot{{
		DeployID: "d1",
		Replicas: []ReplicaSnapshot{{InstanceID: "a", Endpoint: engine.URL, Quarantined: true}},
	}}}
	r := New(Config{
		PollInterval:     time.Hour,
		FailureThreshold: 3,
		SuccessThreshold: 3,
		ProbeTimeout:     500 * time.Millisecond,
		MaxConcurrent:    4,
	}, src, q, nil)

	ctx := context.Background()
	r.tick(ctx)
	r.tick(ctx) // passes=2
	// One failure resets passes.
	failNext.Store(true)
	r.tick(ctx)
	failNext.Store(false)
	r.tick(ctx)
	r.tick(ctx)
	if q.rCount() != 0 {
		t.Fatalf("flap should have reset streak; restore happened prematurely after 2 successes")
	}
	// Third success after the flap finally trips Restore.
	r.tick(ctx)
	if q.rCount() != 1 {
		t.Fatalf("expected 1 Restore after fresh K passes, got %d", q.rCount())
	}
}

// TestRunner_SkipsEmptyEndpoints: a replica whose endpoint is the
// empty string (still-provisioning sentinel from #85) is silently
// skipped by the probe fan-out -- no streak entry, no Quarantine
// call.
func TestRunner_SkipsEmptyEndpoints(t *testing.T) {
	q := &fakeQuarantiner{}
	src := &fakeSource{snap: []DeploymentSnapshot{{
		DeployID: "d1",
		Replicas: []ReplicaSnapshot{
			{InstanceID: "a", Endpoint: ""},
			{InstanceID: "b", Endpoint: ""},
		},
	}}}
	r := New(Config{
		PollInterval:     time.Hour,
		FailureThreshold: 1,
		SuccessThreshold: 1,
		ProbeTimeout:     500 * time.Millisecond,
		MaxConcurrent:    4,
	}, src, q, nil)
	for range 5 {
		r.tick(context.Background())
	}
	if q.qCount() != 0 || q.rCount() != 0 {
		t.Errorf("empty endpoints should be skipped; got q=%d r=%d", q.qCount(), q.rCount())
	}
}

// TestRunner_FourxxIsHealthy: HTTP 4xx (e.g., engine doesn't
// expose /health and returns 404) is treated as "engine responding"
// and does NOT trigger quarantine. Only network errors and 5xx
// count as failure, per the probe contract.
func TestRunner_FourxxIsHealthy(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer engine.Close()

	q := &fakeQuarantiner{}
	src := &fakeSource{snap: []DeploymentSnapshot{{
		DeployID: "d1",
		Replicas: []ReplicaSnapshot{{InstanceID: "a", Endpoint: engine.URL}},
	}}}
	r := New(Config{
		PollInterval:     time.Hour,
		FailureThreshold: 2,
		SuccessThreshold: 2,
		ProbeTimeout:     500 * time.Millisecond,
		MaxConcurrent:    4,
	}, src, q, nil)
	for range 5 {
		r.tick(context.Background())
	}
	if q.qCount() != 0 {
		t.Errorf("4xx should not quarantine; got q=%d", q.qCount())
	}
}

// TestRunner_TickTimeoutDoesNotBlockOthers: a hung probe (engine
// holds the connection longer than ProbeTimeout) should not block
// the fan-out across other replicas. After the tick returns, the
// hung probe's streak has incremented (timeout = fail) while
// healthy replicas have a clean pass.
func TestRunner_TickTimeoutDoesNotBlockOthers(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // hold until probe timeout cancels
	}))
	defer hang.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()

	q := &fakeQuarantiner{}
	src := &fakeSource{snap: []DeploymentSnapshot{{
		DeployID: "d",
		Replicas: []ReplicaSnapshot{
			{InstanceID: "hung", Endpoint: hang.URL},
			{InstanceID: "ok", Endpoint: ok.URL},
		},
	}}}
	r := New(Config{
		PollInterval:     time.Hour,
		FailureThreshold: 2,
		SuccessThreshold: 2,
		ProbeTimeout:     200 * time.Millisecond,
		MaxConcurrent:    4,
	}, src, q, nil)

	start := time.Now()
	r.tick(context.Background())
	r.tick(context.Background())
	if elapsed := time.Since(start); elapsed > 1*time.Second {
		t.Errorf("two ticks should complete near-instantly; took %s", elapsed)
	}
	// hung replica should have been quarantined after 2 failures.
	if q.qCount() != 1 {
		t.Errorf("hung replica should be quarantined after 2 failed ticks; q=%d", q.qCount())
	}
	if q.quars[0] != "d/hung" {
		t.Errorf("wrong replica quarantined: %q", q.quars[0])
	}
}

// TestRunner_RunStopsOnContextCancel: Run blocks while ctx is open
// and returns within a tick interval after cancellation. Used by
// `iplane serve`'s graceful shutdown path.
func TestRunner_RunStopsOnContextCancel(t *testing.T) {
	q := &fakeQuarantiner{}
	src := &fakeSource{}
	r := New(Config{
		PollInterval:     50 * time.Millisecond,
		FailureThreshold: 3,
		SuccessThreshold: 3,
		ProbeTimeout:     10 * time.Millisecond,
		MaxConcurrent:    4,
	}, src, q, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	time.Sleep(120 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit within 1s of context cancel")
	}
}
