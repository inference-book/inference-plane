package common

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// ResolveIplane returns the path to the iplane binary the CLI-driving
// demos (02, 04) should exec. The demos assume a prebuilt binary already
// exists -- they do not build the control plane themselves (that would
// couple a demo to the build toolchain and, since examples/ is its own
// module that replaces the parent, drag the CLI's full transitive deps
// into examples/go.sum). Build it once with `make build` (-> bin/iplane)
// or `make install` (-> $PATH), then run the demo.
//
// Resolution order, first match wins:
//
//  1. binFlag        -- the --bin flag (explicit override)
//  2. $IPLANE_BIN    -- env override
//  3. <repo>/bin/iplane -- what `make build` produces; preferred over
//     PATH so the demo drives THIS checkout, not a stale global iplane
//  4. iplane on $PATH   -- what `make install` produces
//
// Returns an error with a build hint if none resolve.
func ResolveIplane(binFlag string) (string, error) {
	if binFlag != "" {
		return binFlag, nil
	}
	if env := os.Getenv("IPLANE_BIN"); env != "" {
		return env, nil
	}
	if root, ok := repoRoot(); ok {
		cand := filepath.Join(root, "bin", "iplane")
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand, nil
		}
	}
	if p, err := exec.LookPath("iplane"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("iplane binary not found; build it first with `make build` " +
		"(or `make install`), or pass --bin / set IPLANE_BIN")
}

// repoRoot resolves the inference-plane checkout root from this source
// file's location (examples/common/iplane.go -> ../.. is the root), so it
// works regardless of the operator's cwd.
func repoRoot() (string, bool) {
	_, src, _, ok := runtime.Caller(0)
	if !ok {
		return "", false
	}
	return filepath.Join(filepath.Dir(src), "..", ".."), true
}
