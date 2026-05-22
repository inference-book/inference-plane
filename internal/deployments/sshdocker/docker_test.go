package sshdocker

import (
	"context"
	"strings"
	"testing"
)

// fakeRunner records every command and serves canned responses. Tests
// register response handlers per command (by substring match); the
// most-recently-registered handler wins. Default behavior: success
// with empty stdout/stderr, exit 0.
type fakeRunner struct {
	calls    []string
	handlers []handler
}

type handler struct {
	match func(cmd string) bool
	resp  fakeResp
}

type fakeResp struct {
	stdout   string
	stderr   string
	exitCode int
	err      error
}

func (f *fakeRunner) Run(ctx context.Context, cmd string) ([]byte, []byte, int, error) {
	f.calls = append(f.calls, cmd)
	for i := len(f.handlers) - 1; i >= 0; i-- {
		h := f.handlers[i]
		if h.match(cmd) {
			return []byte(h.resp.stdout), []byte(h.resp.stderr), h.resp.exitCode, h.resp.err
		}
	}
	return nil, nil, 0, nil
}

func (f *fakeRunner) Close() error { return nil }

// on registers a handler. last-registered wins (LIFO match), so
// tests can override broader handlers with narrower ones.
func (f *fakeRunner) on(match string, resp fakeResp) {
	f.handlers = append(f.handlers, handler{
		match: func(cmd string) bool { return strings.Contains(cmd, match) },
		resp:  resp,
	})
}

// callsContaining counts how many recorded commands contain a substring.
func (f *fakeRunner) callsContaining(s string) int {
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, s) {
			n++
		}
	}
	return n
}

func TestContainerName_StableShape(t *testing.T) {
	if got := ContainerName("my-llama"); got != "iplane-deployment-my-llama" {
		t.Errorf("ContainerName = %q, want iplane-deployment-my-llama", got)
	}
}

func TestInspect_Missing_ReturnsNotExists(t *testing.T) {
	r := &fakeRunner{}
	r.on("docker inspect", fakeResp{
		stderr:   "Error response from daemon: No such object: iplane-deployment-foo",
		exitCode: 1,
	})
	d := NewDocker(r)
	state, err := d.Inspect(context.Background(), "iplane-deployment-foo")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if state.Exists {
		t.Error("Exists should be false when docker reports No such object")
	}
}

func TestInspect_Running_ReturnsState(t *testing.T) {
	r := &fakeRunner{}
	r.on("docker inspect", fakeResp{
		stdout: `[{
			"Id": "abc1234",
			"Config": {
				"Image": "vllm/vllm-openai:0.7.0",
				"Labels": {"iplane.model": "Qwen/Qwen2.5-7B-Instruct"}
			},
			"State": {
				"Running": true,
				"Status": "running",
				"ExitCode": 0
			}
		}]`,
		exitCode: 0,
	})
	d := NewDocker(r)
	state, err := d.Inspect(context.Background(), "iplane-deployment-foo")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !state.Exists || !state.Running {
		t.Errorf("expected exists=true running=true; got %+v", state)
	}
	if state.Image != "vllm/vllm-openai:0.7.0" {
		t.Errorf("Image = %q", state.Image)
	}
	if state.Model != "Qwen/Qwen2.5-7B-Instruct" {
		t.Errorf("Model = %q", state.Model)
	}
}

func TestInspect_Stopped_ReturnsRunningFalse(t *testing.T) {
	r := &fakeRunner{}
	r.on("docker inspect", fakeResp{
		stdout: `[{
			"Id": "abc1234",
			"Config": {"Image": "vllm:0.7", "Labels": {}},
			"State": {"Running": false, "Status": "exited", "ExitCode": 137}
		}]`,
	})
	d := NewDocker(r)
	state, err := d.Inspect(context.Background(), "iplane-deployment-foo")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !state.Exists || state.Running {
		t.Errorf("expected exists=true running=false; got %+v", state)
	}
	if state.ExitCode != 137 {
		t.Errorf("ExitCode = %d, want 137", state.ExitCode)
	}
}

func TestContainerState_Matches_ExactImageAndModel(t *testing.T) {
	state := &ContainerState{
		Exists: true, Running: true,
		Image: "vllm/vllm-openai:0.7.0",
		Model: "Qwen/Qwen2.5-7B-Instruct",
	}
	if !state.Matches("vllm/vllm-openai:0.7.0", "Qwen/Qwen2.5-7B-Instruct") {
		t.Error("Matches should be true on exact image + model")
	}
	if state.Matches("vllm/vllm-openai:0.8.0", "Qwen/Qwen2.5-7B-Instruct") {
		t.Error("Matches should be false on image drift")
	}
	if state.Matches("vllm/vllm-openai:0.7.0", "Llama-3-8B") {
		t.Error("Matches should be false on model drift")
	}
	notRunning := *state
	notRunning.Running = false
	if notRunning.Matches("vllm/vllm-openai:0.7.0", "Qwen/Qwen2.5-7B-Instruct") {
		t.Error("Matches should be false when container is not running")
	}
}

func TestPull_Success(t *testing.T) {
	r := &fakeRunner{}
	r.on("docker pull", fakeResp{stdout: "Pulled.\n"})
	d := NewDocker(r)
	if err := d.Pull(context.Background(), "vllm/vllm-openai:0.7.0"); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if r.callsContaining("docker pull 'vllm/vllm-openai:0.7.0'") != 1 {
		t.Errorf("expected exactly one docker pull call with the shell-escaped image; got calls %v", r.calls)
	}
}

func TestPull_NonZeroExit_ReturnsError(t *testing.T) {
	r := &fakeRunner{}
	r.on("docker pull", fakeResp{
		stderr:   "Error response from daemon: pull access denied",
		exitCode: 1,
	})
	d := NewDocker(r)
	err := d.Pull(context.Background(), "private/image:0.1")
	if err == nil {
		t.Fatal("expected error on non-zero exit")
	}
	if !strings.Contains(err.Error(), "pull access denied") {
		t.Errorf("error should propagate the remote stderr; got %q", err)
	}
}

func TestRun_BuildsExpectedCommand(t *testing.T) {
	r := &fakeRunner{}
	r.on("docker run", fakeResp{stdout: "abc1234\n"})
	d := NewDocker(r)
	id, err := d.Run(context.Background(), RunSpec{
		Name:       "iplane-deployment-foo",
		Image:      "vllm/vllm-openai:0.7.0",
		Model:      "Qwen/Qwen2.5-7B-Instruct",
		EngineArgs: []string{"--gpu-memory-utilization", "0.9"},
		Env:        map[string]string{"HF_TOKEN": "tok_x"},
		Port:       8000,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if id != "abc1234" {
		t.Errorf("container id = %q, want abc1234", id)
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected 1 call, got %d: %v", len(r.calls), r.calls)
	}
	cmd := r.calls[0]
	wantSubs := []string{
		"docker run -d",
		"--name 'iplane-deployment-foo'",
		"--gpus all",
		"--label 'iplane.deployment=iplane-deployment-foo'",
		"--label 'iplane.model=Qwen/Qwen2.5-7B-Instruct'",
		"-p 8000:8000",
		"-e 'HF_TOKEN=tok_x'",
		"'vllm/vllm-openai:0.7.0'",
		"--model 'Qwen/Qwen2.5-7B-Instruct'",
		"'--gpu-memory-utilization'",
		"'0.9'",
	}
	for _, sub := range wantSubs {
		if !strings.Contains(cmd, sub) {
			t.Errorf("docker run command missing %q\nfull cmd: %s", sub, cmd)
		}
	}
}

func TestStop_Idempotent_OnNoSuchContainer(t *testing.T) {
	r := &fakeRunner{}
	r.on("docker stop", fakeResp{
		stderr:   "Error response from daemon: No such container: foo",
		exitCode: 1,
	})
	d := NewDocker(r)
	if err := d.Stop(context.Background(), "foo"); err != nil {
		t.Errorf("Stop should be idempotent on No-such-container; got %v", err)
	}
}

func TestRemove_Idempotent_OnNoSuchContainer(t *testing.T) {
	r := &fakeRunner{}
	r.on("docker rm", fakeResp{
		stderr:   "Error response from daemon: No such container: foo",
		exitCode: 1,
	})
	d := NewDocker(r)
	if err := d.Remove(context.Background(), "foo"); err != nil {
		t.Errorf("Remove should be idempotent on No-such-container; got %v", err)
	}
}

func TestHealth_2xx_ReturnsTrue(t *testing.T) {
	r := &fakeRunner{}
	r.on("curl", fakeResp{stdout: "200"})
	d := NewDocker(r)
	ok, err := d.Health(context.Background(), 8000)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !ok {
		t.Error("Health should report ready on 200")
	}
}

func TestHealth_503_ReturnsFalse(t *testing.T) {
	r := &fakeRunner{}
	r.on("curl", fakeResp{stdout: "503"})
	d := NewDocker(r)
	ok, err := d.Health(context.Background(), 8000)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if ok {
		t.Error("Health should report not-ready on 503")
	}
}

func TestEnsureInstalled_AlreadyPresent_FastPath(t *testing.T) {
	// `command -v docker` returns 0 -> docker is on the path. The
	// helper must short-circuit; apt-get should never be invoked.
	r := &fakeRunner{}
	r.on("command -v docker", fakeResp{stdout: "/usr/bin/docker\n", exitCode: 0})

	d := NewDocker(r)
	if err := d.EnsureInstalled(context.Background()); err != nil {
		t.Fatalf("EnsureInstalled: %v", err)
	}
	if got := r.callsContaining("apt-get"); got != 0 {
		t.Errorf("apt-get should not run when docker is present; got %d calls", got)
	}
	if got := r.callsContaining("command -v docker"); got != 1 {
		t.Errorf("expected 1 `command -v docker` call; got %d", got)
	}
}

func TestEnsureInstalled_InstallsViaApt(t *testing.T) {
	// `command -v docker` returns non-zero -> helper proceeds to
	// apt-install. The install command must include apt-get update,
	// docker.io install, daemon start, and a docker info verification.
	r := &fakeRunner{}
	r.on("command -v docker", fakeResp{exitCode: 1})
	// The single install line is matched by any of its substrings.
	r.on("apt-get install -y docker.io", fakeResp{exitCode: 0})

	d := NewDocker(r)
	if err := d.EnsureInstalled(context.Background()); err != nil {
		t.Fatalf("EnsureInstalled: %v", err)
	}

	// Verify the install command shape -- catches regressions where
	// any of the four required steps is dropped.
	wantSubstrings := []string{
		"apt-get update",
		"apt-get install -y docker.io",
		"systemctl start docker",
		"service docker start",
		"docker info",
	}
	for _, want := range wantSubstrings {
		if got := r.callsContaining(want); got == 0 {
			t.Errorf("install command missing %q; got calls:\n  %s", want, strings.Join(r.calls, "\n  "))
		}
	}
}

func TestEnsureInstalled_AptFailure_ActionableError(t *testing.T) {
	// command -v fails AND apt-get install fails (e.g., alpine-based
	// image with no apt). The error must name the most likely cause
	// and point operators at --base-image rather than leaving them
	// with the raw exit code.
	r := &fakeRunner{}
	r.on("command -v docker", fakeResp{exitCode: 1})
	r.on("apt-get install", fakeResp{
		stderr:   "bash: line 1: apt-get: command not found",
		exitCode: 127,
	})

	d := NewDocker(r)
	err := d.EnsureInstalled(context.Background())
	if err == nil {
		t.Fatal("EnsureInstalled should error when apt install fails")
	}
	for _, want := range []string{"--base-image", "doesn't include docker"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestShellEscape(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "''"},
		{"simple", "'simple'"},
		{"with spaces", "'with spaces'"},
		{"o'reilly", `'o'\''reilly'`},
		{"$DOLLAR `tick`", "'$DOLLAR `tick`'"},
	}
	for _, c := range cases {
		if got := shellEscape(c.in); got != c.want {
			t.Errorf("shellEscape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
