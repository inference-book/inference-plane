// Package local implements the Provider interface against the operator's
// laptop. It is the zero-cost on-ramp for the chapter audience and the
// second implementation that proves the Provider abstraction holds with
// N=2 from day one -- single-implementation interfaces lie.
//
// The local provider has no persistent provider-side state. Spawn
// validates the spec, returns an Instance describing the laptop, and
// has no side effect of its own (no process started, no port bound).
// Terminate is a no-op. Describe and List return empty because there
// is no provider-side registry to query -- the iplane state file is
// the only persistent record of a "local instance."
//
// Heavy lifting (running an engine container on the laptop) happens
// in phase 2's deploy primitive, which docker-runs against this
// provider's Instance record. Phase 1's job is just to be a valid
// Provider that exercises the contract.
package local

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/inference-book/inference-plane/internal/provisioners"
	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Provider implements provisioners.Provider for the operator's laptop.
type Provider struct {
	detected gpuDetection
	clock    func() time.Time
}

// gpuDetection caches what we saw at construction time. GPU detection
// is best-effort -- if it fails or returns nothing, we synthesize a
// "cpu-only" entry so the adapter still functions on dev machines
// without a GPU (notably: CI runners).
type gpuDetection struct {
	class  string
	sku    string
	count  int
	vramGB int
}

// New constructs a local Provider, running GPU detection once at
// construction. Subsequent Spawn calls return the cached info; the
// detection cost is paid one time per process.
func New() *Provider {
	return &Provider{
		detected: detectGPU(),
		clock:    time.Now,
	}
}

// Name satisfies provisioners.Provider.
func (p *Provider) Name() string { return provisioners.ProviderLocal }

// Spawn returns an Instance describing the laptop. No side effect --
// the laptop already exists. The returned record is State=ACTIVE
// immediately because there is no asynchronous provisioning step.
func (p *Provider) Spawn(ctx context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, provisioners.NewProviderError(p.Name(), "spawn", err, 0)
	}
	if spec == nil {
		return nil, provisioners.NewProviderError(p.Name(), "spawn", fmt.Errorf("spec is nil"), 0)
	}
	now := timestamppb.New(p.clock())
	return &provisionerv1.Instance{
		Id:            spec.Id,
		ProviderId:    "local:" + spec.Id,
		Provider:      p.Name(),
		Spec:          spec,
		Region:        spec.Region,
		Gpu:           p.gpuInfo(),
		HourlyRateUsd: 0,
		State:         provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
		CreatedAt:     now,
		ActivatedAt:   now,
		// Ssh intentionally empty: cp and dp are on the same machine.
	}, nil
}

// Terminate is a no-op: there is no provider-side process to stop. The
// Service still patches the state-file record to TERMINATED so list
// shows it correctly.
func (p *Provider) Terminate(ctx context.Context, providerID string) error {
	if err := ctx.Err(); err != nil {
		return provisioners.NewProviderError(p.Name(), "terminate", err, 0)
	}
	return nil
}

// Describe always returns ErrNotFound. Local has no persistent
// provider-side state to query; the Service's local-state lookup is
// the only authoritative answer for "what does iplane think this id
// is?" This is consistent with the contract: Describe reports the
// provider's view, and the provider's view for local is "I have
// nothing to tell you about individual instances."
func (p *Provider) Describe(ctx context.Context, providerID string) (*provisionerv1.Instance, error) {
	return nil, provisioners.NewProviderError(p.Name(), "describe", provisioners.ErrNotFound, 0)
}

// List always returns empty. Same rationale as Describe: no
// provider-side registry. The Service's idempotency lookup falls
// through to a fresh Spawn for the local provider, which is correct
// because Spawn is cheap and side-effect-free.
func (p *Provider) List(ctx context.Context, filter map[string]string) ([]*provisionerv1.InstanceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, provisioners.NewProviderError(p.Name(), "list", err, 0)
	}
	return nil, nil
}

// gpuInfo constructs a fresh GpuInfo proto from the cached detection.
// Cloned per call so the Service can mutate the returned Instance
// without aliasing.
func (p *Provider) gpuInfo() *provisionerv1.GpuInfo {
	return &provisionerv1.GpuInfo{
		Class:  p.detected.class,
		Sku:    p.detected.sku,
		Count:  int32(p.detected.count),
		VramGb: int32(p.detected.vramGB),
	}
}

// detectGPU runs the platform-specific probe and falls back to a
// "cpu-only" entry on any failure or empty result. Best-effort by
// design -- the local adapter must work on machines without a GPU
// (Apple Silicon laptops without an integrated NVIDIA, CI runners).
func detectGPU() gpuDetection {
	if runtime.GOOS == "darwin" {
		if g, ok := detectAppleSilicon(); ok {
			return g
		}
	}
	if g, ok := detectNvidia(); ok {
		return g
	}
	return gpuDetection{
		class:  provisioners.GPUClassSmall,
		sku:    "cpu-only",
		count:  0,
		vramGB: 0,
	}
}

// detectNvidia parses nvidia-smi output for the first GPU. Returns
// (_, false) if nvidia-smi is missing or returns nothing.
func detectNvidia() (gpuDetection, bool) {
	cmd := exec.Command("nvidia-smi",
		"--query-gpu=name,memory.total",
		"--format=csv,noheader,nounits",
	)
	out, err := cmd.Output()
	if err != nil {
		return gpuDetection{}, false
	}
	line := firstLine(string(out))
	if line == "" {
		return gpuDetection{}, false
	}
	// "NVIDIA GeForce RTX 4090, 24564"
	parts := strings.SplitN(line, ",", 2)
	if len(parts) != 2 {
		return gpuDetection{}, false
	}
	sku := strings.TrimSpace(parts[0])
	vramMB, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return gpuDetection{}, false
	}
	vramGB := vramMB / 1024
	return gpuDetection{
		class:  classifyByVRAM(vramGB),
		sku:    sku,
		count:  1,
		vramGB: vramGB,
	}, true
}

// detectAppleSilicon parses `system_profiler SPDisplaysDataType` for an
// Apple Silicon GPU. Apple GPUs share VRAM with system RAM; we report
// the system RAM as VRAM since that is what Metal can actually
// address. Best-effort -- Apple's output format is not API-stable.
func detectAppleSilicon() (gpuDetection, bool) {
	out, err := exec.Command("system_profiler", "SPDisplaysDataType").Output()
	if err != nil {
		return gpuDetection{}, false
	}
	text := string(out)
	// Look for a Chipset Model line. Apple Silicon entries look like:
	//   Chipset Model: Apple M4 Pro
	sku := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Chipset Model:") {
			sku = strings.TrimSpace(strings.TrimPrefix(line, "Chipset Model:"))
			break
		}
	}
	if sku == "" || !strings.Contains(strings.ToLower(sku), "apple") {
		return gpuDetection{}, false
	}
	vramGB := systemRAMGB() // unified memory; Metal sees most of this
	return gpuDetection{
		class:  classifyByVRAM(vramGB),
		sku:    sku,
		count:  1,
		vramGB: vramGB,
	}, true
}

func systemRAMGB() int {
	// macOS: `sysctl -n hw.memsize` returns bytes.
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	bytes, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0
	}
	return int(bytes / (1024 * 1024 * 1024))
}

func classifyByVRAM(vramGB int) string {
	switch {
	case vramGB >= 96:
		return provisioners.GPUClassXLarge
	case vramGB >= 80:
		return provisioners.GPUClassLarge
	case vramGB >= 40:
		return provisioners.GPUClassMedium
	default:
		return provisioners.GPUClassSmall
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// Compile-time check.
var _ provisioners.Provider = (*Provider)(nil)
