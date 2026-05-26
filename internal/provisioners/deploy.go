package provisioners

import (
	"context"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/sshkeys"
)

// DeployStateUpdate is what a deployment executor emits as it
// progresses through the state machine. The Service consumes these
// to patch the state file + emit WatchDeployment events.
//
// Lives in the provisioners package (not in sshdocker) so a provider
// adapter can implement the Deployer capability without importing
// the sshdocker sibling. sshdocker.StateUpdate is a type alias to
// this struct for backward compatibility.
type DeployStateUpdate struct {
	State           provisionerv1.DeploymentState
	Phase           string
	ProgressMessage string
	ContainerID     string
	EngineEndpoint  string
	FailureReason   string
}

// Deployer is an optional Provider capability for image-native
// providers (RunPod, Vast.ai, Modal, Replicate -- anything that
// accepts a docker image as the workload primitive). When the
// provider satisfies this interface, Service.CreateDeployment
// dispatches to it instead of the configured fallback executor
// (typically sshdocker, used for VM-style providers like Lambda
// Labs and raw AWS/GCP instances).
//
// The capability check fits the same pattern as KeyRegistrar and
// SSHReadyWaiter: providers opt in by satisfying the interface;
// the Service picks the best path at runtime.
//
// Method shape mirrors the existing DeploymentExecutor interface so
// the dispatch is a one-line `provider.(Deployer)` check; no
// translation layer needed between the two paths.
type Deployer interface {
	Deploy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, key *sshkeys.KeyPair, emit func(DeployStateUpdate)) error
	Destroy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, key *sshkeys.KeyPair, emit func(DeployStateUpdate)) error
}
