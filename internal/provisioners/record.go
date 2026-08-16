package provisioners

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// runtimeDeploymentFields are the fields the control plane owns rather than the
// operator. A fresh record starts with all of them cleared, whatever the
// request said.
//
// Everything not on this list is carried through from the request, and that
// default is the point. The old constructors listed the fields to KEEP, so a
// new operator-facing field was dropped unless someone remembered to add it in
// two places. Nothing failed when they forgot: the request carried the value,
// validation saw it, and the stored record did not have it, so every later
// reader behaved as though the operator had never set it. Outbound auth shipped
// that way and no forwarded request ever carried a credential (#182, #312).
//
// Inverting the default moves the failure. A new RUNTIME field left off this
// list is now trusted from the request rather than reset. That is the milder
// mistake by some distance: operators do not send progress messages, and a
// stray one is cosmetic, where a dropped credential is a feature that silently
// does nothing. TestEveryDeploymentFieldIsClassified fails when a new field is
// added without deciding which kind it is, so neither mistake should survive
// review.
var runtimeDeploymentFields = []protoreflect.Name{
	"state",
	"created_at",
	"started_at",
	"ready_at",
	"terminated_at",
	"last_activity_at",
	"current_phase",
	"progress_message",
	"failure_reason",
	"container_id",
	"engine_endpoint",
	"engine_endpoints",
	"instance_ids",
	"unhealthy_instance_ids",
	"replica_specs",
}

// newDeploymentRecord builds the stored record for a newly created deployment.
//
// Callers set whatever else their path owns afterwards. The two create paths
// (single-instance and fan-out) had drifted before this existed: one carried
// engine_entrypoint and the other did not, for no reason anybody had recorded.
func newDeploymentRecord(req *provisionerv1.Deployment, now *timestamppb.Timestamp) *provisionerv1.Deployment {
	rec := proto.Clone(req).(*provisionerv1.Deployment)

	m := rec.ProtoReflect()
	fields := m.Descriptor().Fields()
	for _, name := range runtimeDeploymentFields {
		if fd := fields.ByName(name); fd != nil {
			m.Clear(fd)
		}
	}

	rec.State = provisionerv1.DeploymentState_DEPLOYMENT_STATE_PENDING
	rec.CreatedAt = now
	return rec
}
