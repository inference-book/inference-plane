package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// CLI tests run against a real Service over a real httptest.Server,
// reached via the --service-url flag (the remote transport mode).
// This exercises the same code path operators use when forwarding to
// a running iplane serve, and lets us count provider-side calls via
// the injected mockProvider -- the load-bearing assertion for
// --dry-run later on this branch.
//
// The Service is real (idempotency, state-file hygiene); only the
// Provider is mock. Tests pick which verbs run and assert on:
//
//   - Stdout shape (table headers, "Created instance ..." lines, etc.)
//   - Provider call counts (zero under --dry-run, one under real run)
//   - State-file effects (record present after create, absent after
//     destroy, untouched under --dry-run -- covered in dry-run tests)

// mockProvider implements provisioners.Provider with counted calls
// and easy customization per test. Mirrors the pattern in
// internal/provisioners/service_test.go but lives here because the
// internal one is package-private.
type mockProvider struct {
	name string

	spawnCalls atomic.Int32
	termCalls  atomic.Int32

	spawn func(ctx context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error)
}

func (m *mockProvider) Name() string { return m.name }

func (m *mockProvider) Spawn(ctx context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error) {
	m.spawnCalls.Add(1)
	if m.spawn != nil {
		return m.spawn(ctx, spec)
	}
	return &provisionerv1.Instance{
		Id:         spec.GetId(),
		ProviderId: "mock:" + spec.GetId(),
		Provider:   m.name,
		Spec:       spec,
		Region:     spec.GetRegion(),
		Hardware: &provisionerv1.Hardware{
			GpuSku:    "mock-sku",
			GpuCount:  1,
			GpuVramMb: 24 * 1024,
		},
		HourlyRateUsd: 0.42,
		State:         provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
	}, nil
}

func (m *mockProvider) Terminate(ctx context.Context, providerID string) error {
	m.termCalls.Add(1)
	return nil
}

func (m *mockProvider) Describe(ctx context.Context, providerID string) (*provisionerv1.Instance, error) {
	return nil, provisioners.NewProviderError(m.name, "describe", provisioners.ErrNotFound, 0)
}

func (m *mockProvider) List(ctx context.Context, filter map[string]string) ([]*provisionerv1.InstanceRef, error) {
	return nil, nil
}

// testEnv is a fresh Service + state file + httptest.Server per test.
// We point --service-url at server.URL so the CLI exercises its
// remote transport path against an in-memory backend.
type testEnv struct {
	server   *httptest.Server
	provider *mockProvider
	stateDir string
}

func newTestEnv(t *testing.T, providerName string) *testEnv {
	t.Helper()
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	mp := &mockProvider{name: providerName}
	svc := provisioners.New([]provisioners.Provider{mp}, store, "default")

	mux := http.NewServeMux()
	path, handler := provisionerv1connect.NewProvisionerServiceHandler(provisioners.NewConnectProvisionerAdapter(svc))
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &testEnv{server: server, provider: mp, stateDir: store.Path()}
}

// runCmd resets the CLI flag state, runs rootCmd with the given args,
// and returns stdout and any error. Tests always include
// --service-url <test server>, so no real provider is touched and no
// real state file gets opened on the developer's home dir.
func runCmd(t *testing.T, env *testEnv, args ...string) (string, error) {
	t.Helper()
	resetInstanceFlags()
	all := append([]string{"--service-url", env.server.URL}, args...)
	rootCmd.SetArgs(append([]string{"instance"}, all...))

	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)

	err := rootCmd.Execute()
	return buf.String(), err
}

// resetInstanceFlags clears the package-level flag state cobra wrote
// on a previous Execute. Without this, flags from test A leak into
// test B and we get spurious successes / failures. Positional args
// (provider, id) are not package-level state, so no reset needed.
func resetInstanceFlags() {
	instanceServiceURL = ""
	instanceStateDir = ""
	instanceOperatorID = "default"
	instanceOutput = "table"

	createRegion = ""
	createClass = ""
	createSKU = ""
	createGPUCount = 0
	createMinVRAM = 0
	createMinRAM = 0
	createMinDisk = 0
	createBaseImage = ""
	createDryRun = false

	listProvider = ""
	listRemote = false
	listAll = false

	describeRemote = false

	destroyForce = false
	destroyDryRun = false
}

func TestCreate_HappyPath(t *testing.T) {
	env := newTestEnv(t, "mock")

	out, err := runCmd(t, env,
		"create", "mock", "my-pod",
		"--class", "small",
	)
	if err != nil {
		t.Fatalf("create: %v\n%s", err, out)
	}
	if !strings.Contains(out, `Created instance "my-pod"`) {
		t.Errorf("missing created line; got:\n%s", out)
	}
	if !strings.Contains(out, "state:        ACTIVE") {
		t.Errorf("missing ACTIVE state line; got:\n%s", out)
	}
	if got := env.provider.spawnCalls.Load(); got != 1 {
		t.Errorf("Spawn call count = %d, want 1", got)
	}
}

func TestCreate_Idempotent(t *testing.T) {
	env := newTestEnv(t, "mock")

	if _, err := runCmd(t, env,
		"create", "mock", "my-pod",
		"--class", "small",
	); err != nil {
		t.Fatalf("first create: %v", err)
	}
	out, err := runCmd(t, env,
		"create", "mock", "my-pod",
		"--class", "small",
	)
	if err != nil {
		t.Fatalf("second create: %v\n%s", err, out)
	}
	if !strings.Contains(out, `Found existing instance "my-pod"`) {
		t.Errorf("idempotent path should say 'Found existing'; got:\n%s", out)
	}
	if got := env.provider.spawnCalls.Load(); got != 1 {
		t.Errorf("Spawn call count = %d, want 1 across two creates", got)
	}
}

func TestCreate_RejectsReservedPrefix(t *testing.T) {
	env := newTestEnv(t, "mock")
	out, err := runCmd(t, env,
		"create", "mock", "iplane-reserved",
		"--class", "small",
	)
	if err == nil {
		t.Fatalf("expected error for reserved prefix; got output:\n%s", out)
	}
}

func TestList_LocalSource(t *testing.T) {
	env := newTestEnv(t, "mock")
	if _, err := runCmd(t, env,
		"create", "mock", "alpha", "--class", "small",
	); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := runCmd(t, env,
		"create", "mock", "beta", "--class", "medium",
	); err != nil {
		t.Fatalf("create: %v", err)
	}
	out, err := runCmd(t, env, "list")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	for _, want := range []string{"ID", "PROVIDER", "STATE", "alpha", "beta", "ACTIVE"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q; got:\n%s", want, out)
		}
	}
}

func TestList_Empty(t *testing.T) {
	env := newTestEnv(t, "mock")
	out, err := runCmd(t, env, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "(no instances)") {
		t.Errorf("empty list should say (no instances); got:\n%s", out)
	}
}

func TestList_HidesTerminatedByDefault(t *testing.T) {
	// Create + destroy -> state=TERMINATED. Default list should
	// hide it; --all should surface it. Mirrors `docker ps` (no -a)
	// hiding exited containers.
	env := newTestEnv(t, "mock")
	if _, err := runCmd(t, env,
		"create", "mock", "alpha", "--class", "small",
	); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := runCmd(t, env, "destroy", "alpha"); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	out, err := runCmd(t, env, "list")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "(no instances)") {
		t.Errorf("default list should hide terminated; got:\n%s", out)
	}

	outAll, err := runCmd(t, env, "list", "--all")
	if err != nil {
		t.Fatalf("list --all: %v\n%s", err, outAll)
	}
	if !strings.Contains(outAll, "alpha") {
		t.Errorf("--all should show terminated alpha; got:\n%s", outAll)
	}
	if !strings.Contains(outAll, "TERMINATED") {
		t.Errorf("--all should report TERMINATED state; got:\n%s", outAll)
	}
}

func TestList_HidesTerminatedAlongsideLive(t *testing.T) {
	// Mix of states: one live, one terminated. Default list shows
	// only the live one; --all shows both.
	env := newTestEnv(t, "mock")
	if _, err := runCmd(t, env,
		"create", "mock", "alpha", "--class", "small",
	); err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	if _, err := runCmd(t, env,
		"create", "mock", "beta", "--class", "small",
	); err != nil {
		t.Fatalf("create beta: %v", err)
	}
	if _, err := runCmd(t, env, "destroy", "beta"); err != nil {
		t.Fatalf("destroy beta: %v", err)
	}

	out, err := runCmd(t, env, "list")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "alpha") {
		t.Errorf("default list missing live alpha; got:\n%s", out)
	}
	if strings.Contains(out, "beta") {
		t.Errorf("default list should hide terminated beta; got:\n%s", out)
	}
}

func TestList_RemoteRequiresProvider(t *testing.T) {
	env := newTestEnv(t, "mock")
	out, err := runCmd(t, env, "list", "--remote")
	if err == nil {
		t.Fatalf("--remote without --provider should fail; got output:\n%s", out)
	}
}

func TestDescribe_HappyPath(t *testing.T) {
	env := newTestEnv(t, "mock")
	if _, err := runCmd(t, env,
		"create", "mock", "my-pod", "--class", "small",
	); err != nil {
		t.Fatalf("create: %v", err)
	}
	out, err := runCmd(t, env, "describe", "my-pod")
	if err != nil {
		t.Fatalf("describe: %v\n%s", err, out)
	}
	for _, want := range []string{
		"id:            my-pod",
		"provider:      mock",
		"state:         ACTIVE",
		"gpu sku:       mock-sku",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("describe missing %q; got:\n%s", want, out)
		}
	}
}

func TestDestroy_HappyPath(t *testing.T) {
	env := newTestEnv(t, "mock")
	if _, err := runCmd(t, env,
		"create", "mock", "my-pod", "--class", "small",
	); err != nil {
		t.Fatalf("create: %v", err)
	}
	out, err := runCmd(t, env, "destroy", "my-pod")
	if err != nil {
		t.Fatalf("destroy: %v\n%s", err, out)
	}
	if !strings.Contains(out, "TERMINATED") {
		t.Errorf("destroy should report TERMINATED; got:\n%s", out)
	}
	if got := env.provider.termCalls.Load(); got != 1 {
		t.Errorf("Terminate call count = %d, want 1", got)
	}
}

func TestDestroy_NotFound(t *testing.T) {
	env := newTestEnv(t, "mock")
	out, err := runCmd(t, env, "destroy", "does-not-exist")
	if err == nil {
		t.Fatalf("destroy of missing id should fail; got:\n%s", out)
	}
}

func TestCreate_OutputJSON(t *testing.T) {
	env := newTestEnv(t, "mock")
	out, err := runCmd(t, env,
		"create", "mock", "my-pod", "--class", "small",
		"--output", "json",
	)
	if err != nil {
		t.Fatalf("create --output json: %v\n%s", err, out)
	}
	// Must parse as JSON and carry the proto-name fields the state
	// file uses (provider_id, hourly_rate_usd, etc.) -- not the
	// camelCase variants. The contract is: `iplane ... --output json
	// | jq` works without translation.
	var resp struct {
		Instance struct {
			ID         string `json:"id"`
			ProviderID string `json:"provider_id"`
			State      string `json:"state"`
		} `json:"instance"`
		AlreadyExisted bool `json:"already_existed"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if resp.Instance.ID != "my-pod" {
		t.Errorf("instance.id = %q, want my-pod", resp.Instance.ID)
	}
	if resp.Instance.ProviderID == "" {
		t.Errorf("instance.provider_id is empty in JSON output")
	}
	if resp.Instance.State != "INSTANCE_STATE_ACTIVE" {
		t.Errorf("instance.state = %q, want INSTANCE_STATE_ACTIVE", resp.Instance.State)
	}
}

func TestList_OutputJSON(t *testing.T) {
	env := newTestEnv(t, "mock")
	if _, err := runCmd(t, env,
		"create", "mock", "alpha", "--class", "small",
	); err != nil {
		t.Fatalf("create: %v", err)
	}
	out, err := runCmd(t, env, "list", "--output", "json")
	if err != nil {
		t.Fatalf("list --output json: %v\n%s", err, out)
	}
	var resp struct {
		Instances []struct {
			ID string `json:"id"`
		} `json:"instances"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(resp.Instances) != 1 || resp.Instances[0].ID != "alpha" {
		t.Errorf("instances = %+v, want one entry with id=alpha", resp.Instances)
	}
}

func TestDescribe_OutputJSON(t *testing.T) {
	env := newTestEnv(t, "mock")
	if _, err := runCmd(t, env,
		"create", "mock", "my-pod", "--class", "small",
	); err != nil {
		t.Fatalf("create: %v", err)
	}
	out, err := runCmd(t, env, "describe", "my-pod", "--output", "json")
	if err != nil {
		t.Fatalf("describe --output json: %v\n%s", err, out)
	}
	// The describe response wraps a single Instance at the top level
	// (no envelope), matching the protojson encoding of
	// DescribeInstanceResponse.
	var inst struct {
		ID       string `json:"id"`
		Hardware struct {
			// writeProtoJSON uses UseProtoNames=true; field names
			// stay snake_case in the JSON output.
			GpuSku string `json:"gpu_sku"`
		} `json:"hardware"`
	}
	if err := json.Unmarshal([]byte(out), &inst); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if inst.ID != "my-pod" || inst.Hardware.GpuSku != "mock-sku" {
		t.Errorf("decoded instance = %+v", inst)
	}
}

func TestCreate_DryRun_FreshID(t *testing.T) {
	env := newTestEnv(t, "mock")
	out, err := runCmd(t, env,
		"create", "mock", "my-pod", "--class", "small", "--dry-run",
	)
	if err != nil {
		t.Fatalf("dry-run create: %v\n%s", err, out)
	}
	if !strings.Contains(out, "[dry-run] would create") {
		t.Errorf("missing would-create line; got:\n%s", out)
	}
	if !strings.Contains(out, "vram>=24GB") {
		t.Errorf("constraints line missing expanded vram floor; got:\n%s", out)
	}
	if !strings.Contains(out, "no provider calls made, no state file changes") {
		t.Errorf("missing tail summary; got:\n%s", out)
	}
	// Load-bearing assertions: zero provider calls AND state file empty.
	if got := env.provider.spawnCalls.Load(); got != 0 {
		t.Errorf("Spawn call count = %d, want 0 under --dry-run", got)
	}
	listOut, _ := runCmd(t, env, "list")
	if !strings.Contains(listOut, "(no instances)") {
		t.Errorf("state file was modified by dry-run; list reports:\n%s", listOut)
	}
}

func TestCreate_DryRun_IdempotentPath(t *testing.T) {
	env := newTestEnv(t, "mock")
	if _, err := runCmd(t, env,
		"create", "mock", "my-pod", "--class", "small",
	); err != nil {
		t.Fatalf("create: %v", err)
	}
	spawnsBefore := env.provider.spawnCalls.Load()

	out, err := runCmd(t, env,
		"create", "mock", "my-pod", "--class", "small", "--dry-run",
	)
	if err != nil {
		t.Fatalf("dry-run on existing record: %v\n%s", err, out)
	}
	if !strings.Contains(out, "[dry-run] would no-op") {
		t.Errorf("idempotent dry-run should say 'would no-op'; got:\n%s", out)
	}
	if !strings.Contains(out, "already exists") {
		t.Errorf("missing 'already exists' note; got:\n%s", out)
	}
	if got := env.provider.spawnCalls.Load(); got != spawnsBefore {
		t.Errorf("Spawn count went from %d to %d during dry-run", spawnsBefore, got)
	}
}

func TestCreate_DryRun_InvalidSpecRejected(t *testing.T) {
	env := newTestEnv(t, "mock")
	out, err := runCmd(t, env,
		"create", "mock", "my-pod", "--dry-run",
		// No --class, no --sku, no --min-vram-gb -> ValidateAndExpandRequirements rejects.
	)
	if err == nil {
		t.Fatalf("dry-run with under-specified spec should fail; got:\n%s", out)
	}
	if !strings.Contains(err.Error(), "requirements") {
		t.Errorf("error %q does not mention requirements", err)
	}
}

func TestDestroy_DryRun(t *testing.T) {
	env := newTestEnv(t, "mock")
	if _, err := runCmd(t, env,
		"create", "mock", "my-pod", "--class", "small",
	); err != nil {
		t.Fatalf("create: %v", err)
	}
	termsBefore := env.provider.termCalls.Load()

	out, err := runCmd(t, env, "destroy", "my-pod", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run destroy: %v\n%s", err, out)
	}
	if !strings.Contains(out, "[dry-run] would destroy") {
		t.Errorf("missing would-destroy line; got:\n%s", out)
	}
	if got := env.provider.termCalls.Load(); got != termsBefore {
		t.Errorf("Terminate count went from %d to %d during dry-run", termsBefore, got)
	}
	// Record must still be ACTIVE -- dry-run does not touch state.
	describeOut, _ := runCmd(t, env, "describe", "my-pod")
	if !strings.Contains(describeOut, "state:         ACTIVE") {
		t.Errorf("dry-run destroy mutated state; describe shows:\n%s", describeOut)
	}
}

func TestDestroy_DryRun_NotFound(t *testing.T) {
	env := newTestEnv(t, "mock")
	out, err := runCmd(t, env, "destroy", "does-not-exist", "--dry-run")
	if err == nil {
		t.Fatalf("dry-run destroy of missing id should fail; got:\n%s", out)
	}
	if !strings.Contains(err.Error(), `no instance with id "does-not-exist"`) {
		t.Errorf("error %q should name the missing id", err)
	}
}

// (TestInstanceSSH_RemoteMode_Refused removed: --service-url mode now
// works via the GetInstanceSSHKey RPC. Happy-path remote ssh requires
// a real ssh server to actually validate, which the demokit walkthrough
// exercises against a real RunPod pod.)

func TestInstanceSSH_NoInstance(t *testing.T) {
	resetInstanceFlags()
	rootCmd.SetArgs([]string{"instance", "ssh", "nope",
		"--state-dir", t.TempDir()})
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	err := rootCmd.Execute()
	if err == nil {
		t.Fatalf("ssh for missing instance should fail; got:\n%s", buf.String())
	}
	if !strings.Contains(err.Error(), `no instance with id "nope"`) {
		t.Errorf("error %q should name the missing id", err)
	}
}

func TestUnknownProvider_PreCheck(t *testing.T) {
	// checkProviderAvailable runs before any client construction. With
	// --service-url unset, an unknown provider hits the local check.
	resetInstanceFlags()
	rootCmd.SetArgs([]string{"instance", "create", "bogus", "my-pod",
		"--class", "small",
		"--state-dir", t.TempDir()})
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	if err := rootCmd.Execute(); err == nil {
		t.Fatalf("expected error for bogus provider; got:\n%s", buf.String())
	} else if !strings.Contains(err.Error(), `unknown provider "bogus"`) {
		t.Errorf("error %q does not name the provider; want 'unknown provider \"bogus\"'", err)
	}
}
