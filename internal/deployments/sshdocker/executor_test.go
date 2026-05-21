package sshdocker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
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
	if r.callsContaining("docker stop") != 1 || r.callsContaining("docker rm") != 1 {
		t.Errorf("expected 1 stop + 1 rm; got %d stop, %d rm", r.callsContaining("docker stop"), r.callsContaining("docker rm"))
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
