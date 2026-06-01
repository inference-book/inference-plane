package lifecycle

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// fakeService stands in for *provisioners.Service. Tests configure
// the deployments ListDeployments returns and observe which
// DestroyDeployment calls fire.
type fakeService struct {
	deployments []*provisionerv1.Deployment

	destroyCalls atomic.Int32
	destroyed    []string

	listErr    error
	destroyErr error
}

func (f *fakeService) ListDeployments(_ context.Context, _ *provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &provisionerv1.ListDeploymentsResponse{Deployments: f.deployments}, nil
}

func (f *fakeService) DestroyDeployment(_ context.Context, req *provisionerv1.DestroyDeploymentRequest) (*provisionerv1.DestroyDeploymentResponse, error) {
	f.destroyCalls.Add(1)
	if f.destroyErr != nil {
		return nil, f.destroyErr
	}
	f.destroyed = append(f.destroyed, req.GetId())
	return &provisionerv1.DestroyDeploymentResponse{}, nil
}

// fixedClock returns a clock function pinned at t. Tests use this to
// drive the reaper to a specific "now" without sleeping.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// dep is a deployment-builder helper for the test cases. id, state,
// ttl, and lastActivity are the dimensions that matter; everything
// else defaults to sensible zeros.
func dep(id string, state provisionerv1.DeploymentState, ttl int32, lastActivity time.Time) *provisionerv1.Deployment {
	d := &provisionerv1.Deployment{
		Id:             id,
		Model:          "test/model",
		State:          state,
		IdleTtlSeconds: ttl,
	}
	if !lastActivity.IsZero() {
		d.LastActivityAt = timestamppb.New(lastActivity)
	}
	return d
}

func TestReaper_IdlePastTTL_Reaps(t *testing.T) {
	now := time.Now()
	svc := &fakeService{deployments: []*provisionerv1.Deployment{
		dep("idle-llama", provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, 60, now.Add(-2*time.Minute)),
	}}
	r := New(svc, WithClock(fixedClock(now)))

	reaped := r.RunOnce(context.Background())
	if reaped != 1 {
		t.Errorf("reaped count = %d, want 1", reaped)
	}
	if svc.destroyCalls.Load() != 1 {
		t.Errorf("DestroyDeployment call count = %d, want 1", svc.destroyCalls.Load())
	}
	if len(svc.destroyed) != 1 || svc.destroyed[0] != "idle-llama" {
		t.Errorf("destroyed list = %v, want [idle-llama]", svc.destroyed)
	}
}

func TestReaper_FreshActivity_DoesNotReap(t *testing.T) {
	now := time.Now()
	svc := &fakeService{deployments: []*provisionerv1.Deployment{
		// Active 10s ago; TTL is 60s; still within window.
		dep("active-llama", provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, 60, now.Add(-10*time.Second)),
	}}
	r := New(svc, WithClock(fixedClock(now)))

	r.RunOnce(context.Background())
	if svc.destroyCalls.Load() != 0 {
		t.Errorf("active deployment should not be reaped; destroy count = %d", svc.destroyCalls.Load())
	}
}

func TestReaper_NoIdleDestroyFlag_Skipped(t *testing.T) {
	now := time.Now()
	d := dep("pinned-llama", provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, 60, now.Add(-2*time.Minute))
	d.NoIdleDestroy = true
	svc := &fakeService{deployments: []*provisionerv1.Deployment{d}}
	r := New(svc, WithClock(fixedClock(now)))

	r.RunOnce(context.Background())
	if svc.destroyCalls.Load() != 0 {
		t.Error("pinned deployment should not be reaped despite being idle")
	}
}

func TestReaper_ZeroTTL_Skipped(t *testing.T) {
	// Default behavior pre-#70: ttl=0 means "no TTL set", reaper
	// skips. Preserves v0.1 behavior for deployments created before
	// the operator opted in.
	now := time.Now()
	svc := &fakeService{deployments: []*provisionerv1.Deployment{
		dep("no-ttl", provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, 0, now.Add(-1*time.Hour)),
	}}
	r := New(svc, WithClock(fixedClock(now)))

	r.RunOnce(context.Background())
	if svc.destroyCalls.Load() != 0 {
		t.Error("deployment with idle_ttl_seconds=0 should never be reaped")
	}
}

func TestReaper_NonRunningStates_Skipped(t *testing.T) {
	// Reaper only touches RUNNING. Destroying STARTING / CONFIGURING
	// would clobber an in-progress deploy; FAILED / TERMINATED /
	// TERMINATING are already done; DEGRADED stays for diagnostics.
	now := time.Now()
	stale := now.Add(-1 * time.Hour) // way past any reasonable TTL
	cases := []provisionerv1.DeploymentState{
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_PENDING,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_STARTING,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_DEGRADED,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATING,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED,
	}
	for _, st := range cases {
		t.Run(st.String(), func(t *testing.T) {
			svc := &fakeService{deployments: []*provisionerv1.Deployment{
				dep("nonrun", st, 60, stale),
			}}
			r := New(svc, WithClock(fixedClock(now)))
			r.RunOnce(context.Background())
			if svc.destroyCalls.Load() != 0 {
				t.Errorf("state %v should not be reaped", st)
			}
		})
	}
}

func TestReaper_NoLastActivity_UsesCreatedAt(t *testing.T) {
	// Fresh deployment with traffic never recorded -- last_activity
	// is nil. Reaper falls back to created_at. If created_at is
	// also old enough, reap.
	now := time.Now()
	d := dep("just-born", provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, 60, time.Time{})
	d.CreatedAt = timestamppb.New(now.Add(-2 * time.Minute))
	svc := &fakeService{deployments: []*provisionerv1.Deployment{d}}
	r := New(svc, WithClock(fixedClock(now)))

	r.RunOnce(context.Background())
	if svc.destroyCalls.Load() != 1 {
		t.Errorf("fresh-but-idle deployment should be reaped using created_at fallback; destroy count = %d", svc.destroyCalls.Load())
	}
}

func TestReaper_MultipleDeployments_MixedOutcomes(t *testing.T) {
	now := time.Now()
	old := now.Add(-2 * time.Minute)
	young := now.Add(-10 * time.Second)

	svc := &fakeService{deployments: []*provisionerv1.Deployment{
		dep("idle-a", provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, 60, old),    // reap
		dep("active", provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, 60, young),  // skip
		dep("idle-b", provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, 60, old),    // reap
		dep("no-ttl", provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, 0, old),     // skip
	}}
	r := New(svc, WithClock(fixedClock(now)))

	reaped := r.RunOnce(context.Background())
	if reaped != 2 {
		t.Errorf("reaped count = %d, want 2", reaped)
	}
	if len(svc.destroyed) != 2 {
		t.Errorf("destroyed list len = %d, want 2: %v", len(svc.destroyed), svc.destroyed)
	}
}

func TestReaper_ListError_DoesNotPanic(t *testing.T) {
	// Transient ListDeployments failure shouldn't take down leak
	// protection. The loop logs and continues; RunOnce returns 0.
	svc := &fakeService{listErr: errors.New("simulated transient list error")}
	r := New(svc)

	reaped := r.RunOnce(context.Background())
	if reaped != 0 {
		t.Errorf("reaped on list-error = %d, want 0", reaped)
	}
	if svc.destroyCalls.Load() != 0 {
		t.Error("DestroyDeployment should not fire when ListDeployments fails")
	}
}

func TestReaper_DestroyError_ContinuesSweep(t *testing.T) {
	// One destroy fails -- the sweep should still attempt other
	// candidates. The failure is logged but doesn't terminate.
	now := time.Now()
	old := now.Add(-2 * time.Minute)
	svc := &fakeService{
		deployments: []*provisionerv1.Deployment{
			dep("first", provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, 60, old),
			dep("second", provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, 60, old),
		},
		destroyErr: errors.New("simulated destroy failure"),
	}
	r := New(svc, WithClock(fixedClock(now)))

	r.RunOnce(context.Background())
	if svc.destroyCalls.Load() != 2 {
		t.Errorf("expected 2 destroy attempts (both should be tried despite errors); got %d", svc.destroyCalls.Load())
	}
}

func TestReaper_Run_StopsOnContextCancel(t *testing.T) {
	// The production loop must exit cleanly when its context is
	// cancelled (daemon shutdown). Set a very short interval so the
	// loop ticks at least once before we cancel.
	svc := &fakeService{}
	r := New(svc, WithInterval(10*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	// Let the loop run briefly, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Good -- loop exited.
	case <-time.After(1 * time.Second):
		t.Fatal("reaper.Run did not exit within 1s of context cancellation")
	}
}
