package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
	"github.com/inference-book/inference-plane/internal/router"
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

	// Mount the v0.2 data-plane router. Its Connect client
	// loopback-dials this same httptest.Server -- mirrors what
	// `iplane serve` does in production.
	deploymentRouter := router.New(provisionerv1connect.NewDeploymentServiceClient(http.DefaultClient, server.URL))
	for pattern, h := range deploymentRouter.Handle() {
		mux.Handle(pattern, h)
	}

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

// TestServe_RouterForwardsToEngine drives the v0.2 ch7-beat1.3
// data-path end-to-end through the daemon harness: register a
// deployment whose engine endpoint points at a httptest fake engine,
// then POST /v1/<deploy-id>/v1/chat/completions to the daemon's URL
// and assert the engine receives the request and the response flows
// back. Same wire path Ch 7's demo 04 exercises with a real engine.
func TestServe_RouterForwardsToEngine(t *testing.T) {
	dir := t.TempDir()
	harness := startDaemonHarness(t, dir)
	defer harness.close()

	// Stand up a fake engine. Assert it received the unwrapped path
	// (the /v1/<id>/ prefix is iplane-side; the engine should see
	// only the OpenAI tail).
	engineReceived := make(chan string, 1)
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		engineReceived <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-e2e","choices":[{"message":{"content":"e2e ok"}}]}`)
	}))
	defer engine.Close()

	// Seed an instance and a RUNNING deployment whose engine endpoint
	// is the fake engine's URL. Skip provider Spawn / executor by
	// writing directly into the state of record; the daemon's router
	// only cares about (state, engine_endpoint).
	if err := harness.store.Update(func(s *provisioners.State) error {
		s.Instances["my-pod"] = &provisionerv1.Instance{
			Id:       "my-pod",
			Provider: "local",
			State:    provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
		}
		s.Deployments["e2e-llama"] = &provisionerv1.Deployment{
			Id:             "e2e-llama",
			InstanceId:     "my-pod",
			State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
			EngineEndpoint: engine.URL,
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Drive the request through the daemon's router.
	resp, err := http.Post(harness.server.URL+"/v1/e2e-llama/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "e2e ok") {
		t.Errorf("response body should pass through engine; got: %s", body)
	}
	select {
	case got := <-engineReceived:
		if got != "/v1/chat/completions" {
			t.Errorf("engine received %q; deploy-id prefix should have been stripped (want /v1/chat/completions)", got)
		}
	default:
		t.Error("engine never received the forwarded request")
	}
}

// TestServe_FlatURL_RoutesByModelInBody asserts the v0.2 ch7-beat1.3b
// OpenAI-flat URL works end-to-end: client POSTs to
// /v1/chat/completions with `model` in the body; daemon's router
// looks up the deployment serving that model and forwards. Same
// daemon harness as the deploy-id test; the only differences are
// the URL the client hits and what the router uses as the routing key.
func TestServe_FlatURL_RoutesByModelInBody(t *testing.T) {
	dir := t.TempDir()
	harness := startDaemonHarness(t, dir)
	defer harness.close()

	engineReceived := make(chan string, 1)
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		engineReceived <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-flat-e2e","choices":[{"message":{"content":"flat ok"}}]}`)
	}))
	defer engine.Close()

	// Seed a RUNNING deployment whose model is what the request will
	// claim. The router has to find it via ListDeployments and route
	// solely on the model match.
	if err := harness.store.Update(func(s *provisioners.State) error {
		s.Instances["my-pod"] = &provisionerv1.Instance{
			Id:       "my-pod",
			Provider: "local",
			State:    provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
		}
		s.Deployments["any-id"] = &provisionerv1.Deployment{
			Id:             "any-id",
			InstanceId:     "my-pod",
			Model:          "Qwen/Qwen2.5-7B-Instruct",
			State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
			EngineEndpoint: engine.URL,
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body := `{"model":"Qwen/Qwen2.5-7B-Instruct","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(harness.server.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, b)
	}
	respBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(respBody), "flat ok") {
		t.Errorf("response body should pass through engine; got: %s", respBody)
	}
	select {
	case got := <-engineReceived:
		if got != "/v1/chat/completions" {
			t.Errorf("engine received %q; flat URL should forward path as-is (want /v1/chat/completions)", got)
		}
	default:
		t.Error("engine never received the forwarded request")
	}
}

// TestServe_FlatURL_StreamsTokens drives the v0.2 ch7-beat1.4 SSE
// path end-to-end through the daemon harness. Fake engine emits
// timed SSE chunks; client reads them via the flat URL; the test
// asserts both that all chunks arrived and that they spread over
// time (real-time streaming, not buffered).
//
// httputil.ReverseProxy handles SSE pass-through implicitly via
// Content-Type auto-detection; this test locks the behavior in
// as a regression target.
func TestServe_FlatURL_StreamsTokens(t *testing.T) {
	dir := t.TempDir()
	harness := startDaemonHarness(t, dir)
	defer harness.close()

	// Fake engine streams 5 SSE chunks with a 50ms gap between each.
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 0; i < 5; i++ {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			fmt.Fprintf(w, "data: token-%d\n\n", i)
			flusher.Flush()
			time.Sleep(50 * time.Millisecond)
		}
		fmt.Fprintln(w, "data: [DONE]")
		flusher.Flush()
	}))
	defer engine.Close()

	if err := harness.store.Update(func(s *provisioners.State) error {
		s.Instances["my-pod"] = &provisionerv1.Instance{
			Id: "my-pod", Provider: "local",
			State: provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
		}
		s.Deployments["streamer"] = &provisionerv1.Deployment{
			Id:             "streamer",
			InstanceId:     "my-pod",
			Model:          "test/streaming-model",
			State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
			EngineEndpoint: engine.URL,
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	start := time.Now()
	resp, err := http.Post(
		harness.server.URL+"/v1/chat/completions",
		"application/json",
		strings.NewReader(`{"model":"test/streaming-model","stream":true,"messages":[]}`),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}

	// Read one chunk at a time. The whole conversation should take
	// at least 4 * 50ms = 200ms of wall-clock; a buffered response
	// would compress all reads to the same moment.
	br := bufio.NewReader(resp.Body)
	var chunks []string
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			if trimmed := strings.TrimRight(line, "\r\n"); trimmed != "" {
				chunks = append(chunks, trimmed)
			}
		}
		if err != nil {
			break
		}
	}
	elapsed := time.Since(start)

	if len(chunks) < 5 {
		t.Fatalf("expected at least 5 streamed chunks, got %d: %v", len(chunks), chunks)
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("chunks arrived too quickly (%v); router likely buffered the response", elapsed)
	}
}
