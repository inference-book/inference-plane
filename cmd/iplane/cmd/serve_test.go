package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// daemonHarness boots the daemon's RPC surface against a temp state
// directory. It mirrors what runServe does for the provisioner Service
// without the full HTTP server / telemetry / config-load boot path.
// Tests use it to assert state-of-record semantics across daemon
// restarts without paying the full runServe cost.
type daemonHarness struct {
	dir     string
	store   *file.Store
	svc     *provisioners.Service
	server  *httptest.Server
	release func()
}

// startDaemonHarness opens the state store, takes the lifetime lock,
// builds the provisioner Service, and mounts the Connect handlers on
// an httptest.Server. Returns the harness with a Close() helper.
func startDaemonHarness(t *testing.T, dir string) *daemonHarness {
	t.Helper()
	store, err := file.Open(dir, "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	release, err := store.LockForLifetime()
	if err != nil {
		t.Fatalf("LockForLifetime: %v", err)
	}
	svc, err := buildLocalService(store, "default")
	if err != nil {
		release()
		t.Fatalf("buildLocalService: %v", err)
	}

	mux := http.NewServeMux()
	provPath, provHandler := provisionerv1connect.NewProvisionerServiceHandler(provisioners.NewConnectProvisionerAdapter(svc))
	mux.Handle(provPath, provHandler)
	depPath, depHandler := provisionerv1connect.NewDeploymentServiceHandler(provisioners.NewConnectDeploymentAdapter(svc))
	mux.Handle(depPath, depHandler)
	server := httptest.NewServer(mux)

	return &daemonHarness{
		dir:     dir,
		store:   store,
		svc:     svc,
		server:  server,
		release: release,
	}
}

func (h *daemonHarness) close() {
	h.server.Close()
	h.release()
}

// TestServe_StateOfRecord_SurvivesRestart simulates the daemon
// lifecycle the chapter narrative requires: an operator boots
// `iplane serve`, registers a record, kills the daemon, restarts it,
// and the record still exists. Implemented as two harness lifetimes
// against the same state directory so the test does not pay the cost
// of spawning the real binary.
func TestServe_StateOfRecord_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	// Boot the daemon. Provision via Connect against the harness's
	// HTTP listener -- same wire path `iplane instance --service-url`
	// would take in production.
	first := startDaemonHarness(t, dir)
	client := provisionerv1connect.NewProvisionerServiceClient(http.DefaultClient, first.server.URL)
	_, err := client.CreateInstance(context.Background(), connect.NewRequest(&provisionerv1.CreateInstanceRequest{
		Spec: &provisionerv1.Spec{
			Id:       "survives-restart",
			Provider: "local",
			Region:   "local",
			Requirements: &provisionerv1.ResourceRequirements{
				MinVramGb: 8,
				Class:     "small",
			},
		},
	}))
	if err != nil {
		first.close()
		t.Fatalf("CreateInstance: %v", err)
	}
	first.close()

	// Restart: fresh harness on the same dir. The pre-restart
	// instance must still be visible.
	second := startDaemonHarness(t, dir)
	defer second.close()
	client = provisionerv1connect.NewProvisionerServiceClient(http.DefaultClient, second.server.URL)
	resp, err := client.ListInstances(context.Background(), connect.NewRequest(&provisionerv1.ListInstancesRequest{}))
	if err != nil {
		t.Fatalf("ListInstances after restart: %v", err)
	}
	found := false
	for _, inst := range resp.Msg.GetInstances() {
		if inst.GetId() == "survives-restart" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("instance %q did not survive daemon restart; got %d instances", "survives-restart", len(resp.Msg.GetInstances()))
	}
}

// TestServe_RefusesSecondDaemon asserts the lifetime-lock contract:
// two daemons against the same state directory cannot run
// concurrently. The first holds the flock; the second sees
// *file.ErrLockHeld with the first's PID populated.
func TestServe_RefusesSecondDaemon(t *testing.T) {
	dir := t.TempDir()
	first := startDaemonHarness(t, dir)
	defer first.close()

	// Try to open a second store + LockForLifetime against the same
	// dir. This is what the second `iplane serve` would do at startup.
	second, err := file.Open(dir, "default")
	if err != nil {
		t.Fatalf("Open second store: %v", err)
	}
	_, err = second.LockForLifetime()
	if err == nil {
		t.Fatal("second LockForLifetime: expected error, got nil")
	}
	var held *file.ErrLockHeld
	if !errors.As(err, &held) {
		t.Fatalf("expected *file.ErrLockHeld, got %T: %v", err, err)
	}
	if held.HolderPID == 0 {
		t.Error("HolderPID should be populated from the lock-pid sidecar")
	}
}

// TestInstance_NoServiceURL_WhileDaemonHolds asserts the operator
// experience promised by the ticket: with a daemon running and no
// --service-url passed, the CLI surfaces a clear "iplane serve is
// running at PID N" message rather than blocking on the flock.
func TestInstance_NoServiceURL_WhileDaemonHolds(t *testing.T) {
	dir := t.TempDir()
	harness := startDaemonHarness(t, dir)
	defer harness.close()

	// Point the CLI globals at this temp state dir, no --service-url.
	prevDir := instanceStateDir
	prevSvc := instanceServiceURL
	instanceStateDir = dir
	instanceServiceURL = ""
	t.Cleanup(func() {
		instanceStateDir = prevDir
		instanceServiceURL = prevSvc
	})

	_, err := buildClient()
	if err == nil {
		t.Fatal("buildClient: expected error while daemon holds the lock, got nil")
	}
	if !strings.Contains(err.Error(), "iplane serve is running") {
		t.Errorf("error should mention iplane serve; got: %v", err)
	}
	if !strings.Contains(err.Error(), "--service-url") {
		t.Errorf("error should point at --service-url remedy; got: %v", err)
	}
}
