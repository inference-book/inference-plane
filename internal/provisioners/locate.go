package provisioners

import (
	"fmt"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// LocateEngineNode returns the node identity and external endpoint the
// control plane recorded for engineID, which is the replica instance id the
// deploy path stamped into the container.
//
// This exists because the two fields an agent would most like to report are
// the two it cannot know. The provider's machine id is unreadable from
// inside a container (docs/design/0007 finding 4), and the endpoint on
// RunPod is derived from a pod id that does not exist until create returns,
// so neither fits in an env var written at container-create time. Both are
// already recorded here: the machine id in Hardware.metadata under the
// adapter's "<provider>.machine_id" key, and the endpoint in the
// deployment's engine_endpoints slot.
//
// So the box is told a correlation key and the control plane completes the
// record. found is false for an engine that registered without iplane
// having provisioned it, which is a legitimate case and not an error: such
// an engine reports whatever it knows and nothing is filled in.
func (s *Service) LocateEngineNode(engineID string) (hostID, provider, endpoint string, found bool, err error) {
	file, err := s.store.Read()
	if err != nil {
		return "", "", "", false, fmt.Errorf("locate engine %q: %w", engineID, err)
	}

	inst, ok := file.Instances[engineID]
	if !ok {
		return "", "", "", false, nil
	}
	provider = inst.GetProvider()
	hostID = machineIDFromMetadata(inst, provider)

	// The endpoint lives on the deployment, at the slot whose instance this
	// is. Scanning rather than indexing because the deployment id is not
	// derivable from the instance id in the single-replica case, where
	// replicaInstanceID collapses the slot suffix away.
	for _, dep := range file.Deployments {
		for slot, id := range dep.GetInstanceIds() {
			if id != engineID {
				continue
			}
			if eps := dep.GetEngineEndpoints(); slot < len(eps) {
				endpoint = eps[slot]
			}
			// engine_endpoints is the plural, per-replica set. The legacy
			// singular is only stamped for the count==1 case, so it is a
			// fallback and never the primary read.
			if endpoint == "" {
				endpoint = dep.GetEngineEndpoint()
			}
			return hostID, provider, endpoint, true, nil
		}
	}

	// An instance with no deployment pointing at it is still a located node:
	// the host id is real even when nothing is deployed onto it yet.
	return hostID, provider, "", true, nil
}

// machineIDFromMetadata pulls the provider's machine id out of the instance's
// provider-specific metadata bag.
//
// The key convention is "<provider>.machine_id", which the RunPod and Vast
// adapters both already populate. Returning empty for a provider that does
// not record one is correct rather than a failure: not every provider
// exposes a host identity, and an empty host id reads as an unattributable
// node, which is exactly what it is.
func machineIDFromMetadata(inst *provisionerv1.Instance, provider string) string {
	meta := inst.GetMetadata()
	if meta == nil || provider == "" {
		return ""
	}
	v, ok := meta[provider+".machine_id"]
	if !ok {
		return ""
	}
	// Vast records the id as a number and RunPod as a string, so both shapes
	// are read rather than one being declared canonical at this seam. The
	// value is opaque to the control plane either way.
	if s := v.GetStringValue(); s != "" {
		return s
	}
	if n := v.GetNumberValue(); n != 0 {
		return fmt.Sprintf("%d", int64(n))
	}
	return ""
}
