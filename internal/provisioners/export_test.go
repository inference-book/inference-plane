package provisioners

import (
	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// OwnsInstanceForTest exposes the deployment-owns-instance rule to the
// external test package. The rule decides whether tearing down a deployment
// may also terminate an Instance record, so it is worth testing directly
// rather than only through the teardown path.
func OwnsInstanceForTest(deployID, instanceID string) bool {
	return ownsInstance(deployID, instanceID)
}

// ProvisioningAdvanceForTest exposes the slot-to-deployment state rule.
// Worth testing directly: it encodes which states a single slot may write
// onto the shared record, and getting it wrong would let one replica
// declare the whole deployment running while its siblings are still
// provisioning.
func ProvisioningAdvanceForTest(current, update provisionerv1.DeploymentState) (provisionerv1.DeploymentState, bool) {
	return provisioningAdvance(current, update)
}

// SlotLabelForTest exposes the per-slot message prefixing.
func SlotLabelForTest(instanceIDs []string, replicaInstanceID, msg string) string {
	return slotLabel(instanceIDs, replicaInstanceID, msg)
}
