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

// Replaces a characterization test that asserted the destination was always
// last, which pinned the defect rather than the contract: it meant the
// `-- ls /workspace` form in this command's own help could never work. That
// test's comment called it a "documented limitation" and asked that any
// change be deliberate. This is that change, made deliberately.
//
// The behaviour it protected (forwarding flags reaching ssh before the
// destination) is asserted by the two tests below it and is unchanged.

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

// The form the command's own help documents. It used to fail with "Could not
// resolve hostname ls", because every pass-through word went before the
// destination and ssh read the command's first word as the machine.
func TestBuildSSHArgvPutsARemoteCommandAfterTheDestination(t *testing.T) {
	argv := buildSSHArgv("ssh", "/k", "root", "h", 22, []string{"ls", "/workspace"})
	dest := indexOf(argv, "root@h")
	if dest < 0 {
		t.Fatalf("no destination in %v", argv)
	}
	if got := argv[len(argv)-2:]; got[0] != "ls" || got[1] != "/workspace" {
		t.Fatalf("command is %v, want it last", got)
	}
	if indexOf(argv, "ls") < dest {
		t.Fatalf("command precedes the destination: %v", argv)
	}
}

// Forwarding flags must still precede it, which is the ordering the old code
// was written for and must not be regressed.
func TestBuildSSHArgvKeepsOptionsBeforeTheDestination(t *testing.T) {
	argv := buildSSHArgv("ssh", "/k", "root", "h", 22, []string{"-A", "-L", "8080:localhost:8000"})
	dest := indexOf(argv, "root@h")
	for _, opt := range []string{"-A", "-L", "8080:localhost:8000"} {
		if i := indexOf(argv, opt); i < 0 || i > dest {
			t.Fatalf("%q is not before the destination: %v", opt, argv)
		}
	}
	if argv[len(argv)-1] != "root@h" {
		t.Fatalf("argv does not end at the destination: %v", argv)
	}
}

// A value-taking flag carries its value across with it, or ssh reads the
// value as the host.
func TestBuildSSHArgvMixesOptionsAndACommand(t *testing.T) {
	argv := buildSSHArgv("ssh", "/k", "root", "h", 22,
		[]string{"-o", "BatchMode=yes", "-L", "9:localhost:9", "du", "-sb", "/root"})
	dest := indexOf(argv, "root@h")
	for _, before := range []string{"-o", "BatchMode=yes", "-L", "9:localhost:9"} {
		if i := indexOf(argv, before); i < 0 || i > dest {
			t.Fatalf("%q should precede the destination: %v", before, argv)
		}
	}
	if got := argv[dest+1:]; len(got) != 3 || got[0] != "du" {
		t.Fatalf("command after the destination is %v, want [du -sb /root]", got)
	}
}

func TestSplitSSHArgs(t *testing.T) {
	cases := []struct {
		in      []string
		opts    []string
		command []string
		whatFor string
	}{
		{nil, nil, nil, "nothing"},
		{[]string{"-A"}, []string{"-A"}, nil, "a bare flag"},
		{[]string{"ls"}, nil, []string{"ls"}, "a bare command"},
		{[]string{"-L", "8080:x", "ls"}, []string{"-L", "8080:x"}, []string{"ls"}, "value flag then command"},
		{[]string{"-o", "-A", "ls"}, []string{"-o", "-A"}, []string{"ls"}, "a flag is not swallowed as a value"},
		{[]string{"--", "ls", "-la"}, nil, []string{"ls", "-la"}, "an explicit separator"},
		{[]string{"-p2222", "uptime"}, []string{"-p2222"}, []string{"uptime"}, "a combined flag"},
	}
	for _, c := range cases {
		opts, command := splitSSHArgs(c.in)
		if !equalStrings(opts, c.opts) || !equalStrings(command, c.command) {
			t.Fatalf("%s: splitSSHArgs(%v) = (%v, %v), want (%v, %v)",
				c.whatFor, c.in, opts, command, c.opts, c.command)
		}
	}
}

func indexOf(hay []string, needle string) int {
	for i, s := range hay {
		if s == needle {
			return i
		}
	}
	return -1
}
