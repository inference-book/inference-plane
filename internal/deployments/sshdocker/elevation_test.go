package sshdocker

import (
	"context"
	"strings"
	"testing"
)

// denyDockerSocket makes bare docker commands fail the way Lambda's box
// does: the binary is installed, the daemon is up, and the SSH user is not
// in the docker group.
func denyDockerSocket(r *fakeRunner) {
	r.on("docker info", fakeResp{
		exitCode: 1,
		stderr:   "permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock",
	})
}

func allowSudo(r *fakeRunner) {
	r.on("sudo -n docker info", fakeResp{exitCode: 0})
}

// The executor was written against providers whose SSH user is root. Lambda
// logs in as `ubuntu`, whose groups are `ubuntu, users, admin` and not
// `docker`, so every command came back "permission denied while trying to
// connect to the Docker daemon socket". Found on a rented A10 (#427).
func TestDockerCommandsUseSudoWhenTheSocketIsDenied(t *testing.T) {
	r := &fakeRunner{}
	denyDockerSocket(r)
	allowSudo(r)
	r.on("docker inspect", fakeResp{exitCode: 1, stderr: "No such object: x"})

	d := NewDocker(r)
	if _, err := d.Inspect(context.Background(), "x"); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	cmd := r.lastCallContaining(t, "docker inspect")
	if !strings.HasPrefix(cmd, "sudo docker ") {
		t.Errorf("inspect ran as %q, want it elevated", cmd)
	}
}

// A root host must not pay for sudo it does not need.
func TestDockerCommandsStayBareWhenTheSocketIsReachable(t *testing.T) {
	r := &fakeRunner{}
	r.on("docker inspect", fakeResp{exitCode: 1, stderr: "No such object: x"})

	d := NewDocker(r)
	if _, err := d.Inspect(context.Background(), "x"); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	cmd := r.lastCallContaining(t, "docker inspect")
	if strings.Contains(cmd, "sudo") {
		t.Errorf("inspect ran as %q, want it bare on a host that does not need sudo", cmd)
	}
}

// The probe costs a round trip over SSH and the answer cannot change
// mid-deploy, so it happens once however many commands follow.
func TestElevationIsProbedOnce(t *testing.T) {
	r := &fakeRunner{}
	denyDockerSocket(r)
	allowSudo(r)
	r.on("docker inspect", fakeResp{exitCode: 1, stderr: "No such object: x"})

	d := NewDocker(r)
	for range 3 {
		if _, err := d.Inspect(context.Background(), "x"); err != nil {
			t.Fatalf("Inspect: %v", err)
		}
	}
	if got := r.callsContaining("sudo -n docker info"); got != 1 {
		t.Errorf("probed sudo %d times across 3 commands, want 1", got)
	}
}

// Destroy builds its own Docker and never calls EnsureInstalled, so
// anything resolved only there is missing on every teardown. That is why the
// probe is lazy rather than part of setup: the first live teardown on Lambda
// failed exactly this way, after the deploy path had been fixed.
func TestTeardownPathElevatesWithoutEnsureInstalled(t *testing.T) {
	r := &fakeRunner{}
	denyDockerSocket(r)
	allowSudo(r)

	d := NewDocker(r)
	if err := d.Stop(context.Background(), "iplane-deployment-foo"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := d.Remove(context.Background(), "iplane-deployment-foo"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	for _, verb := range []string{"docker stop", "docker rm"} {
		if cmd := r.lastCallContaining(t, verb); !strings.HasPrefix(cmd, "sudo docker ") {
			t.Errorf("%s ran as %q, want it elevated", verb, cmd)
		}
	}
}

// The container the engine runs in is started through the same path.
func TestRunElevates(t *testing.T) {
	r := &fakeRunner{}
	denyDockerSocket(r)
	allowSudo(r)
	r.on("docker run", fakeResp{stdout: "abc1234\n"})

	d := NewDocker(r)
	if _, err := d.Run(context.Background(), RunSpec{
		Name: "iplane-deployment-foo", Image: "vllm/vllm-openai:0.7.0",
		Model: "m", Port: 8000,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cmd := r.lastCallContaining(t, "docker run"); !strings.HasPrefix(cmd, "sudo docker run") {
		t.Errorf("run ran as %q, want it elevated", cmd)
	}
}

// Neither route works: a non-root user with no docker group and no
// passwordless sudo. The error has to name both attempts and the fix, since
// nothing else on the box explains why an installed docker is unusable.
func TestUnreachableDaemonFailsWithBothAttemptsNamed(t *testing.T) {
	r := &fakeRunner{}
	denyDockerSocket(r)
	r.on("sudo -n docker info", fakeResp{exitCode: 1, stderr: "sudo: a password is required"})

	d := NewDocker(r)
	_, err := d.Inspect(context.Background(), "x")
	if err == nil {
		t.Fatal("expected an error when neither bare docker nor sudo works")
	}
	for _, want := range []string{"sudo", "docker` group", "usermod -aG docker"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}
