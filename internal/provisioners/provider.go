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
//	              provisioners/state (JSON state file with flock)
//
// The Service owns the failure-mode contract (idempotency lookup,
// pending -> active hygiene, self-heal on next list). Provider
// adapters are dumb provider operations.
package provisioners

import (
	"context"
	"errors"
	"fmt"
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
)

// GPU class taxonomy. The chapter teaches one vocabulary across providers;
// each adapter ships its own class -> []SKU table.
const (
	GPUClassSmall  = "small"  // ~24 GB consumer (RTX 4090, RTX 5090)
	GPUClassMedium = "medium" // 40 - 48 GB (A6000, A100 40 GB)
	GPUClassLarge  = "large"  // 80 GB (A100 80 GB, H100 80 GB)
	GPUClassXLarge = "xlarge" // 96 GB+ (H100 96 GB, H200, B-series)
)

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
