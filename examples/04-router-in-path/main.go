// Example: Beat 1 router-in-path walkthrough.
//
// Closes the v0.2 / Ch 7 Beat 1 narrative. By this point the chapter has
// taught:
//
//	client -> iplane router -> engine
//
// (replacing v0.1's `client -> engine`). The router lives in
// `internal/router/`, ships request/latency/token metrics, propagates
// W3C TraceContext + Baggage, and a background reaper protects against
// abandoned-deployment leaks via an idle TTL. This walkthrough exercises
// the full chain end-to-end against a real RunPod pod, populates the
// v0.2 Grafana dashboard, and points at the resulting Tempo trace.
//
// Two-process layout (matches 03-deploy-end-to-end):
//
//	Terminal 1:  iplane serve         # full daemon on :8080
//	             (optional) make up   # Tempo + Mimir + Grafana stack
//	Terminal 2:  make demo            # this walkthrough
//
// Demo 04 is the operator-facing client. It does NOT host any service;
// it talks Connect over HTTP at the daemon's :8080 and drives traffic
// through the router using the flat URL (`/v1/chat/completions` with
// `model` in the body) and the deploy-id URL
// (`/v1/<deploy-id>/v1/models`) for sanity checks.
//
// Cost: ~$0.05 (1.5B default) up to ~$0.25 (7B). The pod is created with
// `--no-idle-destroy` so demos 05 and 06 can re-use it without paying
// for a fresh provisioning cycle. The last step prompts the operator to
// destroy or leave running; default is LEAVE, matching the shared-daemon
// design.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/panyam/demokit"

	"github.com/inference-book/inference-plane/examples/common"
	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// Model sizes the demo offers. Same shape as 03-deploy-end-to-end so
// the operator's mental model carries forward. The smallest default
// keeps the smoke read cheap.
var modelOptions = map[string]struct {
	id           string
	approxVRAM   string
	coldStartHi  string
	estCostUSD   float64
	estDurationS int
}{
	"1.5B": {id: "Qwen/Qwen2.5-1.5B-Instruct", approxVRAM: "~3 GB", coldStartHi: "3-8 min cold / ~60s hot", estCostUSD: 0.05, estDurationS: 360},
	"3B":   {id: "Qwen/Qwen2.5-3B-Instruct", approxVRAM: "~6 GB", coldStartHi: "5-10 min cold / ~90s hot", estCostUSD: 0.10, estDurationS: 480},
	"7B":   {id: "Qwen/Qwen2.5-7B-Instruct", approxVRAM: "~14 GB", coldStartHi: "8-15 min cold / 2-3 min hot", estCostUSD: 0.25, estDurationS: 720},
}

const (
	defaultEnginePort = 8000
	engineImage       = "vllm/vllm-openai:v0.7.0"
)

func main() {
	url := flag.String("url", "http://localhost:8080", "iplane serve HTTP URL")
	grafanaURL := flag.String("grafana-url", "http://localhost:3000", "Grafana base URL (used for panel links; nothing fails if unreachable)")
	tempoURL := flag.String("tempo-url", "http://localhost:3200", "Tempo base URL (used for the trace pointer)")
	provider := flag.String("provider", common.DefaultProvider(),
		"provider (default: "+common.EnvProvider+" env, else runpod)")
	region := flag.String("region", "", "region override (default: unpinned)")
	loadRPS := flag.Float64("load-rps", 5.0, "iplane load --rps to fire after the deployment is RUNNING")
	loadDuration := flag.Duration("load-duration", 30*time.Second, "iplane load --duration")
	binPath := flag.String("bin", "", "path to a prebuilt iplane binary; built from source if empty")

	flag.CommandLine.Parse(demokit.FilterArgs(os.Args[1:],
		demokit.ValueFlag("--record"),
		demokit.ValueFlag("--replay"),
		demokit.ValueFlag("--out"),
		demokit.ValueFlag("--input-timeout"),
	))

	if !common.IsDeployableProvider(*provider) {
		log.Fatalf("provider %q is not deployable (local has no SSH endpoint); set --provider or %s to runpod, vast, or lambdalabs", *provider, common.EnvProvider)
	}
	if err := common.EnsureProviderAPIKey(*provider); err != nil {
		log.Fatal(err)
	}

	// Build the CLI binary so iplane load drives the local checkout
	// (not whatever stale iplane sits on $PATH).
	iplane := *binPath
	if iplane == "" {
		built, err := buildIplane()
		if err != nil {
			fmt.Fprintf(os.Stderr, "build iplane: %v\n", err)
			os.Exit(1)
		}
		iplane = built
		defer os.RemoveAll(filepath.Dir(iplane))
	}

	provisionerClient := provisionerv1connect.NewProvisionerServiceClient(http.DefaultClient, *url)
	deploymentClient := provisionerv1connect.NewDeploymentServiceClient(http.DefaultClient, *url)

	// Cleanup tracks what THIS run created. Re-used deployments are
	// never auto-destroyed -- the cleanup closure leaves them alone so
	// demos 05 and 06 can land on the same pod. The destroy step at
	// the end is opt-in.
	var (
		mu              sync.Mutex
		createdDeploy   string
		createdInstance string
		cleanupCalled   bool
	)
	cleanup := func() {
		mu.Lock()
		defer mu.Unlock()
		if cleanupCalled {
			return
		}
		cleanupCalled = true
		if createdDeploy == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if _, err := deploymentClient.DestroyDeployment(ctx, connect.NewRequest(&provisionerv1.DestroyDeploymentRequest{Id: createdDeploy})); err != nil {
			fmt.Fprintf(os.Stderr, "WARN: cleanup DestroyDeployment(%s): %v\n", createdDeploy, err)
			fmt.Fprintln(os.Stderr, "Inspect / clean up manually: https://www.runpod.io/console/pods")
		} else {
			fmt.Fprintf(os.Stderr, "Terminated deployment %s (cleanup)\n", createdDeploy)
		}
		// Defensive instance destroy: image-as-pod auto-provision makes the
		// instance and deployment 1:1, so DestroyDeployment typically tears
		// down the pod already and this is a no-op. Still worth the call so
		// a partial-deploy crash that left an instance behind without a
		// deployment record doesn't orphan the pod.
		if createdInstance != "" {
			if _, err := provisionerClient.DestroyInstance(ctx, connect.NewRequest(&provisionerv1.DestroyInstanceRequest{Id: createdInstance})); err != nil {
				fmt.Fprintf(os.Stderr, "WARN: cleanup DestroyInstance(%s): %v\n", createdInstance, err)
				fmt.Fprintln(os.Stderr, "Inspect / clean up manually: https://www.runpod.io/console/pods")
			} else {
				fmt.Fprintf(os.Stderr, "Terminated instance %s (cleanup)\n", createdInstance)
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

	// Per-run state captured during steps. Set during pick-model and
	// the deployment-or-reuse step; read by every step downstream.
	var (
		chosenSize      string
		activeDeployID  string
		activeModel     string
		activeEngineURL string
		reusedExisting  bool
	)

	demo := demokit.New("Router in path (Beat 1 closer)").
		Description("Drive a chat completion through the v0.2 control plane router, fire synthetic load to populate the v0.2 Grafana dashboard, and walk the resulting Tempo trace. Operator runs `iplane serve` separately; the deployment is created with --no-idle-destroy so demos 05 and 06 can reuse it.").
		Dir("04-router-in-path").
		MaxSteps(40).
		Actors(
			demokit.Actor("Operator", "You"),
			demokit.Actor("Client", "this demo / iplane load"),
			demokit.Actor("iplane", "router + provisioner + deployment service on :8080"),
			demokit.Actor("State", "state.json owned by iplane serve"),
			demokit.Actor("RunPod", "GPU provider"),
			demokit.Actor("Engine", "vLLM on the rented pod"),
			demokit.Actor("OTel", "collector -> Tempo + Mimir"),
		)

	demo.Section("Setup",
		"This walkthrough assumes `iplane serve` is running in another terminal on :8080 and (optionally) `make up` is hosting the local observability stack on :3000 / :3200. The demo never starts those itself -- it expects them already up so the same `iplane serve` survives across demos 04/05/06.",
		fmt.Sprintf("Target URL:    %s", *url),
		fmt.Sprintf("Grafana:       %s", *grafanaURL),
		fmt.Sprintf("Tempo:         %s", *tempoURL),
		fmt.Sprintf("Provider:      %s", *provider),
		fmt.Sprintf("Load profile:  %.1f rps for %s after deploy is RUNNING", *loadRPS, *loadDuration),
		"Cost: ~$0.05 (1.5B default) up to ~$0.25 (7B). Pod stays alive by default (--no-idle-destroy) so 05/06 can reuse it; opt in to destroy at the end if you're done for the day.",
	)

	demo.Step("Check `iplane serve` is reachable").ID("ping").
		Note("ListDeployments is the cheapest call that touches the full daemon stack. If this fails, start `iplane serve` in another terminal and retry.").
		Arrow("Client", "iplane", "ListDeployments (empty filter)").
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := deploymentClient.ListDeployments(rctx, connect.NewRequest(&provisionerv1.ListDeploymentsRequest{})); err != nil {
				return abortDemo(cleanup, "cannot reach %s: %v (is `iplane serve` running?)", *url, err)
			}
			fmt.Printf("  iplane serve reachable at %s\n", *url)
			return nil
		})

	demo.Step("Probe the local observability stack").ID("obs-ping").
		Note("Grafana and Tempo back the panel/trace tour at the end. The demo only WARNS if either is unreachable -- you can still walk the chapter against a hosted Grafana Cloud or skip dashboards entirely.").
		Arrow("Client", "Grafana", "GET /api/health").
		Arrow("Client", "Tempo", "GET /ready").
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			pingHealth(*grafanaURL+"/api/health", "Grafana")
			pingHealth(*tempoURL+"/ready", "Tempo")
			return nil
		})

	demo.Step("Choose a model size").ID("pick-model").
		Note("Smallest default keeps the smoke read cheap. Pick 3B or 7B for a fuller dashboard story (more tokens/s, more interesting latency curves).").
		Input(demokit.Choice("1.5B", "3B", "7B").
			Named("size", "Model size to deploy").
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
			fmt.Printf("  cold-start:      %s\n", opt.coldStartHi)
			fmt.Printf("  est. cost:       ~$%.2f for a full run\n", opt.estCostUSD)
			return nil
		})

	demo.Step("Find or create a RUNNING deployment for the chosen model").ID("deploy").
		Note("Looks for a RUNNING deployment whose Model matches the choice from the previous step. If one exists, the demo reuses it (zero cost, zero wait). Otherwise CreateDeployment with --no-idle-destroy provisions a fresh pod; the pin ensures the reaper won't destroy it between demo runs.\n\nCLI form (if you want to create it by hand instead):\n  iplane deployment deploy demo-router-<stamp> --provider runpod --class small --image " + engineImage + " --model <chosen> --no-idle-destroy --service-url " + *url).
		Arrow("Client", "iplane", "ListDeployments (filter by model)").
		Arrow("Client", "iplane", "CreateDeployment(no-idle-destroy=true, wait=true) -- if no match").
		Arrow("iplane", "RunPod", "rent pod + start engine -- if no match").
		Arrow("iplane", "State", "patch RUNNING + engine_endpoint").
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			opt := modelOptions[chosenSize]
			lctx, lcancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer lcancel()
			listResp, err := deploymentClient.ListDeployments(lctx, connect.NewRequest(&provisionerv1.ListDeploymentsRequest{}))
			if err != nil {
				return abortDemo(cleanup, "ListDeployments: %v", err)
			}
			for _, d := range listResp.Msg.GetDeployments() {
				if d.GetModel() == opt.id && d.GetState() == provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
					activeDeployID = d.GetId()
					activeModel = d.GetModel()
					activeEngineURL = d.GetEngineEndpoint()
					reusedExisting = true
					fmt.Printf("  reusing existing %s (state=RUNNING)\n", activeDeployID)
					fmt.Printf("  model:           %s\n", activeModel)
					fmt.Printf("  engine endpoint: %s\n", activeEngineURL)
					if d.GetNoIdleDestroy() {
						fmt.Println("  reaper-safe:     --no-idle-destroy set")
					} else {
						fmt.Println("  note:            existing deployment does NOT have --no-idle-destroy set; reaper may evict it.")
					}
					return nil
				}
			}

			// No match -- provision fresh.
			deploymentID := "demo-router-" + time.Now().UTC().Format("20060102t150405")
			rctx, cancel := context.WithTimeout(context.Background(), time.Duration(opt.estDurationS)*time.Second+3*time.Minute)
			defer cancel()

			watchCtx, watchCancel := context.WithCancel(rctx)
			defer watchCancel()
			go streamDeployProgress(watchCtx, deploymentClient, deploymentID)

			depEnv := map[string]string{}
			if otelEndpoint := os.Getenv("IPLANE_OTEL_ENDPOINT"); otelEndpoint != "" {
				depEnv["OTEL_EXPORTER_OTLP_ENDPOINT"] = otelEndpoint
				depEnv["OTEL_EXPORTER_OTLP_PROTOCOL"] = "http/protobuf"
				if hdrs := os.Getenv("IPLANE_OTEL_HEADERS"); hdrs != "" {
					depEnv["OTEL_EXPORTER_OTLP_HEADERS"] = hdrs
				}
			}

			resp, err := deploymentClient.CreateDeployment(rctx, connect.NewRequest(&provisionerv1.CreateDeploymentRequest{
				Deployment: &provisionerv1.Deployment{
					Id:             deploymentID,
					Image:          engineImage,
					Model:          opt.id,
					EnginePort:     defaultEnginePort,
					Env:            depEnv,
					NoIdleDestroy:  true,
				},
				ReplicasSpec: []*provisionerv1.ReplicaSpec{{
					Provider:     *provider,
					Region:       *region,
					Requirements: &provisionerv1.ResourceRequirements{Class: provisioners.GPUClassSmall},
					Replicas:     1,
				}},
				Wait: true,
			}))
			watchCancel()
			if err != nil {
				return abortDemo(cleanup, "CreateDeployment: %v", err)
			}
			dep := resp.Msg.GetDeployment()
			if dep.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
				return abortDemo(cleanup, "deploy reached %s, want RUNNING (reason: %s)",
					dep.GetState(), dep.GetFailureReason())
			}
			mu.Lock()
			createdDeploy = dep.GetId()
			createdInstance = dep.GetInstanceId()
			mu.Unlock()
			activeDeployID = dep.GetId()
			activeModel = dep.GetModel()
			activeEngineURL = dep.GetEngineEndpoint()
			reusedExisting = false
			fmt.Printf("  created:         %s\n", activeDeployID)
			fmt.Printf("  on instance:     %s\n", dep.GetInstanceId())
			fmt.Printf("  model:           %s\n", activeModel)
			fmt.Printf("  engine endpoint: %s\n", activeEngineURL)
			fmt.Printf("  reaper-safe:     --no-idle-destroy=true\n")
			if ts := dep.GetReadyAt(); ts != nil {
				elapsed := ts.AsTime().Sub(dep.GetCreatedAt().AsTime())
				fmt.Printf("  cold-start:      %s\n", elapsed.Round(time.Second))
			}
			return nil
		})

	demo.Step("Sanity: GET /v1/models THROUGH the router (deploy-id URL)").ID("models").
		Note("The deploy-id URL shape (`/v1/<deploy-id>/v1/...`) is the unambiguous escape hatch for endpoints with no body to peek at. The router strips the `/v1/<deploy-id>` prefix and forwards the rest to the engine_endpoint. A 2xx here proves the router can reach the engine pod from the operator's laptop.\n\nThe alternative (`engine_endpoint` from describe + curl) bypasses the router entirely -- v0.1's story. v0.2's whole point is that the router IS in the path.").
		Arrow("Client", "iplane", "GET /v1/{id}/v1/models").
		Arrow("iplane", "Engine", "GET /v1/models").
		Arrow("Engine", "Client", "200 + served-model list").
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			modelsURL := strings.TrimRight(*url, "/") + "/v1/" + activeDeployID + "/v1/models"
			req, err := http.NewRequestWithContext(rctx, http.MethodGet, modelsURL, nil)
			if err != nil {
				return abortDemo(cleanup, "build request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return abortDemo(cleanup, "GET %s: %v", modelsURL, err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode/100 != 2 {
				return abortDemo(cleanup, "%s -> %d: %s", modelsURL, resp.StatusCode, strings.TrimSpace(string(body)))
			}
			var parsed struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			_ = json.Unmarshal(body, &parsed)
			fmt.Printf("  GET %s -> %d\n", modelsURL, resp.StatusCode)
			for _, m := range parsed.Data {
				fmt.Printf("  served model:    %s\n", m.ID)
			}
			return nil
		})

	demo.Step("One-shot chat through the FLAT URL (body-peek route)").ID("roundtrip").
		Note("The flat URL (`/v1/chat/completions`) is the OpenAI-SDK-compatible shape. The router reads the `model` field from the JSON body, picks the newest RUNNING deployment serving that model, and reverse-proxies the request. This is the URL the chapter's prose calls out: an OpenAI SDK pointed at http://localhost:8080 just works.").
		Arrow("Client", "iplane", "POST /v1/chat/completions {model: <id>}").
		Arrow("iplane", "Engine", "POST /v1/chat/completions (proxied)").
		Arrow("Engine", "iplane", "200 + completion + token usage").
		Arrow("iplane", "OTel", "router span + request_latency + completion_tokens").
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			rctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			reqBody := map[string]any{
				"model": activeModel,
				"messages": []map[string]string{
					{"role": "user", "content": "In two sentences: what is an inference plane?"},
				},
				"max_tokens":  120,
				"temperature": 0.4,
			}
			bodyBytes, _ := json.Marshal(reqBody)
			flatURL := strings.TrimRight(*url, "/") + "/v1/chat/completions"
			httpReq, err := http.NewRequestWithContext(rctx, http.MethodPost, flatURL, bytes.NewReader(bodyBytes))
			if err != nil {
				return abortDemo(cleanup, "build request: %v", err)
			}
			httpReq.Header.Set("Content-Type", "application/json")
			started := time.Now()
			resp, err := http.DefaultClient.Do(httpReq)
			if err != nil {
				return abortDemo(cleanup, "POST %s: %v", flatURL, err)
			}
			defer resp.Body.Close()
			respBytes, _ := io.ReadAll(resp.Body)
			elapsed := time.Since(started).Round(time.Millisecond)
			if resp.StatusCode/100 != 2 {
				return abortDemo(cleanup, "engine returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
			}
			var parsed struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
				Usage struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(respBytes, &parsed); err != nil || len(parsed.Choices) == 0 {
				return abortDemo(cleanup, "unparseable response: %s", strings.TrimSpace(string(respBytes)))
			}
			fmt.Printf("\n  > %s\n\n", strings.TrimSpace(parsed.Choices[0].Message.Content))
			fmt.Printf("  (%s · %d prompt + %d completion tokens · finish %s)\n",
				elapsed, parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, parsed.Choices[0].FinishReason)
			return nil
		})

	demo.Step("Fire synthetic traffic so the dashboard has something to draw").ID("load").
		Note("Runs `iplane load` against the flat URL for the configured duration. The router's request/latency/token metrics populate the v0.2 dashboard panels; the trace per request flows through to Tempo. Open Grafana in a browser BEFORE this step starts if you want to watch the panels paint live.").
		Arrow("Operator", "CLI", "iplane load --url --rps --duration --model").
		Arrow("CLI", "iplane", "constant-rate POST /v1/chat/completions").
		Arrow("iplane", "Engine", "proxied requests").
		Arrow("iplane", "OTel", "request_total / latency_seconds / completion_tokens_total").
		Shell(fmt.Sprintf("%s load --url=%s --rps=%.1f --duration=%s --model=%s --max-tokens=80 --chat-fraction=1.0",
			iplane, *url, *loadRPS, *loadDuration, activeModel)).
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			rctx, cancel := context.WithTimeout(context.Background(), *loadDuration+30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(rctx, iplane, "load",
				"--url="+*url,
				fmt.Sprintf("--rps=%.1f", *loadRPS),
				"--duration="+(*loadDuration).String(),
				"--model="+activeModel,
				"--max-tokens=80",
				"--chat-fraction=1.0",
			)
			cmd.Env = os.Environ()
			var buf bytes.Buffer
			cmd.Stdout = &buf
			cmd.Stderr = &buf
			err := cmd.Run()
			fmt.Println(indentBlock(buf.String()))
			if err != nil {
				return abortDemo(cleanup, "iplane load failed: %v", err)
			}
			return nil
		})

	demo.Step("Tour the v0.2 dashboard").ID("panels").
		Note("The v0.2 dashboard (uid=inference-plane-v02) has three panels populated by what you just fired:\n\n  - Request rate (req/s, per deploy_id)\n  - Request latency (p50 / p95 / p99)\n  - Completion tokens / sec (per deploy_id)\n\nAll three are deploy_id-scoped -- once demos 05/06 add a second deployment, the panels will split by id and the queueing/replica stories will be visible at a glance.").
		Arrow("Operator", "Grafana", "open dashboard inference-plane-v02").
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			fmt.Printf("  dashboard:       %s/d/inference-plane-v02\n", strings.TrimRight(*grafanaURL, "/"))
			fmt.Printf("  deploy_id:       %s\n", activeDeployID)
			fmt.Printf("  filter panels by `deploy_id=\"%s\"` if other demos have left noise behind.\n", activeDeployID)
			return nil
		})

	demo.Step("Walk a Tempo trace").ID("trace").
		Note("Each routed request becomes a trace with one root span on the router side (`iplane.router`) and a child span on the engine side (`engine.generate`). The router span carries deploy_id, tenant_id (if set by client baggage), and the request size; the engine span carries token counts. The chapter's trace narrative reads top-to-bottom from this pair.").
		Arrow("Operator", "Tempo", "search service.name = iplane (last 15m)").
		Arrow("Operator", "Tempo", "open one trace -> router -> engine span pair").
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			fmt.Printf("  Tempo (direct):  %s\n", strings.TrimRight(*tempoURL, "/"))
			fmt.Printf("  Grafana Explore: %s/explore?left=%%5B%%22now-15m%%22,%%22now%%22,%%22tempo%%22,%%7B%%22query%%22:%%22%%7B.deploy_id%%3D%%5C%%22%s%%5C%%22%%7D%%22%%7D%%5D\n",
				strings.TrimRight(*grafanaURL, "/"), activeDeployID)
			fmt.Println("  Look for: a parent span on iplane.router with a child on engine.generate; both share deploy_id; baggage propagates if the client set tenant_id.")
			return nil
		})

	demo.Step("Leave the deployment running, or destroy it now").ID("cleanup").
		Note("Default: LEAVE it running. The deployment was created with --no-idle-destroy so the reaper will not evict it; demos 05 (fair-queueing) and 06 (multi-replica) will attach to the same daemon and reuse this deployment.\n\nIf you're done for the day or running this demo standalone, pick `destroy` -- billing stops as soon as the pod terminates.\n\nReused-existing deployments are never auto-destroyed here even if you pick `destroy`; the demo only tears down what THIS run created, on the assumption that an existing reusable deployment belongs to a longer-lived workflow.").
		Input(demokit.Choice("leave", "destroy").
			Named("action", "Leave the deployment for demos 05/06, or destroy it now?").
			WithDefault("leave")).
		Arrow("Operator", "iplane", "DestroyDeployment (only if action=destroy)").
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			action, _ := ctx.Inputs["action"].(string)
			if action == "" {
				action = "leave"
			}
			if action == "leave" {
				fmt.Printf("  leaving %s running (--no-idle-destroy holds the pin)\n", activeDeployID)
				if reusedExisting {
					fmt.Println("  this run reused an existing deployment; nothing to clean.")
				}
				mu.Lock()
				createdDeploy = ""
				createdInstance = ""
				cleanupCalled = true
				mu.Unlock()
				return nil
			}

			if reusedExisting {
				fmt.Printf("  NOT destroying %s: reused-existing deployments are kept alive even on `destroy` here.\n", activeDeployID)
				fmt.Println("  Run `iplane deployment destroy " + activeDeployID + " --service-url " + *url + "` if you really want it gone.")
				return nil
			}

			rctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			resp, err := deploymentClient.DestroyDeployment(rctx, connect.NewRequest(&provisionerv1.DestroyDeploymentRequest{Id: activeDeployID}))
			if err != nil {
				return abortDemo(cleanup, "DestroyDeployment: %v", err)
			}
			fmt.Printf("  final state:     %s\n", resp.Msg.GetDeployment().GetState())
			mu.Lock()
			createdDeploy = ""
			createdInstance = ""
			cleanupCalled = true
			mu.Unlock()
			return nil
		})

	demo.Section("Done",
		"Beat 1 closer complete: a request crossed the router, populated the v0.2 dashboard, and laid down a router+engine span pair in Tempo. The router code lives in `internal/router/`; the metrics names live in `metric-names.yaml`; the trace propagation contract is W3C TraceContext + Baggage.",
		"Re-runnable: bring up `iplane serve` once and run this demo again; the detect-and-reuse step on `deploy` will skip provisioning. Demos 05 and 06 (Beat 2 and Beat 3) attach to the same daemon and the same deployment.",
	)

	common.SetupRenderer(demo)

	demo.Execute()
}

// pingHealth GETs a health URL and prints "ok" / "warn" without aborting.
// Used for soft observability-stack pre-flight: missing Grafana is
// inconvenient but not fatal.
func pingHealth(url, label string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Printf("  %-9s WARN: build request: %v\n", label+":", err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("  %-9s unreachable at %s (panel/trace tour will only print pointers)\n", label+":", url)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		fmt.Printf("  %-9s %d at %s (panel/trace tour will only print pointers)\n", label+":", resp.StatusCode, url)
		return
	}
	fmt.Printf("  %-9s ok (%s)\n", label+":", url)
}

// streamDeployProgress mirrors the helper in 03-deploy-end-to-end: subscribes
// to WatchDeployment and prints each progress_message change while the main
// goroutine blocks on CreateDeployment{Wait: true}. Best-effort; silently
// returns on stream error.
func streamDeployProgress(ctx context.Context, client provisionerv1connect.DeploymentServiceClient, deploymentID string) {
	stream, err := client.WatchDeployment(ctx, connect.NewRequest(&provisionerv1.WatchDeploymentRequest{Id: deploymentID}))
	if err != nil {
		return
	}
	for stream.Receive() {
		ev := stream.Msg()
		phase := ev.GetPhase()
		msg := ev.GetProgressMessage()
		switch {
		case msg != "":
			fmt.Printf("  ... %s\n", msg)
		case phase != "":
			fmt.Printf("  ... %s\n", phase)
		}
	}
}

// abortDemo is the fail-fast helper -- same shape as 03-deploy-end-to-end.
// Runs cleanup, prints the failure to stderr, and exits non-zero.
func abortDemo(cleanup func(), format string, args ...any) *demokit.StepResult {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "\n\nStep failed: %s\n", msg)
	if cleanup != nil {
		cleanup()
	}
	os.Exit(1)
	return nil
}

// buildIplane compiles the local checkout's iplane binary into a temp dir.
// Resolves the cmd/iplane path from the source file's location via
// runtime.Caller so the build works regardless of the operator's cwd
// (running via `make demo`, `go run ./examples/04-router-in-path`, or
// directly via the built binary all work).
func buildIplane() (string, error) {
	_, srcFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed; cannot locate cmd/iplane")
	}
	cmdDir := filepath.Join(filepath.Dir(srcFile), "..", "..", "cmd", "iplane")
	dir, err := os.MkdirTemp("", "iplane-router-example-*")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "iplane")
	cmd := exec.Command("go", "build", "-o", bin, cmdDir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("go build: %w", err)
	}
	return bin, nil
}

// indentBlock prefixes every line with two spaces so embedded command
// output reads as a quoted block in the demokit transcript.
func indentBlock(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

