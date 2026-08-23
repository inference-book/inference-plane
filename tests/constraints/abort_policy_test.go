package constraints

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The abort policy exists twice and must not drift.
//
// internal/provisioners/staging.go makes the judgement from the agent's
// reading; hack/measure-run.sh makes the same one from deploy-watch's,
// because the agent does not run on any deploy we have driven yet
// (agentPrelude needs both a stamped service URL and a fetchable binary).
//
// Two implementations of one policy is a real risk: someone tunes the Go
// thresholds after a bad run, the shell keeps the old ones, and the two
// disagree about when to spend an operator's money. Neither copy is
// obviously the stale one from inside the other.
//
// This is a repo invariant rather than a behaviour test, which is why it
// lives here next to the other constraint checks rather than in either
// package.
func TestAbortThresholdsMatchBetweenGoAndShell(t *testing.T) {
	root := repoRoot(t)
	goSrc := readFile(t, filepath.Join(root, "internal", "provisioners", "staging.go"))
	shSrc := readFile(t, filepath.Join(root, "hack", "measure-run.sh"))

	for _, c := range []struct {
		what         string
		goRe         string
		shRe         string
		whyItMatters string
	}{
		{
			what:         "minimum observation window",
			goRe:         `abortMinWindow\s*=\s*(\d+)\s*\*\s*time\.Second`,
			shRe:         `ABORT_MIN_WINDOW=(\d+)`,
			whyItMatters: "how long a download is watched before its rate is allowed to end a run",
		},
		{
			what:         "consecutive agreeing readings",
			goRe:         `abortConsecutive\s*=\s*(\d+)`,
			shRe:         `ABORT_CONSECUTIVE=(\d+)`,
			whyItMatters: "how many slow readings in a row are needed, since throughput is bursty",
		},
		{
			what:         "projection overshoot factor",
			goRe:         `abortSlack\s*=\s*([\d.]+)`,
			shRe:         `ABORT_SLACK=([\d.]+)`,
			whyItMatters: "how far past the deadline a projection must land before giving up",
		},
	} {
		goVal := capture(t, c.goRe, goSrc, "internal/provisioners/staging.go")
		shVal := capture(t, c.shRe, shSrc, "hack/measure-run.sh")
		if goVal != shVal {
			t.Errorf("%s disagrees: staging.go says %s, measure-run.sh says %s.\n"+
				"This sets %s. Change both together.", c.what, goVal, shVal, c.whyItMatters)
		}
	}
}

// Both copies must refuse to act on a rate of zero. A stalled download
// projects to infinite time, so treating zero as too-slow abandons every
// deploy that pauses between shards, and fires hardest on exactly the
// multi-shard checkpoints the abort exists to protect.
//
// Asserted as the presence of the guard rather than by running it, because
// the shell half cannot be unit-tested under this repo's conventions and a
// silent deletion is the failure worth catching.
func TestBothAbortsGuardAgainstAStalledDownload(t *testing.T) {
	root := repoRoot(t)
	for path, want := range map[string]*regexp.Regexp{
		filepath.Join("internal", "provisioners", "staging.go"): regexp.MustCompile(`rate <= 0`),
		filepath.Join("hack", "measure-run.sh"):                 regexp.MustCompile(`rate <= 0`),
	} {
		if src := readFile(t, filepath.Join(root, path)); !want.MatchString(src) {
			t.Errorf("%s no longer guards against a zero rate (%s). A stalled download "+
				"projects to infinity and would abort every deploy that paused.", path, want)
		}
	}
}

func capture(t *testing.T, pattern, src, where string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("could not find /%s/ in %s; the constant was renamed or removed, "+
			"and this guard cannot compare what it cannot find", pattern, where)
	}
	return strings.TrimSuffix(m[1], ".0")
}

// repoRoot walks up from the test's working directory to the module root,
// so the guard reads the real files rather than a fixture that could agree
// with itself while the shipped ones disagree.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find the module root above the test's working directory")
	return ""
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
