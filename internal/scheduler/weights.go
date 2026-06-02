package scheduler

// Weights maps tenant_id to per-tenant weight for fair-share dispatch
// within a priority lane (v0.2 ch7-beat2.5). Higher weight means a
// larger share of the lane's worker time. Tenants not in the map
// get DefaultTenantWeight on first Submit.
//
// Weights are immutable after Scheduler construction in v0.2 --
// changes require a daemon restart. Hot-reload via fsnotify is a
// filed follow-up; the chapter narrative doesn't require it.
type Weights map[string]int

// DefaultTenantWeight is the weight assigned to a tenant_id not
// listed in the operator's Weights config. Setting it to 1 means
// configured tenants compete on equal footing with anonymous traffic
// unless the operator explicitly bumps a tenant's weight.
const DefaultTenantWeight = 1

// WeightFor returns the weight configured for tenantID, falling
// back to DefaultTenantWeight when the tenant has no explicit
// entry. Always returns a positive value -- a configured weight of
// 0 or negative is treated as the default to keep the lottery
// arithmetic well-defined.
func (w Weights) WeightFor(tenantID string) int {
	if v, ok := w[tenantID]; ok && v > 0 {
		return v
	}
	return DefaultTenantWeight
}
