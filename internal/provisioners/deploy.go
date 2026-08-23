package provisioners

import (
	"context"
	"time"

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

	// HourlyRateUSD is what the provider charges for the machine this
	// deploy just rented, reported the moment it is known and zero on
	// every other update.
	//
	// An image-native deploy rents its own hardware, so it is the only
	// thing that ever sees the quoted price; Spawn's instance record has
	// nowhere to learn it from. Without this field a deployed instance
	// carried a zero rate, and zero means unknown rather than free, so
	// CostRecorder omitted the rental from instance.rate.usd_per_second
	// and spend, which is a join on instance_id, silently lost the box
	// (#397).
	HourlyRateUSD float64

	// Deadline is when the wait producing this update gives up, or the zero
	// time when the emitter has no deadline to report.
	//
	// Carried so a consumer can reason about the time left rather than only
	// about the time spent. The engine-ready timeout belongs to the provider
	// (it is per-adapter and IPLANE_ENGINE_READY_TIMEOUT overrides it), so
	// nothing upstream could otherwise tell whether an hour of downloading
	// has fifty minutes left or five.
	Deadline time.Time
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
