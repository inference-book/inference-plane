package cmd

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/state"
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
		Gpu: &provisionerv1.GpuInfo{
			Class:  spec.GetRequirements().GetClass(),
			Sku:    "mock-sku",
			Count:  1,
			VramGb: 24,
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
	store, err := state.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	mp := &mockProvider{name: providerName}
	svc := provisioners.New([]provisioners.Provider{mp}, store, "default")

	mux := http.NewServeMux()
	path, handler := provisionerv1connect.NewProvisionerServiceHandler(svc)
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
// on a previous Execute. Without this, --provider from test A leaks
// into test B and we get spurious successes / failures.
func resetInstanceFlags() {
	instanceServiceURL = ""
	instanceStateDir = ""
	instanceOperatorID = "default"
	instanceOutput = "table"

	createProvider = ""
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

	describeRemote = false

	destroyForce = false
	destroyDryRun = false
}

func TestCreate_HappyPath(t *testing.T) {
	env := newTestEnv(t, "mock")

	out, err := runCmd(t, env,
		"create", "my-pod",
		"--provider", "mock",
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
		"create", "my-pod",
		"--provider", "mock",
		"--class", "small",
	); err != nil {
		t.Fatalf("first create: %v", err)
	}
	out, err := runCmd(t, env,
		"create", "my-pod",
		"--provider", "mock",
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
		"create", "iplane-reserved",
		"--provider", "mock",
		"--class", "small",
	)
	if err == nil {
		t.Fatalf("expected error for reserved prefix; got output:\n%s", out)
	}
}

func TestList_LocalSource(t *testing.T) {
	env := newTestEnv(t, "mock")
	if _, err := runCmd(t, env,
		"create", "alpha", "--provider", "mock", "--class", "small",
	); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := runCmd(t, env,
		"create", "beta", "--provider", "mock", "--class", "medium",
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
		"create", "my-pod", "--provider", "mock", "--class", "small",
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
		"create", "my-pod", "--provider", "mock", "--class", "small",
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

func TestUnknownProvider_PreCheck(t *testing.T) {
	// checkProviderAvailable runs before any client construction. With
	// --service-url unset, an unknown provider hits the local check.
	resetInstanceFlags()
	rootCmd.SetArgs([]string{"instance", "create", "my-pod",
		"--provider", "bogus", "--class", "small",
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
