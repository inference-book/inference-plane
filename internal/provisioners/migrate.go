package provisioners

import (
	"context"
	"fmt"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// MigrateRequest asks for a running deployment to be served from somewhere
// else.
type MigrateRequest struct {
	// DeploymentID is the deployment to move. Its id does not change: the
	// whole point is that callers keep addressing the same thing.
	DeploymentID string

	// To names the destination. Provider is required; Requirements is
	// optional and defaults to whatever the source replicas were asked for,
	// so "same shape, different vendor" is the short form.
	To *provisionerv1.ReplicaSpec

	// Drain tunes the release half. The source replicas are quarantined and
	// given this long to finish in-flight work.
	Drain DrainOptions

	// DryRun plans the move and returns it without provisioning anything.
	DryRun bool
}

// MigratePlan is what a migration would do, or did.
type MigratePlan struct {
	DeploymentID string
	FromProvider string
	ToProvider   string

	// SourceInstances are the replicas that will be drained and released.
	SourceInstances []string

	// AddedInstances are the replicas provisioned at the destination. Empty
	// on a dry run.
	AddedInstances []string

	// ReplicaCount is how many replicas move.
	ReplicaCount int

	// WarmCacheFollows is false when the model is not staged at the
	// destination and the move therefore pays a cold start.
	WarmCacheFollows bool

	// Warnings are the things an operator should read before starting, not
	// after. Cold start is the usual one and it dominates the cost.
	Warnings []string
}

// Migrate moves a running deployment to another provider without
// changing its id or dropping a request.
//
// # Why it is composed rather than written
//
// Both halves already ship. ScaleDeployment's heterogeneous form provisions
// replicas on any provider and adds them to an existing deployment;
// DrainReplicas quarantines a subset and waits out in-flight work. The router
// already fans out over whatever endpoints a deployment currently has, so
// traffic moves as a consequence of the endpoint set changing rather than
// through a cutover step that could drop something.
//
// So a migration is: grow onto the destination, wait for it to be genuinely
// serving, then drain the source. No new graceful-shutdown code, and no second
// deployment id for callers to chase.
//
// # Why grow-then-drain, and why order is the whole design
//
// This ordering is only available when nothing is being taken away. A
// migration has no deadline, which is what lets the destination be fully ready
// before the source stops, and that is the ordering that never drops a
// request.
//
// A reclaim (#289) does not get that luxury: a reclaim notice is often a
// couple of minutes and a cold start is longer, so the hardware can vanish
// mid-provision. Building the unhurried case first gives that one something to
// degrade from rather than having to invent the sequence under time pressure.
//
// # The cost nobody expects
//
// Weights are staged per provider and per region. A migration to a vendor
// where the model is not pinned pays Ch 9's cold start again, and on a big
// model that is minutes, not seconds. An operator who did not expect it will
// think the migration hung. So the plan says so before anything is
// provisioned, rather than leaving them to infer it from a progress bar that
// has stopped moving.
func (s *Service) Migrate(ctx context.Context, req MigrateRequest) (*MigratePlan, error) {
	if err := ValidateID(req.DeploymentID); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if req.To.GetProvider() == "" {
		return nil, status.Error(codes.InvalidArgument, "migrate: --to requires a destination provider")
	}

	file, err := s.store.Read()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	rec, ok := file.Deployments[req.DeploymentID]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no deployment with id %q", req.DeploymentID)
	}
	switch rec.GetState() {
	case provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_DEGRADED:
		// migrate-eligible, same precondition as scale: this is capacity
		// tuning of something already serving, not a recovery mechanism.
	default:
		return nil, status.Errorf(codes.FailedPrecondition,
			"deployment %q must be RUNNING or DEGRADED to migrate (got %s)", req.DeploymentID, rec.GetState())
	}

	source := EffectiveInstanceIDs(rec)
	if len(source) == 0 && rec.GetInstanceId() != "" {
		source = []string{rec.GetInstanceId()}
	}
	if len(source) == 0 {
		return nil, status.Errorf(codes.FailedPrecondition,
			"deployment %q has no replicas to migrate", req.DeploymentID)
	}

	dest := destinationSpec(req.To, rec, file, source)
	plan := &MigratePlan{
		DeploymentID:    req.DeploymentID,
		FromProvider:    sourceProvider(rec, file, source),
		ToProvider:      dest.GetProvider(),
		SourceInstances: source,
		ReplicaCount:    len(source),
	}

	plan.WarmCacheFollows = modelStagedAt(file, rec.GetModel(), dest.GetProvider(), dest.GetRegion())
	if !plan.WarmCacheFollows {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"%s is not pinned on %s%s, so every new replica downloads the weights again; "+
				"on a large model that is minutes of cold start, not seconds",
			rec.GetModel(), dest.GetProvider(), regionSuffix(dest.GetRegion())))
	}
	if plan.FromProvider == plan.ToProvider {
		plan.Warnings = append(plan.Warnings,
			"source and destination are the same provider; this re-places rather than migrates")
	}

	if req.DryRun {
		return plan, nil
	}

	// Grow first. The destination has to be genuinely serving before anything
	// is taken away, and ScaleDeployment already waits for engine readiness
	// rather than for the rental to exist.
	added := make([]*provisionerv1.ReplicaSpec, 0, len(source))
	for range source {
		one := &provisionerv1.ReplicaSpec{
			Provider:     dest.GetProvider(),
			Region:       dest.GetRegion(),
			Requirements: dest.GetRequirements(),
			Replicas:     1,
		}
		added = append(added, one)
	}
	scaled, err := s.ScaleDeployment(ctx, &provisionerv1.ScaleDeploymentRequest{
		Id:          req.DeploymentID,
		AddReplicas: added,
	})
	if err != nil {
		return nil, fmt.Errorf("migrate %q: growing onto %s: %w", req.DeploymentID, dest.GetProvider(), err)
	}

	// Whatever is in instance_ids now and was not in the source set is what we
	// just added. Derived rather than assumed, because a concurrent scale
	// would otherwise have this draining somebody else's new replicas.
	for _, id := range EffectiveInstanceIDs(scaled.GetDeployment()) {
		if !slices.Contains(source, id) {
			plan.AddedInstances = append(plan.AddedInstances, id)
		}
	}
	if len(plan.AddedInstances) == 0 {
		return nil, fmt.Errorf("migrate %q: growing onto %s added no replicas; the source is untouched",
			req.DeploymentID, dest.GetProvider())
	}

	// Only now release the source. A drain whose grace period expires is not
	// an error: the timeout is the operator's stated budget.
	if err := s.DrainReplicas(ctx, req.DeploymentID, source, req.Drain); err != nil {
		return plan, fmt.Errorf("migrate %q: destination is serving but draining the source failed: %w",
			req.DeploymentID, err)
	}
	return plan, nil
}

// destinationSpec fills in what the caller did not say.
//
// Requirements default to the source replicas' own, so "same shape, somewhere
// else" is the short form and the common one. An operator moving because a
// vendor ran out of stock wants the same hardware, not a new negotiation.
func destinationSpec(to *provisionerv1.ReplicaSpec, rec *provisionerv1.Deployment, file *State, source []string) *provisionerv1.ReplicaSpec {
	out := &provisionerv1.ReplicaSpec{
		Provider:     to.GetProvider(),
		Region:       to.GetRegion(),
		Requirements: to.GetRequirements(),
	}
	if out.Requirements != nil {
		return out
	}
	for _, spec := range rec.GetReplicaSpecs() {
		if spec.GetRequirements() != nil {
			out.Requirements = spec.GetRequirements()
			return out
		}
	}
	// Older records predate replica_specs; the anchor instance's resolved
	// Spec is the best available signal, same fallback ScaleDeployment uses.
	for _, id := range source {
		if inst := file.Instances[id]; inst.GetSpec().GetRequirements() != nil {
			out.Requirements = inst.GetSpec().GetRequirements()
			return out
		}
	}
	return out
}

func sourceProvider(rec *provisionerv1.Deployment, file *State, source []string) string {
	for _, id := range source {
		if inst := file.Instances[id]; inst != nil && inst.GetProvider() != "" {
			return inst.GetProvider()
		}
	}
	for _, spec := range rec.GetReplicaSpecs() {
		if spec.GetProvider() != "" {
			return spec.GetProvider()
		}
	}
	return ""
}

// modelStagedAt reports whether the model is already pinned on a volume at the
// destination, which is the difference between a warm move and a cold one.
//
// A volume is provider-scoped and datacenter-locked, so both have to match. An
// empty destination region cannot be matched against a region-locked volume,
// and the honest reading of that is "we do not know it follows" rather than
// "it does".
func modelStagedAt(file *State, model, provider, region string) bool {
	if model == "" || provider == "" || region == "" {
		return false
	}
	for _, v := range file.Volumes {
		if v.GetProvider() == provider && v.GetRegion() == region && slices.Contains(v.GetModels(), model) {
			return true
		}
	}
	return false
}

func regionSuffix(region string) string {
	if region == "" {
		return ""
	}
	return " in " + region
}

// MigrateDeployment is the RPC surface over Migrate.
//
// Thin on purpose. The proto types stop at this boundary and the reasoning
// lives in Migrate, so the CLI, a future scheduler, and a reclaim handler all
// reach the same decision rather than three adaptations of it.
func (s *Service) MigrateDeployment(ctx context.Context, req *provisionerv1.MigrateDeploymentRequest) (*provisionerv1.MigrateDeploymentResponse, error) {
	plan, err := s.Migrate(ctx, MigrateRequest{
		DeploymentID: req.GetId(),
		To:           req.GetTo(),
		Drain: DrainOptions{
			Timeout: time.Duration(req.GetDrainTimeoutSec()) * time.Second,
			Force:   req.GetForce(),
		},
		DryRun: req.GetDryRun(),
	})
	if err != nil {
		return nil, err
	}
	return &provisionerv1.MigrateDeploymentResponse{
		Id:                plan.DeploymentID,
		FromProvider:      plan.FromProvider,
		ToProvider:        plan.ToProvider,
		SourceInstanceIds: plan.SourceInstances,
		AddedInstanceIds:  plan.AddedInstances,
		WarmCacheFollows:  plan.WarmCacheFollows,
		Warnings:          plan.Warnings,
	}, nil
}
