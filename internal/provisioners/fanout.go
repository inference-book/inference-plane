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
// multi-replica Deployment. Beat 1+2's 1:1 mapping continues to use
// the bare deploy id; multi-replica fan-out appends -r0, -r1, ... so
// each replica's Instance record can be looked up independently
// (iplane instance describe my-llama-r1 → the GPU behind slot 1).
//
// Stable naming -- no collision risk with arbitrary operator-supplied
// instance ids because deploy ids cannot contain "-r<digits>" by
// ValidateID's character set. Predictable for operators reading state.
func replicaInstanceID(deployID string, i int) string {
	return fmt.Sprintf("%s-r%d", deployID, i)
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
func (s *Service) createMultiReplicaDeployment(ctx context.Context, req *provisionerv1.CreateDeploymentRequest, count int) (*provisionerv1.CreateDeploymentResponse, error) {
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
		s.fanOutProvision(runCtx, req, record.GetId(), count)
		// Re-read the record to surface terminal aggregate state.
		if file, rerr := s.store.Read(); rerr == nil {
			if final, ok := file.Deployments[record.GetId()]; ok {
				record = final
			}
		}
	} else {
		go s.fanOutProvision(runCtx, req, record.GetId(), count)
	}
	return &provisionerv1.CreateDeploymentResponse{Deployment: record}, nil
}

// fanOutProvision drives the multi-replica path. Called from
// CreateDeployment when replicas > 1. Sequence:
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
func (s *Service) fanOutProvision(ctx context.Context, req *provisionerv1.CreateDeploymentRequest, deployID string, count int) {
	placements, placeErrs := s.placeAllReplicas(ctx, req, deployID, count)

	// Persist instance_ids for placed slots so subsequent
	// patchDeploymentSlot calls have a slot index to match against.
	// Place-failed slots get empty-string ids; engine_endpoints is
	// sized to match so the parallel-arrays invariant holds.
	s.recordPlacedSlots(deployID, placements, count)

	// Phase 2: load the SSH key once (same provider for all replicas
	// in this PR -- heterogeneous fleets are #143). A keyStore error
	// fails every replica that was successfully placed.
	var key = s.loadFanOutKey(req)

	results := make(chan fanOutResult, count)
	var launched int
	for i, inst := range placements {
		if inst == nil {
			results <- fanOutResult{
				instanceID: replicaInstanceID(deployID, i),
				err:        placeErrs[i],
			}
			continue
		}
		if key == nil {
			results <- fanOutResult{
				instanceID: inst.GetId(),
				err:        fmt.Errorf("ssh keypair unavailable for provider %q", req.GetProvider()),
			}
			continue
		}
		launched++
		go s.launchReplica(ctx, deployID, i, inst, key, req.GetDeployment(), results)
	}

	// Phase 3: collect outcomes. Note: we drained the trivial
	// "couldn't even try" cases synchronously above; goroutines fire
	// the remaining `launched` results. Total result count == count
	// (synchronous + async).
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

	// Phase 4: aggregate state. Order matters: DEGRADED if any
	// instance succeeded (operator can still serve traffic from the
	// healthy subset via router #85); FAILED only when zero
	// succeeded.
	s.applyAggregateState(deployID, count, successes, failureReasons)
}

// placeAllReplicas spawns count goroutines, each placing one
// per-replica Instance. Returns parallel slices: placements[i] is
// the Instance for slot i (nil on failure), placeErrs[i] is the
// matching error (nil on success).
//
// Idempotent: placeReplicaInstance reuses an existing Instance
// record if one already exists at the synthesized id (re-runs of a
// failed CreateDeployment don't double-rent).
func (s *Service) placeAllReplicas(ctx context.Context, req *provisionerv1.CreateDeploymentRequest, deployID string, count int) ([]*provisionerv1.Instance, []error) {
	placements := make([]*provisionerv1.Instance, count)
	placeErrs := make([]error, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inst, err := s.placeReplicaInstance(ctx, req, deployID, i)
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

// recordPlacedSlots persists the instance_ids / engine_endpoints
// parallel arrays sized to count. Successful placements stamp their
// instance id; place-failed slots remain "". This is the snapshot
// the router (#85) reads to decide which slots are eligible for
// round-robin selection.
func (s *Service) recordPlacedSlots(deployID string, placements []*provisionerv1.Instance, count int) {
	_ = s.store.Update(func(f *State) error {
		rec, ok := f.Deployments[deployID]
		if !ok {
			return nil
		}
		rec.InstanceIds = make([]string, count)
		rec.EngineEndpoints = make([]string, count)
		for i, inst := range placements {
			if inst != nil {
				rec.InstanceIds[i] = inst.GetId()
			}
		}
		return nil
	})
}

// loadFanOutKey returns the SSH keypair shared across all N replicas
// (one provider per fan-out in this PR; heterogeneous-provider key
// loading per slot is #143's job). Returns nil if the key store is
// unconfigured or EnsureKeyPair fails; the caller treats nil as
// "every replica fails with a clear error" rather than aborting the
// whole fan-out.
func (s *Service) loadFanOutKey(req *provisionerv1.CreateDeploymentRequest) *sshkeys.KeyPair {
	if s.keyStore == nil {
		return nil
	}
	key, err := s.keyStore.EnsureKeyPair(s.operatorID, req.GetProvider())
	if err != nil {
		return nil
	}
	return key
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
	emit := func(u DeployStateUpdate) {
		_ = s.patchDeploymentSlot(deployID, replicaID, u)
	}
	err := s.deployerFor(inst).Deploy(ctx, dep, inst, key, emit)
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
func (s *Service) placeReplicaInstance(_ context.Context, req *provisionerv1.CreateDeploymentRequest, deployID string, slot int) (*provisionerv1.Instance, error) {
	reqs := req.GetRequirements()
	if reqs == nil {
		return nil, status.Error(codes.InvalidArgument, "multi-replica deployment requires resource requirements (--class, --min-vram-gb, or --sku)")
	}
	providerName := req.GetProvider()
	if providerName == "" {
		return nil, status.Error(codes.InvalidArgument, "multi-replica deployment requires a provider")
	}
	if _, ok := s.providers[providerName]; !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unknown provider %q", providerName)
	}

	instanceID := replicaInstanceID(deployID, slot)
	if f, err := s.store.Read(); err == nil {
		if existing, ok := f.Instances[instanceID]; ok {
			return existing, nil
		}
	}
	spec := &provisionerv1.Spec{
		Id:           instanceID,
		Provider:     providerName,
		Region:       req.GetRegion(),
		BaseImage:    req.GetDeployment().GetImage(),
		Requirements: reqs,
	}
	if err := ValidateAndExpandRequirements(spec); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	inst := newPendingInstance(spec, providerName, s.clock())
	if err := s.patchRecord(inst.GetId(), inst); err != nil {
		return nil, status.Errorf(codes.Internal, "record placed replica: %v", err)
	}
	return inst, nil
}
