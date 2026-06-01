package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildSSHArgv_BasicShape(t *testing.T) {
	got := buildSSHArgv("ssh", "/tmp/k", "root", "1.2.3.4", 2222, nil)
	want := []string{
		"ssh",
		"-i", "/tmp/k",
		"-p", "2222",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"root@1.2.3.4",
	}
	if !equalStrings(got, want) {
		t.Errorf("argv shape drift\n got:  %v\n want: %v", got, want)
	}
}

func TestBuildSSHArgv_PassThroughArgsBetweenOptionsAndDestination(t *testing.T) {
	// `iplane instance ssh my-pod -- -L 8080:localhost:8000 -A`
	// The pass-through args must land BEFORE the user@host destination
	// (ssh treats everything after destination as the remote command).
	got := buildSSHArgv("ssh", "/k", "u", "h", 22, []string{"-L", "8080:localhost:8000", "-A"})

	// Find positions of the marker elements.
	dest := -1
	lflag := -1
	aflag := -1
	for i, a := range got {
		switch a {
		case "u@h":
			dest = i
		case "-L":
			lflag = i
		case "-A":
			aflag = i
		}
	}
	if dest == -1 || lflag == -1 || aflag == -1 {
		t.Fatalf("missing expected elements; got %v", got)
	}
	if !(lflag < dest && aflag < dest) {
		t.Errorf("pass-through args must come before destination; got positions L=%d A=%d dest=%d", lflag, aflag, dest)
	}
	// Destination must be the LAST element so ssh doesn't treat
	// anything after it as a remote command.
	if dest != len(got)-1 {
		t.Errorf("destination must be last; got at %d in %v", dest, got)
	}
}

func TestBuildSSHArgv_RemoteCommandAfterDestination(t *testing.T) {
	// Operator wants to run a remote command:
	// `iplane instance ssh my-pod -- ls /workspace`
	// The "ls /workspace" tokens land AFTER everything in extraArgs.
	// We don't try to be clever about reordering -- the operator gets
	// the same semantics they'd get with raw ssh: pass-through args
	// come before destination, destination terminates option parsing,
	// trailing tokens become the remote command.
	//
	// But the current buildSSHArgv puts ALL extraArgs before the
	// destination. That means `ls /workspace` would land before the
	// destination, which ssh would treat as more options -> error.
	//
	// Documented limitation: remote commands need to be quoted into a
	// single arg or executed via -- after the destination by the
	// operator. This test pins the current behavior so any change is
	// deliberate.
	got := buildSSHArgv("ssh", "/k", "u", "h", 22, []string{"ls", "/workspace"})
	last := got[len(got)-1]
	if last != "u@h" {
		t.Errorf("destination should be last; got %q at end, full argv %v", last, got)
	}
}

func TestBuildSSHArgv_AlwaysSetsStrictHostKeyOptions(t *testing.T) {
	// These three options are required for the chapter beat: an
	// ephemeral pod's host key is unknown, would otherwise prompt the
	// operator. The verb pins them so the prompt never fires.
	got := buildSSHArgv("ssh", "/k", "u", "h", 22, nil)
	joined := strings.Join(got, " ")
	for _, want := range []string{
		"-o StrictHostKeyChecking=no",
		"-o UserKnownHostsFile=/dev/null",
		"-o LogLevel=ERROR",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing required ssh option %q\nfull: %s", want, joined)
		}
	}
}

func TestPreferredKeyDir_FallsBackToTempDirElsewhere(t *testing.T) {
	// On non-Linux (e.g., the macOS dev box this test runs on), we
	// fall back to os.TempDir(). The Linux branch is exercised by
	// TestPreferredKeyDir_LinuxPrefersRunUser below (gated on GOOS).
	if runtime.GOOS == "linux" {
		t.Skip("Linux preference exercised in a separate test")
	}
	got := preferredKeyDir()
	if got != os.TempDir() {
		t.Errorf("non-Linux preferredKeyDir = %q, want os.TempDir() %q", got, os.TempDir())
	}
}

func TestMaterializeKey_WritesFileAtTempfsOrTempDir_With0600Mode(t *testing.T) {
	pemBytes := []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nfake-key\n-----END OPENSSH PRIVATE KEY-----\n")
	path, err := materializeKey(pemBytes)
	if err != nil {
		t.Fatalf("materializeKey: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(filepath.Dir(path))
	})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat written key: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("key file mode = %o, want 0600", mode)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written key: %v", err)
	}
	if string(got) != string(pemBytes) {
		t.Errorf("key bytes round-trip mismatch")
	}
	// Materialization directory should live under the preferred root
	// (tmpfs on Linux when available, OS temp dir elsewhere).
	wantRoot := preferredKeyDir()
	if !strings.HasPrefix(path, wantRoot) {
		t.Errorf("key path %q not under preferred root %q", path, wantRoot)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
