// Example: end-to-end provisioning walkthrough driving the iplane CLI
// binary (no separate server, no gRPC client construction). This is
// the operator-terminal path: what the chapter reader actually types.
//
// Sibling to 01-end-to-end/, which exercises the gRPC client surface.
// Both walkthroughs land the same lifecycle (create -> describe ->
// idempotent re-create -> list -> destroy); the difference is the
// caller. v0.1 supports both as first-class transports (see
// cmd/iplane/cmd/instance.go's provisionerClient interface).
//
// Run:
//
//	# Local (zero cost, no API key)
//	make demo
//
//	# RunPod (~$0.02 per run, requires RUNPOD_API_KEY)
//	make demo PROVIDER=runpod
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/panyam/demokit"
	"github.com/panyam/demokit/tui"

	"github.com/inference-book/inference-plane/internal/provisioners"
)

func main() {
	provider := flag.String("provider", provisioners.ProviderLocal, "provider to use (local | runpod)")
	stateDir := flag.String("state-dir", "/tmp/iplane-cli-example", "state file directory (state.json + .lock land here)")
	binPath := flag.String("bin", "", "path to a prebuilt iplane binary; built from source if empty")
	flag.CommandLine.Parse(demokit.FilterArgs(os.Args[1:],
		demokit.ValueFlag("--record"),
		demokit.ValueFlag("--replay"),
		demokit.ValueFlag("--out"),
		demokit.ValueFlag("--input-timeout"),
	))

	if *provider == provisioners.ProviderRunPod && os.Getenv("RUNPOD_API_KEY") == "" {
		fmt.Fprintln(os.Stderr, "RUNPOD_API_KEY required for --provider runpod. Aborting.")
		os.Exit(2)
	}

	// Build the CLI binary into a temp location so the demo is
	// guaranteed to drive the local checkout (not whatever stale
	// iplane is on $PATH). Skipped if --bin is supplied.
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

	// Stable id (not time-stamped) so the demoID embedded in the
	// Shell/VerbatimVariants snippets matches the demoID the recorded
	// run used. With a time-based id, `--doc md --from <recording>`
	// rebuilds the demo with a FRESH id while reading captured output
	// referencing the OLD id, and the two drift visibly in RUN.md.
	// Same-id reruns are safe: the Service's idempotency on PENDING/
	// ACTIVE returns the existing record, and the destroy step leaves
	// behind a TERMINATED record that does not block a re-create.
	demoID := "cli-demo"

	// Cleanup tracking. We always defer-terminate via the destroy
	// step, but if anything aborts mid-flow (Ctrl-C, demokit panic),
	// the signal handler fires `iplane instance destroy` so the pod
	// does not orphan.
	var (
		mu             sync.Mutex
		createdID      string
		cleanupCalled  bool
	)
	cleanup := func() {
		mu.Lock()
		defer mu.Unlock()
		if cleanupCalled || createdID == "" {
			return
		}
		cleanupCalled = true
		fmt.Fprintf(os.Stderr, "\nCleaning up %s ...\n", createdID)
		out, err := runIplane(iplane, *stateDir, "instance", "destroy", createdID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARN: cleanup destroy failed: %v\n%s\n", err, out)
			if *provider == provisioners.ProviderRunPod {
				fmt.Fprintln(os.Stderr, "Inspect / clean up manually: https://www.runpod.io/console/pods")
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

	costNote := "Zero cost (local provider; the laptop provisions itself, no API calls leave the machine)."
	if *provider == provisioners.ProviderRunPod {
		costNote = "Cost ~$0.02 per run (one small-class RunPod pod up for ~60 seconds at metered rates)."
	}

	demo := demokit.New("iplane instance CLI end-to-end").
		Description("Walk through the v0.1 provisioner lifecycle from the operator's terminal -- the `iplane instance` command set. " + costNote).
		Dir("02-cli-end-to-end").
		MaxSteps(30).
		Actors(
			demokit.Actor("Operator", "You (terminal)"),
			demokit.Actor("CLI", "iplane instance"),
			demokit.Actor("State", filepath.Join(*stateDir, "state.json")),
			demokit.Actor("Provider", fmt.Sprintf("Provider adapter (%s)", *provider)),
		)

	demo.Section("Setup",
		"This walkthrough drives the iplane binary directly -- no separate `iplane serve` running. The CLI opens the state file under flock, instantiates provider adapters in-process, and prints the result.",
		fmt.Sprintf("Binary:    %s", iplane),
		fmt.Sprintf("Provider:  %s", *provider),
		fmt.Sprintf("State dir: %s", *stateDir),
		fmt.Sprintf("Demo id:   %s (class=small)", demoID),
		"All commands are what you would type in a real shell. Defer-terminates on exit or Ctrl-C.",
	)

	demo.Step("Check the CLI is wired").ID("ping").
		Arrow("Operator", "CLI", "list (empty state)").
		Arrow("CLI", "State", "open + read").
		Shell(fmt.Sprintf("iplane instance list --state-dir %s", *stateDir)).
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			out, err := runIplane(iplane, *stateDir, "instance", "list")
			if err != nil {
				return demokit.Errf("list: %v\n%s", err, out)
			}
			fmt.Println(indentBlock(string(out)))
			return nil
		})

	demo.Step("Create with --class small").ID("create").
		Note(
			"The Service exposes three layers of resource specification (see docs/design/0001-provisioner.md). The walkthrough actually runs the class shorthand below; the variant block also shows the numeric-constraints form and the exact-SKU escape hatch so you can see what each layer expands to.",
			"",
			"class=small expands server-side to min_vram_gb=24 / min_disk_gb=20 / min_ram_gb=16. The "+*provider+" resolver picks the cheapest SKU that satisfies those constraints.",
		).
		Arrow("Operator", "CLI", "create "+demoID).
		Arrow("CLI", "State", "write PENDING").
		Arrow("CLI", "Provider", "Spawn (idempotency lookup first)").
		Arrow("CLI", "State", "patch to ACTIVE").
		VerbatimVariants("Three ways to ask for the same shape",
			demokit.MakeVariant("class shorthand", "bash",
				fmt.Sprintf("iplane instance create %s %s --class small", *provider, demoID)).Default(),
			demokit.MakeVariant("numeric constraints", "bash",
				fmt.Sprintf("iplane instance create %s %s --min-vram-gb 24 --min-ram-gb 16 --min-disk-gb 20", *provider, demoID)),
			demokit.MakeVariant("exact SKU (escape hatch)", "bash",
				fmt.Sprintf("iplane instance create %s %s --sku \"NVIDIA RTX A5000\"", *provider, demoID)),
		).
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			out, err := runIplane(iplane, *stateDir,
				"instance", "create", *provider, demoID,
				"--class", "small",
			)
			if err != nil {
				return demokit.Errf("create: %v\n%s", err, out)
			}
			fmt.Println(indentBlock(string(out)))
			mu.Lock()
			createdID = demoID
			mu.Unlock()
			return nil
		})

	demo.Step("Describe (state-file source)").ID("describe").
		Arrow("Operator", "CLI", "describe "+demoID).
		Arrow("CLI", "State", "read").
		VerbatimVariants("Describe sources",
			demokit.MakeVariant("state file (default)", "bash",
				fmt.Sprintf("iplane instance describe %s", demoID)).Default(),
			demokit.MakeVariant("provider (live)", "bash",
				fmt.Sprintf("iplane instance describe %s --remote", demoID)),
		).
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			out, err := runIplane(iplane, *stateDir, "instance", "describe", demoID)
			if err != nil {
				return demokit.Errf("describe: %v\n%s", err, out)
			}
			fmt.Println(indentBlock(string(out)))
			return nil
		})

	demo.Step("Idempotent re-create").ID("idempotent").
		Note("Same id; the Service hits its state-file cache, finds an ACTIVE record, returns it. Zero provider calls. Safe to rerun a CLI command without leaking duplicates.").
		Arrow("Operator", "CLI", "create "+demoID+" (rerun)").
		Arrow("CLI", "State", "read; ACTIVE match found").
		Shell(fmt.Sprintf("iplane instance create %s %s --class small", *provider, demoID)).
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			out, err := runIplane(iplane, *stateDir,
				"instance", "create", *provider, demoID,
				"--class", "small",
			)
			if err != nil {
				return demokit.Errf("create (idempotent): %v\n%s", err, out)
			}
			if !strings.Contains(string(out), "Found existing") {
				return demokit.Errf("expected 'Found existing' on rerun; got:\n%s", out)
			}
			fmt.Println(indentBlock(string(out)))
			return nil
		})

	demo.Step("List (state-file source)").ID("list-local").
		Arrow("Operator", "CLI", "list").
		Arrow("CLI", "State", "read").
		VerbatimVariants("List sources",
			demokit.MakeVariant("state file (default)", "bash",
				"iplane instance list").Default(),
			demokit.MakeVariant("provider (live, per-provider)", "bash",
				fmt.Sprintf("iplane instance list --remote --provider %s", *provider)),
		).
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			out, err := runIplane(iplane, *stateDir, "instance", "list")
			if err != nil {
				return demokit.Errf("list: %v\n%s", err, out)
			}
			fmt.Println(indentBlock(string(out)))
			return nil
		})

	if *provider != provisioners.ProviderLocal {
		demo.Step("List --remote (query "+*provider+" directly)").ID("list-remote").
			Note("Calls the provider's List with the iplane-operator tag filter. Expect EMPTY against RunPod in v0.1: the adapter only encodes iplane-id (in the pod name), so the operator-tag filter matches nothing. v0.2 onwards will add template-based tag stamping and this step will return the active record.").
			Arrow("Operator", "CLI", "list --remote").
			Arrow("CLI", "Provider", "List with iplane-operator tag filter").
			Shell(fmt.Sprintf("iplane instance list --remote --provider %s", *provider)).
			Run(func(ctx demokit.StepContext) *demokit.StepResult {
				out, err := runIplane(iplane, *stateDir,
					"instance", "list",
					"--remote",
					"--provider", *provider,
				)
				if err != nil {
					return demokit.Errf("list --remote: %v\n%s", err, out)
				}
				fmt.Println(indentBlock(string(out)))
				return nil
			})
	}

	demo.Step("Destroy").ID("destroy").
		Arrow("Operator", "CLI", "destroy "+demoID).
		Arrow("CLI", "State", "patch to TERMINATING").
		Arrow("CLI", "Provider", "Terminate").
		Arrow("CLI", "State", "patch to TERMINATED").
		VerbatimVariants("Destroy options",
			demokit.MakeVariant("normal", "bash",
				fmt.Sprintf("iplane instance destroy %s", demoID)).Default(),
			demokit.MakeVariant("--force (skip provider call for recovery)", "bash",
				fmt.Sprintf("iplane instance destroy %s --force", demoID)),
		).
		Run(func(ctx demokit.StepContext) *demokit.StepResult {
			out, err := runIplane(iplane, *stateDir, "instance", "destroy", demoID)
			if err != nil {
				return demokit.Errf("destroy: %v\n%s", err, out)
			}
			fmt.Println(indentBlock(string(out)))
			mu.Lock()
			createdID = "" // already cleaned; defer cleanup is a no-op
			cleanupCalled = true
			mu.Unlock()
			return nil
		})

	demo.Section("Done",
		"Instance terminated. The state file at "+filepath.Join(*stateDir, "state.json")+" carries the audit record (state=TERMINATED) -- rerunning the demo with the same id reuses the slot.",
		"Two transports exercise the same Service contract: this walkthrough drives the CLI; 01-end-to-end drives the gRPC client. Operators pick the one that fits their workflow.",
	)

	if demokit.IsTUI() {
		demo.WithRenderer(tui.New())
	}

	demo.Execute()
}

// runIplane invokes the built iplane binary with --state-dir prepended
// to whatever positional verb the caller passed. Returns combined
// stdout+stderr so the demo step can render the actual operator-visible
// output.
func runIplane(bin, stateDir string, args ...string) ([]byte, error) {
	// state-dir is a persistent flag on `iplane instance`, so it must
	// land between "instance" and the verb. The caller passes
	// ("instance", "create", ...); we splice --state-dir after the
	// first arg.
	if len(args) < 2 || args[0] != "instance" {
		return nil, fmt.Errorf("runIplane expects args starting with \"instance\"; got %v", args)
	}
	spliced := append([]string{args[0], "--state-dir", stateDir}, args[1:]...)
	cmd := exec.Command(bin, spliced...)
	cmd.Env = os.Environ()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.Bytes(), err
}

// buildIplane compiles the local checkout's iplane binary into a temp
// dir. Returns the absolute path. Caller is responsible for cleanup
// (os.RemoveAll on the parent dir).
func buildIplane() (string, error) {
	dir, err := os.MkdirTemp("", "iplane-cli-example-*")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "iplane")
	cmd := exec.Command("go", "build", "-o", bin, "../../../cmd/iplane")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("go build: %w", err)
	}
	return bin, nil
}

// indentBlock prefixes every line with two spaces so the CLI output
// reads as a quoted block inside the demokit step transcript.
func indentBlock(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		lines[i] = "  " + line
	}
	return strings.Join(lines, "\n")
}
