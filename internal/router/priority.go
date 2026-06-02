package router

import (
	"context"
	"net/http"
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// PriorityHeader is the operator-asserted priority lane on inbound
// requests. Like TenantHeader (Beat 2.2), this is iplane-internal:
// engines don't need to know about lanes; the router strips it at
// proxyTo.Rewrite before forwarding. Two recognized values today:
// "interactive" and "batch" (case-insensitive). Anything else
// (including missing) means "no operator preference" -- the
// effective priority falls through to the deployment's default and
// finally to INTERACTIVE.
const PriorityHeader = "X-IPlane-Priority"

// priorityCtxKey is the unexported context key storing the
// header-decoded Priority for a request. Unexported type so
// out-of-package callers can't collide on a shared key.
type priorityCtxKey struct{}

// priorityFromHeader decodes the X-IPlane-Priority header.
// Unrecognized / missing values return PRIORITY_UNSPECIFIED so
// effectivePriority can fall back to the deployment default.
func priorityFromHeader(req *http.Request) provisionerv1.Priority {
	switch strings.ToLower(strings.TrimSpace(req.Header.Get(PriorityHeader))) {
	case "interactive":
		return provisionerv1.Priority_PRIORITY_INTERACTIVE
	case "batch":
		return provisionerv1.Priority_PRIORITY_BATCH
	default:
		return provisionerv1.Priority_PRIORITY_UNSPECIFIED
	}
}

// withPriority extracts the header-decoded priority and stores it on
// the request's context. UNSPECIFIED on ctx means "no explicit
// header"; the post-deployment-lookup resolver (effectivePriority)
// replaces it with the deployment default (or INTERACTIVE).
//
// Mirrors withTenant in shape: middleware wraps both URL families
// in Router.Handle so the queued and direct paths both see a
// populated value.
func withPriority(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		p := priorityFromHeader(req)
		ctx := context.WithValue(req.Context(), priorityCtxKey{}, p)
		next.ServeHTTP(w, req.WithContext(ctx))
	})
}

// effectivePriority returns the resolved Priority for this request,
// applying the chapter's precedence: explicit header > router-level
// default (INTERACTIVE). Always returns a non-UNSPECIFIED value so
// the router's lane-routing code can switch on it without an extra
// fallback.
//
// Priority is a request-level concept: there is intentionally NO
// per-deployment fallback. The engine inside the deployment is
// priority-blind (it just batches what it gets); putting a default
// on Deployment would be policy-on-the-runtime-artifact, which
// belongs at the routing layer instead. Operators who need a
// deployment-specific default wrap their clients to inject the
// header.
//
// The dep argument is unused today; kept on the signature because
// future routing policies (e.g., model-based defaults) may want
// it. Tagged with _ to silence the linter.
func effectivePriority(ctx context.Context, _ *provisionerv1.Deployment) provisionerv1.Priority {
	if p, ok := ctx.Value(priorityCtxKey{}).(provisionerv1.Priority); ok && p != provisionerv1.Priority_PRIORITY_UNSPECIFIED {
		return p
	}
	return provisionerv1.Priority_PRIORITY_INTERACTIVE
}

// effectivePriorityFromCtx reads the priority stored on ctx by
// either the withPriority middleware (header value, may be
// UNSPECIFIED) or by enqueueOrServe after deployment lookup
// (post-fallback value, always non-UNSPECIFIED). Used by
// handleWithObservability which runs after enqueueOrServe and
// always sees the resolved value.
func effectivePriorityFromCtx(ctx context.Context) provisionerv1.Priority {
	if p, ok := ctx.Value(priorityCtxKey{}).(provisionerv1.Priority); ok && p != provisionerv1.Priority_PRIORITY_UNSPECIFIED {
		return p
	}
	return provisionerv1.Priority_PRIORITY_INTERACTIVE
}

// priorityLabel renders a Priority enum value as the lowercased
// operator-facing label (e.g., "interactive"). Used for metric
// labels and span attributes -- low-cardinality strings that align
// with the CLI's --priority flag vocabulary.
func priorityLabel(p provisionerv1.Priority) string {
	switch p {
	case provisionerv1.Priority_PRIORITY_INTERACTIVE:
		return "interactive"
	case provisionerv1.Priority_PRIORITY_BATCH:
		return "batch"
	default:
		return "unspecified"
	}
}
