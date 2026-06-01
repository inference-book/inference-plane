package cmd

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/deployments/sshdocker"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
	"github.com/inference-book/inference-plane/internal/sshkeys"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// CLI deployment tests run against a real Service over a real
// httptest.Server, the same pattern instance_test.go uses. The
// in-process Service is wired with a fakeDeploymentExecutor so no
// real SSH or docker call ever happens; the Service handles state
// machine + state file as in production.
//
// Both ProvisionerService and DeploymentService connect handlers are
// mounted on the test server. Deployment tests seed an ACTIVE
// instance "my-pod" with an SSH endpoint via direct Service calls,
// then run deployment verbs against it.

// fakeDeploymentExecutor implements provisioners.DeploymentExecutor.
// Default behavior: emit STARTING -> CONFIGURING -> RUNNING with a
// fixed engine endpoint, mirroring a healthy deploy. Override deployFn
// or destroyFn per test to simulate failure modes.
type fakeDeploymentExecutor struct {
	deployCalls  atomic.Int32
	destroyCalls atomic.Int32

	deployFn  func(emit func(sshdocker.StateUpdate)) error
	destroyFn func(emit func(sshdocker.StateUpdate)) error
}

func (f *fakeDeploymentExecutor) Deploy(_ context.Context, _ *provisionerv1.Deployment, _ *provisionerv1.Instance, _ *sshkeys.KeyPair, emit func(sshdocker.StateUpdate)) error {
	f.deployCalls.Add(1)
	if f.deployFn != nil {
		return f.deployFn(emit)
	}
	emit(sshdocker.StateUpdate{
		State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_STARTING,
		Phase: "ssh",
	})
	emit(sshdocker.StateUpdate{
		State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING,
		Phase: "pull",
	})
	emit(sshdocker.StateUpdate{
		State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
		Phase:           "healthy",
		ContainerID:     "fake-container-id",
		EngineEndpoint:  "http://1.2.3.4:8000",
		ProgressMessage: "engine serving",
	})
	return nil
}

func (f *fakeDeploymentExecutor) Destroy(_ context.Context, _ *provisionerv1.Deployment, _ *provisionerv1.Instance, _ *sshkeys.KeyPair, emit func(sshdocker.StateUpdate)) error {
	f.destroyCalls.Add(1)
	if f.destroyFn != nil {
		return f.destroyFn(emit)
	}
	emit(sshdocker.StateUpdate{
		State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATING,
		Phase: "stop",
	})
	emit(sshdocker.StateUpdate{
		State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED,
		Phase: "done",
	})
	return nil
}

// deploymentTestEnv seeds an in-process Service with a fake executor
// and exposes it through httptest.Server. The seeded instance "my-pod"
// is ACTIVE with an SSH endpoint set so deployments against it pass
// Service validation.
type deploymentTestEnv struct {
	server   *httptest.Server
	svc      *provisioners.Service
	store    *file.Store
	executor *fakeDeploymentExecutor
	stateDir string
}

// seedActiveInstance writes an ACTIVE-with-SSH instance directly into
// the state file, bypassing the provider's Spawn path. Used to set up
// preconditions for deployment tests without exercising the CreateInstance
// verb (which is a separate test surface).
func seedActiveInstance(t *testing.T, store *file.Store, id string) {
	t.Helper()
	if err := store.Update(func(f *provisioners.State) error {
		f.Instances[id] = seedInstance(id)
		return nil
	}); err != nil {
		t.Fatalf("seed instance %q: %v", id, err)
	}
}

func newDeploymentTestEnv(t *testing.T) *deploymentTestEnv {
	t.Helper()
	dir := t.TempDir()
	store, err := file.Open(dir, "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	keyStore, err := sshkeys.New(sshkeys.WithDir(dir + "/keys"))
	if err != nil {
		t.Fatalf("sshkeys.New: %v", err)
	}

	mp := &mockProvider{name: "mock"}
	exec := &fakeDeploymentExecutor{}
	svc := provisioners.New([]provisioners.Provider{mp}, store, "default",
		provisioners.WithKeyStore(keyStore),
		provisioners.WithDeploymentExecutor(exec),
	)

	mux := http.NewServeMux()
	pPath, pHandler := provisionerv1connect.NewProvisionerServiceHandler(provisioners.NewConnectProvisionerAdapter(svc))
	mux.Handle(pPath, pHandler)
	dPath, dHandler := provisionerv1connect.NewDeploymentServiceHandler(provisioners.NewConnectDeploymentAdapter(svc))
	mux.Handle(dPath, dHandler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	env := &deploymentTestEnv{
		server:   server,
		svc:      svc,
		store:    store,
		executor: exec,
		stateDir: dir,
	}
	// Seed an ACTIVE instance with an SSH endpoint so deployments
	// pass the Service's "instance must be reachable" guard.
	seedActiveInstance(t, store, "my-pod")
	return env
}

func seedInstance(id string) *provisionerv1.Instance {
	return &provisionerv1.Instance{
		Id:         id,
		ProviderId: "mock:" + id,
		Provider:   "mock",
		Region:     "test",
		Gpu: &provisionerv1.GpuInfo{
			Class:  "small",
			Sku:    "mock-sku",
			Count:  1,
			VramGb: 24,
		},
		HourlyRateUsd: 0.42,
		State:         provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
		Ssh: &provisionerv1.SshTarget{
			Host: "1.2.3.4",
			Port: 22,
			User: "root",
		},
	}
}

func runDeploymentCmd(t *testing.T, env *deploymentTestEnv, args ...string) (string, error) {
	t.Helper()
	resetDeploymentFlags()
	all := append([]string{"--service-url", env.server.URL}, args...)
	rootCmd.SetArgs(append([]string{"deployment"}, all...))

	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)

	err := rootCmd.Execute()
	return buf.String(), err
}

func resetDeploymentFlags() {
	deploymentServiceURL = ""
	deploymentStateDir = ""
	deploymentOperatorID = "default"
	deploymentOutput = "table"

	deployInstanceID = ""
	deployProvider = ""
	deployRegion = ""
	deployImage = ""
	deployModel = ""
	deployClass = ""
	deploySKU = ""
	deployMinVRAM = 0
	deployMinRAM = 0
	deployMinDisk = 0
	deployGPUCount = 0
	deployDebugShell = false
	deployEnginePort = 8000
	deployEngineArgs = nil
	deployEnv = nil
	deployWait = true
	deployTimeout = 5 * time.Minute
	deployDryRun = false

	listDeploymentInstance = ""
	listDeploymentState = ""
	listDeploymentAll = false

	watchTimeout = 10 * time.Minute

	waitForState = ""
	waitQuiet = false
	waitTimeout = 10 * time.Minute

	deploymentDestroyForce = false
	deploymentDestroyDryRun = false

	querySystem = ""
	queryMaxTokens = 256
	queryTemperature = 0.7
	queryTimeout = 60 * time.Second
}

func TestDeploy_HappyPath(t *testing.T) {
	env := newDeploymentTestEnv(t)

	out, err := runDeploymentCmd(t, env,
		"deploy", "my-llama",
		"--instance", "my-pod",
		"--image", "vllm/vllm-openai:0.7.0",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
	)
	if err != nil {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	for _, want := range []string{
		`Deployed deployment "my-llama" on instance "my-pod"`,
		"state:           RUNNING",
		"engine endpoint: http://1.2.3.4:8000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("deploy output missing %q; got:\n%s", want, out)
		}
	}
	if got := env.executor.deployCalls.Load(); got != 1 {
		t.Errorf("Deploy call count = %d, want 1", got)
	}
}

func TestDeploy_Idempotent(t *testing.T) {
	env := newDeploymentTestEnv(t)

	if _, err := runDeploymentCmd(t, env,
		"deploy", "my-llama",
		"--instance", "my-pod",
		"--image", "vllm/vllm-openai:0.7.0",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
	); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	out, err := runDeploymentCmd(t, env,
		"deploy", "my-llama",
		"--instance", "my-pod",
		"--image", "vllm/vllm-openai:0.7.0",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
	)
	if err != nil {
		t.Fatalf("second deploy: %v\n%s", err, out)
	}
	if !strings.Contains(out, `Found existing deployment "my-llama"`) {
		t.Errorf("idempotent path should say 'Found existing'; got:\n%s", out)
	}
	if got := env.executor.deployCalls.Load(); got != 1 {
		t.Errorf("Deploy call count = %d, want 1 across two deploys", got)
	}
}

func TestDeploy_RequiresInstanceImageModel(t *testing.T) {
	env := newDeploymentTestEnv(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		// --instance is now optional: omitting it triggers auto-provision,
		// which instead requires --provider and a GPU-shape flag.
		{"no-instance-no-provider", []string{"deploy", "my-llama", "--image", "x", "--model", "y"}, "--provider is required"},
		{"no-instance-no-shape", []string{"deploy", "my-llama", "--image", "x", "--model", "y", "--provider", "runpod"}, "auto-provision requires"},
		{"no-image", []string{"deploy", "my-llama", "--instance", "my-pod", "--model", "y"}, "--image is required"},
		{"no-model", []string{"deploy", "my-llama", "--instance", "my-pod", "--image", "x"}, "--model is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runDeploymentCmd(t, env, tc.args...)
			if err == nil {
				t.Fatalf("expected error; got:\n%s", out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestDeploy_InstanceMissing(t *testing.T) {
	env := newDeploymentTestEnv(t)
	out, err := runDeploymentCmd(t, env,
		"deploy", "my-llama",
		"--instance", "no-such-pod",
		"--image", "vllm/vllm-openai:0.7.0",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
	)
	if err == nil {
		t.Fatalf("expected error for missing instance; got:\n%s", out)
	}
	if got := env.executor.deployCalls.Load(); got != 0 {
		t.Errorf("executor should not have been called for missing instance; got %d calls", got)
	}
}

func TestDeploy_ExecutorFailure_PatchesFAILED(t *testing.T) {
	env := newDeploymentTestEnv(t)
	env.executor.deployFn = func(emit func(sshdocker.StateUpdate)) error {
		emit(sshdocker.StateUpdate{
			State:         provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED,
			Phase:         "pull",
			FailureReason: "image not found",
		})
		return errors.New("docker pull failed")
	}
	out, err := runDeploymentCmd(t, env,
		"deploy", "my-llama",
		"--instance", "my-pod",
		"--image", "bogus/image",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
	)
	// CreateDeployment with Wait=true returns OK even on FAILED; the
	// failure shows up in the returned Deployment's state. Operators
	// checking deploy's exit code want it to surface failure, but for
	// this PR the contract is "Wait=true returns the terminal state in
	// the response" -- the operator inspects state in the output.
	if err != nil {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	if !strings.Contains(out, "state:           FAILED") {
		t.Errorf("expected FAILED in output; got:\n%s", out)
	}
	if !strings.Contains(out, "image not found") {
		t.Errorf("expected failure reason; got:\n%s", out)
	}
}

func TestDeploy_DryRun_FreshID(t *testing.T) {
	env := newDeploymentTestEnv(t)
	out, err := runDeploymentCmd(t, env,
		"deploy", "my-llama",
		"--instance", "my-pod",
		"--image", "vllm/vllm-openai:0.7.0",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
		"--dry-run",
	)
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "[dry-run] would deploy") {
		t.Errorf("missing would-deploy line; got:\n%s", out)
	}
	if !strings.Contains(out, "no SSH or docker calls made") {
		t.Errorf("missing tail summary; got:\n%s", out)
	}
	if got := env.executor.deployCalls.Load(); got != 0 {
		t.Errorf("executor called under dry-run; got %d", got)
	}
}

func TestDeploy_DryRun_IdempotentPath(t *testing.T) {
	env := newDeploymentTestEnv(t)
	if _, err := runDeploymentCmd(t, env,
		"deploy", "my-llama",
		"--instance", "my-pod",
		"--image", "vllm/vllm-openai:0.7.0",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
	); err != nil {
		t.Fatalf("seed deploy: %v", err)
	}
	deploysBefore := env.executor.deployCalls.Load()

	out, err := runDeploymentCmd(t, env,
		"deploy", "my-llama",
		"--instance", "my-pod",
		"--image", "vllm/vllm-openai:0.7.0",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
		"--dry-run",
	)
	if err != nil {
		t.Fatalf("dry-run on existing: %v\n%s", err, out)
	}
	if !strings.Contains(out, "[dry-run] would no-op") {
		t.Errorf("matching dry-run should say would no-op; got:\n%s", out)
	}
	if got := env.executor.deployCalls.Load(); got != deploysBefore {
		t.Errorf("Deploy count went from %d to %d during dry-run", deploysBefore, got)
	}
}

func TestDeploy_DryRun_DriftReportsReplace(t *testing.T) {
	env := newDeploymentTestEnv(t)
	if _, err := runDeploymentCmd(t, env,
		"deploy", "my-llama",
		"--instance", "my-pod",
		"--image", "vllm/vllm-openai:0.7.0",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
	); err != nil {
		t.Fatalf("seed deploy: %v", err)
	}
	out, err := runDeploymentCmd(t, env,
		"deploy", "my-llama",
		"--instance", "my-pod",
		"--image", "vllm/vllm-openai:0.8.0", // drift on image
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
		"--dry-run",
	)
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "[dry-run] would replace") {
		t.Errorf("drift dry-run should say would replace; got:\n%s", out)
	}
}

func TestDeploymentDescribe_HappyPath(t *testing.T) {
	env := newDeploymentTestEnv(t)
	if _, err := runDeploymentCmd(t, env,
		"deploy", "my-llama",
		"--instance", "my-pod",
		"--image", "vllm/vllm-openai:0.7.0",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
	); err != nil {
		t.Fatalf("seed deploy: %v", err)
	}
	out, err := runDeploymentCmd(t, env, "describe", "my-llama")
	if err != nil {
		t.Fatalf("describe: %v\n%s", err, out)
	}
	for _, want := range []string{
		"id:              my-llama",
		"instance:        my-pod",
		"image:           vllm/vllm-openai:0.7.0",
		"state:           RUNNING",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("describe missing %q; got:\n%s", want, out)
		}
	}
}

func TestDeploymentDescribe_NotFound(t *testing.T) {
	env := newDeploymentTestEnv(t)
	out, err := runDeploymentCmd(t, env, "describe", "nope")
	if err == nil {
		t.Fatalf("expected error; got:\n%s", out)
	}
}

func TestDeploymentDescribe_NewFields_JSON(t *testing.T) {
	// Verifies that v0.2 ch7-beat1.1's three new Deployment fields
	// (idle_ttl_seconds, last_activity_at, no_idle_destroy) round-trip
	// through `iplane deployment describe --output json`. Seeds the
	// deployment directly into the state file: the CreateDeployment
	// flow doesn't expose these fields yet (the --no-idle-destroy
	// flag is its own follow-up ticket), so we exercise the persistence
	// + describe-render path without depending on flag wiring that
	// doesn't exist in this PR.
	env := newDeploymentTestEnv(t)
	activity := time.Date(2026, 5, 31, 12, 34, 56, 0, time.UTC)
	if err := env.store.Update(func(f *provisioners.State) error {
		f.Deployments["pinned-llama"] = &provisionerv1.Deployment{
			Id:             "pinned-llama",
			InstanceId:     "my-pod",
			Image:          "vllm/vllm-openai:0.7.0",
			Model:          "Qwen/Qwen2.5-7B-Instruct",
			State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
			IdleTtlSeconds: 300,
			LastActivityAt: timestamppb.New(activity),
			NoIdleDestroy:  true,
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out, err := runDeploymentCmd(t, env, "describe", "pinned-llama", "--output", "json")
	if err != nil {
		t.Fatalf("describe: %v\n%s", err, out)
	}
	// Match key/value pieces independently so the test stays robust to
	// protojson's whitespace-between-colon-and-value (which has shifted
	// between library versions).
	for _, want := range []string{
		`"idle_ttl_seconds":`, `300`,
		`"last_activity_at":`, `"2026-05-31T12:34:56Z"`,
		`"no_idle_destroy":`, `true`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("describe json missing %q; got:\n%s", want, out)
		}
	}
}

func TestDeploymentList_Empty(t *testing.T) {
	env := newDeploymentTestEnv(t)
	out, err := runDeploymentCmd(t, env, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "(no deployments)") {
		t.Errorf("empty list should say (no deployments); got:\n%s", out)
	}
}

func TestDeploymentList_FilterByInstance(t *testing.T) {
	env := newDeploymentTestEnv(t)
	seedActiveInstance(t, env.store, "other-pod")
	for _, dep := range []struct{ id, inst string }{
		{"alpha", "my-pod"},
		{"beta", "my-pod"},
		{"gamma", "other-pod"},
	} {
		if _, err := runDeploymentCmd(t, env,
			"deploy", dep.id,
			"--instance", dep.inst,
			"--image", "vllm/vllm-openai:0.7.0",
			"--model", "Qwen/Qwen2.5-1.5B-Instruct",
		); err != nil {
			t.Fatalf("seed deploy %s: %v", dep.id, err)
		}
	}
	out, err := runDeploymentCmd(t, env, "list", "--instance", "my-pod")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Errorf("list missing alpha/beta; got:\n%s", out)
	}
	if strings.Contains(out, "gamma") {
		t.Errorf("list should not include gamma (other-pod); got:\n%s", out)
	}
}

func TestDeploymentList_FilterByState(t *testing.T) {
	env := newDeploymentTestEnv(t)
	if _, err := runDeploymentCmd(t, env,
		"deploy", "alpha",
		"--instance", "my-pod",
		"--image", "vllm/vllm-openai:0.7.0",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out, err := runDeploymentCmd(t, env, "list", "--state", "running")
	if err != nil {
		t.Fatalf("list --state running: %v\n%s", err, out)
	}
	if !strings.Contains(out, "alpha") {
		t.Errorf("list missing alpha; got:\n%s", out)
	}
	out2, err := runDeploymentCmd(t, env, "list", "--state", "failed")
	if err != nil {
		t.Fatalf("list --state failed: %v\n%s", err, out2)
	}
	if !strings.Contains(out2, "(no deployments)") {
		t.Errorf("list --state failed should be empty; got:\n%s", out2)
	}
}

func TestDeploymentList_InvalidState(t *testing.T) {
	env := newDeploymentTestEnv(t)
	_, err := runDeploymentCmd(t, env, "list", "--state", "bogus")
	if err == nil {
		t.Fatal("expected error for bogus state")
	}
}

func TestDeploymentList_HidesTerminatedByDefault(t *testing.T) {
	// Deploy + destroy -> state=TERMINATED. Default list should
	// hide it; --all should surface it.
	env := newDeploymentTestEnv(t)
	if _, err := runDeploymentCmd(t, env,
		"deploy", "my-llama",
		"--instance", "my-pod",
		"--image", "vllm/vllm-openai:0.7.0",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
	); err != nil {
		t.Fatalf("seed deploy: %v", err)
	}
	if _, err := runDeploymentCmd(t, env, "destroy", "my-llama"); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	out, err := runDeploymentCmd(t, env, "list")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "(no deployments)") {
		t.Errorf("default list should hide terminated; got:\n%s", out)
	}

	outAll, err := runDeploymentCmd(t, env, "list", "--all")
	if err != nil {
		t.Fatalf("list --all: %v\n%s", err, outAll)
	}
	if !strings.Contains(outAll, "my-llama") {
		t.Errorf("--all should show terminated my-llama; got:\n%s", outAll)
	}
}

func TestDeploymentList_StateOverridesHide(t *testing.T) {
	// An explicit --state TERMINATED bypasses the default hide and
	// returns terminated records (the operator named the state they
	// want, so honor it).
	env := newDeploymentTestEnv(t)
	if _, err := runDeploymentCmd(t, env,
		"deploy", "my-llama",
		"--instance", "my-pod",
		"--image", "vllm/vllm-openai:0.7.0",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
	); err != nil {
		t.Fatalf("seed deploy: %v", err)
	}
	if _, err := runDeploymentCmd(t, env, "destroy", "my-llama"); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	out, err := runDeploymentCmd(t, env, "list", "--state", "terminated")
	if err != nil {
		t.Fatalf("list --state terminated: %v\n%s", err, out)
	}
	if !strings.Contains(out, "my-llama") {
		t.Errorf("--state terminated should surface my-llama without --all; got:\n%s", out)
	}
}

func TestStatus_RunningExits0(t *testing.T) {
	env := newDeploymentTestEnv(t)
	if _, err := runDeploymentCmd(t, env,
		"deploy", "my-llama",
		"--instance", "my-pod",
		"--image", "vllm/vllm-openai:0.7.0",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out, err := runDeploymentCmd(t, env, "status", "my-llama")
	if err != nil {
		t.Fatalf("status RUNNING should return nil error; got %v\n%s", err, out)
	}
	if !strings.Contains(out, "my-llama") || !strings.Contains(out, "RUNNING") {
		t.Errorf("status output unexpected: %q", out)
	}
}

func TestStatus_FailedExits1(t *testing.T) {
	env := newDeploymentTestEnv(t)
	env.executor.deployFn = func(emit func(sshdocker.StateUpdate)) error {
		emit(sshdocker.StateUpdate{
			State:         provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED,
			Phase:         "pull",
			FailureReason: "boom",
		})
		return errors.New("boom")
	}
	if _, err := runDeploymentCmd(t, env,
		"deploy", "my-llama",
		"--instance", "my-pod",
		"--image", "vllm/vllm-openai:0.7.0",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := runDeploymentCmd(t, env, "status", "my-llama")
	if err == nil {
		t.Fatal("status FAILED should return exitWithCode(1)")
	}
	var ec exitCoder
	if !errors.As(err, &ec) || ec.ExitCode() != 1 {
		t.Errorf("status FAILED exit code = %v, want 1 (err=%v)", err, err)
	}
}

func TestWatch_TerminatesOnRunning(t *testing.T) {
	env := newDeploymentTestEnv(t)
	if _, err := runDeploymentCmd(t, env,
		"deploy", "my-llama",
		"--instance", "my-pod",
		"--image", "vllm/vllm-openai:0.7.0",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
	); err != nil {
		t.Fatalf("seed deploy: %v", err)
	}
	// Once RUNNING is in the state file, Watch's first event will be
	// "(prev) -> RUNNING" and the verb terminates.
	out, err := runDeploymentCmd(t, env, "watch", "my-llama", "--timeout", "5s")
	if err != nil {
		t.Fatalf("watch: %v\n%s", err, out)
	}
	if !strings.Contains(out, "RUNNING") {
		t.Errorf("watch did not show RUNNING transition; got:\n%s", out)
	}
}

func TestWait_RunningHappyPath(t *testing.T) {
	env := newDeploymentTestEnv(t)
	if _, err := runDeploymentCmd(t, env,
		"deploy", "my-llama",
		"--instance", "my-pod",
		"--image", "vllm/vllm-openai:0.7.0",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
	); err != nil {
		t.Fatalf("seed deploy: %v", err)
	}
	out, err := runDeploymentCmd(t, env, "wait", "my-llama",
		"--for", "running", "--timeout", "5s")
	if err != nil {
		t.Fatalf("wait running: %v\n%s", err, out)
	}
}

func TestWait_FailedExits2(t *testing.T) {
	env := newDeploymentTestEnv(t)
	env.executor.deployFn = func(emit func(sshdocker.StateUpdate)) error {
		emit(sshdocker.StateUpdate{
			State:         provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED,
			Phase:         "pull",
			FailureReason: "boom",
		})
		return errors.New("boom")
	}
	if _, err := runDeploymentCmd(t, env,
		"deploy", "my-llama",
		"--instance", "my-pod",
		"--image", "vllm/vllm-openai:0.7.0",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
	); err != nil {
		t.Fatalf("seed deploy: %v", err)
	}
	_, err := runDeploymentCmd(t, env, "wait", "my-llama",
		"--for", "running", "--timeout", "5s")
	if err == nil {
		t.Fatal("wait running should fail when deployment FAILED")
	}
	var ec exitCoder
	if !errors.As(err, &ec) || ec.ExitCode() != 2 {
		t.Errorf("wait FAILED exit code = %v (err=%v); want 2", ec, err)
	}
}

func TestWait_InvalidTarget(t *testing.T) {
	env := newDeploymentTestEnv(t)
	_, err := runDeploymentCmd(t, env, "wait", "my-llama", "--for", "bogus")
	if err == nil {
		t.Fatal("invalid --for should error")
	}
}

func TestDeploymentDestroy_HappyPath(t *testing.T) {
	env := newDeploymentTestEnv(t)
	if _, err := runDeploymentCmd(t, env,
		"deploy", "my-llama",
		"--instance", "my-pod",
		"--image", "vllm/vllm-openai:0.7.0",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out, err := runDeploymentCmd(t, env, "destroy", "my-llama")
	if err != nil {
		t.Fatalf("destroy: %v\n%s", err, out)
	}
	if !strings.Contains(out, "TERMINATED") {
		t.Errorf("destroy should report TERMINATED; got:\n%s", out)
	}
	if got := env.executor.destroyCalls.Load(); got != 1 {
		t.Errorf("Destroy call count = %d, want 1", got)
	}
}

func TestDeploymentDestroy_DryRun(t *testing.T) {
	env := newDeploymentTestEnv(t)
	if _, err := runDeploymentCmd(t, env,
		"deploy", "my-llama",
		"--instance", "my-pod",
		"--image", "vllm/vllm-openai:0.7.0",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	destroysBefore := env.executor.destroyCalls.Load()
	out, err := runDeploymentCmd(t, env, "destroy", "my-llama", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run destroy: %v\n%s", err, out)
	}
	if !strings.Contains(out, "[dry-run] would destroy") {
		t.Errorf("missing would-destroy line; got:\n%s", out)
	}
	if got := env.executor.destroyCalls.Load(); got != destroysBefore {
		t.Errorf("Destroy count moved from %d to %d during dry-run", destroysBefore, got)
	}
}

func TestDeploymentDestroy_NotFound(t *testing.T) {
	env := newDeploymentTestEnv(t)
	_, err := runDeploymentCmd(t, env, "destroy", "no-such-deploy", "--dry-run")
	if err == nil {
		t.Fatal("destroy of missing id should error in dry-run")
	}
}

func TestDeploymentModels_ListsCuratedSet(t *testing.T) {
	env := newDeploymentTestEnv(t)
	out, err := runDeploymentCmd(t, env, "models")
	if err != nil {
		t.Fatalf("models: %v\n%s", err, out)
	}
	// Every curated id must appear in the listing.
	for _, m := range curatedModels {
		if !strings.Contains(out, m.ID) {
			t.Errorf("models output missing %q; got:\n%s", m.ID, out)
		}
	}
	// And the HF search URL must be surfaced so operators have somewhere
	// to look beyond the starter list.
	if !strings.Contains(out, hfHubSearchURL) {
		t.Errorf("models output missing HF Hub URL; got:\n%s", out)
	}
}
