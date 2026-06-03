// Example: end-to-end provisioning walkthrough.
//
// Two-process architecture (mirrors the mcpkit pattern):
//
//	Terminal 1:  make serve     # iplane provisioner service on :9091
//	Terminal 2:  make demo      # demokit walkthrough as one client
//
// The serve side mounts the ProvisionerService on a connect/grpc handler
// so ANY client can hit it -- the demo is one example. Point your own
// gRPC tooling at :9091 to exercise the same surface independently.
//
// Two providers wired by default:
//
//   - local  : provisions against the operator's laptop. Zero cost, no
//              API key required. Default for the demo.
//   - runpod : provisions a real RunPod pod (~$0.02 per run). Requires
//              RUNPOD_API_KEY in the SERVER's env.
//
// Switch with `--provider runpod` on the demo side. The server registers
// both providers; whichever the demo names is the one that gets hit.
package main

import (
	"context"
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
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/local"
	"github.com/inference-book/inference-plane/internal/provisioners/runpod"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
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
	stateDir := flag.String("state-dir", "/tmp/iplane-example", "state file directory (state.json + .lock land here)")
	operatorID := flag.String("operator", "default", "operator id stamped on instances")
	flag.CommandLine.Parse(demokit.FilterArgs(os.Args[1:], demokit.BoolFlag("--serve")))

	store, err := file.Open(*stateDir, *operatorID)
	if err != nil {
		log.Fatalf("file.Open: %v", err)
	}

	providers := []provisioners.Provider{local.New()}
	apiKey := os.Getenv("RUNPOD_API_KEY")
	if apiKey != "" {
		providers = append(providers, runpod.New(runpod.NewClient(apiKey)))
	}

	svc := provisioners.New(providers, store, *operatorID)

	mux := http.NewServeMux()
	path, handler := provisionerv1connect.NewProvisionerServiceHandler(provisioners.NewConnectProvisionerAdapter(svc))
	mux.Handle(path, handler)

	fmt.Printf("iplane provisioner serving on %s\n", *addr)
	fmt.Printf("  state file:   %s/state.json\n", *stateDir)
	fmt.Printf("  operator:     %s\n", *operatorID)
	fmt.Print("  providers:    local")
	if apiKey != "" {
		fmt.Print(", runpod")
	} else {
		fmt.Print("  (set RUNPOD_API_KEY to also register runpod)")
	}
	fmt.Println()
	fmt.Println("Try the demo: go run . --tui [--provider local|runpod]")
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

// ── Demo side ─────────────────────────────────────────────────────────

func runDemo() {
	url := flag.String("url", "http://localhost:9091", "provisioner service URL")
	provider := flag.String("provider", provisioners.ProviderLocal, "provider to use (local | runpod)")
	region := flag.String("region", "", "region (default: laptop for local, unpinned for runpod -- pass an explicit datacenter like US-WA-1 to pin)")
	// demokit's built-in FilterArgs only strips --tui / --non-interactive
	// / --doc / --from. Explicitly strip the other built-ins so stdlib
	// flag.Parse doesn't choke when callers pass them.
	flag.CommandLine.Parse(demokit.FilterArgs(os.Args[1:],
		demokit.ValueFlag("--record"),
		demokit.ValueFlag("--replay"),
		demokit.ValueFlag("--out"),
		demokit.ValueFlag("--input-timeout"),
	))

	if *region == "" {
		switch *provider {
		case provisioners.ProviderRunPod:
			// Leave empty -- the adapter will not pin dataCenterIds, so
			// RunPod schedules wherever capacity exists for the
			// requested gpuTypeIds. Operators who want to pin a specific
			// datacenter (latency-sensitive workloads, data-residency)
			// pass --region US-WA-1 explicitly.
		default:
			*region = "laptop"
		}
	}

	client := provisionerv1connect.NewProvisionerServiceClient(http.DefaultClient, *url)

	// Cleanup tracking. Any spawned instance's iplane id lands here so
	// the signal handler and deferred cleanup can destroy it. Ctrl-C
	// triggers cleanup before exit; otherwise the demo's destroy step
	// handles it.
	var (
		mu             sync.Mutex
		spawnedID      string
		cleanupCalled bool
	)
	cleanup := func() {
		mu.Lock()
		defer mu.Unlock()
		if cleanupCalled || spawnedID == "" {
			return
		}
		cleanupCalled = true
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, err := client.DestroyInstance(ctx, connect.NewRequest(&provisionerv1.DestroyInstanceRequest{Id: spawnedID}))
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nWARN: cleanup terminate failed: %v\n", err)
			if *provider == provisioners.ProviderRunPod {
				fmt.Fprintln(os.Stderr, "Inspect / clean up manually: https://www.runpod.io/console/pods")
			}
		} else {
			fmt.Fprintf(os.Stderr, "\nTerminated %s (cleanup)\n", spawnedID)
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

	demoID := "demo-" + time.Now().UTC().Format("20060102t150405")

	costNote := "Zero cost (local provider; the laptop provisions itself, no API calls leave the machine)."
	if *provider == provisioners.ProviderRunPod {
		costNote = "Cost ~$0.02 per run (one small-class pod up for ~60 seconds at RunPod metered rates)."
	}

	demo := demokit.New("Provisioner end-to-end").
		Description("Walk through the v0.1 provisioner Service end-to-end. " + costNote).
		Dir("01-end-to-end").
		MaxSteps(30).
		Actors(
			demokit.Actor("Operator", "You"),
			demokit.Actor("iplane", "Provisioner Service"),
			demokit.Actor("State", "~/.iplane/state.json"),
			demokit.Actor("Provider", fmt.Sprintf("Provider adapter (%s)", *provider)),
		)

	demo.Section("Setup",
		"The provisioner Service is a connect-rpc handler. This demo connects via a generated ProvisionerServiceClient -- the same client the CLI (phase 1.4) will use.",
		fmt.Sprintf("Target URL:  %s", *url),
		fmt.Sprintf("Provider:    %s", *provider),
		fmt.Sprintf("Region:      %s", regionLabel(*region)),
		fmt.Sprintf("Spawning id: %s with class=small (cheapest matching SKU)", demoID),
		"All operations idempotent; defer-terminates on exit or Ctrl-C.",
	)

	demo.Step("Check the service is reachable").ID("ping").
		Arrow("Operator", "iplane", "ListInstances (empty filter, source=local)").
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := client.ListInstances(rctx, connect.NewRequest(&provisionerv1.ListInstancesRequest{}))
			if err != nil {
				return abortDemo(cleanup, "cannot reach %s: %v (is `make serve` running?)", *url, err)
			}
			fmt.Println("  service reachable")
			return nil
		})

	demo.Step("Create with class=small shorthand").ID("create").
		Note("class=small expands to min_vram_gb=24, min_disk_gb=20, min_ram_gb=16 server-side. The "+*provider+" resolver picks the cheapest SKU satisfying those constraints.").
		Arrow("Operator", "iplane", "CreateInstance{id="+demoID+", class=small, provider="+*provider+"}").
		Arrow("iplane", "State", "write pending").
		Arrow("iplane", "Provider", "idempotency lookup + spawn").
		Arrow("iplane", "State", "patch to active").
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			rctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			resp, err := client.CreateInstance(rctx, connect.NewRequest(&provisionerv1.CreateInstanceRequest{
				Spec: &provisionerv1.Spec{
					Id:       demoID,
					Provider: *provider,
					Region:   *region,
					Requirements: &provisionerv1.ResourceRequirements{
						Class: provisioners.GPUClassSmall,
					},
				},
			}))
			if err != nil {
				return abortDemo(cleanup, "CreateInstance: %v", err)
			}
			inst := resp.Msg.GetInstance()
			mu.Lock()
			spawnedID = inst.GetId()
			mu.Unlock()
			fmt.Printf("  iplane id:       %s\n", inst.GetId())
			fmt.Printf("  provider id:     %s\n", inst.GetProviderId())
			fmt.Printf("  state:           %s\n", inst.GetState())
			fmt.Printf("  resolved SKU:    %s\n", inst.GetHardware().GetGpuSku())
			fmt.Printf("  hourly rate:     $%.4f/hr\n", inst.GetHourlyRateUsd())
			fmt.Printf("  already existed: %v\n", resp.Msg.GetAlreadyExisted())
			return nil
		})

	demo.Step("Describe (local view)").ID("describe").
		Arrow("Operator", "iplane", "DescribeInstance{id, source=local}").
		Arrow("iplane", "State", "read").
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			resp, err := client.DescribeInstance(rctx, connect.NewRequest(&provisionerv1.DescribeInstanceRequest{
				Id:     demoID,
				Source: provisionerv1.Source_SOURCE_LOCAL,
			}))
			if err != nil {
				return abortDemo(cleanup, "DescribeInstance: %v", err)
			}
			inst := resp.Msg.GetInstance()
			fmt.Printf("  state file says: state=%s, gpu=%s (%d MB), rate=$%.4f/hr\n",
				inst.GetState(), inst.GetHardware().GetGpuSku(), inst.GetHardware().GetGpuVramMb(), inst.GetHourlyRateUsd())
			return nil
		})

	demo.Step("Idempotent re-create").ID("idempotent").
		Note("Same spec.id; the service hits its local state cache, sees an active record, returns it. Zero provider calls. This is the safety the abstraction promised -- up-arrow + Enter cannot leak a duplicate instance.").
		Arrow("Operator", "iplane", "CreateInstance{id="+demoID+", ...} (same spec)").
		Arrow("iplane", "State", "read; pending/active match found").
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			resp, err := client.CreateInstance(rctx, connect.NewRequest(&provisionerv1.CreateInstanceRequest{
				Spec: &provisionerv1.Spec{
					Id:       demoID,
					Provider: *provider,
					Region:   *region,
					Requirements: &provisionerv1.ResourceRequirements{
						Class: provisioners.GPUClassSmall,
					},
				},
			}))
			if err != nil {
				return abortDemo(cleanup, "CreateInstance (idempotent): %v", err)
			}
			fmt.Printf("  already_existed = %v (no provider call)\n", resp.Msg.GetAlreadyExisted())
			return nil
		})

	demo.Step("List (local source)").ID("list-local").
		Arrow("Operator", "iplane", "ListInstances{source=local}").
		Arrow("iplane", "State", "read").
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			resp, err := client.ListInstances(rctx, connect.NewRequest(&provisionerv1.ListInstancesRequest{}))
			if err != nil {
				return abortDemo(cleanup, "ListInstances: %v", err)
			}
			fmt.Printf("  local state: %d record(s)\n", len(resp.Msg.GetInstances()))
			for _, inst := range resp.Msg.GetInstances() {
				fmt.Printf("    - %s @ %s  state=%s  $%.4f/hr\n",
					inst.GetId(), inst.GetProvider(), inst.GetState(), inst.GetHourlyRateUsd())
			}
			return nil
		})

	// SOURCE_REMOTE only makes sense for providers that have a separate
	// provider-side registry. Local has no remote view -- the state
	// file is the only truth.
	if *provider != provisioners.ProviderLocal {
		demo.Step("List (remote source — query "+*provider+" directly)").ID("list-remote").
			Note("Confirms the iplane-id tag we stamped at Spawn time is server-side visible -- the recovery mechanism for a wiped state file is exactly this query.").
			Arrow("Operator", "iplane", "ListInstances{source=remote, provider="+*provider+"}").
			Arrow("iplane", "Provider", "List with iplane-id tag filter").
			Run(func(ctx demokit.StepContext) *demokit.StepResult {
				rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				resp, err := client.ListInstances(rctx, connect.NewRequest(&provisionerv1.ListInstancesRequest{
					Source:   provisionerv1.Source_SOURCE_REMOTE,
					Provider: *provider,
				}))
				if err != nil {
					return abortDemo(cleanup, "ListInstances (remote): %v", err)
				}
				fmt.Printf("  %s sees: %d instance(s) under this operator\n", *provider, len(resp.Msg.GetInstances()))
				for _, inst := range resp.Msg.GetInstances() {
					fmt.Printf("    - provider_id=%s state=%s\n", inst.GetProviderId(), inst.GetState())
				}
				return nil
			})
	}

	demo.Step("Destroy").ID("destroy").
		Arrow("Operator", "iplane", "DestroyInstance{id="+demoID+"}").
		Arrow("iplane", "State", "patch to terminating").
		Arrow("iplane", "Provider", "terminate").
		Arrow("iplane", "State", "patch to terminated").
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			rctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			resp, err := client.DestroyInstance(rctx, connect.NewRequest(&provisionerv1.DestroyInstanceRequest{Id: demoID}))
			if err != nil {
				return abortDemo(cleanup, "DestroyInstance: %v", err)
			}
			fmt.Printf("  final state: %s (terminated_at=%s)\n",
				resp.Msg.GetInstance().GetState(),
				resp.Msg.GetInstance().GetTerminatedAt().AsTime().Format("15:04:05Z"))
			mu.Lock()
			spawnedID = "" // mark cleaned up so cleanup() / defer is a no-op
			cleanupCalled = true
			mu.Unlock()
			return nil
		})

	demo.Section("Done",
		"Instance terminated. State file at the server's --state-dir holds the audit record (state=terminated).",
		"Re-running this demo with the same id reuses the terminated record's slot (id is reusable; idempotency adoption only fires for pending/active records).",
	)

	if demokit.IsTUI() {
		demo.WithRenderer(tui.New())
	}

	demo.Execute()
}

// regionLabel renders empty region as "(unpinned)" for the Setup block.
// An empty value here is meaningful -- the runpod default -- not a bug.
func regionLabel(r string) string {
	if r == "" {
		return "(unpinned)"
	}
	return r
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
