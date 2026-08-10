// Package version carries the build's identity.
//
// It exists because something outside this binary needs to name a specific
// build: the engine agent is fetched onto a rented box by URL, and that URL
// has to point at a pinned artifact rather than "latest". A control plane
// that cannot say which version it is cannot ask for a matching agent.
package version

// Version is the build's version, stamped at link time by the release
// pipeline (see the Makefile's dist targets).
//
// "dev" on an unstamped build, matching pinned-versions.env's CP_VERSION on
// main. A dev build has no published artifact to point an agent at, which is
// a real limitation rather than a placeholder: the delivery path treats an
// unreleased control plane as "no agent to fetch" instead of guessing at a
// version that was never published.
var Version = "dev"

// IsRelease reports whether this build came from the release pipeline, and
// therefore whether a matching published artifact can be expected to exist.
func IsRelease() bool { return Version != "dev" && Version != "" }
