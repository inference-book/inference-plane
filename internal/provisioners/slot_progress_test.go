package provisioners_test

import (
	"context"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
	"github.com/inference-book/inference-plane/internal/sshkeys"
)

// slowProvider is image-native and reports the phases a real deploy reports,
// so the test exercises the same path a Vast or RunPod deploy takes.
type slowProvider struct {
	*vmProvider
	emitPhases []provisioners.DeployStateUpdate
}

func (p *slowProvider) Deploy(_ context.Context, dep *provisionerv1.Deployment, _ *provisionerv1.Instance, _ *sshkeys.KeyPair, emit func(provisioners.DeployStateUpdate)) error {
	for _, u := range p.emitPhases {
		emit(u)
	}
	return nil
}

func (p *slowProvider) List(context.Context, map[string]string) ([]*provisionerv1.InstanceRef, error) {
	return nil, nil
}

func (p *slowProvider) Destroy(context.Context, *provisionerv1.Deployment, *provisionerv1.Instance, *sshkeys.KeyPair, func(provisioners.DeployStateUpdate)) error {
	return nil
}

func progressSvc(t *testing.T, p provisioners.Provider) (*provisioners.Service, *file.Store) {
	t.Helper()
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	return provisioners.New([]provisioners.Provider{p}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
	), store
}

// The defect: a deploy that spends twenty minutes pulling an image and
// loading weights read as PENDING with no phase and no message, so a slow
// deploy was indistinguishable from a wedged one without an ssh session.
func TestSlotProgressReachesTheDeploymentRecord(t *testing.T) {
	p := &slowProvider{vmProvider: &vmProvider{}, emitPhases: []provisioners.DeployStateUpdate{
		{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_STARTING, Phase: "vast:rent", ProgressMessage: "renting offer 42"},
		{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING, Phase: "vast:engine-ready", ProgressMessage: "waiting for port 8000 to be mapped (3m elapsed)"},
	}}
	svc, store := progressSvc(t, p)

	_, _ = svc.CreateDeployment(context.Background(), deployReq("vmfake"))

	f, _ := store.Read()
	rec := f.Deployments["d1"]
	if rec.GetCurrentPhase() != "vast:engine-ready" {
		t.Errorf("current_phase = %q, want the latest slot phase", rec.GetCurrentPhase())
	}
	if !strings.Contains(rec.GetProgressMessage(), "waiting for port 8000") {
		t.Errorf("progress_message = %q, want the latest slot message", rec.GetProgressMessage())
	}
}

// The aggregate outcome belongs to applyAggregateState once every slot has
// reported. One slot reaching RUNNING must not declare the whole deployment
// running while a sibling is still pulling an image.
func TestSlotUpdatesNeverWriteATerminalState(t *testing.T) {
	for _, terminal := range []provisionerv1.DeploymentState{
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_DEGRADED,
	} {
		t.Run(terminal.String(), func(t *testing.T) {
			got, ok := provisioners.ProvisioningAdvanceForTest(
				provisionerv1.DeploymentState_DEPLOYMENT_STATE_PENDING, terminal)
			if ok {
				t.Errorf("a slot was allowed to set %v (got %v)", terminal, got)
			}
		})
	}
}

// A slow slot emitting STARTING after a faster one reached CONFIGURING must
// not appear to undo progress.
func TestSlotStateIsForwardOnly(t *testing.T) {
	_, ok := provisioners.ProvisioningAdvanceForTest(
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_STARTING)
	if ok {
		t.Error("state regressed from CONFIGURING to STARTING")
	}

	got, ok := provisioners.ProvisioningAdvanceForTest(
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_PENDING,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING)
	if !ok || got != provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING {
		t.Errorf("advance = %v, ok=%v; want forward movement to CONFIGURING", got, ok)
	}
}

// "Which replica is stuck" has to be answerable from the record, and a
// single-slot deployment reads better without the noise.
func TestSlotLabelling(t *testing.T) {
	if got := provisioners.SlotLabelForTest([]string{"d1"}, "d1", "vast:rent"); got != "vast:rent" {
		t.Errorf("single-slot label = %q, want it unprefixed", got)
	}
	got := provisioners.SlotLabelForTest([]string{"d1-r0", "d1-r1"}, "d1-r1", "vast:rent")
	if !strings.HasPrefix(got, "d1-r1: ") {
		t.Errorf("multi-slot label = %q, want the replica id prefixed", got)
	}
}
