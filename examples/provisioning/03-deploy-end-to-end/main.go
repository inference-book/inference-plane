// Example: end-to-end deployment walkthrough.
//
// Walks through the full v0.1 control plane loop:
//
//	provision -> deploy -> serve -> destroy
//
// Two-process architecture mirrors 01-end-to-end:
//
//	Terminal 1:  make serve   # iplane provisioner + deployment service on :9091
//	Terminal 2:  make demo    # demokit walkthrough as one client
//
// Unlike 01-end-to-end, this example REQUIRES the runpod provider --
// the deployment executor SSHes into a real pod and runs docker.
// Local-provider instances have no SSH endpoint so the deployment
// surface rejects them at the Service layer. Set RUNPOD_API_KEY in
// the SERVER's env before `make serve`.
//
// Cost: ~$0.05-0.20 per run depending on the chosen model size and
// cold-start time. Defaults to the smallest model (Qwen2.5-1.5B) so
// the smoke run is cheap. The demo always defer-terminates and
// catches Ctrl-C; if anything goes wrong, the pod URL is printed and
// you can clean up manually via the RunPod console.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/panyam/demokit"
	"github.com/panyam/demokit/tui"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/deployments/sshdocker"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/local"
	"github.com/inference-book/inference-plane/internal/provisioners/runpod"
	"github.com/inference-book/inference-plane/internal/provisioners/state"
	"github.com/inference-book/inference-plane/internal/sshkeys"
)

// Model sizes the demo offers. Spans the realistic 1B - 8B range an
// operator might pick on a 24GB small-class GPU. All Qwen so the demo
// works without HuggingFace gated-access ceremony.
//
// Cold-start time is rough: HF download + weights into VRAM + vLLM
// engine spin-up. First run on a fresh pod will be at the high end;
// subsequent runs (cached weights) take ~30s regardless of size.
var modelOptions = map[string]struct {
	id           string
	approxVRAM   string
	coldStartHi  string
	estCostUSD   float64
	estDurationS int
}{
	"1.5B": {id: "Qwen/Qwen2.5-1.5B-Instruct", approxVRAM: "~3 GB", coldStartHi: "30-60s", estCostUSD: 0.02, estDurationS: 90},
	"3B":   {id: "Qwen/Qwen2.5-3B-Instruct", approxVRAM: "~6 GB", coldStartHi: "60-90s", estCostUSD: 0.05, estDurationS: 150},
	"7B":   {id: "Qwen/Qwen2.5-7B-Instruct", approxVRAM: "~14 GB", coldStartHi: "90-180s", estCostUSD: 0.12, estDurationS: 300},
}

const (
	defaultEnginePort = 8000
	engineImage       = "vllm/vllm-openai:v0.7.0"
)

func main() {
	for _, arg := range os.Args[1:] {
		if strings.TrimSpace(arg) == "--serve" {
			serve()
			return
		}
	}
	runDemo()
}

// ── Server side ───────────────────────────────────────────────────────

func serve() {
	addr := flag.String("addr", ":9091", "listen address")
	stateDir := flag.String("state-dir", "/tmp/iplane-deploy-example", "state file directory")
	operatorID := flag.String("operator", "default", "operator id stamped on records")
	flag.CommandLine.Parse(demokit.FilterArgs(os.Args[1:], demokit.BoolFlag("--serve")))

	apiKey := os.Getenv("RUNPOD_API_KEY")
	if apiKey == "" {
		log.Fatal("RUNPOD_API_KEY is required (this example deploys to a real RunPod pod). Set it in the server's env before `make serve`.")
	}

	store, err := state.Open(*stateDir, *operatorID)
	if err != nil {
		log.Fatalf("state.Open: %v", err)
	}
	keyStore, err := sshkeys.New(sshkeys.WithDir(*stateDir + "/keys"))
	if err != nil {
		log.Fatalf("sshkeys.New: %v", err)
	}

	providers := []provisioners.Provider{
		local.New(),
		runpod.New(runpod.NewClient(apiKey)),
	}
	svc := provisioners.New(providers, store, *operatorID,
		provisioners.WithKeyStore(keyStore),
		provisioners.WithDeploymentExecutor(sshdocker.NewExecutor()),
	)

	mux := http.NewServeMux()
	pPath, pHandler := provisionerv1connect.NewProvisionerServiceHandler(provisioners.NewConnectProvisionerAdapter(svc))
	mux.Handle(pPath, pHandler)
	dPath, dHandler := provisionerv1connect.NewDeploymentServiceHandler(provisioners.NewConnectDeploymentAdapter(svc))
	mux.Handle(dPath, dHandler)

	fmt.Printf("iplane provisioner+deployment serving on %s\n", *addr)
	fmt.Printf("  state file:   %s/state.json\n", *stateDir)
	fmt.Printf("  operator:     %s\n", *operatorID)
	fmt.Println("  providers:    local, runpod")
	fmt.Println("  executor:     sshdocker (SSH + docker on target instance)")
	fmt.Println("Try the demo: go run . --tui")
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

// ── Demo side ─────────────────────────────────────────────────────────

func runDemo() {
	url := flag.String("url", "http://localhost:9091", "iplane service URL")
	provider := flag.String("provider", provisioners.ProviderRunPod, "provider to use (only runpod is deployable in v0.1)")
	region := flag.String("region", "", "region override (default: unpinned, RunPod schedules where capacity exists)")
	flag.CommandLine.Parse(demokit.FilterArgs(os.Args[1:],
		demokit.ValueFlag("--record"),
		demokit.ValueFlag("--replay"),
		demokit.ValueFlag("--out"),
		demokit.ValueFlag("--input-timeout"),
	))

	if *provider != provisioners.ProviderRunPod {
		log.Fatalf("only --provider runpod is deployable in v0.1 (got %q); local instances have no SSH endpoint", *provider)
	}

	provisionerClient := provisionerv1connect.NewProvisionerServiceClient(http.DefaultClient, *url)
	deploymentClient := provisionerv1connect.NewDeploymentServiceClient(http.DefaultClient, *url)

	// Cleanup tracking. Both the instance and any deployment land here
	// so the signal handler tears them down on Ctrl-C. Defer cleanup
	// catches the happy-path tail.
	var (
		mu               sync.Mutex
		spawnedInstance  string
		spawnedDeploy    string
		cleanupCalled    bool
	)
	cleanup := func() {
		mu.Lock()
		defer mu.Unlock()
		if cleanupCalled {
			return
		}
		cleanupCalled = true
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if spawnedDeploy != "" {
			if _, err := deploymentClient.DestroyDeployment(ctx, connect.NewRequest(&provisionerv1.DestroyDeploymentRequest{Id: spawnedDeploy})); err != nil {
				fmt.Fprintf(os.Stderr, "WARN: cleanup DestroyDeployment(%s): %v\n", spawnedDeploy, err)
			} else {
				fmt.Fprintf(os.Stderr, "Terminated deployment %s (cleanup)\n", spawnedDeploy)
			}
		}
		if spawnedInstance != "" {
			if _, err := provisionerClient.DestroyInstance(ctx, connect.NewRequest(&provisionerv1.DestroyInstanceRequest{Id: spawnedInstance})); err != nil {
				fmt.Fprintf(os.Stderr, "WARN: cleanup DestroyInstance(%s): %v\n", spawnedInstance, err)
				fmt.Fprintln(os.Stderr, "Inspect / clean up manually: https://www.runpod.io/console/pods")
			} else {
				fmt.Fprintf(os.Stderr, "Terminated instance %s (cleanup)\n", spawnedInstance)
			}
		}
	}
	defer cleanup()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		cleanup()
		os.Exit(1)
	}()

	stamp := time.Now().UTC().Format("20060102t150405")
	deploymentID := "demo-llama-" + stamp

	// Per-run state captured during steps. The chosen model size lands
	// here after the operator selects it; deploy reads from here.
	var chosenSize string

	demo := demokit.New("Deploy end-to-end").
		Description("Provision a GPU instance, deploy vLLM with an OpenAI-compatible API, hit /v1/models to prove it serves, then tear it all down.").
		Dir("03-deploy-end-to-end").
		MaxSteps(40).
		Actors(
			demokit.Actor("Operator", "You"),
			demokit.Actor("iplane", "Provisioner + Deployment Service"),
			demokit.Actor("State", "state.json"),
			demokit.Actor("RunPod", "GPU provider"),
			demokit.Actor("Pod", "Provisioned GPU instance"),
			demokit.Actor("Engine", "vLLM container on the pod"),
		)

	demo.Section("Setup",
		"This walkthrough deploys a model with one command. The control plane provisions a GPU pod whose container IS the engine image (image-as-pod) -- no SSH, no docker-in-docker. The instance + deployment are recorded 1:1 (the instance shares the deployment id: two views, GPU and model, of the same pod).",
		fmt.Sprintf("Target URL:    %s", *url),
		fmt.Sprintf("Provider:      %s", *provider),
		fmt.Sprintf("Deployment id: %s (the instance shares this id)", deploymentID),
		"Cost depends on chosen model size + cold-start. The 1.5B default is ~$0.02 for a full run; 7B is ~$0.12. Defer-terminates on exit / Ctrl-C.",
	)

	demo.Step("Check the service is reachable").ID("ping").
		Note("CLI form:\n  iplane instance list --service-url " + *url).
		Arrow("Operator", "iplane", "ListInstances (empty filter)").
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := provisionerClient.ListInstances(rctx, connect.NewRequest(&provisionerv1.ListInstancesRequest{}))
			if err != nil {
				return abortDemo(cleanup, "cannot reach %s: %v (is `make serve` running?)", *url, err)
			}
			fmt.Println("  service reachable")
			return nil
		})

	demo.Step("Choose a model size").ID("pick-model").
		Note("All three are open-weight Qwen models that fit on a 24 GB small-class GPU. Bigger = more capable but slower cold-start and more $.").
		Input(demokit.Choice("1.5B", "3B", "7B").
			Named("size", "Model size to deploy (1.5B = fastest demo, 7B = realistic upper bound)").
			WithDefault("1.5B")).
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			sel, _ := ctx.Inputs["size"].(string)
			if sel == "" {
				sel = "1.5B"
			}
			chosenSize = sel
			opt := modelOptions[chosenSize]
			fmt.Printf("  selected:        %s -> %s\n", chosenSize, opt.id)
			fmt.Printf("  approx VRAM:     %s\n", opt.approxVRAM)
			fmt.Printf("  cold-start:     %s (worst-case on first pull)\n", opt.coldStartHi)
			fmt.Printf("  est. cost:       ~$%.2f for a full run\n", opt.estCostUSD)
			return nil
		})

	demo.Step("Deploy: provision a pod running the engine image, wait for RUNNING").ID("deploy").
		Note("One step. CreateDeployment with no instance_id auto-provisions: the control plane rents a small-class pod whose container IS the engine image (image-as-pod), passing the model via the pod's dockerStartCmd (the engine's argv), then polls the engine's /health from the operator side until 2xx. No SSH, no docker-in-docker. The instance + deployment are recorded 1:1 (two views -- GPU and model -- of the same pod).\n\nCLI form:\n  iplane deployment deploy " + deploymentID + " --provider runpod --class small --image " + engineImage + " --model <chosen> --service-url " + *url).
		Arrow("Operator", "iplane", "CreateDeployment{image=vllm, model=qwen, class=small, wait=true}").
		Arrow("iplane", "State", "write PENDING (instance + deployment)").
		Arrow("iplane", "RunPod", "create pod with engine image + model").
		Arrow("iplane", "Engine", "HTTP-poll /health until 2xx").
		Arrow("iplane", "State", "patch RUNNING + engine endpoint").
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			opt := modelOptions[chosenSize]
			rctx, cancel := context.WithTimeout(context.Background(), time.Duration(opt.estDurationS)*time.Second+3*time.Minute)
			defer cancel()
			resp, err := deploymentClient.CreateDeployment(rctx, connect.NewRequest(&provisionerv1.CreateDeploymentRequest{
				Deployment: &provisionerv1.Deployment{
					Id:         deploymentID,
					Image:      engineImage,
					Model:      opt.id,
					EnginePort: defaultEnginePort,
				},
				Provider:     *provider,
				Region:       *region,
				Requirements: &provisionerv1.ResourceRequirements{Class: provisioners.GPUClassSmall},
				Wait:         true,
			}))
			if err != nil {
				return abortDemo(cleanup, "CreateDeployment: %v", err)
			}
			dep := resp.Msg.GetDeployment()
			mu.Lock()
			spawnedDeploy = dep.GetId()
			spawnedInstance = dep.GetInstanceId() // 1:1 -- the same pod
			mu.Unlock()
			if dep.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
				return abortDemo(cleanup, "deploy reached %s, want RUNNING (reason: %s)",
					dep.GetState(), dep.GetFailureReason())
			}
			fmt.Printf("  deployment id:   %s\n", dep.GetId())
			fmt.Printf("  on instance:     %s\n", dep.GetInstanceId())
			fmt.Printf("  state:           %s\n", dep.GetState())
			fmt.Printf("  engine endpoint: %s\n", dep.GetEngineEndpoint())
			if ts := dep.GetReadyAt(); ts != nil {
				elapsed := ts.AsTime().Sub(dep.GetCreatedAt().AsTime())
				fmt.Printf("  cold-start:      %s (created->ready)\n", elapsed.Round(time.Second))
			}
			return nil
		})

	demo.Step("Wait for SSH on the engine pod").ID("wait-ssh").
		Note("Deploy promotes the 1:1 instance to ACTIVE once the engine /health returns 2xx, but the instance's SSH port is unverified at that point. This step explicitly waits for sshd on the same pod to accept TCP -- the operator's debugging affordance ('shell into the box running my model'). The deployer doesn't need SSH; this is purely for `iplane instance ssh` to work after.\n\nCLI form:\n  iplane instance wait " + deploymentID + " --service-url " + *url).
		Arrow("Operator", "iplane", "WaitForInstanceReady{id}").
		Arrow("iplane", "RunPod", "poll GET /pods/{id} for publicIp + portMappings[22]").
		Arrow("iplane", "Pod", "dial sshd port (NAT-mapped) until accept").
		Arrow("iplane", "State", "patch ssh.host / ssh.port / ssh.user (verified)").
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			rctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			resp, err := provisionerClient.WaitForInstanceReady(rctx, connect.NewRequest(&provisionerv1.WaitForInstanceReadyRequest{
				Id:             deploymentID, // 1:1 -- the instance shares the deployment id
				TimeoutSeconds: 150,
			}))
			if err != nil {
				return abortDemo(cleanup, "WaitForInstanceReady: %v", err)
			}
			ssh := resp.Msg.GetInstance().GetSsh()
			if ssh == nil || ssh.GetHost() == "" {
				return abortDemo(cleanup, "provider returned without populating ssh endpoint")
			}
			fmt.Printf("  ssh endpoint:    %s@%s:%d\n", ssh.GetUser(), ssh.GetHost(), ssh.GetPort())
			fmt.Printf("  already_ready:   %v\n", resp.Msg.GetAlreadyReady())
			return nil
		})

	demo.Step("Hit /v1/models to prove the engine serves").ID("verify").
		Note("vLLM's OpenAI-compatible surface exposes /v1/models for the served-model list. A 2xx here means a real OpenAI SDK can hit /v1/chat/completions next.\n\nCLI form (no native verb; uses the engine_endpoint from `iplane deployment describe`):\n  endpoint=$(iplane deployment describe " + deploymentID + " --service-url " + *url + " -o json | jq -r .engine_endpoint)\n  curl -fsS \"${endpoint}/v1/models\"").
		Arrow("Operator", "Engine", "GET /v1/models").
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// Re-describe to capture the engine_endpoint -- avoids holding
			// state across steps via closure if the deploy step retried.
			descResp, err := deploymentClient.DescribeDeployment(rctx, connect.NewRequest(&provisionerv1.DescribeDeploymentRequest{Id: deploymentID}))
			if err != nil {
				return abortDemo(cleanup, "DescribeDeployment: %v", err)
			}
			endpoint := descResp.Msg.GetDeployment().GetEngineEndpoint()
			if endpoint == "" {
				return abortDemo(cleanup, "engine_endpoint not set on RUNNING deployment")
			}
			fullURL := strings.TrimRight(endpoint, "/") + "/v1/models"
			req, err := http.NewRequestWithContext(rctx, http.MethodGet, fullURL, nil)
			if err != nil {
				return abortDemo(cleanup, "build request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return abortDemo(cleanup, "GET %s: %v", fullURL, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				return abortDemo(cleanup, "%s -> %d (want 2xx)", fullURL, resp.StatusCode)
			}
			var body struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&body)
			fmt.Printf("  GET %s -> %d\n", fullURL, resp.StatusCode)
			for _, m := range body.Data {
				fmt.Printf("  served model:    %s\n", m.ID)
			}
			return nil
		})

	demo.Step("Destroy the deployment (tears down the pod)").ID("destroy-deploy").
		Note("Terminates the engine pod. Because this deployment auto-provisioned its instance (1:1), the pod IS the instance -- destroying the deployment terminates the pod and marks both records TERMINATED. (For an explicitly-placed deployment on a shared instance, the instance would survive.) Idempotent: already-TERMINATED is a no-op.\n\nCLI form:\n  iplane deployment destroy " + deploymentID + " --service-url " + *url).
		Arrow("Operator", "iplane", "DestroyDeployment{id}").
		Arrow("iplane", "RunPod", "terminate pod").
		Arrow("iplane", "State", "patch deployment + instance to TERMINATED").
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			rctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			resp, err := deploymentClient.DestroyDeployment(rctx, connect.NewRequest(&provisionerv1.DestroyDeploymentRequest{Id: deploymentID}))
			if err != nil {
				return abortDemo(cleanup, "DestroyDeployment: %v", err)
			}
			fmt.Printf("  final state:     %s\n", resp.Msg.GetDeployment().GetState())
			mu.Lock()
			spawnedDeploy = ""
			spawnedInstance = "" // 1:1 -- the pod is gone with the deployment
			cleanupCalled = true
			mu.Unlock()
			return nil
		})

	demo.Section("Done",
		"Pod terminated -- billing stopped. Because the deployment auto-provisioned its instance (1:1), destroying the deployment tore down the pod; the instance record is marked TERMINATED in the same step.",
		"The instance + deployment records remain in the state file as TERMINATED -- an audit trail of what ran. Re-running provisions a fresh pod (each run gets a new timestamped id).",
	)

	if demokit.IsTUI() {
		demo.WithRenderer(tui.New())
	}

	demo.Execute()
}

// abortDemo is the fail-fast helper used by step Run callbacks in
// place of `return demokit.Errf(...)`. Reasoning: demokit v0.0.23
// records an errored step and proceeds to the next one, which
// cascades unrelated failures from a single root cause. For these
// walkthroughs we want the demo to stop where it first goes wrong.
//
// abortDemo runs the cleanup closure (deferred-terminate paid
// resources), prints the failure to stderr, and exits non-zero.
// Returns *demokit.StepResult only so callers can `return
// abortDemo(...)` to match the existing call shape; the function
// never actually returns to the caller.
func abortDemo(cleanup func(), format string, args ...any) *demokit.StepResult {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "\n\nStep failed: %s\n", msg)
	if cleanup != nil {
		cleanup()
	}
	os.Exit(1)
	return nil // unreachable
}
