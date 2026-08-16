// Package provisioners hosts the operator-facing ProvisionerService
// (CLI-callable today as an in-process struct, registerable with a
// connect/grpc mux later), the Go-internal Provider interface every
// per-provider adapter satisfies, and the helpers (ID validation,
// tag-key constants, error wrapping) shared across both.
//
// The wire types travel as the generated provisioner.v1 messages from
// gen/go/provisioner/v1 -- there is no parallel Go struct to keep in
// sync. The design that locks every shape in this package is
// docs/design/0001-provisioner.md.
//
// Layering:
//
//	cmd/iplane  -->  provisioners.Service  -->  Provider (RunPod, Local)
//	                       |
//	                       v
//	              provisioners/stores/file (JSON state file with flock)
//
// The Service owns the failure-mode contract (idempotency lookup,
// pending -> active hygiene, self-heal on next list). Provider
// adapters are dumb provider operations.
package provisioners

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// Provider is the Go-internal interface every per-provider adapter
// satisfies. Not exposed via gRPC -- only the higher-level Service is.
// This mirrors the existing Backend / InferenceService split: dumb
// adapter underneath, smart service on top.
//
// Errors from the provider SDK surface up wrapped in *ProviderError so
// callers can errors.As to the cause and surface the raw provider
// message. The design doc explicitly forbids normalizing provider
// errors into iplane-canonical codes -- that strips rate-limit headers,
// retry-after, and the actual rejection reason.
type Provider interface {
	// Name identifies the adapter ("runpod", "local"). The Service
	// dispatches by matching spec.Provider against Name().
	Name() string

	// Spawn acquires an instance matching the spec and returns the
	// fully-resolved Instance with provider_id, region as scheduled,
	// gpu as scheduled, hourly_rate_usd, and ssh populated.
	//
	// Spawn is NOT idempotent on its own. The Service enforces
	// idempotency on (operator, id) via local-state and List-by-tag
	// lookups before invoking Spawn; adapters can rely on Spawn never
	// being called for an id that already has an active or pending
	// instance under the same operator.
	Spawn(ctx context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error)

	// Terminate releases the named provider instance. Idempotent by
	// contract -- calling twice on the same id returns nil the second
	// time, ideally without a network call. Without that property the
	// state-file recovery path has to special-case "already gone"
	// everywhere.
	Terminate(ctx context.Context, providerID string) error

	// Describe returns the provider's current view of the instance.
	// Does not consult local state. Returns ErrNotFound wrapped in a
	// *ProviderError if the provider does not know the id.
	Describe(ctx context.Context, providerID string) (*provisionerv1.Instance, error)

	// List enumerates instances under the operator's account, filtered
	// by tag (match-all). The two cases it serves: ghost-record recovery
	// (local pending record needs reconciliation against what the
	// provider actually created -- filter {iplane-id: <id>}), and
	// leaked-instance detection (provider has an instance with no
	// matching local record -- filter {iplane-operator: <op>}).
	//
	// Returns InstanceRef rather than Instance because some provider
	// APIs return less information from list than from describe. The
	// Service calls Describe(ref.ProviderId) when it needs the full
	// instance.
	List(ctx context.Context, filter map[string]string) ([]*provisionerv1.InstanceRef, error)
}

// KeyRegistrar is an optional Provider capability. Adapters implement
// it when the provider needs an SSH public key registered before
// Spawn so newly-created instances boot with the operator's key
// already installed in /root/.ssh/authorized_keys.
//
// The Service calls EnsurePublicKey(ctx, pub, comment) once per
// CreateInstance, before Spawn, when both (a) a key store is wired
// into the Service via WithKeyStore, and (b) the target provider
// satisfies this interface. Adapters that do not need this (local)
// simply do not implement it and the call is skipped.
//
// EnsurePublicKey is expected to be idempotent: when the provider
// already has this exact public key on file (matched by comment via
// IsIplaneComment + exact-bytes check), the implementation should
// be a no-op. RunPod's pubKey blob is a read-modify-write surface,
// so the runpod adapter does both checks.
//
// Errors abort CreateInstance with FailedPrecondition; no Spawn
// happens. This is the cost-gate behavior the design doc commits
// to -- operators see "couldn't register SSH key" before any pod
// gets billed.
type KeyRegistrar interface {
	EnsurePublicKey(ctx context.Context, publicKey []byte, comment string) error
}

// SSHReadyWaiter is an optional capability a provider implements when
// the SSH endpoint is not immediately available after Spawn (RunPod
// assigns the public IP a few seconds after scheduling, for example).
// The Service exposes it via WaitForInstanceReady so callers explicitly
// drive the "Join" half of an asynchronous Spawn -- one-shot operators
// who don't need SSH never pay the wait; deployment-bound flows call
// it before CreateDeployment.
//
// Providers without an SSH-readiness gap (local, providers whose
// Spawn already blocks for full IP assignment) simply do not implement
// this; the Service returns the current Instance unchanged in that
// case (the wait is a no-op).
//
// The returned SshTarget is the populated endpoint; nil + non-nil err
// signals timeout / network failure / provider error. On error the
// Service does NOT patch state -- the caller can retry.
type SSHReadyWaiter interface {
	WaitForSSHReady(ctx context.Context, providerID string) (*provisionerv1.SshTarget, error)
}

// FailureReporter is an optional Provider capability for providers that can
// tell "this instance will never run" apart from "it is still working".
//
// A readiness wait cannot answer that question on its own. It sees an endpoint
// that has not answered yet, which looks identical whether the image is still
// pulling or the container exited ten minutes ago. Without a provider signal
// the only safe behaviour is to keep waiting, so a dead host bills the entire
// engine-ready timeout: on a 4x A100 that is roughly $1.20 per attempt instead
// of $0.02, and #214 means a retry can land on the same host again.
//
// Providers that cannot answer simply do not implement it, and the wait keeps
// its previous behaviour. That is not a gap to fill later for its own sake:
// whether a provider can answer is a real property of its API, measured rather
// than assumed. See the per-provider notes below.
//
// # The contract
//
// TerminalFailure reports a fault ONLY when the provider has said so. It must
// never report one it merely suspects, because the cost of the two mistakes is
// wildly asymmetric: failing to notice a dead host wastes a timeout, while
// falsely calling a live one dead kills a deploy that was working. Slow is the
// normal case here -- a 10 GB engine image on community capacity routinely
// takes minutes.
//
// Consequently a transport error is NOT a terminal failure. Implementations
// swallow it and return false, which is why this returns no error: a provider
// API that is briefly unreachable says nothing about the instance behind it,
// and Vast's control API was observed going slow in bursts and recovering
// mid-deploy.
//
// reason carries the provider's own words and is shown to the operator. It IS
// the diagnosis: an IPv6 address in a pull error is what identified a broken
// host network path, and "deploy failed" would have left nothing to act on.
//
// # Which providers can answer, measured 2026-08-11
//
//   - vast: yes. The instance record carries cur_state plus a status_msg
//     holding docker's verbatim error, and two hosts failed this way in one
//     session (a broken IPv6 path to the registry CDN, and a broken NVIDIA
//     CDI configuration).
//
//   - runpod: no, and deliberately not implemented rather than left as an
//     oversight. Its two failure modes were induced and inspected. A missing
//     image is rejected at pod-create with a clear message, so it never
//     reaches a readiness wait and costs nothing. A container that exits is
//     invisible: RunPod restarts it, desiredStatus stays "RUNNING",
//     lastStatusChange reports the rental rather than the container, and the
//     v1 pod record has no status or runtime field at all. The only surface
//     carrying the failure is the v2 log stream, which is a much larger and
//     more fragile lift than this capability wants.
//
//   - lambdalabs, local, external: not implemented, unexamined.
type FailureReporter interface {
	TerminalFailure(ctx context.Context, providerID string) (failed bool, reason string)
}

// TerminalFailure asks a provider whether an instance has definitively failed,
// returning false for providers that cannot answer.
//
// The nil-and-type-assertion dance lives here so every readiness wait treats a
// provider without the capability the same way, rather than each one growing
// its own opinion about what a missing sensor means.
func TerminalFailure(ctx context.Context, p Provider, providerID string) (bool, string) {
	fr, ok := p.(FailureReporter)
	if !ok || providerID == "" {
		return false, ""
	}
	return fr.TerminalFailure(ctx, providerID)
}

// VolumeManager is an optional Provider capability for providers that
// offer persistent volumes a model can be pre-staged onto (RunPod
// network volumes today), so warm-cache deploys mount weights instead of
// re-downloading them. Providers without persistent volumes simply do
// not implement it, and the warm-cache pin surface reports the provider
// as unsupported. Asserted via provider.(VolumeManager), the same
// runtime opt-in pattern as Deployer / KeyRegistrar / SSHReadyWaiter.
//
// A volume is a shared cache: one volume holds many models under a
// single HuggingFace cache layout, and it is datacenter-locked (a pod
// must be scheduled in the volume's region to mount it).
type VolumeManager interface {
	// EnsureVolume finds or creates a cache volume for (region, name)
	// and returns its handle. Idempotent: an existing volume of the same
	// name in the region is reused, never duplicated.
	EnsureVolume(ctx context.Context, spec VolumeSpec) (VolumeRef, error)

	// StageModel downloads a model onto an existing volume so later
	// deploys mount it. Idempotent at the HuggingFace-cache level:
	// already-present files are skipped. Blocks until the download
	// completes (it spins a throwaway pod that mounts the volume, fetches
	// the weights, and exits).
	StageModel(ctx context.Context, spec StageSpec) error

	// ListVolumes returns the provider's cache volumes.
	ListVolumes(ctx context.Context) ([]VolumeRef, error)

	// DeleteVolume destroys a volume and everything staged on it.
	DeleteVolume(ctx context.Context, volumeID string) error
}

// VolumeSpec requests a cache volume. Name is the deterministic
// per-region cache name (or an operator-supplied one); Region is the
// provider datacenter the volume is pinned to; SizeGB is the capacity to
// create when the volume does not yet exist.
type VolumeSpec struct {
	Name   string
	Region string
	SizeGB int
}

// VolumeRef identifies a provider cache volume.
type VolumeRef struct {
	ID     string
	Name   string
	Region string
	SizeGB int
}

// StageSpec requests a model download onto a volume. MountPath is where
// the volume attaches in the staging pod (HF_HOME points at
// MountPath/hf); HFToken authenticates gated-model fetches; Region is
// the volume's datacenter (the staging pod must be scheduled there).
type StageSpec struct {
	VolumeID  string
	Region    string
	Model     string
	MountPath string
	HFToken   string
}

// Tag keys stamped on every provider instance Spawn creates. The Service
// uses them as List filters for the idempotency lookup and the
// post-v0.1 reconcile loop.
const (
	TagID       = "iplane-id"
	TagOperator = "iplane-operator"
)

// Reserved provider names recognized by the Service.
const (
	ProviderLocal  = "local"
	ProviderRunPod = "runpod"
	ProviderVast   = "vast"
	// ProviderLambdaLabs follows in #151. Const reserved here so the
	// future PR's diff stays small.
	ProviderLambdaLabs = "lambdalabs"
	// ProviderExternal is the non-owning provider: it registers a
	// RUNNING replica pointing at an engine URL the operator runs
	// themselves, rather than provisioning. See internal/provisioners/external.
	ProviderExternal = "external"
)

// ExternalEndpointTag carries the operator-supplied engine URL for the
// external provider from ReplicaSpec.engine_endpoint through the Spec's
// tag map to the external adapter's Spawn/Deploy. Internal plumbing, not
// an operator-facing label.
const ExternalEndpointTag = "iplane-external-engine-endpoint"

// Default upstream-auth shape. Almost every hosted OpenAI-compatible API
// wants a bearer token in Authorization, so an operator naming only the
// environment variable gets a working credential.
const (
	DefaultUpstreamAuthHeader = "Authorization"
	DefaultUpstreamAuthPrefix = "Bearer "
)

// ValidateUpstreamAuth checks a credential description and fills its defaults.
//
// The env var is required to exist NOW, at deploy time. A deployment
// registered against a credential that is not set looks perfectly healthy and
// 401s every request, so the operator finds out from production traffic.
// Failing the create is the cheaper place to learn it.
//
// The value itself is never read into the record. Only the variable's name is
// persisted, because the record goes to the state file and to every
// DescribeDeployment response.
func ValidateUpstreamAuth(auth *provisionerv1.UpstreamAuth) (*provisionerv1.UpstreamAuth, error) {
	if auth == nil {
		return nil, nil
	}
	if auth.GetValueEnv() == "" {
		return nil, fmt.Errorf("upstream auth requires value_env, the NAME of an environment variable holding the credential")
	}
	if os.Getenv(auth.GetValueEnv()) == "" {
		return nil, fmt.Errorf("upstream auth names $%s but it is empty or unset; export it before deploying", auth.GetValueEnv())
	}
	out := &provisionerv1.UpstreamAuth{
		Header:      auth.GetHeader(),
		ValueEnv:    auth.GetValueEnv(),
		ValuePrefix: auth.GetValuePrefix(),
	}
	if out.Header == "" {
		out.Header = DefaultUpstreamAuthHeader
	}
	// An explicitly empty prefix is a real choice (a gateway wanting the bare
	// token in X-Api-Key), so only an unset header gets the bearer default.
	if out.ValuePrefix == "" && out.Header == DefaultUpstreamAuthHeader {
		out.ValuePrefix = DefaultUpstreamAuthPrefix
	}
	return out, nil
}

// GPU class taxonomy. The chapter teaches one vocabulary across providers;
// each adapter ships its own class -> []SKU table.
const (
	GPUClassSmall  = "small"  // ~24 GB consumer (RTX 4090, RTX 5090)
	GPUClassMedium = "medium" // 40 - 48 GB (A6000, A100 40 GB)
	GPUClassLarge  = "large"  // 80 GB (A100 80 GB, H100 80 GB)
	GPUClassXLarge = "xlarge" // 96 GB+ (H100 96 GB, H200, B-series)
)

// ClassifyByVRAM names the class a card falls into, derived from its memory
// rather than from a reverse lookup table. An RTX 4090 is small because 24 GB
// lands in the [24, 40) band, full stop.
//
// Deriving it means the classification cannot drift from the class defaults in
// classDefaults, which are stated in the same units. A hand-maintained
// SKU-to-class table on each adapter could, and there were three of them.
//
// Adapters call this after their own catalog lookup, so an operator-supplied
// --gpu-sku outside a curated catalog still yields "" (no opinion) rather than
// a guess. Callers pass 0 for an unknown card and get "" back.
func ClassifyByVRAM(vramGb int) string {
	switch {
	case vramGb <= 0:
		return ""
	case vramGb >= 96:
		return GPUClassXLarge
	case vramGb >= 80:
		return GPUClassLarge
	case vramGb >= 40:
		return GPUClassMedium
	default:
		return GPUClassSmall
	}
}

// ReservedIDPrefix is the prefix the Service rejects on operator-supplied
// ids, reserving the namespace for a future relaxation to auto-generated
// ids without colliding with anything that exists.
const ReservedIDPrefix = "iplane-"

// ErrNotFound is the canonical cause adapters wrap when the provider
// reports the instance no longer exists. Callers test for it via
// errors.Is(err, ErrNotFound).
var ErrNotFound = errors.New("provider: instance not found")

// ProviderError wraps a provider SDK or HTTP error so callers can
// errors.As to the cause and surface the raw provider message. Adapters
// SHOULD return this for every failure mode so the Service can attach
// the wrapped message to the failed-state record without losing detail.
type ProviderError struct {
	Provider string // "runpod" | "local"
	Op       string // "spawn" | "terminate" | "describe" | "list"
	Cause    error  // the original error from the provider SDK or HTTP layer
	HTTP     int    // HTTP status if available, 0 otherwise
}

func (e *ProviderError) Error() string {
	if e.HTTP != 0 {
		return fmt.Sprintf("provider %s: %s failed (http %d): %v", e.Provider, e.Op, e.HTTP, e.Cause)
	}
	return fmt.Sprintf("provider %s: %s failed: %v", e.Provider, e.Op, e.Cause)
}

func (e *ProviderError) Unwrap() error { return e.Cause }

// NewProviderError builds the wrapped error every adapter returns on
// failure. Pass http=0 when the underlying error did not have an HTTP
// status (SDK error, transport failure, parse error).
func NewProviderError(provider, op string, cause error, httpStatus int) *ProviderError {
	return &ProviderError{Provider: provider, Op: op, Cause: cause, HTTP: httpStatus}
}

// idPattern enforces DNS-safe IDs: lowercase alphanumeric and hyphens,
// 1 - 63 chars, must start and end alphanumeric. The constraint matters
// before IDs start appearing in OTel resource attributes, cluster
// manager records, or hostnames in v0.2 onward.
var idPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ValidateID returns nil if the supplied id is well-formed, otherwise
// an error explaining why. Two checks: DNS-safe format, and absence
// of the reserved "iplane-" prefix.
func ValidateID(id string) error {
	if id == "" {
		return errors.New("id is required")
	}
	if strings.HasPrefix(id, ReservedIDPrefix) {
		return fmt.Errorf("id %q starts with reserved prefix %q", id, ReservedIDPrefix)
	}
	if !idPattern.MatchString(id) {
		return fmt.Errorf("id %q must be DNS-safe (lowercase alphanumeric and hyphens, 1-63 chars, start and end alphanumeric)", id)
	}
	return nil
}
