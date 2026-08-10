package sshdocker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/engineagent"
	"github.com/inference-book/inference-plane/internal/sshkeys"
)

func testKey(t *testing.T) *sshkeys.KeyPair {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &sshkeys.KeyPair{
		Operator: "default",
		Provider: "runpod",
		Public:   pub,
		Private:  priv,
		Comment:  "iplane-default-runpod-2026-05-20T15:30:00Z",
	}
}

func testDep() *provisionerv1.Deployment {
	return &provisionerv1.Deployment{
		Id:         "my-llama",
		InstanceId: "my-pod",
		Image:      "vllm/vllm-openai:0.7.0",
		Model:      "Qwen/Qwen2.5-7B-Instruct",
		EnginePort: 8000,
	}
}

func testInst() *provisionerv1.Instance {
	return &provisionerv1.Instance{
		Id:       "my-pod",
		Provider: "runpod",
		Ssh: &provisionerv1.SshTarget{
			Host: "1.2.3.4",
			Port: 22,
			User: "root",
		},
	}
}

// newExecutorWithFake returns an Executor whose dial returns the
// supplied fakeRunner. Health timing is shortened so tests do not
// wait the full 2-minute production default.
func newExecutorWithFake(r *fakeRunner) *Executor {
	return NewExecutor(
		WithDial(func(ctx context.Context, _ *provisionerv1.Instance, _ *sshkeys.KeyPair) (RemoteRunner, error) {
			return r, nil
		}),
		WithHealthPoll(10*time.Millisecond, 200*time.Millisecond),
	)
}

// collect captures every emitted StateUpdate so tests can assert
// on the sequence.
type collector struct {
	updates []StateUpdate
}

func (c *collector) emit(u StateUpdate) { c.updates = append(c.updates, u) }

func (c *collector) lastState() provisionerv1.DeploymentState {
	if len(c.updates) == 0 {
		return provisionerv1.DeploymentState_DEPLOYMENT_STATE_UNSPECIFIED
	}
	return c.updates[len(c.updates)-1].State
}

func (c *collector) sawState(s provisionerv1.DeploymentState) bool {
	for _, u := range c.updates {
		if u.State == s {
			return true
		}
	}
	return false
}

func TestDeploy_FreshContainer_Pulls_Runs_GoesToRUNNING(t *testing.T) {
	r := &fakeRunner{}
	r.on("docker inspect", fakeResp{
		stderr:   "No such object: iplane-deployment-my-llama",
		exitCode: 1,
	})
	r.on("docker pull", fakeResp{stdout: "Pulled.\n"})
	r.on("docker run", fakeResp{stdout: "abc1234\n"})
	r.on("curl", fakeResp{stdout: "200"})

	c := &collector{}
	exec := newExecutorWithFake(r)
	if err := exec.Deploy(context.Background(), testDep(), testInst(), testKey(t), c.emit); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("final state = %v, want RUNNING", c.lastState())
	}
	if r.callsContaining("docker pull") != 1 {
		t.Errorf("expected 1 docker pull call, got %d", r.callsContaining("docker pull"))
	}
	if r.callsContaining("docker run") != 1 {
		t.Errorf("expected 1 docker run call, got %d", r.callsContaining("docker run"))
	}
	// Should NOT stop/remove on a fresh deploy.
	if r.callsContaining("docker stop") > 0 || r.callsContaining("docker rm") > 0 {
		t.Errorf("fresh deploy should not stop/rm; got %d stop, %d rm", r.callsContaining("docker stop"), r.callsContaining("docker rm"))
	}
}

// On a released build with an identity stamped, the sidecar starts beside
// the engine. Two `docker run` calls, and the engine's own invocation is
// untouched: this path never overrides the engine's entrypoint the way the
// image-native wrapper has to.
func TestDeploy_StartsTheAgentSidecar(t *testing.T) {
	releaseBuild(t)
	r := &fakeRunner{}
	r.on("docker inspect", fakeResp{stderr: "No such object", exitCode: 1})
	r.on("docker pull", fakeResp{stdout: "Pulled.\n"})
	r.on("docker run", fakeResp{stdout: "abc1234\n"})
	r.on("curl", fakeResp{stdout: "200"})

	dep := testDep()
	dep.Env = map[string]string{
		engineagent.EnvEngineID:   "my-llama",
		engineagent.EnvServiceURL: "https://cp.example.com",
	}

	c := &collector{}
	exec := newExecutorWithFake(r)
	if err := exec.Deploy(context.Background(), dep, testInst(), testKey(t), c.emit); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("final state = %v, want RUNNING", c.lastState())
	}
	if got := r.callsContaining("docker run"); got != 2 {
		t.Errorf("docker run count = %d, want 2 (engine + agent)", got)
	}
	all := strings.Join(r.calls, "\n")
	if !strings.Contains(all, "--network 'container:iplane-deployment-my-llama'") {
		t.Errorf("agent did not join the engine's netns:\n%s", all)
	}
	// The engine's own run must not have gained an entrypoint override.
	// Matched on --name, not a bare name match: the agent's command also
	// mentions the engine's container inside --network container:<engine>.
	for _, call := range r.calls {
		if strings.Contains(call, "docker run") &&
			strings.Contains(call, "--name "+shellEscape(ContainerName("my-llama"))) &&
			strings.Contains(call, "--entrypoint") {
			t.Errorf("engine run gained an entrypoint override on the sidecar path: %s", call)
		}
	}
}

// A serving engine must not be failed because its fleet-view agent would
// not start. Liveness still comes from the /health poller.
func TestDeploy_AgentFailureDoesNotFailTheDeploy(t *testing.T) {
	releaseBuild(t)
	r := &fakeRunner{}
	r.on("docker inspect", fakeResp{stderr: "No such object", exitCode: 1})
	r.on("docker pull", fakeResp{stdout: "Pulled.\n"})
	r.on("docker run", fakeResp{stdout: "abc1234\n"})
	r.on("curl", fakeResp{stdout: "200"})
	// The agent's run is the one that carries --network; fail only that.
	r.on("--network 'container:", fakeResp{stderr: "no such image", exitCode: 1})

	dep := testDep()
	dep.Env = map[string]string{
		engineagent.EnvEngineID:   "my-llama",
		engineagent.EnvServiceURL: "https://cp.example.com",
	}

	c := &collector{}
	exec := newExecutorWithFake(r)
	if err := exec.Deploy(context.Background(), dep, testInst(), testKey(t), c.emit); err != nil {
		t.Fatalf("Deploy failed because the agent did not start: %v", err)
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("final state = %v, want RUNNING", c.lastState())
	}
	// The operator is told, rather than the failure being silent.
	var told bool
	for _, u := range c.updates {
		if strings.Contains(u.ProgressMessage, "registration agent not started") {
			told = true
		}
	}
	if !told {
		t.Error("agent failure was swallowed without telling the operator")
	}
}

func TestDeploy_MatchingContainer_NoPullNoRun(t *testing.T) {
	r := &fakeRunner{}
	r.on("docker inspect", fakeResp{
		stdout: `[{
			"Id": "abc",
			"Config": {"Image": "vllm/vllm-openai:0.7.0", "Labels": {"iplane.model": "Qwen/Qwen2.5-7B-Instruct"}},
			"State": {"Running": true, "Status": "running", "ExitCode": 0}
		}]`,
	})
	r.on("curl", fakeResp{stdout: "200"})

	c := &collector{}
	exec := newExecutorWithFake(r)
	if err := exec.Deploy(context.Background(), testDep(), testInst(), testKey(t), c.emit); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if r.callsContaining("docker pull") > 0 {
		t.Errorf("matching container should not trigger pull; got %d calls", r.callsContaining("docker pull"))
	}
	if r.callsContaining("docker run") > 0 {
		t.Errorf("matching container should not trigger run; got %d calls", r.callsContaining("docker run"))
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("final state = %v, want RUNNING", c.lastState())
	}
}

func TestDeploy_DriftedContainer_StopsAndReruns(t *testing.T) {
	r := &fakeRunner{}
	r.on("docker inspect", fakeResp{
		stdout: `[{
			"Id": "abc",
			"Config": {"Image": "vllm/vllm-openai:0.6.0", "Labels": {"iplane.model": "Qwen/Qwen2.5-7B-Instruct"}},
			"State": {"Running": true, "Status": "running", "ExitCode": 0}
		}]`,
	})
	r.on("docker stop", fakeResp{stdout: "iplane-deployment-my-llama\n"})
	r.on("docker rm", fakeResp{stdout: "iplane-deployment-my-llama\n"})
	r.on("docker pull", fakeResp{stdout: "Pulled.\n"})
	r.on("docker run", fakeResp{stdout: "xyz789\n"})
	r.on("curl", fakeResp{stdout: "200"})

	c := &collector{}
	exec := newExecutorWithFake(r)
	if err := exec.Deploy(context.Background(), testDep(), testInst(), testKey(t), c.emit); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if r.callsContaining("docker stop") != 1 {
		t.Errorf("expected 1 stop, got %d", r.callsContaining("docker stop"))
	}
	if r.callsContaining("docker rm") != 1 {
		t.Errorf("expected 1 rm, got %d", r.callsContaining("docker rm"))
	}
	if r.callsContaining("docker pull") != 1 {
		t.Errorf("expected 1 pull after drift, got %d", r.callsContaining("docker pull"))
	}
	if r.callsContaining("docker run") != 1 {
		t.Errorf("expected 1 run after drift, got %d", r.callsContaining("docker run"))
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("final state = %v, want RUNNING", c.lastState())
	}
}

func TestDeploy_HealthTimeout_GoesToFAILED(t *testing.T) {
	r := &fakeRunner{}
	r.on("docker inspect", fakeResp{
		stderr:   "No such object: iplane-deployment-my-llama",
		exitCode: 1,
	})
	r.on("docker pull", fakeResp{stdout: "Pulled.\n"})
	r.on("docker run", fakeResp{stdout: "abc1234\n"})
	// curl always returns 503 -- health never ready
	r.on("curl", fakeResp{stdout: "503"})

	c := &collector{}
	exec := newExecutorWithFake(r)
	err := exec.Deploy(context.Background(), testDep(), testInst(), testKey(t), c.emit)
	if err == nil {
		t.Fatal("expected error when health never returns 2xx")
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
		t.Errorf("final state = %v, want FAILED", c.lastState())
	}
	last := c.updates[len(c.updates)-1]
	if !strings.Contains(last.FailureReason, "health") {
		t.Errorf("failure reason should mention health; got %q", last.FailureReason)
	}
}

func TestDeploy_PullFailure_GoesToFAILED_NoRun(t *testing.T) {
	r := &fakeRunner{}
	r.on("docker inspect", fakeResp{stderr: "No such object", exitCode: 1})
	r.on("docker pull", fakeResp{stderr: "pull access denied", exitCode: 1})

	c := &collector{}
	exec := newExecutorWithFake(r)
	err := exec.Deploy(context.Background(), testDep(), testInst(), testKey(t), c.emit)
	if err == nil {
		t.Fatal("expected error on docker pull failure")
	}
	if r.callsContaining("docker run") > 0 {
		t.Errorf("docker run should not fire after pull failure; got %d calls", r.callsContaining("docker run"))
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
		t.Errorf("final state = %v, want FAILED", c.lastState())
	}
}

func TestDeploy_DockerInstallFailure_GoesToFAILED_NoPull(t *testing.T) {
	// command -v docker fails AND apt-get install fails -> the
	// executor must fail BEFORE pulling. Catches the runpod/pytorch
	// regression where the base image has no docker and we'd crash
	// at the first inspect with `command not found`.
	r := &fakeRunner{}
	r.on("command -v docker", fakeResp{exitCode: 1})
	r.on("apt-get install", fakeResp{
		stderr:   "bash: line 1: apt-get: command not found",
		exitCode: 127,
	})

	c := &collector{}
	exec := newExecutorWithFake(r)
	err := exec.Deploy(context.Background(), testDep(), testInst(), testKey(t), c.emit)
	if err == nil {
		t.Fatal("expected error when docker install fails")
	}
	// No docker.* command should have fired after the failed install.
	if r.callsContaining("docker inspect") > 0 {
		t.Errorf("docker inspect should not fire after install failure; got %d", r.callsContaining("docker inspect"))
	}
	if r.callsContaining("docker pull") > 0 {
		t.Errorf("docker pull should not fire after install failure; got %d", r.callsContaining("docker pull"))
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
		t.Errorf("final state = %v, want FAILED", c.lastState())
	}
	last := c.updates[len(c.updates)-1]
	if !strings.Contains(last.FailureReason, "--base-image") {
		t.Errorf("failure reason should point at --base-image; got %q", last.FailureReason)
	}
}

func TestDeploy_DockerNeedsInstall_StillProceeds(t *testing.T) {
	// command -v docker fails BUT apt-get install succeeds. The
	// executor should install, then continue to the normal
	// inspect/pull/run/health flow. Verifies the install isn't a
	// dead-end -- once installed, the deploy progresses.
	r := &fakeRunner{}
	// First check fails.
	r.on("command -v docker", fakeResp{exitCode: 1})
	// Apt install succeeds (default-success branch matched by substring).
	r.on("apt-get install", fakeResp{exitCode: 0})
	// Then the standard fresh-container flow proceeds.
	r.on("docker inspect", fakeResp{
		stderr:   "No such object: iplane-deployment-my-llama",
		exitCode: 1,
	})
	r.on("docker pull", fakeResp{stdout: "Pulled.\n"})
	r.on("docker run", fakeResp{stdout: "abc1234\n"})
	r.on("curl", fakeResp{stdout: "200"})

	c := &collector{}
	exec := newExecutorWithFake(r)
	if err := exec.Deploy(context.Background(), testDep(), testInst(), testKey(t), c.emit); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("final state = %v, want RUNNING (install -> proceed)", c.lastState())
	}
	if r.callsContaining("apt-get install -y docker.io") != 1 {
		t.Errorf("expected exactly 1 apt install call; got %d", r.callsContaining("apt-get install -y docker.io"))
	}
}

func TestDeploy_SSHConnectFailure_GoesToFAILED(t *testing.T) {
	exec := NewExecutor(
		WithDial(func(ctx context.Context, _ *provisionerv1.Instance, _ *sshkeys.KeyPair) (RemoteRunner, error) {
			return nil, &dialErr{msg: "connection refused"}
		}),
	)
	c := &collector{}
	err := exec.Deploy(context.Background(), testDep(), testInst(), testKey(t), c.emit)
	if err == nil {
		t.Fatal("expected error on dial failure")
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
		t.Errorf("final state = %v, want FAILED", c.lastState())
	}
	if !c.sawState(provisionerv1.DeploymentState_DEPLOYMENT_STATE_STARTING) {
		t.Error("should have emitted STARTING before failing")
	}
}

type dialErr struct{ msg string }

func (d *dialErr) Error() string { return d.msg }

func TestDestroy_HappyPath_GoesToTERMINATED(t *testing.T) {
	r := &fakeRunner{}
	r.on("docker stop", fakeResp{stdout: "ok\n"})
	r.on("docker rm", fakeResp{stdout: "ok\n"})

	c := &collector{}
	exec := newExecutorWithFake(r)
	if err := exec.Destroy(context.Background(), testDep(), testInst(), testKey(t), c.emit); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED {
		t.Errorf("final state = %v, want TERMINATED", c.lastState())
	}
	// Two containers now: the engine and its registration agent sidecar.
	// Asserted per-container rather than as a total, so a regression that
	// tore down the agent twice and the engine never would still fail.
	if got := r.callsContaining("docker stop " + shellEscape(ContainerName("my-llama"))); got != 1 {
		t.Errorf("engine stop count = %d, want 1", got)
	}
	if got := r.callsContaining("docker rm " + shellEscape(ContainerName("my-llama"))); got != 1 {
		t.Errorf("engine rm count = %d, want 1", got)
	}
	if got := r.callsContaining("docker stop " + shellEscape(AgentContainerName("my-llama"))); got != 1 {
		t.Errorf("agent stop count = %d, want 1", got)
	}
	if got := r.callsContaining("docker rm " + shellEscape(AgentContainerName("my-llama"))); got != 1 {
		t.Errorf("agent rm count = %d, want 1", got)
	}
	if r.callsContaining("docker stop") != 2 || r.callsContaining("docker rm") != 2 {
		t.Errorf("expected exactly 2 stop + 2 rm; got %d stop, %d rm",
			r.callsContaining("docker stop"), r.callsContaining("docker rm"))
	}
}

// The agent shares the engine's network namespace, so removing the engine
// first would strand a container renewing a lease for something that no
// longer exists.
func TestDestroy_StopsAgentBeforeEngine(t *testing.T) {
	r := &fakeRunner{}
	r.on("docker stop", fakeResp{stdout: "ok\n"})
	r.on("docker rm", fakeResp{stdout: "ok\n"})

	c := &collector{}
	exec := newExecutorWithFake(r)
	if err := exec.Destroy(context.Background(), testDep(), testInst(), testKey(t), c.emit); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	agentAt, engineAt := -1, -1
	for i, call := range r.calls {
		if agentAt == -1 && strings.Contains(call, AgentContainerName("my-llama")) {
			agentAt = i
		}
		if engineAt == -1 && strings.Contains(call, "docker stop "+shellEscape(ContainerName("my-llama"))) {
			engineAt = i
		}
	}
	if agentAt == -1 || engineAt == -1 {
		t.Fatalf("missing teardown calls: agent=%d engine=%d\n%v", agentAt, engineAt, r.calls)
	}
	if agentAt > engineAt {
		t.Errorf("agent torn down after the engine (agent=%d, engine=%d)", agentAt, engineAt)
	}
}

func TestDestroy_AlreadyGone_StillTERMINATED(t *testing.T) {
	r := &fakeRunner{}
	r.on("docker stop", fakeResp{stderr: "No such container", exitCode: 1})
	r.on("docker rm", fakeResp{stderr: "No such container", exitCode: 1})

	c := &collector{}
	exec := newExecutorWithFake(r)
	if err := exec.Destroy(context.Background(), testDep(), testInst(), testKey(t), c.emit); err != nil {
		t.Fatalf("Destroy should be idempotent on No-such-container; got %v", err)
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED {
		t.Errorf("final state = %v, want TERMINATED", c.lastState())
	}
}

func TestBindMounts_DropsVolumeIDOnlyMounts(t *testing.T) {
	// The sshdocker path can only honor host-path binds. A volume-id-only
	// mount (the RunPod image-native shape) has no host directory to bind
	// and must be dropped, not turned into a broken `-v`.
	in := []*provisionerv1.VolumeMount{
		{VolumeId: "vol-1", MountPath: "/models"},       // image-native only -> drop
		{HostPath: "/mnt/models", MountPath: "/models"}, // bindable -> keep
		{HostPath: "/mnt/x"},                            // no mount path -> drop
	}
	got := bindMounts(in)
	if len(got) != 1 {
		t.Fatalf("bindMounts kept %d, want 1: %+v", len(got), got)
	}
	if got[0].HostPath != "/mnt/models" || got[0].MountPath != "/models" {
		t.Errorf("kept wrong mount: %+v", got[0])
	}
}
