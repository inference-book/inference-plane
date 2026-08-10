package engines_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The registry is observational. Membership, group composition and liveness
// may lag by seconds and cost nobody a request; a routing decision cannot.
//
// So the router must not consume registry state. It keeps its own in-flight
// counts and locality history, and its eligibility comes from the health
// poller's quarantine, precisely so a routing decision never depends on an
// observation pipeline being up. That discipline is what Ch 8 established and
// what the design doc's guardrail preserves through the pull-to-push move.
//
// This is checked as an import-graph fact rather than trusted to a comment,
// because the failure is silent and attractive: the registry will eventually
// carry in-flight counts and cache utilisation, which look exactly like what
// a load-aware policy wants, and wiring them in would be a two-line change
// that quietly puts the data path behind the fleet view.
//
// Deliberately not promoted to CONSTRAINTS.md yet. That file's convention is
// to extract rules from real friction rather than to invent them, and this
// has not yet been violated once. If it ever is, this test is the incident
// that earns it a numbered rule.
func TestRouterDoesNotImportTheRegistry(t *testing.T) {
	const registryPkg = "github.com/inference-book/inference-plane/internal/engines"

	for _, dir := range []string{"../router/...", "../dataplane/..."} {
		out, err := exec.Command("go", "list", "-deps", dir).Output()
		if err != nil {
			// The dataplane package does not exist yet in v0.2. A missing
			// path is not a violation, and must not be read as a pass for
			// the paths that do exist -- each is checked independently.
			continue
		}
		for _, dep := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(dep) == registryPkg {
				t.Errorf("%s depends on %s; the registry is observational and a routing "+
					"decision must not ride on it (see this test's doc comment)", dir, registryPkg)
			}
		}
	}
}
