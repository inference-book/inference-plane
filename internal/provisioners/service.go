package provisioners

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Service is the gRPC implementation of ProvisionerServiceServer and
// DeploymentServiceServer. It owns the failure-mode contract from
// docs/design/0001-provisioner.md: idempotency lookup before Spawn,
// pending -> active state hygiene, list with per-record self-heal,
// terminate idempotency. Phase 2's DeploymentService stubs return
// codes.Unimplemented until the executor lands.
//
// Method signatures and error returns follow the standard gRPC pattern
// (matching internal/services/inference.go): take (ctx, *Req), return
// (*Resp, error) with errors wrapped via status.Error(codes.X, ...).
// HTTP / Connect-RPC bindings live in adapter packages
// (connect_handler.go in this package, internal/web/server for the
// inference-side bindings); they wrap a gRPC client and convert at the
// transport boundary.
//
// In-process usage (CLI, tests):
//
//	svc := provisioners.New(...)
//	resp, err := svc.CreateInstance(ctx, &pb.CreateInstanceRequest{...})
//
// Remote usage (Connect handler dialing a gRPC backend in-process):
//
//	mux.Handle(provisionerv1connect.NewProvisionerServiceHandler(
//	    provisioners.NewConnectProvisionerAdapter(grpcClient),
//	))
type Service struct {
	// Embed both Unimplemented...Server structs for forward compatibility:
	// when a future RPC is added to the proto, the Service compiles
	// without modification (the embedded stub satisfies the new method).
	provisionerv1.UnimplementedProvisionerServiceServer
	provisionerv1.UnimplementedDeploymentServiceServer

	providers  map[string]Provider
	store      *state.Store
	operatorID string
	clock      func() time.Time
}

// Option configures the Service at construction time.
type Option func(*Service)

// WithClock injects a clock function. Tests pass a fixed-clock factory
// so timestamps are assertable.
func WithClock(c func() time.Time) Option {
	return func(s *Service) { s.clock = c }
}

// New constructs a Service. Providers are keyed by their Name() so the
// service can dispatch by spec.provider without an interface assertion
// at call time.
func New(providers []Provider, store *state.Store, operatorID string, opts ...Option) *Service {
	s := &Service{
		providers:  make(map[string]Provider, len(providers)),
		store:      store,
		operatorID: operatorID,
		clock:      time.Now,
	}
	for _, p := range providers {
		s.providers[p.Name()] = p
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// CreateInstance implements the design doc's three-step contract:
//
//  1. Critical section: check local state; if no live record, claim a
//     pending record atomically. This closes the same-laptop race
//     window.
//
//  2. Outside the critical section: ask the target provider whether it
//     already has an instance under our iplane-id tag (catches the
//     wiped-state-file recovery case). If yes, adopt it.
//
//  3. Otherwise: call Spawn, then patch the pending record to active
//     (or failed) in a final critical section.
func (s *Service) CreateInstance(ctx context.Context, req *provisionerv1.CreateInstanceRequest) (*provisionerv1.CreateInstanceResponse, error) {
	spec := req.GetSpec()
	if spec == nil {
		return nil, status.Error(codes.InvalidArgument, "spec is required")
	}
	if err := ValidateID(spec.GetId()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := ValidateAndExpandRequirements(spec); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	// Region is intentionally not validated here: semantics vary by
	// provider (RunPod treats empty as "schedule anywhere", Local
	// ignores it entirely, future cloud adapters may require it).
	// Each Provider.Spawn validates as needed.
	provider, ok := s.providers[spec.GetProvider()]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unknown provider %q", spec.GetProvider())
	}

	// Step 1: critical section.
	var record *provisionerv1.Instance
	var alreadyExisted bool
	var claimedPending bool
	err := s.store.Update(func(f *state.File) error {
		if existing, ok := f.Instances[spec.GetId()]; ok {
			switch existing.GetState() {
			case provisionerv1.InstanceState_INSTANCE_STATE_PENDING, provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE:
				if existing.GetProvider() != spec.GetProvider() {
					return fmt.Errorf("id %q already exists on provider %q; destroy and recreate to move providers", spec.GetId(), existing.GetProvider())
				}
				record = existing
				alreadyExisted = true
				return nil
			case provisionerv1.InstanceState_INSTANCE_STATE_TERMINATING:
				return fmt.Errorf("id %q is currently terminating; wait for completion", spec.GetId())
			}
			// TERMINATED or FAILED: treat as gone, claim pending below.
		}
		record = newPendingInstance(spec, provider.Name(), s.clock())
		f.Instances[spec.GetId()] = record
		claimedPending = true
		return nil
	})
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if alreadyExisted {
		return &provisionerv1.CreateInstanceResponse{
			Instance:       record,
			AlreadyExisted: true,
		}, nil
	}
	if !claimedPending {
		return nil, status.Error(codes.Internal, "no record claimed and no existing record returned")
	}

	// Step 2: remote lookup by iplane-id tag on the target provider.
	// Listing failures are non-fatal: log into the failure path only if
	// we cannot Spawn either.
	refs, _ := provider.List(ctx, map[string]string{
		TagID:       spec.GetId(),
		TagOperator: s.operatorID,
	})
	for _, ref := range refs {
		if !providerSaysActive(provider, ref.GetProviderState()) {
			continue
		}
		adopted, descErr := provider.Describe(ctx, ref.GetProviderId())
		if descErr != nil {
			continue
		}
		adopted = s.finalizeActive(adopted, spec, provider.Name(), record.GetCreatedAt())
		if patchErr := s.patchRecord(spec.GetId(), adopted); patchErr != nil {
			return nil, status.Error(codes.Internal, patchErr.Error())
		}
		return &provisionerv1.CreateInstanceResponse{
			Instance:       adopted,
			AlreadyExisted: true,
		}, nil
	}

	// Step 3: Spawn (no flock held), then patch.
	stampedSpec := withSystemTags(spec, s.operatorID)
	spawned, spawnErr := provider.Spawn(ctx, stampedSpec)
	if spawnErr != nil {
		failed := proto.Clone(record).(*provisionerv1.Instance)
		failed.State = provisionerv1.InstanceState_INSTANCE_STATE_FAILED
		failed.FailureReason = spawnErr.Error()
		if patchErr := s.patchRecord(spec.GetId(), failed); patchErr != nil {
			return nil, status.Error(codes.Internal, errors.Join(spawnErr, patchErr).Error())
		}
		return nil, status.Error(codes.Unknown, spawnErr.Error())
	}
	spawned = s.finalizeActive(spawned, spec, provider.Name(), record.GetCreatedAt())
	if patchErr := s.patchRecord(spec.GetId(), spawned); patchErr != nil {
		return nil, status.Errorf(codes.Internal, "spawn succeeded but state patch failed: %v", patchErr)
	}
	return &provisionerv1.CreateInstanceResponse{Instance: spawned}, nil
}

// DestroyInstance transitions a known record to terminating, calls the
// provider, and settles to terminated or failed.
func (s *Service) DestroyInstance(ctx context.Context, req *provisionerv1.DestroyInstanceRequest) (*provisionerv1.DestroyInstanceResponse, error) {
	id := req.GetId()
	if err := ValidateID(id); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// Step 1: read + transition to terminating, capture providerID.
	var record *provisionerv1.Instance
	var providerID string
	var providerName string
	err := s.store.Update(func(f *state.File) error {
		existing, ok := f.Instances[id]
		if !ok {
			return fmt.Errorf("no instance with id %q", id)
		}
		switch existing.GetState() {
		case provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED:
			record = existing
			return nil
		case provisionerv1.InstanceState_INSTANCE_STATE_PENDING:
			// Pending means Spawn may have started -- still go through the
			// provider Terminate (which is idempotent) so we do not leak.
		}
		providerID = existing.GetProviderId()
		providerName = existing.GetProvider()
		existing.State = provisionerv1.InstanceState_INSTANCE_STATE_TERMINATING
		record = existing
		return nil
	})
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	if record.GetState() == provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED {
		return &provisionerv1.DestroyInstanceResponse{Instance: record}, nil
	}

	// Step 2: provider call (no flock).
	var terminateErr error
	if !req.GetForce() && providerID != "" {
		provider, ok := s.providers[providerName]
		if !ok {
			return nil, status.Errorf(codes.FailedPrecondition, "provider %q not configured", providerName)
		}
		terminateErr = provider.Terminate(ctx, providerID)
	}

	// Step 3: patch.
	now := timestamppb.New(s.clock())
	patchErr := s.store.Update(func(f *state.File) error {
		existing, ok := f.Instances[id]
		if !ok {
			return nil
		}
		if terminateErr != nil {
			existing.State = provisionerv1.InstanceState_INSTANCE_STATE_FAILED
			existing.FailureReason = terminateErr.Error()
		} else {
			existing.State = provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED
			existing.TerminatedAt = now
		}
		record = existing
		return nil
	})
	if terminateErr != nil {
		return nil, status.Error(codes.Unknown, errors.Join(terminateErr, patchErr).Error())
	}
	if patchErr != nil {
		return nil, status.Error(codes.Internal, patchErr.Error())
	}
	return &provisionerv1.DestroyInstanceResponse{Instance: record}, nil
}

// DescribeInstance returns the local-state record (SOURCE_LOCAL, default)
// or asks the provider directly (SOURCE_REMOTE) and refreshes the local
// record from the response.
func (s *Service) DescribeInstance(ctx context.Context, req *provisionerv1.DescribeInstanceRequest) (*provisionerv1.DescribeInstanceResponse, error) {
	id := req.GetId()
	if err := ValidateID(id); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	source := req.GetSource()
	if source == provisionerv1.Source_SOURCE_UNSPECIFIED {
		source = provisionerv1.Source_SOURCE_LOCAL
	}

	file, err := s.store.Read()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	existing, ok := file.Instances[id]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no instance with id %q", id)
	}
	if source == provisionerv1.Source_SOURCE_LOCAL {
		return &provisionerv1.DescribeInstanceResponse{Instance: existing}, nil
	}

	// SOURCE_REMOTE
	provider, ok := s.providers[existing.GetProvider()]
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "provider %q not configured", existing.GetProvider())
	}
	if existing.GetProviderId() == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "id %q has no provider_id yet (state=%s)", id, existing.GetState())
	}
	refreshed, err := provider.Describe(ctx, existing.GetProviderId())
	if err != nil {
		return nil, status.Error(codes.Unknown, err.Error())
	}
	refreshed = s.finalizeActive(refreshed, existing.GetSpec(), provider.Name(), existing.GetCreatedAt())
	if patchErr := s.patchRecord(id, refreshed); patchErr != nil {
		return nil, status.Error(codes.Internal, patchErr.Error())
	}
	return &provisionerv1.DescribeInstanceResponse{Instance: refreshed}, nil
}

// ListInstances returns the local-state view (SOURCE_LOCAL, default,
// with per-record self-heal for pending/terminating records) or the
// provider's view (SOURCE_REMOTE -- requires a provider filter).
func (s *Service) ListInstances(ctx context.Context, req *provisionerv1.ListInstancesRequest) (*provisionerv1.ListInstancesResponse, error) {
	source := req.GetSource()
	if source == provisionerv1.Source_SOURCE_UNSPECIFIED {
		source = provisionerv1.Source_SOURCE_LOCAL
	}
	providerFilter := req.GetProvider()

	if source == provisionerv1.Source_SOURCE_REMOTE {
		if providerFilter == "" {
			return nil, status.Error(codes.InvalidArgument, "--remote requires a provider")
		}
		provider, ok := s.providers[providerFilter]
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "unknown provider %q", providerFilter)
		}
		refs, err := provider.List(ctx, map[string]string{TagOperator: s.operatorID})
		if err != nil {
			return nil, status.Error(codes.Unknown, err.Error())
		}
		instances := make([]*provisionerv1.Instance, 0, len(refs))
		for _, ref := range refs {
			instances = append(instances, refToInstance(ref, provider.Name()))
		}
		return &provisionerv1.ListInstancesResponse{Instances: instances}, nil
	}

	// SOURCE_LOCAL: self-heal pending/terminating before returning.
	file, err := s.store.Read()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	for id, inst := range file.Instances {
		if providerFilter != "" && inst.GetProvider() != providerFilter {
			continue
		}
		st := inst.GetState()
		if st != provisionerv1.InstanceState_INSTANCE_STATE_PENDING && st != provisionerv1.InstanceState_INSTANCE_STATE_TERMINATING {
			continue
		}
		provider, ok := s.providers[inst.GetProvider()]
		if !ok {
			continue
		}
		refs, listErr := provider.List(ctx, map[string]string{
			TagID:       id,
			TagOperator: s.operatorID,
		})
		if listErr != nil {
			continue
		}
		if len(refs) == 0 {
			// Provider has no record; if we are terminating, declare terminated.
			// If we are pending, leave it -- user inspects and decides.
			if st == provisionerv1.InstanceState_INSTANCE_STATE_TERMINATING {
				now := timestamppb.New(s.clock())
				_ = s.store.Update(func(f *state.File) error {
					if rec, ok := f.Instances[id]; ok {
						rec.State = provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED
						rec.TerminatedAt = now
					}
					return nil
				})
			}
			continue
		}
		// Provider has a record. If terminating, leave; if pending, promote to active.
		if st == provisionerv1.InstanceState_INSTANCE_STATE_PENDING {
			adopted, descErr := provider.Describe(ctx, refs[0].GetProviderId())
			if descErr != nil {
				continue
			}
			finalized := s.finalizeActive(adopted, inst.GetSpec(), provider.Name(), inst.GetCreatedAt())
			_ = s.patchRecord(id, finalized)
		}
	}

	// Reread after self-heal so callers see fresh state.
	file, err = s.store.Read()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	instances := make([]*provisionerv1.Instance, 0, len(file.Instances))
	for _, inst := range file.Instances {
		if providerFilter != "" && inst.GetProvider() != providerFilter {
			continue
		}
		instances = append(instances, inst)
	}
	return &provisionerv1.ListInstancesResponse{Instances: instances}, nil
}

// patchRecord writes the supplied instance back to state under the
// given id, taking the flock for the duration. Idempotent: if the
// record was removed concurrently, the patch silently re-creates it.
func (s *Service) patchRecord(id string, inst *provisionerv1.Instance) error {
	return s.store.Update(func(f *state.File) error {
		f.Instances[id] = inst
		return nil
	})
}

// finalizeActive merges provider response fields with the bookkeeping
// (id, provider name, spec snapshot, original created_at) that the
// service is responsible for. Callers pass the original created_at
// from the pending record so the active record carries the same
// timestamp the operator first saw.
func (s *Service) finalizeActive(inst *provisionerv1.Instance, spec *provisionerv1.Spec, providerName string, createdAt *timestamppb.Timestamp) *provisionerv1.Instance {
	if inst.GetState() == provisionerv1.InstanceState_INSTANCE_STATE_UNSPECIFIED {
		inst.State = provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE
	}
	if inst.ActivatedAt == nil {
		inst.ActivatedAt = timestamppb.New(s.clock())
	}
	if createdAt != nil {
		inst.CreatedAt = createdAt
	} else if inst.CreatedAt == nil {
		inst.CreatedAt = timestamppb.New(s.clock())
	}
	inst.Id = spec.GetId()
	inst.Provider = providerName
	if inst.Spec == nil {
		inst.Spec = spec
	}
	return inst
}

// newPendingInstance constructs the record we write before calling
// Spawn. created_at carries forward to the active record.
func newPendingInstance(spec *provisionerv1.Spec, providerName string, now time.Time) *provisionerv1.Instance {
	return &provisionerv1.Instance{
		Id:        spec.GetId(),
		Provider:  providerName,
		Spec:      spec,
		Region:    spec.GetRegion(),
		State:     provisionerv1.InstanceState_INSTANCE_STATE_PENDING,
		CreatedAt: timestamppb.New(now),
	}
}

// withSystemTags returns a clone of spec with iplane-id and
// iplane-operator stamped into Tags. The adapter passes the resulting
// spec to its provider SDK so the stamped tags land on the provider
// instance for later List filtering.
func withSystemTags(spec *provisionerv1.Spec, operatorID string) *provisionerv1.Spec {
	cloned := proto.Clone(spec).(*provisionerv1.Spec)
	if cloned.Tags == nil {
		cloned.Tags = make(map[string]string)
	}
	cloned.Tags[TagID] = spec.GetId()
	cloned.Tags[TagOperator] = operatorID
	return cloned
}

// ValidateAndExpandRequirements normalizes a Spec's ResourceRequirements
// before the adapter sees it. Two responsibilities:
//
//  1. Validate: the operator supplied something the resolver can act on
//     -- an explicit sku, an explicit min_vram_gb, or a class shorthand.
//     sku and class are mutually exclusive (sku is the escape hatch;
//     class implies constraint resolution).
//
//  2. Expand: when class is set, fill in any unset numeric constraints
//     from the class defaults. Class sets floors -- if the operator
//     ALSO supplied a larger min_vram_gb, the explicit value wins.
//     This is what makes "--gpu-class small --vram-min 32" work: class
//     gives you small-shaped disk/ram defaults, the explicit constraint
//     refines vram up.
//
// classDefaults lives in the runpod package today (per-provider). For
// v0.1 with one constraint-resolving provider this is fine; v0.2 with
// Lambda Labs will pull the table into a shared catalog package.
func ValidateAndExpandRequirements(spec *provisionerv1.Spec) error {
	reqs := spec.GetRequirements()
	if reqs == nil {
		return errors.New("requirements is required")
	}
	if reqs.GetSku() != "" && reqs.GetClass() != "" {
		return errors.New("requirements: class and sku are mutually exclusive")
	}
	if reqs.GetSku() == "" && reqs.GetClass() == "" && reqs.GetMinVramGb() == 0 {
		return errors.New("requirements: one of class, sku, or min_vram_gb is required")
	}

	// Expand class shorthand. If the operator passed --gpu-class small
	// and nothing else, after this block min_vram_gb / min_disk_gb /
	// min_ram_gb are filled in. If the operator passed both class AND
	// an explicit constraint, the larger wins (we treat class as a
	// floor, explicit refinement as an override-up).
	if reqs.GetClass() != "" {
		defaults, ok := classDefaults[reqs.GetClass()]
		if !ok {
			return fmt.Errorf("requirements: unknown class %q (known: %s)",
				reqs.GetClass(), strings.Join(knownClassesList(), ", "))
		}
		if reqs.MinVramGb < defaults.MinVRAMGb {
			reqs.MinVramGb = defaults.MinVRAMGb
		}
		if reqs.MinDiskGb < defaults.MinDiskGb {
			reqs.MinDiskGb = defaults.MinDiskGb
		}
		if reqs.MinRamGb < defaults.MinRAMGb {
			reqs.MinRamGb = defaults.MinRAMGb
		}
	}
	return nil
}

// ClassDefaults captures the constraint floors a class shorthand
// expands into. The service holds this rather than each provider so
// the same shorthand means the same thing across providers.
type ClassDefaults struct {
	MinVRAMGb int32
	MinDiskGb int32
	MinRAMGb  int32
}

// classDefaults is the central class -> constraint-floors table.
// Operators reach the same numeric requirements regardless of provider;
// per-provider SKU resolvers (e.g., runpod.MatchSKUs) consume the
// resulting constraints.
var classDefaults = map[string]ClassDefaults{
	GPUClassSmall:  {MinVRAMGb: 24, MinDiskGb: 20, MinRAMGb: 16},
	GPUClassMedium: {MinVRAMGb: 40, MinDiskGb: 40, MinRAMGb: 32},
	GPUClassLarge:  {MinVRAMGb: 80, MinDiskGb: 60, MinRAMGb: 64},
	GPUClassXLarge: {MinVRAMGb: 96, MinDiskGb: 100, MinRAMGb: 128},
}

func knownClassesList() []string {
	return []string{GPUClassSmall, GPUClassMedium, GPUClassLarge, GPUClassXLarge}
}

// ActiveStateChecker is an optional capability adapters can implement
// to tell the Service which of their provider-side state strings count
// as "this instance is up and idempotency-adoptable." Adapters that do
// not implement it default to "nothing is adoptable from a List
// result," which is correct for adapters that never populate
// ProviderState (Local) and conservative for adapters that do.
//
// The point of pushing this into the adapter is that the central
// Service should not know vocabularies like "RUNNING" vs "EXITED" --
// those are RunPod's words, not iplane's. RunPod owns the mapping;
// AWS will own its own; Local trivially needs nothing.
type ActiveStateChecker interface {
	IsActiveProviderState(state string) bool
}

func providerSaysActive(provider Provider, state string) bool {
	if c, ok := provider.(ActiveStateChecker); ok {
		return c.IsActiveProviderState(state)
	}
	return false
}

// refToInstance synthesizes a sparse Instance from a List result. Used
// when the SOURCE_REMOTE caller asked for "show me what the provider
// knows" without prior local state.
func refToInstance(ref *provisionerv1.InstanceRef, providerName string) *provisionerv1.Instance {
	tags := ref.GetTags()
	id := tags[TagID] // may be empty for non-iplane-created instances
	return &provisionerv1.Instance{
		Id:            id,
		ProviderId:    ref.GetProviderId(),
		Provider:      providerName,
		HourlyRateUsd: ref.GetHourlyRateUsd(),
		State:         provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
		CreatedAt:     ref.GetCreatedAt(),
	}
}

// ── DeploymentService stubs ─────────────────────────────────────────
//
// Phase 2's deployment surface. The same Service struct implements both
// gRPC servers (ProvisionerServiceServer + DeploymentServiceServer)
// sharing state.Store + provider registry. The instance<->deployment
// cross-reference via instance_id stays a same-package lookup.
//
// All five methods return codes.Unimplemented in this PR. The SSH+docker
// executor + CLI wiring land in Phase 2 PRs 2-4; this PR commits the
// interface so those PRs slot in without proto churn.

// CreateDeployment will run the deployment idempotency + state-hygiene
// contract from docs/design/0002-deploy.md once the executor lands.
func (s *Service) CreateDeployment(ctx context.Context, req *provisionerv1.CreateDeploymentRequest) (*provisionerv1.CreateDeploymentResponse, error) {
	return nil, status.Error(codes.Unimplemented, "CreateDeployment lands in Phase 2 PR 3 (executor); this PR is proto + state only")
}

// DescribeDeployment will return one deployment record by id.
func (s *Service) DescribeDeployment(ctx context.Context, req *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
	return nil, status.Error(codes.Unimplemented, "DescribeDeployment lands in Phase 2 PR 3 (executor); this PR is proto + state only")
}

// ListDeployments will enumerate deployments with optional instance_id
// or state filters.
func (s *Service) ListDeployments(ctx context.Context, req *provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ListDeployments lands in Phase 2 PR 3 (executor); this PR is proto + state only")
}

// DestroyDeployment will stop the container and patch the record to
// TERMINATED. Idempotent on already-terminated ids.
func (s *Service) DestroyDeployment(ctx context.Context, req *provisionerv1.DestroyDeploymentRequest) (*provisionerv1.DestroyDeploymentResponse, error) {
	return nil, status.Error(codes.Unimplemented, "DestroyDeployment lands in Phase 2 PR 3 (executor); this PR is proto + state only")
}

// WatchDeployment will server-stream DeploymentStateChangedEvent until
// the deployment reaches a terminal state or the client disconnects.
// gRPC's server-streaming signature takes the request directly and a
// stream object (no separate ctx param -- it lives on stream.Context()).
func (s *Service) WatchDeployment(req *provisionerv1.WatchDeploymentRequest, stream provisionerv1.DeploymentService_WatchDeploymentServer) error {
	return status.Error(codes.Unimplemented, "WatchDeployment lands in Phase 2 PR 3 (executor); this PR is proto + state only")
}

// Compile-time checks that Service implements both generated gRPC
// server interfaces. The embedded Unimplemented...Server fields make
// forward-compat (new RPCs added to the proto) a non-event for this
// type -- only methods we explicitly override are subject to drift.
var (
	_ provisionerv1.ProvisionerServiceServer = (*Service)(nil)
	_ provisionerv1.DeploymentServiceServer  = (*Service)(nil)
)
