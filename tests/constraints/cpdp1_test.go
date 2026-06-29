// Package constraints holds tests that cross-check CONSTRAINTS.md's
// verify lines. Each constraint rule has a paired test that synthesizes
// a violation in a temp directory and asserts the verify command catches
// it. This protects against the gate quietly stopping work -- a grep
// command can break in silent ways (an import path renames, the shell
// quoting drifts, a flag changes meaning) and a regular package can lose
// import-graph hygiene without anyone noticing.
//
// These tests run in the default `go test ./...` suite. No build tag --
// they are unit tests of the gate, not behavior tests of the system.
package constraints

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// forbiddenImport is the exact import path CP/DP-1 forbids from the
// data plane. Kept in one place so the grep command in CONSTRAINTS.md,
// the grep command in `make check-constraints`, and this test all
// stay in sync if the project's module path ever changes.
const forbiddenImport = `"github.com/inference-book/inference-plane/internal/provisioners"`

// runGate runs the same grep command the Makefile's check-constraints
// target runs (with stderr suppressed; matches captured to stdout) and
// returns the captured match list. Mirrors the gate's intent: a
// non-empty stdout means at least one CP/DP-1 violator was found, an
// empty stdout means clean. Exit code is intentionally not inspected
// -- BSD grep returns exit 2 when a search path is missing even if
// other paths matched, which would falsely pass a real violation if
// the gate keyed on exit status.
func runGate(t *testing.T, workDir string) string {
	t.Helper()
	cmd := exec.Command("grep", "-rln", forbiddenImport, "internal/router", "internal/dataplane")
	cmd.Dir = workDir
	out, _ := cmd.Output() // exit code intentionally ignored; see comment above.
	return string(out)
}

// TestCPDP1_GrepCatchesViolation synthesizes a data-plane file that
// imports internal/provisioners and asserts the grep command from
// CONSTRAINTS.md detects it. Without this test, a future change to
// the import path (or to grep's flags) could leave the gate vacuously
// passing while real violations slip through.
func TestCPDP1_GrepCatchesViolation(t *testing.T) {
	dir := t.TempDir()
	routerDir := filepath.Join(dir, "internal", "router")
	if err := os.MkdirAll(routerDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	violator := `package router

import _ ` + forbiddenImport + `
`
	if err := os.WriteFile(filepath.Join(routerDir, "bad.go"), []byte(violator), 0o644); err != nil {
		t.Fatalf("write violator: %v", err)
	}

	out := runGate(t, dir)
	if out == "" {
		t.Fatalf("gate produced no output; expected the violator path. CP/DP-1 gate is broken.")
	}
}

// TestCPDP1_GrepIgnoresControlPlaneImport asserts the gate stays
// narrow: control-plane code that imports internal/provisioners is
// fine; the constraint is one-way (data-plane MAY NOT import). The
// grep only walks internal/router and internal/dataplane, so a file
// in any other location does not trigger.
func TestCPDP1_GrepIgnoresControlPlaneImport(t *testing.T) {
	dir := t.TempDir()
	innocentDir := filepath.Join(dir, "internal", "services")
	if err := os.MkdirAll(innocentDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	contents := `package services

import _ ` + forbiddenImport + `
`
	if err := os.WriteFile(filepath.Join(innocentDir, "ok.go"), []byte(contents), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if out := runGate(t, dir); out != "" {
		t.Fatalf("gate matched outside the data plane (false positive): %s", out)
	}
}

// TestCPDP1_GrepCleanWhenDirsAbsent asserts the gate passes vacuously
// when neither internal/router nor internal/dataplane exists -- which
// is the repo's state through most of v0.2 Beat 1 until the router
// ticket lands. Without this guarantee, the Makefile target would
// emit grep errors and fail builds during the pre-router window.
func TestCPDP1_GrepCleanWhenDirsAbsent(t *testing.T) {
	dir := t.TempDir()
	if out := runGate(t, dir); out != "" {
		t.Fatalf("gate matched against missing dirs (gate would falsely fire): %s", out)
	}
}
