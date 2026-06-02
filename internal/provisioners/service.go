package provisioners

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/modelstores"
	"github.com/inference-book/inference-plane/internal/sshkeys"
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
	store      Store
	keyStore   keyEnsurer
	executor   DeploymentExecutor
	modelStore modelstores.ModelStore
	operatorID string
	clock      func() time.Time
}

// keyEnsurer is the narrow interface the Service uses to fetch an
// SSH key pair for the (operator, provider) scope. Satisfied by
// *sshkeys.Store; tests pass a stub. Declared here so the Service
// is the contract owner -- callers must satisfy what Service needs,
// not the other way around.
type keyEnsurer interface {
	EnsureKeyPair(operator, provider string) (*sshkeys.KeyPair, error)
}

// Option configures the Service at construction time.
type Option func(*Service)

// WithClock injects a clock function. Tests pass a fixed-clock factory
// so timestamps are assertable.
func WithClock(c func() time.Time) Option {
	return func(s *Service) { s.clock = c }
}

// WithKeyStore wires a key-management backend into the Service. When
// set, CreateInstance calls EnsureKeyPair(operator, provider) before
// Spawn and (if the provider satisfies KeyRegistrar) calls
// EnsurePublicKey to register the key with the provider. When unset,
// both steps are skipped -- useful for local-only deployments and
// for tests that do not care about keys.
func WithKeyStore(k keyEnsurer) Option {
	return func(s *Service) { s.keyStore = k }
}

// WithModelStore configures the model-spec validation / resolution
// layer the Service calls at CreateDeployment time. When unset the
// Service uses modelstores.Passthrough (no validation; spec is
// forwarded unchanged). Production callers pass a huggingface.Store
// so typos / gated-access errors surface BEFORE a pod is provisioned.
func WithModelStore(ms modelstores.ModelStore) Option {
	return func(s *Service) { s.modelStore = ms }
}

// New constructs a Service. Providers are keyed by their Name() so the
// service can dispatch by spec.provider without an interface assertion
// at call time.
func New(providers []Provider, store Store, operatorID string, opts ...Option) *Service {
	s := &Service{
		providers:  make(map[string]Provider, len(providers)),
		store:      store,
		modelStore: modelstores.Passthrough{}, // safe default; overridden by WithModelStore
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
	err := s.store.Update(func(f *State) error {
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

	// Step 3a: ensure the operator's SSH key is registered with this
	// provider, if the provider supports it and a key store is wired.
	// Errors abort before Spawn so the operator does not pay for a
	// pod that the executor cannot SSH into. Skipped when keyStore is
	// nil (typical for local-only deployments + tests).
	if s.keyStore != nil {
		if reg, ok := provider.(KeyRegistrar); ok {
			kp, err := s.keyStore.EnsureKeyPair(s.operatorID, provider.Name())
			if err != nil {
				return nil, status.Errorf(codes.Internal, "ensure ssh key for %s: %v", provider.Name(), err)
			}
			pubLine, err := kp.MarshalAuthorizedKey()
			if err != nil {
				return nil, status.Errorf(codes.Internal, "marshal ssh public key: %v", err)
			}
			if err := reg.EnsurePublicKey(ctx, pubLine, kp.Comment); err != nil {
				return nil, status.Errorf(codes.FailedPrecondition, "register ssh public key with %s: %v", provider.Name(), err)
			}
		}
	}

	// Step 3b: Spawn (no flock held), then patch.
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
	err := s.store.Update(func(f *State) error {
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
		// Backfill provider_id for 1:1 auto-provisioned instances that
		// were created before patchDeployment learned to stamp it.
		// Without this, a Deploy that POSTed a pod then failed before
		// finalize leaves the instance with empty provider_id, and
		// destroy skips the Terminate call -- the real pod leaks.
		if providerID == "" {
			if dep, ok := f.Deployments[id]; ok && dep.GetContainerId() != "" {
				providerID = dep.GetContainerId()
				existing.ProviderId = providerID
			}
		}
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
	patchErr := s.store.Update(func(f *State) error {
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

// WaitForInstanceReady dispatches to the provider's SSHReadyWaiter
// capability and patches the state file with the populated SshTarget
// on success. Providers without the capability are a no-op -- the
// response carries the unchanged Instance.
//
// Caller's ctx + the optional timeout_seconds field both bound the
// wait. timeout_seconds=0 (the default) defers to whatever cap the
// provider applies internally; non-zero shortens (but does not
// extend) the deadline.
func (s *Service) WaitForInstanceReady(ctx context.Context, req *provisionerv1.WaitForInstanceReadyRequest) (*provisionerv1.WaitForInstanceReadyResponse, error) {
	id := req.GetId()
	if err := ValidateID(id); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	file, err := s.store.Read()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	inst, ok := file.Instances[id]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no instance with id %q", id)
	}

	// Fast path: SSH already populated. Return without touching the
	// provider so repeat callers (idempotent retries, polling scripts)
	// don't hammer the upstream API.
	if ssh := inst.GetSsh(); ssh != nil && ssh.GetHost() != "" {
		return &provisionerv1.WaitForInstanceReadyResponse{
			Instance:     inst,
			AlreadyReady: true,
		}, nil
	}

	// Correctness check: was SSH ever requested on this instance? An
	// auto-provisioned 1:1 instance whose linked deployment has
	// debug_shell=false (the cost-aware default) was created without
	// supportPublicIp at the provider, so the provider's WaitForSSHReady
	// would poll an IP that's NEVER coming and time out hard. Surface
	// that explicitly: not a transient timeout, a permanent "you didn't
	// ask for shell access on this deployment."
	if dep, ok := file.Deployments[id]; ok && dep.GetInstanceId() == id && !dep.GetDebugShell() {
		return nil, status.Errorf(codes.FailedPrecondition,
			"instance %q has no SSH endpoint -- deployment %q was created with debug_shell=false (the cost-aware default). Re-deploy with --debug-shell to opt into a routable publicIp + sshd.", id, dep.GetId())
	}

	provider, ok := s.providers[inst.GetProvider()]
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "provider %q not configured (cannot wait for ssh ready)", inst.GetProvider())
	}
	waiter, ok := provider.(SSHReadyWaiter)
	if !ok {
		// Provider has no SSH-readiness gap (e.g. local). Whatever's
		// in state IS the final SSH view; return it unchanged.
		return &provisionerv1.WaitForInstanceReadyResponse{
			Instance:     inst,
			AlreadyReady: true,
		}, nil
	}

	waitCtx := ctx
	if to := req.GetTimeoutSeconds(); to > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, time.Duration(to)*time.Second)
		defer cancel()
	}
	target, werr := waiter.WaitForSSHReady(waitCtx, inst.GetProviderId())
	if werr != nil {
		// DeadlineExceeded from the provider surfaces as the same gRPC
		// code so the CLI can render an actionable "didn't get IP in
		// time, retry?" message.
		if errors.Is(werr, context.DeadlineExceeded) {
			return nil, status.Errorf(codes.DeadlineExceeded, "wait_for_instance_ready %q: %v", id, werr)
		}
		return nil, status.Errorf(codes.Unavailable, "wait_for_instance_ready %q: %v", id, werr)
	}

	updated := proto.Clone(inst).(*provisionerv1.Instance)
	updated.Ssh = target
	if patchErr := s.patchRecord(id, updated); patchErr != nil {
		return nil, status.Errorf(codes.Internal, "wait_for_instance_ready %q: state patch failed: %v", id, patchErr)
	}
	return &provisionerv1.WaitForInstanceReadyResponse{
		Instance:     updated,
		AlreadyReady: false,
	}, nil
}

// GetInstanceSSHKey returns the operator's iplane-managed private key
// for an instance, so a remote CLI can materialize it locally and run
// ssh. The keystore lives server-side; this RPC is the only way for
// --service-url clients to reach it.
//
// Security: this returns private key bytes over the wire. v0.1 trusts
// any caller with access to the gRPC server (no per-operator auth);
// rely on network isolation (localhost / private network) for safety.
func (s *Service) GetInstanceSSHKey(ctx context.Context, req *provisionerv1.GetInstanceSSHKeyRequest) (*provisionerv1.GetInstanceSSHKeyResponse, error) {
	id := req.GetId()
	if err := ValidateID(id); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if s.keyStore == nil {
		return nil, status.Error(codes.FailedPrecondition, "ssh key store not configured (Service was constructed without WithKeyStore)")
	}

	file, err := s.store.Read()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	inst, ok := file.Instances[id]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no instance with id %q", id)
	}

	kp, err := s.keyStore.EnsureKeyPair(s.operatorID, inst.GetProvider())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load ssh keypair: %v", err)
	}
	privPEM, err := kp.MarshalPrivatePEM()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal private key: %v", err)
	}
	pubLine, err := kp.MarshalAuthorizedKey()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal public key: %v", err)
	}

	user := inst.GetSsh().GetUser()
	if user == "" {
		user = "root"
	}
	return &provisionerv1.GetInstanceSSHKeyResponse{
		PrivateKeyPem:        privPEM,
		PublicKeyAuthorized:  pubLine,
		User:                 user,
	}, nil
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
				_ = s.store.Update(func(f *State) error {
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
	return s.store.Update(func(f *State) error {
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

// ── DeploymentService ───────────────────────────────────────────────
//
// Phase 2's deployment surface. The same Service struct implements both
// gRPC servers (ProvisionerServiceServer + DeploymentServiceServer)
// sharing the Store + provider registry + key store. The
// instance<->deployment cross-reference via instance_id is a same-
// package map lookup.

// DeploymentExecutor is what the Service calls to drive the deploy +
// destroy state machine. Production wraps sshdocker.Executor; tests
// pass a stub. emit is called once per state transition (and on
// progress updates); the Service patches the state file from these.
type DeploymentExecutor interface {
	Deploy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, key *sshkeys.KeyPair, emit func(DeployStateUpdate)) error
	Destroy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, key *sshkeys.KeyPair, emit func(DeployStateUpdate)) error
}

// WithDeploymentExecutor wires the executor. Optional: when unset,
// the deployment surface returns FailedPrecondition (operator gets
// a clear "deployment requires a configured executor" message).
// The CLI's in-process buildClient passes sshdocker.NewExecutor;
// tests pass a stub.
func WithDeploymentExecutor(e DeploymentExecutor) Option {
	return func(s *Service) { s.executor = e }
}

// CreateDeployment runs the design-doc's three-step contract:
//
//  1. Critical section: claim a PENDING record if no live deployment
//     exists for this id; if a live one matches (image, model),
//     return it as AlreadyExisted (idempotent).
//
//  2. Validate: target instance exists, has SSH endpoint, is not
//     terminated. Load the SSH key for the instance's provider.
//
//  3. Launch executor goroutine. Each StateUpdate patches the
//     deployment record in the state file. With Wait=true, block
//     until terminal state; otherwise return after PENDING is
//     written.
func (s *Service) CreateDeployment(ctx context.Context, req *provisionerv1.CreateDeploymentRequest) (*provisionerv1.CreateDeploymentResponse, error) {
	dep := req.GetDeployment()
	if dep == nil {
		return nil, status.Error(codes.InvalidArgument, "deployment is required")
	}
	if err := ValidateID(dep.GetId()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if dep.GetImage() == "" {
		return nil, status.Error(codes.InvalidArgument, "deployment.image is required")
	}
	if dep.GetModel() == "" {
		return nil, status.Error(codes.InvalidArgument, "deployment.model is required")
	}
	// v0.2 ch7-beat3.2 / #84: --replicas N is plumbed through but
	// parallel fan-out (the real work) is filed as a focused
	// follow-up. This PR scaffolds the proto + helpers + CLI flag;
	// values > 1 are rejected explicitly so operators get a clear
	// signal rather than silent single-instance behavior.
	replicas := req.GetReplicas()
	if replicas == 0 {
		replicas = 1
	}
	if replicas < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "replicas must be >= 0 (got %d)", replicas)
	}
	if replicas > 1 {
		return nil, status.Errorf(codes.Unimplemented, "replicas=%d: parallel fan-out lands in a follow-up to v0.2 ch7-beat3.2; this PR scaffolds the proto + helpers only. Use --replicas 1 (or omit) for the v0.2 Beat 1+2 single-instance path.", replicas)
	}

	// Pre-flight: resolve the model spec through the configured
	// ModelStore. Default is Passthrough (no validation); production
	// callers configure huggingface.Store which validates against HF's
	// model-info API + propagates HF_TOKEN. Failing here costs us a
	// network round-trip but saves a ~$0.10-0.50 misfire when the
	// operator typo'd the model id.
	resolved, err := s.modelStore.Resolve(ctx, dep.GetModel())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "model spec: %v", err)
	}
	dep.Model = resolved.EngineModelArg
	// Merge ModelStore env onto the deployment. Operator-supplied
	// dep.Env wins over ModelStore overrides (HF_TOKEN can be
	// overridden via --env if needed).
	if len(resolved.EnvOverrides) > 0 {
		merged := make(map[string]string, len(resolved.EnvOverrides)+len(dep.GetEnv()))
		for k, v := range resolved.EnvOverrides {
			merged[k] = v
		}
		for k, v := range dep.GetEnv() {
			merged[k] = v
		}
		dep.Env = merged
	}

	// Resolve the target instance: place the deployment. v0.1's
	// scheduler is trivial -- if instance_id names an existing
	// instance, deploy there; otherwise provision a fresh instance
	// dedicated to this deployment (1:1). The placement seam is where
	// the v1.0 bin-packing scheduler will eventually live.
	inst, err := s.placeDeployment(ctx, req)
	if err != nil {
		return nil, err
	}
	// Persist the (possibly freshly-synthesized) instance_id back onto
	// the deployment so the record links to its instance.
	dep.InstanceId = inst.GetId()

	// The SSH-endpoint precondition only applies to the sshdocker
	// fallback path (VM-style providers, where the executor SSHes in).
	// Image-native providers (those satisfying Deployer) provision the
	// engine pod themselves and don't need a pre-existing SSH endpoint.
	if _, imageNative := s.providerAsDeployer(inst); !imageNative {
		switch inst.GetState() {
		case provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED, provisionerv1.InstanceState_INSTANCE_STATE_TERMINATING, provisionerv1.InstanceState_INSTANCE_STATE_FAILED:
			return nil, status.Errorf(codes.FailedPrecondition, "instance %q is %s; create a fresh instance first", inst.GetId(), strings.TrimPrefix(inst.GetState().String(), "INSTANCE_STATE_"))
		}
		if inst.GetSsh() == nil || inst.GetSsh().GetHost() == "" {
			return nil, status.Errorf(codes.InvalidArgument, "instance %q has no SSH endpoint; deployment on a non-image-native provider requires an SSH-reachable instance", inst.GetId())
		}
	}

	// Idempotency on (operator, deployment id).
	var record *provisionerv1.Deployment
	var alreadyExisted bool
	err = s.store.Update(func(f *State) error {
		if existing, ok := f.Deployments[dep.GetId()]; ok {
			switch existing.GetState() {
			case provisionerv1.DeploymentState_DEPLOYMENT_STATE_PENDING,
				provisionerv1.DeploymentState_DEPLOYMENT_STATE_STARTING,
				provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING,
				provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				provisionerv1.DeploymentState_DEPLOYMENT_STATE_DEGRADED:
				if existing.GetInstanceId() != dep.GetInstanceId() {
					return fmt.Errorf("deployment %q already exists on instance %q; destroy and recreate to move", dep.GetId(), existing.GetInstanceId())
				}
				if existing.GetImage() == dep.GetImage() && existing.GetModel() == dep.GetModel() {
					record = existing
					alreadyExisted = true
					return nil
				}
				// Same id, different desired state -> overwrite the
				// record (drift handling continues inside the executor).
			case provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATING:
				return fmt.Errorf("deployment %q is currently terminating; wait for completion", dep.GetId())
			}
			// TERMINATED / FAILED: treat as gone; claim a fresh record.
		}
		now := timestamppb.New(s.clock())
		record = &provisionerv1.Deployment{
			Id:             dep.GetId(),
			InstanceId:     dep.GetInstanceId(),
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
		// v0.2 ch7-beat3.1: instance_ids is the canonical multi-
		// instance list. #83 leaves it empty on Service-driven
		// creates -- Beat 1+2 deployments are single-instance and
		// fall back to the singular `instance_id` field via
		// EffectiveInstanceIDs (added in #84). Future operator-
		// supplied lists (heterogeneous fleets) get respected
		// verbatim; pre-populate from request if present.
		if ids := dep.GetInstanceIds(); len(ids) > 0 {
			record.InstanceIds = append(record.InstanceIds, ids...)
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

	// Load SSH key for the instance's provider.
	if s.keyStore == nil {
		return nil, status.Error(codes.FailedPrecondition, "deployment requires a configured key store (use provisioners.WithKeyStore)")
	}
	key, err := s.keyStore.EnsureKeyPair(s.operatorID, inst.GetProvider())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load ssh key: %v", err)
	}

	// Launch executor. The emit callback patches the deployment
	// record on each transition. Wait=true blocks; Wait=false
	// returns after PENDING is recorded (server-side mode).
	runCtx := context.Background() // detach from request ctx so async deploys survive caller disconnect
	if req.GetWait() {
		runCtx = ctx
	}
	done := s.launchDeploy(runCtx, record, inst, key)
	if req.GetWait() {
		<-done
		// Re-read the record to surface terminal state.
		file, err := s.store.Read()
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		if final, ok := file.Deployments[record.GetId()]; ok {
			record = final
		}
	}
	return &provisionerv1.CreateDeploymentResponse{Deployment: record}, nil
}

// launchDeploy starts the executor goroutine and returns a channel
// closed when the executor finishes (terminal state reached). Each
// emit fires a state-file patch.
func (s *Service) launchDeploy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, key *sshkeys.KeyPair) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		emit := func(u DeployStateUpdate) {
			_ = s.patchDeployment(dep.GetId(), u)
		}
		if err := s.deployerFor(inst).Deploy(ctx, dep, inst, key, emit); err == nil {
			s.finalizeInstanceAfterDeploy(ctx, inst, dep.GetId())
		}
	}()
	return done
}

// finalizeInstanceAfterDeploy promotes an auto-provisioned instance
// from PENDING to ACTIVE once its image-native deployment is RUNNING.
// The engine pod the Deployer spawned IS the instance; we look it up
// (the deployment's container_id is the pod id) and Describe it to
// pick up GPU / region metadata for the record. Best-effort: a
// Describe failure still marks the instance ACTIVE with the pod id.
//
// SSH endpoint handling: the default deploy is proxy-only -- no
// publicIp allocated, no SSH endpoint to populate. When the operator
// opts in via deployment.debug_shell, the provider also requested a
// publicIp + sshd; Describe's response carries the host/port, but
// it's UNVERIFIED at this point (the engine /health on the proxy
// proved engine reachability, not sshd). We deliberately leave Ssh
// nil and require the operator to drive wait_for_instance_ready,
// which dials sshd before stamping the verified endpoint into state.
//
// No-op for explicitly-placed instances (already ACTIVE from their own
// CreateInstance) and for non-RUNNING deployments.
func (s *Service) finalizeInstanceAfterDeploy(ctx context.Context, inst *provisionerv1.Instance, depID string) {
	file, err := s.store.Read()
	if err != nil {
		return
	}
	d, ok := file.Deployments[depID]
	if !ok || d.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		return
	}
	cur, ok := file.Instances[inst.GetId()]
	if !ok || cur.GetState() == provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE {
		return
	}
	podID := d.GetContainerId()
	provider, ok := s.providers[cur.GetProvider()]
	if !ok {
		return
	}
	if described, derr := provider.Describe(ctx, podID); derr == nil {
		finalized := s.finalizeActive(described, cur.GetSpec(), cur.GetProvider(), cur.GetCreatedAt())
		finalized.ProviderId = podID
		// Leave Ssh unset: Describe's publicIp+portMapping is unverified
		// (the engine /health proved port 8000, not port 22). The operator
		// drives wait_for_instance_ready next, which dials sshd to confirm
		// reachability before stamping the verified endpoint into state.
		finalized.Ssh = nil
		_ = s.patchRecord(cur.GetId(), finalized)
		return
	}
	// Describe failed -- still record what we know.
	cur.ProviderId = podID
	cur.State = provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE
	cur.ActivatedAt = timestamppb.New(s.clock())
	_ = s.patchRecord(cur.GetId(), cur)
}

// deployerFor returns the Deployer that should run for this instance.
// Capability-check pattern: if the provider satisfies the Deployer
// interface itself (image-native providers like RunPod, Vast.ai,
// Modal), use it. Otherwise fall back to the configured
// DeploymentExecutor (typically sshdocker -- for VM-style providers
// like Lambda Labs and raw AWS/GCP instances).
//
// Returns the same interface shape (Deployer methods are a strict
// subset of DeploymentExecutor + identical signature), so call sites
// don't have to branch.
func (s *Service) deployerFor(inst *provisionerv1.Instance) Deployer {
	if d, imageNative := s.providerAsDeployer(inst); imageNative {
		return d
	}
	return executorAsDeployer{exec: s.executor}
}

// providerAsDeployer reports whether the instance's provider is
// image-native (satisfies Deployer directly). Returns the Deployer
// and true when so; (nil, false) when the provider needs the
// sshdocker fallback. CreateDeployment uses the bool to decide
// whether the SSH-endpoint precondition applies.
func (s *Service) providerAsDeployer(inst *provisionerv1.Instance) (Deployer, bool) {
	if provider, ok := s.providers[inst.GetProvider()]; ok {
		if d, ok := provider.(Deployer); ok {
			return d, true
		}
	}
	return nil, false
}

// placeDeployment resolves the instance a deployment lands on -- the
// v0.1 scheduler seam. Trivial today: an explicit instance_id is used
// as-is; an empty one provisions a fresh instance dedicated to this
// deployment (1:1). The v1.0 bin-packing scheduler replaces the
// auto-provision branch with "find an instance in the group with VRAM
// headroom, or provision one."
//
// The synthesized instance carries the deployment's requirements +
// the engine image on its Spec, so the image-native Deployer can
// resolve the SKU and spawn the engine pod. The instance shares the
// deployment id (for v0.1's 1:1 mapping they are two views -- GPU vs
// model -- of the same pod).
func (s *Service) placeDeployment(ctx context.Context, req *provisionerv1.CreateDeploymentRequest) (*provisionerv1.Instance, error) {
	dep := req.GetDeployment()
	file, err := s.store.Read()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	// Explicit placement: deploy onto a named, existing instance.
	if dep.GetInstanceId() != "" {
		inst, ok := file.Instances[dep.GetInstanceId()]
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "instance %q does not exist", dep.GetInstanceId())
		}
		return inst, nil
	}

	// Auto-provision: synthesize a fresh instance for this deployment.
	reqs := req.GetRequirements()
	if reqs == nil {
		return nil, status.Error(codes.InvalidArgument, "deployment without an explicit instance requires resource requirements (--class, --min-vram-gb, or --sku)")
	}
	providerName := req.GetProvider()
	if providerName == "" {
		return nil, status.Error(codes.InvalidArgument, "deployment without an explicit instance requires a provider")
	}
	if _, ok := s.providers[providerName]; !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unknown provider %q", providerName)
	}

	// The instance shares the deployment id (1:1 in v0.1). Its Spec
	// carries the engine image as base_image + the requirements, so
	// the Deployer resolves the SKU and runs the engine image as the
	// pod. If a same-id instance already exists (idempotent re-deploy),
	// reuse it.
	if existing, ok := file.Instances[dep.GetId()]; ok {
		return existing, nil
	}
	spec := &provisionerv1.Spec{
		Id:           dep.GetId(),
		Provider:     providerName,
		Region:       req.GetRegion(),
		BaseImage:    dep.GetImage(),
		Requirements: reqs,
	}
	if err := ValidateAndExpandRequirements(spec); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	inst := newPendingInstance(spec, providerName, s.clock())
	if err := s.patchRecord(inst.GetId(), inst); err != nil {
		return nil, status.Errorf(codes.Internal, "record placed instance: %v", err)
	}
	return inst, nil
}

// executorAsDeployer adapts a DeploymentExecutor to the Deployer
// interface. They have the same method signatures by construction;
// this wrapper just lets the dispatch above return one type whether
// the path is provider-native or via the fallback executor.
type executorAsDeployer struct {
	exec DeploymentExecutor
}

func (e executorAsDeployer) Deploy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, key *sshkeys.KeyPair, emit func(DeployStateUpdate)) error {
	if e.exec == nil {
		return errors.New("no executor configured and provider does not implement Deployer")
	}
	return e.exec.Deploy(ctx, dep, inst, key, emit)
}

func (e executorAsDeployer) Destroy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, key *sshkeys.KeyPair, emit func(DeployStateUpdate)) error {
	if e.exec == nil {
		return errors.New("no executor configured and provider does not implement Deployer")
	}
	return e.exec.Destroy(ctx, dep, inst, key, emit)
}

// patchDeployment applies a StateUpdate to the deployment record in
// the state file under the flock. Errors are swallowed at the
// emit-callback boundary (the executor cannot meaningfully react);
// terminal state observers will see whatever last successfully
// wrote.
func (s *Service) patchDeployment(id string, u DeployStateUpdate) error {
	return s.store.Update(func(f *State) error {
		rec, ok := f.Deployments[id]
		if !ok {
			return nil
		}
		rec.State = u.State
		rec.CurrentPhase = u.Phase
		rec.ProgressMessage = u.ProgressMessage
		if u.ContainerID != "" {
			rec.ContainerId = u.ContainerID
			// For a 1:1 auto-provisioned deployment (instance shares the
			// deployment id), the container_id IS the pod id at the
			// provider. Stamp it onto the instance's provider_id the
			// moment we learn it -- before this point the instance is
			// PENDING with no provider_id, and a Deploy that fails after
			// POST but before finalize would leave the pod orphaned at
			// the provider (instance destroy would skip the Terminate
			// call because provider_id is empty).
			if inst, ok := f.Instances[id]; ok && inst.GetProviderId() == "" {
				inst.ProviderId = u.ContainerID
			}
		}
		if u.EngineEndpoint != "" {
			rec.EngineEndpoint = u.EngineEndpoint
			// v0.2 ch7-beat3.2 / #84: maintain the parallel
			// engine_endpoints list. For single-instance deployments
			// (the only path in this scaffolding PR), the list has
			// one slot mirroring the singular endpoint. Multi-instance
			// fan-out (follow-up to #84) will populate each slot as
			// per-instance deploys reach RUNNING; the router reads
			// the list via EffectiveEndpoints.
			rec.EngineEndpoints = []string{u.EngineEndpoint}
		}
		if u.FailureReason != "" {
			rec.FailureReason = u.FailureReason
		}
		now := timestamppb.New(s.clock())
		switch u.State {
		case provisionerv1.DeploymentState_DEPLOYMENT_STATE_STARTING:
			if rec.StartedAt == nil {
				rec.StartedAt = now
			}
		case provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING:
			if rec.ReadyAt == nil {
				rec.ReadyAt = now
			}
		case provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED:
			if rec.TerminatedAt == nil {
				rec.TerminatedAt = now
			}
		}
		return nil
	})
}

// DescribeDeployment returns the local-state record for one deployment.
// Pure read -- no side effects. v0.2 ch7-beat1.7 callers that want to
// mark this deployment as active (the router's per-request lookup,
// the operator's `iplane deployment touch` verb) call TouchDeployment
// explicitly; passive inspection via this RPC does not extend the
// idle-TTL reaper's lease.
func (s *Service) DescribeDeployment(ctx context.Context, req *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
	id := req.GetId()
	if err := ValidateID(id); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	file, err := s.store.Read()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	rec, ok := file.Deployments[id]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no deployment with id %q", id)
	}
	return &provisionerv1.DescribeDeploymentResponse{Deployment: rec}, nil
}

// TouchDeployment marks the deployment as active -- bumps
// last_activity_at to "now", resetting the idle-TTL reaper's clock.
// Returns the updated record.
//
// v0.2 ch7-beat1.7 callers:
//
//   - The router (per inference request, after the lookup picks the
//     target deployment). Canonical data-plane source of activity.
//   - The operator's `iplane deployment touch` CLI verb (#71), the
//     "I'm still using this thing, do not reap yet" escape hatch.
//
// NotFound surfaces normally (so the operator's touch verb prints a
// useful error). The touch itself is in the locked Update path; the
// returned record reflects the post-touch state.
func (s *Service) TouchDeployment(ctx context.Context, req *provisionerv1.TouchDeploymentRequest) (*provisionerv1.TouchDeploymentResponse, error) {
	id := req.GetId()
	if err := ValidateID(id); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	var rec *provisionerv1.Deployment
	err := s.store.Update(func(state *State) error {
		dep, ok := state.Deployments[id]
		if !ok {
			return status.Errorf(codes.NotFound, "no deployment with id %q", id)
		}
		dep.LastActivityAt = timestamppb.New(s.clock())
		rec = dep
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &provisionerv1.TouchDeploymentResponse{Deployment: rec}, nil
}

// touchDeployment updates last_activity_at on the named deployment to
// "now" via the service clock. Used as a side effect of operator
// interest -- the v0.2 ch7-beat1.7 reaper reads last_activity_at to
// decide whether a deployment is idle.
//
// Best-effort by design: a state-store write failure is logged but
// doesn't propagate to the RPC caller. last_activity is a leak-
// protection signal, not load-bearing for the operator's request. A
// missed touch at worst causes a one-tick-late reap, which is fine.
//
// Performance note: every router request flows through
// DescribeDeployment which flows through this. For v0.2 demo
// workloads (1-100 req/s) the per-request state.Update is fine
// (atomic temp+rename ~1ms on SSD). Future: batch touches in-memory
// + flush periodically when this becomes a hot path.
func (s *Service) touchDeployment(id string) {
	_ = s.store.Update(func(state *State) error {
		dep, ok := state.Deployments[id]
		if !ok || dep == nil {
			return nil
		}
		dep.LastActivityAt = timestamppb.New(s.clock())
		return nil
	})
}

// Quarantine adds instanceID to the deployment's unhealthy_instance_ids
// set. The router (#85's pickReplica) skips endpoints whose instance_id
// is in this set, removing the replica from rotation until Restore is
// called. v0.2 ch7-beat3.5 (#87).
//
// Idempotent: calling on a replica that is already in the set is a
// no-op. Silently no-ops if the deployment was destroyed concurrently
// (the health-poll loop snapshots the deployment list and can race
// against Destroy). Returns nil in that case rather than NotFound --
// the caller is internal, doesn't bubble up to an operator, and the
// next tick will not find this deployment to quarantine again.
//
// Mutation is performed under the state-file flock via store.Update,
// matching the rest of the service. The endpoint URL in
// engine_endpoints[i] is preserved across quarantine; restoration is
// set-removal, not endpoint-rediscovery.
func (s *Service) Quarantine(deployID, instanceID string) error {
	if deployID == "" || instanceID == "" {
		return nil
	}
	return s.store.Update(func(state *State) error {
		dep, ok := state.Deployments[deployID]
		if !ok || dep == nil {
			return nil
		}
		for _, id := range dep.UnhealthyInstanceIds {
			if id == instanceID {
				return nil
			}
		}
		dep.UnhealthyInstanceIds = append(dep.UnhealthyInstanceIds, instanceID)
		return nil
	})
}

// Restore removes instanceID from the deployment's unhealthy_instance_ids
// set, returning the replica to the router's rotation. Idempotent: a
// no-op if the instance is not currently quarantined, and a silent
// no-op if the deployment has been destroyed.
//
// Pairs with Quarantine; called by the health-poll loop after K
// consecutive successful /health probes on a previously-quarantined
// replica.
func (s *Service) Restore(deployID, instanceID string) error {
	if deployID == "" || instanceID == "" {
		return nil
	}
	return s.store.Update(func(state *State) error {
		dep, ok := state.Deployments[deployID]
		if !ok || dep == nil {
			return nil
		}
		ids := dep.UnhealthyInstanceIds
		for i, id := range ids {
			if id == instanceID {
				dep.UnhealthyInstanceIds = append(ids[:i], ids[i+1:]...)
				return nil
			}
		}
		return nil
	})
}

// ListDeployments returns deployments with optional instance_id +
// state filters.
func (s *Service) ListDeployments(ctx context.Context, req *provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error) {
	file, err := s.store.Read()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	instanceFilter := req.GetInstanceId()
	stateFilter := req.GetState()
	out := make([]*provisionerv1.Deployment, 0, len(file.Deployments))
	for _, dep := range file.Deployments {
		if instanceFilter != "" && dep.GetInstanceId() != instanceFilter {
			continue
		}
		if stateFilter != provisionerv1.DeploymentState_DEPLOYMENT_STATE_UNSPECIFIED && dep.GetState() != stateFilter {
			continue
		}
		out = append(out, dep)
	}
	return &provisionerv1.ListDeploymentsResponse{Deployments: out}, nil
}

// DestroyDeployment terminates the container and patches the record
// to TERMINATED. Idempotent: a destroy of an already-terminated id
// is a no-op.
func (s *Service) DestroyDeployment(ctx context.Context, req *provisionerv1.DestroyDeploymentRequest) (*provisionerv1.DestroyDeploymentResponse, error) {
	id := req.GetId()
	if err := ValidateID(id); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	var rec *provisionerv1.Deployment
	err := s.store.Update(func(f *State) error {
		existing, ok := f.Deployments[id]
		if !ok {
			return fmt.Errorf("no deployment with id %q", id)
		}
		if existing.GetState() == provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED {
			rec = existing
			return nil
		}
		existing.State = provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATING
		rec = existing
		return nil
	})
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	if rec.GetState() == provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED {
		return &provisionerv1.DestroyDeploymentResponse{Deployment: rec}, nil
	}

	// Look up the instance (needed for SSH) + the key.
	file, err := s.store.Read()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	inst, ok := file.Instances[rec.GetInstanceId()]
	if !ok {
		// Instance is gone; mark TERMINATED locally (force-like).
		_ = s.patchDeployment(id, DeployStateUpdate{
			State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED,
			ProgressMessage: "instance gone; marked terminated locally",
		})
		final, _ := s.store.Read()
		return &provisionerv1.DestroyDeploymentResponse{Deployment: final.Deployments[id]}, nil
	}
	if s.keyStore == nil {
		return nil, status.Error(codes.FailedPrecondition, "deployment requires a configured key store")
	}
	key, err := s.keyStore.EnsureKeyPair(s.operatorID, inst.GetProvider())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load ssh key: %v", err)
	}

	// Synchronous destroy (no Wait flag on the request -- destroy
	// always blocks until terminal state, simpler semantics).
	emit := func(u DeployStateUpdate) {
		_ = s.patchDeployment(id, u)
	}
	if err := s.deployerFor(inst).Destroy(ctx, rec, inst, key, emit); err != nil {
		return nil, status.Errorf(codes.Internal, "destroy %s: %v", id, err)
	}

	// For an auto-provisioned 1:1 deployment (instance id == deployment
	// id), the engine pod the Deployer just terminated IS the instance,
	// so mark the instance TERMINATED too. An explicitly-placed
	// instance (different id, possibly shared / reused) is left alone --
	// the operator tears it down via `iplane instance destroy`.
	if inst.GetId() == id {
		if cur, ok := mustReadInstances(s)[id]; ok && cur.GetState() != provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED {
			cur.State = provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED
			cur.TerminatedAt = timestamppb.New(s.clock())
			_ = s.patchRecord(id, cur)
		}
	}

	final, _ := s.store.Read()
	return &provisionerv1.DestroyDeploymentResponse{Deployment: final.Deployments[id]}, nil
}

// mustReadInstances returns the current Instances map (empty on read
// error). Helper for the coupled-teardown path where a read failure
// just means "skip the instance patch."
func mustReadInstances(s *Service) map[string]*provisionerv1.Instance {
	file, err := s.store.Read()
	if err != nil {
		return map[string]*provisionerv1.Instance{}
	}
	return file.Instances
}

// WatchDeployment streams state transitions for one deployment until
// it reaches a terminal state or the client disconnects.
//
// v0.1 implementation: poll the state file every pollEvery; emit
// a DeploymentStateChangedEvent whenever the observed state changes.
// Real fanout from the executor's emit callback (no polling) lands
// in v0.2 when there are multiple consumers; the polling shape is
// sufficient for v0.1's single-CLI-watcher case.
func (s *Service) WatchDeployment(req *provisionerv1.WatchDeploymentRequest, stream provisionerv1.DeploymentService_WatchDeploymentServer) error {
	id := req.GetId()
	if err := ValidateID(id); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	// Watch is pure read -- callers that need to mark activity call
	// TouchDeployment explicitly. (Same reasoning as DescribeDeployment:
	// passive observation must not extend the reaper's lease.)
	pollEvery := 500 * time.Millisecond

	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()

	var lastState provisionerv1.DeploymentState
	var lastPhase, lastProgress string
	first := true
	for {
		file, err := s.store.Read()
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		rec, ok := file.Deployments[id]
		if !ok {
			return status.Errorf(codes.NotFound, "no deployment with id %q", id)
		}
		curState := rec.GetState()
		curPhase := rec.GetCurrentPhase()
		curProgress := rec.GetProgressMessage()
		// Fire on any of: state / phase / progress_message change. The
		// engine-waiting loop holds (state, phase) steady for minutes;
		// progress_message ticks every poll with elapsed-time / HTTP
		// status -- the only signal the operator gets during cold pulls.
		if first || curState != lastState || curPhase != lastPhase || curProgress != lastProgress {
			now := timestamppb.New(s.clock())
			ev := &provisionerv1.DeploymentStateChangedEvent{
				Id:              id,
				From:            lastState,
				To:              curState,
				Phase:           curPhase,
				ProgressMessage: curProgress,
				At:              now,
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
			lastState = curState
			lastPhase = curPhase
			lastProgress = curProgress
			first = false
		}
		// Terminal state -> stream done.
		if curState == provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED ||
			curState == provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
			return nil
		}
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-ticker.C:
		}
	}
}

// Compile-time checks that Service implements both generated gRPC
// server interfaces. The embedded Unimplemented...Server fields make
// forward-compat (new RPCs added to the proto) a non-event for this
// type -- only methods we explicitly override are subject to drift.
var (
	_ provisionerv1.ProvisionerServiceServer = (*Service)(nil)
	_ provisionerv1.DeploymentServiceServer  = (*Service)(nil)
)
