package common

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveIplane_BinFlagWins(t *testing.T) {
	t.Setenv("IPLANE_BIN", "/should/be/ignored")
	got, err := ResolveIplane("/explicit/iplane")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/explicit/iplane" {
		t.Fatalf("binFlag should win, got %q", got)
	}
}

func TestResolveIplane_EnvOverride(t *testing.T) {
	t.Setenv("IPLANE_BIN", "/from/env/iplane")
	got, err := ResolveIplane("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/from/env/iplane" {
		t.Fatalf("IPLANE_BIN should resolve, got %q", got)
	}
}

// TestResolveIplane_NotFound exercises the fail-fast path: no --bin, no
// IPLANE_BIN, no bin/iplane in the (faked) repo root, and an empty PATH so
// no global iplane is found. The demo assumes a prebuilt binary exists, so
// the absence is an error with a build hint, not a silent build.
func TestResolveIplane_NotFound(t *testing.T) {
	t.Setenv("IPLANE_BIN", "")
	t.Setenv("PATH", t.TempDir()) // empty dir => LookPath("iplane") fails
	// Only meaningful if this checkout has no bin/iplane; skip otherwise so
	// the test stays deterministic on a machine that ran `make build`.
	if root, ok := repoRoot(); ok {
		if _, err := os.Stat(filepath.Join(root, "bin", "iplane")); err == nil {
			t.Skip("bin/iplane present in this checkout; not-found path not reachable")
		}
	}
	if _, err := ResolveIplane(""); err == nil {
		t.Fatal("expected an error when no iplane can be resolved")
	}
}
