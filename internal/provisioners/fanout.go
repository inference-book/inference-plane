package provisioners

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/sshkeys"
)

// replicaInstanceID names the per-replica Instance backing slot i of a
// multi-replica Deployment. The naming convention has two cases:
//
//   - totalSlots == 1: single-instance deployment -> use the deploy_id
//     itself (no -r0 suffix). Preserves Ch 6's 1:1 mapping where
//     `iplane instance describe my-llama` finds the GPU pod directly.
//   - totalSlots > 1: multi-replica -> deploy_id-r0, -r1, ... so each
//     replica's Instance record looks up independently.
//
// Stable naming -- no collision risk with arbitrary operator-supplied
// instance ids because deploy ids cannot contain "-r<digits>" by
// ValidateID's character set. Predictable for operators reading state.
func replicaInstanceID(deployID string, slot, totalSlots int) string {
	if totalSlots == 1 {
		return deployID
	}
	return fmt.Sprintf("%s-r%d", deployID, slot)
}

// fanOutResult carries one replica's outcome back to the aggregator.
// endpoint is empty when the deploy failed or wait_for_engine never
// returned a URL; err carries the failure reason for DEGRADED /
// FAILED state messages.
type fanOutResult struct {
	instanceID string
	endpoint   string
	err        error
}

// ScaleDeployment implements the v0.2 ch7-beat3.8 scale verb (#86).
// Reads the deployment, computes delta = target - current. Three
// outcomes:
//
//   - target == current: no-op, returns the current record.
//   - target > current: appendReplicas provisions delta new slots
//     starting at the next slot index (existing slots are not
//     touched, even DEGRADED tombstones -- scale appends, doesn't
//     repair). Aggregate state stays RUNNING if all new succeed,
//     transitions to DEGRADED if any new fail.
//   - target < current: Unimplemented; scale-down's drain semantics
//     are designed in #145.
//
// Precondition: deployment is RUNNING or DEGRADED. Other states
// (PENDING, STARTING, TERMINATING, TERMINATED, FAILED) are rejected
// -- scale is operator capacity-tuning of an already-serving
// deployment, not a recovery mechanism.
//
// dry_run=true returns the planned new instance_ids without
// mutating state. The CLI surfaces these to the operator for
// confirmation.
func (s *Service) ScaleDeployment(ctx context.Context, req *provisionerv1.ScaleDeploymentRequest) (*provisionerv1.ScaleDeploymentResponse, error) {
	id := req.GetId()
	if err := ValidateID(id); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	// target_replicas + add_replicas are mutually exclusive forms:
	// pick one per call. Heterogeneous (add_replicas) is the v0.2
	// ch7-beat3.9 (#143) form; target_replicas is the homogeneous
	// shortcut that extends the existing fleet's anchor shape.
	target := int(req.GetTargetReplicas())
	hetero := req.GetAddReplicas()
	switch {
	case target > 0 && len(hetero) > 0:
		return nil, status.Error(codes.InvalidArgument, "target_replicas and add_replicas are mutually exclusive -- use one form per call")
	case target == 0 && len(hetero) == 0:
		return nil, status.Error(codes.InvalidArgument, "scale requires either target_replicas > 0 or add_replicas (heterogeneous)")
	case target < 0:
		return nil, status.Errorf(codes.InvalidArgument, "target_replicas must be > 0 (got %d)", target)
	}

	file, err := s.store.Read()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	rec, ok := file.Deployments[id]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no deployment with id %q", id)
	}
	switch rec.GetState() {
	case provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_DEGRADED:
		// scale-eligible
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "deployment %q must be RUNNING or DEGRADED to scale (got %s)", id, rec.GetState())
	}

	current := len(rec.GetInstanceIds())
	if current == 0 {
		// Beat 1+2 single-instance deployment: instance_ids is empty,
		// the singular instance_id is the only slot. Treat as current=1
		// so scale can extend it to multi-replica.
		if rec.GetInstanceId() != "" {
			current = 1
		}
	}

	// Normalize Beat 1+2 records before scaling: if the deployment
	// has a singular instance_id but no instance_ids list, promote
	// the singular into instance_ids[0] / engine_endpoints[0] /
	// replica_specs[0]. The scale-up loop below appends starting at
	// slot 1, so without this, slot 0 would be a phantom tombstone
	// (empty id, even though the original instance is still serving
	// via the singular fields). Idempotent: re-running on an
	// already-normalized record is a no-op.
	//
	// replica_specs[0] is derived from the anchor Instance's Spec
	// (the resolved form -- class -> sku expansion already
	// happened). Beat 1+2 records predate replica_specs so the
	// operator-intent shape was never stored; the resolved form
	// from the Instance is the best available signal.
	if len(rec.GetInstanceIds()) == 0 && rec.GetInstanceId() != "" {
		anchorID := rec.GetInstanceId()
		anchorInst, anchorOK := file.Instances[anchorID]
		_ = s.store.Update(func(f *State) error {
			r := f.Deployments[id]
			if r == nil || len(r.GetInstanceIds()) > 0 {
				return nil
			}
			r.InstanceIds = []string{r.GetInstanceId()}
			r.EngineEndpoints = []string{r.GetEngineEndpoint()}
			if anchorOK && anchorInst.GetSpec() != nil {
				r.ReplicaSpecs = []*provisionerv1.ReplicaSpec{{
					Provider:     anchorInst.GetProvider(),
					Region:       anchorInst.GetSpec().GetRegion(),
					Requirements: anchorInst.GetSpec().GetRequirements(),
				}}
			}
			return nil
		})
		// Re-read so subsequent logic sees the normalized record.
		if f2, rerr := s.store.Read(); rerr == nil {
			if updated, ok := f2.Deployments[id]; ok {
				rec = updated
				file = f2
			}
		}
	}

	// Build per-slot ReplicaSpecs for the new replicas. Heterogeneous
	// path takes specs verbatim; homogeneous path replicates the
	// anchor instance's spec delta times.
	var specs []*provisionerv1.ReplicaSpec
	if len(hetero) > 0 {
		specs = hetero
	} else {
		delta := target - current
		switch {
		case delta == 0:
			return &provisionerv1.ScaleDeploymentResponse{Deployment: rec}, nil
		case delta < 0:
			return nil, status.Errorf(codes.Unimplemented, "scale-down (target=%d, current=%d) lands in #145; v0.2 ch7-beat3.8 ships scale-up only", target, current)
		}
		anchorSpec, asErr := s.anchorReplicaSpec(file, rec)
		if asErr != nil {
			return nil, asErr
		}
		specs = make([]*provisionerv1.ReplicaSpec, delta)
		for i := range specs {
			specs[i] = anchorSpec
		}
	}

	// Plan: synthesize the new slot ids. Used for both dry-run
	// preview and the actual provision path. After scale the total
	// slot count is current+len(specs); we pass that as totalSlots
	// so the carve-out for single-instance naming doesn't apply
	// (scale-up always results in totalSlots > 1, so new slots get
	// the -rN form even when growing from a single-instance
	// deployment that named slot 0 as the bare deploy_id).
	totalSlots := current + len(specs)
	plannedIDs := make([]string, len(specs))
	for i := range specs {
		plannedIDs[i] = replicaInstanceID(id, current+i, totalSlots)
	}
	if req.GetDryRun() {
		return &provisionerv1.ScaleDeploymentResponse{
			Deployment:         rec,
			PlannedInstanceIds: plannedIDs,
		}, nil
	}

	runCtx := context.Background()
	if req.GetWait() {
		runCtx = ctx
		s.appendReplicas(runCtx, rec, id, specs, current)
		if file2, rerr := s.store.Read(); rerr == nil {
			if updated, ok := file2.Deployments[id]; ok {
				rec = updated
			}
		}
	} else {
		go s.appendReplicas(runCtx, rec, id, specs, current)
	}
	return &provisionerv1.ScaleDeploymentResponse{Deployment: rec}, nil
}

// anchorReplicaSpec returns the (provider, region, requirements)
// shape a homogeneous scale-up call (target_replicas) should
// replicate. For heterogeneous fleets, scale's add_replicas form
// names each new spec explicitly and this helper is bypassed.
//
// Resolution order:
//  1. dep.replica_specs[0] (v0.2 ch7-beat3.9 onward) -- the
//     operator's intent shape, preserved verbatim. This is the
//     preferred path because it reflects what was requested, not
//     what the provider resolved to (Instance.spec.requirements
//     after class -> sku expansion).
//  2. Slot-0 Instance.spec (Beat 1+2 / pre-#143 records that
//     predate replica_specs population). Resolved shape; sufficient
//     for replication but loses the operator's class-shorthand
//     view.
//
// Fails when neither path yields a spec -- the deployment record
// is inconsistent with both the new operator-intent and the
// per-Instance fallback, and scale can't pick a shape to extend.
func (s *Service) anchorReplicaSpec(file *State, rec *provisionerv1.Deployment) (*provisionerv1.ReplicaSpec, error) {
	if specs := rec.GetReplicaSpecs(); len(specs) > 0 && specs[0] != nil {
		return specs[0], nil
	}
	var anchorID string
	if ids := rec.GetInstanceIds(); len(ids) > 0 && ids[0] != "" {
		anchorID = ids[0]
	} else {
		anchorID = rec.GetInstanceId()
	}
	if anchorID == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "deployment %q has no anchor instance to scale from (state file inconsistent)", rec.GetId())
	}
	anchor, ok := file.Instances[anchorID]
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "deployment %q's anchor instance %q is missing from the state file", rec.GetId(), anchorID)
	}
	spec := anchor.GetSpec()
	if spec == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "anchor instance %q has no spec (cannot extend the fleet)", anchorID)
	}
	return &provisionerv1.ReplicaSpec{
		Provider:     anchor.GetProvider(),
		Region:       spec.GetRegion(),
		Requirements: spec.GetRequirements(),
	}, nil
}

// appendReplicas extends the deployment's slot table by delta new
// replicas starting at currentCount. Reuses provisionSlots + the
// scale-specific recordAppendedSlots + applyScaleAggregate.
//
// Aggregate semantics:
//   - all delta succeed: deployment stays RUNNING (existing slots
//     were RUNNING per ScaleDeployment's precondition).
//   - any delta fail: transitions to DEGRADED with a failure_reason
//     listing the failed new slots. (Existing tombstones from a prior
//     fan-out partial failure stay; scale doesn't touch them. The
//     deployment's pre-scale state may have already been DEGRADED;
//     scale-up cannot make that better.)
func (s *Service) appendReplicas(ctx context.Context, dep *provisionerv1.Deployment, deployID string, specs []*provisionerv1.ReplicaSpec, currentCount int) {
	delta := len(specs)
	successes, failureReasons := s.provisionSlots(ctx, specs, dep, deployID, currentCount, currentCount+len(specs), s.recordAppendedSlots)
	s.applyScaleAggregate(deployID, delta, successes, failureReasons)
}

// applyScaleAggregate transitions deployment state after a scale-up
// append. Unlike applyAggregateState (which computes whole-deployment
// state from scratch for the create path), this preserves the
// existing slot table and only considers whether the new delta
// replicas all succeeded.
//
//   - delta successes == delta count: stay RUNNING (or DEGRADED if
//     pre-scale was DEGRADED -- this method doesn't lift DEGRADED
//     back to RUNNING; that's #93's reconciliation job).
//   - else: DEGRADED + failure_reason describing the new failures.
//
// failure_reason is overwritten on each scale call. A future
// reconciliation pass (#93) clears it when all slots become healthy.
func (s *Service) applyScaleAggregate(deployID string, delta, successes int, failureReasons []string) {
	_ = s.store.Update(func(f *State) error {
		rec, ok := f.Deployments[deployID]
		if !ok {
			return nil
		}
		if successes == delta {
			// All new slots succeeded. If the deployment was RUNNING
			// pre-scale, it stays RUNNING. If it was DEGRADED (existing
			// tombstones), it stays DEGRADED -- scale doesn't repair.
			return nil
		}
		rec.State = provisionerv1.DeploymentState_DEPLOYMENT_STATE_DEGRADED
		rec.FailureReason = fmt.Sprintf("%d of %d new replicas failed at scale-up: %s",
			delta-successes, delta, strings.Join(failureReasons, "; "))
		return nil
	})
}

// createMultiReplicaDeployment is the CreateDeployment branch for
// replicas > 1. The single-instance shared path handled validation
// + model resolve before dispatching here; this method owns the
// idempotency check, the deployment record persist, and the fan-out
// kick-off.
//
// Idempotency mirrors single-instance: an existing record at the
// same id with matching image+model is returned as AlreadyExisted
// (operator's re-run is a no-op). TERMINATED / FAILED records get
// reclaimed; TERMINATING records are rejected (wait for completion).
//
// Wait semantics:
//   - wait=true: fan-out runs synchronously on the request ctx;
//     return when aggregate state is terminal (RUNNING / DEGRADED /
//     FAILED).
//   - wait=false: persist PENDING immediately, dispatch fan-out on
//     a background ctx detached from the request, return PENDING.
//
// CP/DP-1: this method imports nothing from internal/router or
// internal/dataplane; it stays on the control-plane side of the
// constraint.
func (s *Service) createMultiReplicaDeployment(ctx context.Context, req *provisionerv1.CreateDeploymentRequest, specs []*provisionerv1.ReplicaSpec) (*provisionerv1.CreateDeploymentResponse, error) {
	dep := req.GetDeployment()
	var record *provisionerv1.Deployment
	var alreadyExisted bool
	err := s.store.Update(func(f *State) error {
		if existing, ok := f.Deployments[dep.GetId()]; ok {
			switch existing.GetState() {
			case provisionerv1.DeploymentState_DEPLOYMENT_STATE_PENDING,
				provisionerv1.DeploymentState_DEPLOYMENT_STATE_STARTING,
				provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING,
				provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				provisionerv1.DeploymentState_DEPLOYMENT_STATE_DEGRADED:
				if existing.GetImage() == dep.GetImage() && existing.GetModel() == dep.GetModel() {
					record = existing
					alreadyExisted = true
					return nil
				}
			case provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATING:
				return fmt.Errorf("deployment %q is currently terminating; wait for completion", dep.GetId())
			}
			// TERMINATED / FAILED: treat as gone; claim a fresh record.
		}
		now := timestamppb.New(s.clock())
		record = &provisionerv1.Deployment{
			Id:             dep.GetId(),
			Image:          dep.GetImage(),
			Model:          dep.GetModel(),
			EngineArgs:     dep.GetEngineArgs(),
			Env:            dep.GetEnv(),
			EnginePort:     dep.GetEnginePort(),
			State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_PENDING,
			CreatedAt:      now,
			DebugShell:     dep.GetDebugShell(),
			IdleTtlSeconds: dep.GetIdleTtlSeconds(),
			NoIdleDestroy:  dep.GetNoIdleDestroy(),
		}
		f.Deployments[dep.GetId()] = record
		return nil
	})
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if alreadyExisted {
		return &provisionerv1.CreateDeploymentResponse{Deployment: record, AlreadyExisted: true}, nil
	}

	runCtx := context.Background()
	if req.GetWait() {
		runCtx = ctx
		s.fanOutProvision(runCtx, req, record.GetId(), specs)
		// Re-read the record to surface terminal aggregate state.
		if file, rerr := s.store.Read(); rerr == nil {
			if final, ok := file.Deployments[record.GetId()]; ok {
				record = final
			}
		}
	} else {
		go s.fanOutProvision(runCtx, req, record.GetId(), specs)
	}
	return &provisionerv1.CreateDeploymentResponse{Deployment: record}, nil
}

// fanOutProvision drives the multi-replica create path. Called from
// CreateDeployment when replicas > 1. startingSlot is always 0 here
// (Create starts from an empty slot table); the parameter exists so
// the placement + launch machinery is shared with Scale, where
// startingSlot is the current replica count and the new slots
// append. Sequence:
//
//  1. Place N per-replica Instances in parallel (deploy_id-r0..r(N-1)).
//  2. Persist the Deployment record's instance_ids list -- placed
//     slots get their synthesized id; place-failures get "" (router
//     skips empty-id slots).
//  3. Launch the deployment executor for each placed Instance in
//     parallel. Each replica's emit closure writes only its slot in
//     engine_endpoints via patchDeploymentSlot (not the deployment-
//     level State / Phase fields).
//  4. After all goroutines drain, aggregate state:
//     - all N succeed -> RUNNING
//     - 1 <= M < N succeed -> DEGRADED + failure_reason
//     - 0 succeed -> FAILED + failure_reason
//
// The deployment record must already exist in the state file before
// this call (CreateDeployment persists it with State=PENDING and
// empty instance_ids). fanOutProvision drives the slot transitions
// and the terminal state.
//
// Synchronous wrt the caller: it returns when the aggregate state
// is stamped. CreateDeployment runs it on the request ctx when
// wait=true, or in a detached background goroutine when wait=false.
func (s *Service) fanOutProvision(ctx context.Context, req *provisionerv1.CreateDeploymentRequest, deployID string, specs []*provisionerv1.ReplicaSpec) {
	count := len(specs)
	successes, failureReasons := s.provisionSlots(ctx, specs, req.GetDeployment(), deployID, 0, len(specs), s.recordCreateSlots)
	s.applyAggregateState(deployID, count, successes, failureReasons)
}

// provisionSlots is the shared placement + launch primitive used by
// both create (startingSlot=0) and scale (startingSlot=current).
// specs carries the per-slot (provider, region, requirements);
// dep is the Deployment record (carries Image, Env, EnginePort
// shared across all slots). recordSlots is the caller's slot-
// persistence strategy: create overwrites; scale appends.
//
// Returns the count of successful slots and the failure reasons for
// failed slots (used by the caller's aggregate-state routine).
//
// Heterogeneous-aware: each slot's executor runs against its own
// provider's Deployer (resolved via deployerFor on the per-slot
// Instance). The SSH key per-slot is loaded by provider name; a
// keystore failure for one provider doesn't break other slots.
func (s *Service) provisionSlots(ctx context.Context, specs []*provisionerv1.ReplicaSpec, dep *provisionerv1.Deployment, deployID string, startingSlot, totalSlots int, recordSlots func(deployID string, placements []*provisionerv1.Instance, startingSlot int)) (int, []string) {
	count := len(specs)
	placements, placeErrs := s.placeSlots(ctx, specs, dep.GetImage(), deployID, startingSlot, totalSlots)
	// Stash the per-slot specs on the Service so recordSlots can
	// pick them up (the recordSlots signature is shared between
	// create/scale variants; passing specs through would have meant
	// either a wider signature or threading specs into every Option
	// type). Single fan-out call per Service per moment, so the
	// stash is contention-free; even so, takePendingReplicaSpecs
	// clears it on read.
	s.pendingReplicaSpecsMu.Lock()
	s.pendingReplicaSpecs = specs
	s.pendingReplicaSpecsMu.Unlock()
	recordSlots(deployID, placements, startingSlot)

	results := make(chan fanOutResult, count)
	for i, inst := range placements {
		slot := startingSlot + i
		if inst == nil {
			results <- fanOutResult{
				instanceID: replicaInstanceID(deployID, slot, totalSlots),
				err:        placeErrs[i],
			}
			continue
		}
		// Load the SSH key per slot (heterogeneous: each provider has
		// its own keypair). One provider's keystore failure doesn't
		// break other slots -- the failure for that slot is recorded
		// as a fanOutResult with err set, and the aggregator counts
		// it like any other failed replica.
		key, keyErr := s.replicaKey(specs[i].GetProvider())
		if keyErr != nil {
			results <- fanOutResult{
				instanceID: inst.GetId(),
				err:        keyErr,
			}
			continue
		}
		go s.launchReplica(ctx, deployID, slot, inst, key, dep, results)
	}

	successes := 0
	var failureReasons []string
	for range count {
		r := <-results
		if r.err == nil {
			successes++
		} else {
			failureReasons = append(failureReasons, fmt.Sprintf("%s: %v", r.instanceID, r.err))
		}
	}
	return successes, failureReasons
}

// takePendingReplicaSpecs returns the specs stashed by the most
// recent provisionSlots call, clearing the stash so a subsequent
// fan-out can stash its own without leaking. Returns nil if no
// specs are pending (e.g., a legacy code path that doesn't use the
// new shape).
func (s *Service) takePendingReplicaSpecs() []*provisionerv1.ReplicaSpec {
	s.pendingReplicaSpecsMu.Lock()
	defer s.pendingReplicaSpecsMu.Unlock()
	specs := s.pendingReplicaSpecs
	s.pendingReplicaSpecs = nil
	return specs
}

// replicaKey loads the SSH keypair for one provider. Per-slot helper
// for heterogeneous fan-out: each provider has its own keystore
// entry. Returns a non-nil error if the keystore is unconfigured or
// the load fails -- the caller treats this as a per-slot failure
// (the other slots in the fan-out can still succeed).
func (s *Service) replicaKey(providerName string) (*sshkeys.KeyPair, error) {
	if s.keyStore == nil {
		return nil, fmt.Errorf("keystore unconfigured (use provisioners.WithKeyStore)")
	}
	key, err := s.keyStore.EnsureKeyPair(s.operatorID, providerName)
	if err != nil {
		return nil, fmt.Errorf("load ssh key for provider %q: %v", providerName, err)
	}
	return key, nil
}

// placeSlots spawns len(specs) goroutines, each placing one per-replica
// Instance at globalSlot = startingSlot+i with specs[i]. Returns
// parallel slices (indexed 0..len(specs)-1, mapping to global slots
// startingSlot..startingSlot+len(specs)-1): placements[i] is the
// Instance for the local index i (nil on failure), placeErrs[i] is
// the matching error (nil on success).
//
// Heterogeneous-aware: each slot uses its own (provider, region,
// requirements) from specs[i]. Homogeneous fan-out passes N copies
// of the same spec; the helper does not distinguish.
//
// Idempotent: placeReplicaInstance reuses an existing Instance
// record if one already exists at the synthesized id (re-runs of a
// failed CreateDeployment don't double-rent).
func (s *Service) placeSlots(ctx context.Context, specs []*provisionerv1.ReplicaSpec, baseImage, deployID string, startingSlot, totalSlots int) ([]*provisionerv1.Instance, []error) {
	count := len(specs)
	placements := make([]*provisionerv1.Instance, count)
	placeErrs := make([]error, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inst, err := s.placeReplicaInstance(ctx, specs[i], baseImage, deployID, startingSlot+i, totalSlots)
			if err != nil {
				placeErrs[i] = err
				return
			}
			placements[i] = inst
		}(i)
	}
	wg.Wait()
	return placements, placeErrs
}

// recordCreateSlots persists the instance_ids / engine_endpoints /
// replica_specs parallel arrays for the create path (startingSlot
// is always 0; the call resizes the slot table from scratch).
// Successful placements stamp their instance id; place-failed slots
// remain "". This is the snapshot the router (#85) reads to decide
// which slots are eligible for round-robin selection.
//
// replica_specs is set by the caller (provisionSlots stashes the
// specs on the Service for the recorder to pick up). For
// recordCreateSlots, specs reflects the full slot table starting
// at slot 0.
func (s *Service) recordCreateSlots(deployID string, placements []*provisionerv1.Instance, _ int) {
	specs := s.takePendingReplicaSpecs()
	_ = s.store.Update(func(f *State) error {
		rec, ok := f.Deployments[deployID]
		if !ok {
			return nil
		}
		count := len(placements)
		rec.InstanceIds = make([]string, count)
		rec.EngineEndpoints = make([]string, count)
		for i, inst := range placements {
			if inst != nil {
				rec.InstanceIds[i] = inst.GetId()
			}
		}
		// Single-instance carve-out: preserve Ch 6's 1:1 mapping
		// by also populating the singular dep.instance_id for the
		// count==1 case. The slot's id is the deploy_id itself
		// (per replicaInstanceID's totalSlots==1 rule), so this is
		// just mirroring the field. Multi-replica leaves
		// dep.instance_id empty; readers consult instance_ids[]
		// via EffectiveInstanceIDs.
		if count == 1 && placements[0] != nil {
			rec.InstanceId = placements[0].GetId()
		}
		// Stamp replica_specs in lockstep with the other arrays.
		// Each slot's spec is the operator's intent for that slot
		// regardless of whether placement succeeded -- a failed
		// slot keeps its spec so #93's reconciliation knows what
		// to retry with.
		if len(specs) == count {
			rec.ReplicaSpecs = specs
		}
		return nil
	})
}

// recordAppendedSlots is the scale-up analog of recordCreateSlots:
// the deployment already has slots 0..startingSlot-1 stamped;
// appendReplicas extends instance_ids / engine_endpoints /
// replica_specs by len(placements) slots starting at startingSlot.
// Existing slots (including DEGRADED tombstones with empty ids)
// are untouched -- scale appends, never repairs.
func (s *Service) recordAppendedSlots(deployID string, placements []*provisionerv1.Instance, startingSlot int) {
	specs := s.takePendingReplicaSpecs()
	_ = s.store.Update(func(f *State) error {
		rec, ok := f.Deployments[deployID]
		if !ok {
			return nil
		}
		// Grow all three arrays to startingSlot+len(placements). If
		// the existing arrays were shorter (Beat-1+2 legacy
		// deployment with len < startingSlot), pad with empties.
		need := startingSlot + len(placements)
		for len(rec.InstanceIds) < need {
			rec.InstanceIds = append(rec.InstanceIds, "")
		}
		for len(rec.EngineEndpoints) < need {
			rec.EngineEndpoints = append(rec.EngineEndpoints, "")
		}
		for len(rec.ReplicaSpecs) < startingSlot {
			rec.ReplicaSpecs = append(rec.ReplicaSpecs, nil)
		}
		for i, inst := range placements {
			if inst != nil {
				rec.InstanceIds[startingSlot+i] = inst.GetId()
			}
		}
		// Append the new slots' specs. Failed slots still get their
		// spec recorded so #93's reconciliation can retry.
		if len(specs) == len(placements) {
			rec.ReplicaSpecs = append(rec.ReplicaSpecs, specs...)
		}
		return nil
	})
}

// launchReplica runs one replica's executor in a goroutine and emits
// the result on the shared channel. Mirrors launchDeploy's single-
// instance pattern, but the emit closure routes through
// patchDeploymentSlot so only the slot's engine_endpoint is touched
// -- deployment-level State / Phase stays under aggregator control.
//
// Synchronous (within its own goroutine): the deploy call blocks
// until the executor returns. Successful deploys also finalize the
// underlying instance from PENDING -> ACTIVE via the existing
// finalizeInstanceAfterDeploy path, identical to single-instance.
func (s *Service) launchReplica(ctx context.Context, deployID string, slot int, inst *provisionerv1.Instance, key *sshkeys.KeyPair, dep *provisionerv1.Deployment, results chan<- fanOutResult) {
	replicaID := inst.GetId()
	obs := s.newDeployObserver(ctx, deployKindProvision, deployID, inst)
	emit := func(u DeployStateUpdate) {
		obs.observe(u)
		_ = s.patchDeploymentSlot(deployID, replicaID, u)
	}
	err := s.deployerFor(inst).Deploy(obs.ctx(), dep, inst, key, emit)
	obs.finish(err)
	endpoint := s.readSlotEndpoint(deployID, slot)
	if err == nil {
		s.finalizeInstanceAfterDeploy(ctx, inst, deployID)
	}
	results <- fanOutResult{
		instanceID: replicaID,
		endpoint:   endpoint,
		err:        err,
	}
}

// applyAggregateState stamps the deployment's terminal state after
// all replicas have reported. The transitions table:
//
//	all succeed     -> RUNNING (ReadyAt = now)
//	partial (M < N) -> DEGRADED + failure_reason (ReadyAt = now;
//	                   the healthy subset is serving traffic now)
//	zero            -> FAILED + failure_reason (no ReadyAt)
//
// failure_reason is operator-readable plain text. A future
// structured FailureDetail repeated field can replace it without
// breaking this contract -- the field name stays.
func (s *Service) applyAggregateState(deployID string, count, successes int, failureReasons []string) {
	var aggState provisionerv1.DeploymentState
	var failureReason string
	switch {
	case successes == count:
		aggState = provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING
	case successes > 0:
		aggState = provisionerv1.DeploymentState_DEPLOYMENT_STATE_DEGRADED
		failureReason = fmt.Sprintf("%d of %d instances failed at provision: %s",
			count-successes, count, strings.Join(failureReasons, "; "))
	default:
		aggState = provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED
		failureReason = fmt.Sprintf("all %d instances failed at provision: %s",
			count, strings.Join(failureReasons, "; "))
	}

	_ = s.store.Update(func(f *State) error {
		rec, ok := f.Deployments[deployID]
		if !ok {
			return nil
		}
		rec.State = aggState
		if failureReason != "" {
			rec.FailureReason = failureReason
		}
		now := timestamppb.New(s.clock())
		if aggState == provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING ||
			aggState == provisionerv1.DeploymentState_DEPLOYMENT_STATE_DEGRADED {
			if rec.ReadyAt == nil {
				rec.ReadyAt = now
			}
		}
		return nil
	})
}

// patchDeploymentSlot updates engine_endpoints[i] for the slot
// whose instance_ids[i] == replicaInstanceID. Multi-replica analog
// of patchDeployment: deliberately does NOT touch deployment-level
// State / CurrentPhase / ProgressMessage -- the fan-out aggregator
// owns those (see applyAggregateState).
//
// container_id stamps onto the per-replica Instance record (each
// replica has its own Instance and its own provider pod id) rather
// than onto the Deployment's container_id, which is reserved for
// the singular-instance shape.
//
// Idempotent: silently no-ops if the deployment was destroyed
// concurrently or the slot can't be found (race against partial
// rollback).
func (s *Service) patchDeploymentSlot(deployID, replicaInstanceID string, u DeployStateUpdate) error {
	return s.store.Update(func(f *State) error {
		rec, ok := f.Deployments[deployID]
		if !ok {
			return nil
		}
		slot := -1
		for i, id := range rec.GetInstanceIds() {
			if id == replicaInstanceID {
				slot = i
				break
			}
		}
		if slot < 0 {
			return nil
		}
		// Defensive: grow engine_endpoints if it lags instance_ids
		// (recordPlacedSlots should have sized them in lockstep,
		// but a partial-restore race could leave them mismatched).
		for len(rec.EngineEndpoints) <= slot {
			rec.EngineEndpoints = append(rec.EngineEndpoints, "")
		}
		if u.EngineEndpoint != "" {
			rec.EngineEndpoints[slot] = u.EngineEndpoint
			// Singular engine_endpoint mirrors slot 0 for backward
			// compat: forwardable()'s precondition checks the
			// singular, and dashboards predating the parallel-
			// arrays shape read it too. Set-once: leave alone if
			// already populated so a later slot-0 re-write
			// (post-quarantine restore, future replacement) doesn't
			// flip the singular field around.
			if slot == 0 && rec.EngineEndpoint == "" {
				rec.EngineEndpoint = u.EngineEndpoint
			}
		}
		if u.ContainerID != "" {
			if inst, ok := f.Instances[replicaInstanceID]; ok && inst.GetProviderId() == "" {
				inst.ProviderId = u.ContainerID
			}
		}
		return nil
	})
}

// readSlotEndpoint is a small read-only helper the fan-out's result
// collector uses to capture the URL that patchDeploymentSlot stamped
// for slot i. Returns "" when the slot is empty, the deployment is
// missing, or the index is out of range -- all of which produce a
// fanOutResult that the aggregator counts as failed.
func (s *Service) readSlotEndpoint(deployID string, slot int) string {
	f, err := s.store.Read()
	if err != nil {
		return ""
	}
	rec, ok := f.Deployments[deployID]
	if !ok {
		return ""
	}
	if slot < 0 || slot >= len(rec.GetEngineEndpoints()) {
		return ""
	}
	return rec.EngineEndpoints[slot]
}

// placeReplicaInstance synthesizes one of the N per-replica Instance
// records for a multi-replica deployment. Auto-provision only: the
// explicit-instance-id branch from placeDeployment cannot apply
// here -- fan-out callers don't get to name underlying instances.
//
// Idempotent: if an Instance already exists at the synthesized id,
// reuse it (re-runs of a partially-failed CreateDeployment don't
// double-rent the slot).
// placeReplicaInstance synthesizes one per-replica Instance record
// at slot. Per-slot (provider, region, requirements) come from spec,
// not from a singular request field -- heterogeneous fleets (#143)
// can mix providers across slots, so this helper is spec-agnostic.
// Homogeneous fan-out passes N copies of the same spec.
//
// Auto-provision only: the explicit-instance-id branch from
// placeDeployment cannot apply here -- fan-out callers don't get to
// name underlying instances.
//
// Idempotent: if an Instance already exists at the synthesized id,
// reuse it (re-runs of a partially-failed CreateDeployment don't
// double-rent the slot).
func (s *Service) placeReplicaInstance(_ context.Context, spec *provisionerv1.ReplicaSpec, baseImage, deployID string, slot, totalSlots int) (*provisionerv1.Instance, error) {
	if spec == nil {
		return nil, status.Error(codes.InvalidArgument, "replica spec is required")
	}
	reqs := spec.GetRequirements()
	if reqs == nil {
		return nil, status.Errorf(codes.InvalidArgument, "replica slot r%d: resource requirements are required (--class, --min-vram-gb, or --sku)", slot)
	}
	providerName := spec.GetProvider()
	if providerName == "" {
		return nil, status.Errorf(codes.InvalidArgument, "replica slot r%d: provider is required", slot)
	}
	if _, ok := s.providers[providerName]; !ok {
		return nil, status.Errorf(codes.InvalidArgument, "replica slot r%d: unknown provider %q", slot, providerName)
	}

	instanceID := replicaInstanceID(deployID, slot, totalSlots)
	if f, err := s.store.Read(); err == nil {
		if existing, ok := f.Instances[instanceID]; ok {
			return existing, nil
		}
	}
	pspec := &provisionerv1.Spec{
		Id:           instanceID,
		Provider:     providerName,
		Region:       spec.GetRegion(),
		BaseImage:    baseImage,
		Requirements: reqs,
	}
	if err := ValidateAndExpandRequirements(pspec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "replica slot r%d: %v", slot, err)
	}
	inst := newPendingInstance(pspec, providerName, s.clock())
	if err := s.patchRecord(inst.GetId(), inst); err != nil {
		return nil, status.Errorf(codes.Internal, "record placed replica r%d: %v", slot, err)
	}
	return inst, nil
}

// resolveCreateReplicaSpecs expands a CreateDeploymentRequest's
// replicas_spec from the compressed instance-group form (each entry
// carries replicas=N) into the per-slot form the fan-out machinery
// uses (one ReplicaSpec per slot, replicas=1 implicit). v0.2
// ch7-beat3.10 (#143 + refactor).
//
// Example:
//
//	request: [{runpod, small, replicas=3}, {vast, medium, replicas=1}]
//	output:  [{runpod,small,1}, {runpod,small,1}, {runpod,small,1},
//	          {vast,medium,1}]
//
// Validation:
//   - Each entry must name a provider and requirements (the per-slot
//     placeReplicaInstance enforces this further; we surface a clearer
//     error here for empty entries).
//   - replicas=0 normalizes to 1 (proto3 zero-value semantics).
//   - replicas<0 is invalid.
//   - len(replicas_spec)==0 returns nil so callers can branch on
//     "single-instance via dep.instance_id" vs "auto-provision via
//     replicas_spec".
func resolveCreateReplicaSpecs(req *provisionerv1.CreateDeploymentRequest) ([]*provisionerv1.ReplicaSpec, error) {
	groups := req.GetReplicasSpec()
	if len(groups) == 0 {
		return nil, nil
	}
	var out []*provisionerv1.ReplicaSpec
	for i, g := range groups {
		if g == nil {
			return nil, status.Errorf(codes.InvalidArgument, "replicas_spec entry %d is nil", i)
		}
		count := g.GetReplicas()
		if count == 0 {
			count = 1 // proto3 zero-value normalization
		}
		if count < 0 {
			return nil, status.Errorf(codes.InvalidArgument, "replicas_spec entry %d has replicas=%d (must be >= 0)", i, count)
		}
		// Expand: emit `count` entries, each with replicas=1 implicit.
		// Share the spec pointer across the expanded entries -- the
		// per-slot view is read-only after expansion (place + executor
		// don't mutate). One allocation per expanded slot for the
		// inner struct so persisted Deployment.replica_specs entries
		// can be inspected independently in describe / debug output
		// without entries aliasing each other.
		for range int(count) {
			out = append(out, &provisionerv1.ReplicaSpec{
				Provider:     g.GetProvider(),
				Region:       g.GetRegion(),
				Requirements: g.GetRequirements(),
				// replicas left zero == 1 on the persisted form.
			})
		}
	}
	return out, nil
}
