package provisioners

import (
	"strconv"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/engineagent"
	"google.golang.org/protobuf/proto"
)

// withAgentEnv returns a copy of dep whose Env additionally carries this
// slot's agent identity. The original is left alone: it is the shared record
// every slot in a fan-out reads from, and each slot needs a different
// identity.
//
// # What can and cannot be stamped here
//
// Env is fixed when the container is created, and two of the fields the
// agent would most like are not known at that moment:
//
//   - the **endpoint**, which on RunPod is derived from the pod id
//     (proxyEndpointForPod) and so does not exist until create returns
//   - the **host id**, the provider's machine id, which populates on the pod
//     record seconds after create (see the RunPod machine-field gotcha) and
//     is unreadable from inside the container in any case
//
// So the deploy path stamps a **correlation key** rather than a full
// identity. The control plane already records both missing fields
// (`Hardware.metadata["<provider>.machine_id"]` and the deployment's
// `engine_endpoints[slot]`), so it completes the record when the
// registration arrives rather than trying to have told the box in advance.
// See engines.Locator.
//
// serviceURL is where the agent should register. It is left unstamped when
// the daemon has not been told its own externally reachable address, because
// a daemon behind NAT genuinely does not know it; the operator supplies it
// the same way `IPLANE_OTEL_ENDPOINT` is supplied for engine telemetry, and
// `iplane telemetry url` discovers a cloudflared tunnel for the local case.
// An unstamped URL means the agent does not register, which shows up as a
// missing fleet member rather than as a wrong one.
//
// Operator-supplied values in dep.Env win over everything computed here.
// Deployment.Env is the operator's pass-through, and silently overwriting a
// key they set would be the kind of surprise that costs an afternoon. The
// identity keys are ours to compute only because nothing else sets them.
func withAgentEnv(dep *provisionerv1.Deployment, engineID string, slot int, provider, serviceURL string) *provisionerv1.Deployment {
	out := proto.Clone(dep).(*provisionerv1.Deployment)
	if out.Env == nil {
		out.Env = map[string]string{}
	}

	stamp := map[string]string{
		engineagent.EnvEngineID:     engineID,
		engineagent.EnvDeploymentID: dep.GetId(),
		engineagent.EnvModel:        dep.GetModel(),
		engineagent.EnvProvider:     provider,
		engineagent.EnvNodeIndex:    strconv.Itoa(slot),
	}
	if serviceURL != "" {
		stamp[engineagent.EnvServiceURL] = serviceURL
	}
	if port := dep.GetEnginePort(); port != 0 {
		// The agent probes the engine over loopback inside the container, so
		// this follows the port the engine was actually told to listen on
		// rather than the agent's own default.
		stamp[engineagent.EnvHealthURL] = "http://127.0.0.1:" + strconv.Itoa(int(port)) + "/health"
	}

	for k, v := range stamp {
		if v == "" {
			continue
		}
		if _, operatorSet := out.Env[k]; operatorSet {
			continue
		}
		out.Env[k] = v
	}
	return out
}
