package router

import (
	"context"
	"net/http"
	"strings"
)

// TenantHeader is the operator-asserted tenant identifier on inbound
// requests. The header is intentionally iplane-internal: it controls
// queueing, fair-share, per-tenant metrics, and (Part V) auth, but it
// is NEVER forwarded to the engine. Engines stay tenant-agnostic --
// correlation across iplane → engine spans goes through OTel
// trace_id, not through replicating tenant on every layer.
//
// Real authentication / signed tenant claims arrive in Part V; v0.2
// trusts the operator who runs `iplane serve` to assert valid IDs.
const TenantHeader = "X-IPlane-Tenant"

// DefaultTenantID is the tenant assigned to requests that arrive
// without a TenantHeader. Picked over the empty string so log lines
// and metric labels carry a recognizable value -- operators
// debugging "where did this tenant come from" see "default" and
// understand the request was unannotated, not a misparsed empty
// string.
const DefaultTenantID = "default"

// tenantCtxKey is the unexported context key used to store the
// resolved tenant ID on a request's context. Unexported type so
// callers from outside this package cannot collide on a shared
// string key.
type tenantCtxKey struct{}

// tenantFromContext returns the resolved tenant ID from ctx, or
// DefaultTenantID if no tenant has been set. Used by the
// observability defer in handleWithObservability and by the queued
// path when populating queueEntry.TenantID. Always returns a
// non-empty string -- callers do not need to fall back themselves.
func tenantFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(tenantCtxKey{}).(string); ok && v != "" {
		return v
	}
	return DefaultTenantID
}

// extractTenant resolves the tenant for a request: header → trim →
// fall back to default if empty. Returns the normalized value.
//
// Validation deliberately light: trim whitespace only. Real
// validation (printable ASCII, length cap, regex) lands when Part V
// turns tenant IDs into auth principals; today it's an
// operator-asserted label that we just need to be lossless about.
func extractTenant(req *http.Request) string {
	t := strings.TrimSpace(req.Header.Get(TenantHeader))
	if t == "" {
		return DefaultTenantID
	}
	return t
}

// withTenant returns an http.Handler that resolves the tenant from
// the inbound request header, stores it on the request's context,
// and chains to next. Wrapped around every router handler in
// Router.Handle so both URL families (deploy-id and flat) and both
// dispatch paths (direct and queued) see a populated tenant on the
// request ctx.
//
// The handler does NOT strip TenantHeader from the inbound request;
// the engine-facing strip happens in proxyTo (via the
// httputil.ReverseProxy Rewrite hook) so the queue's snapshot of
// req.Header still carries the original headers and an in-process
// reader of the entry can inspect them for debugging.
func withTenant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		tenant := extractTenant(req)
		ctx := context.WithValue(req.Context(), tenantCtxKey{}, tenant)
		next.ServeHTTP(w, req.WithContext(ctx))
	})
}
