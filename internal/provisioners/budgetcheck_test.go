package provisioners_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/modelstores"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/local"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// llama70B is the model the capstone deploys: 70.6B parameters over 80
// layers, 8 KV heads, head dimension 128, hidden size 8192.
var llama70B = &provisionerv1.ModelArchitecture{
	Params: 70_600_000_000, Layers: 80, KvHeads: 8, HeadDim: 128, HiddenSize: 8192,
	MaxPositionEmbeddings: 131_072,
}

// a100 is 80 GiB, the exact capacity the catalog resolves for an
// "80GB" card.
const a100Bytes int64 = 80 << 30

// b200Bytes is 180 GiB, the exact capacity behind a "180GB" Blackwell.
const b200Bytes int64 = 180 << 30

// capacityProvider is a Provider that also answers the optional
// card-capacity capability and counts every spawn, so a test can assert
// that a refusal happened before anything was rented rather than merely
// that an error came back.
type capacityProvider struct {
	provisioners.Provider
	name       string
	bytes      int64
	spawnCalls atomic.Int32
}

func (c *capacityProvider) Name() string { return c.name }

func (c *capacityProvider) CardCapacityBytes(string) int64 { return c.bytes }

func (c *capacityProvider) Spawn(ctx context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error) {
	c.spawnCalls.Add(1)
	return c.Provider.Spawn(ctx, spec)
}

// budgetStore answers the architecture capability from a fixture.
type budgetStore struct {
	arch *provisionerv1.ModelArchitecture
}

func (b *budgetStore) Resolve(_ context.Context, spec string) (modelstores.Resolved, error) {
	return modelstores.Resolved{EngineModelArg: spec}, nil
}

func (b *budgetStore) Architecture(_ context.Context, _ *provisionerv1.DescribeModelRequest) (*provisionerv1.DescribeModelResponse, error) {
	return &provisionerv1.DescribeModelResponse{Architecture: b.arch}, nil
}

func budgetService(t *testing.T, p provisioners.Provider, arch *provisionerv1.ModelArchitecture) *provisioners.Service {
	t.Helper()
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	return provisioners.New([]provisioners.Provider{p}, store, "default",
		provisioners.WithModelStore(&budgetStore{arch: arch}))
}

func budgetDeployReq(id string, engineArgs []string, cards int32) *provisionerv1.CreateDeploymentRequest {
	return &provisionerv1.CreateDeploymentRequest{
		// Wait so the fan-out finishes inside the test. Without it the
		// provisioning goroutine outlives t.TempDir's cleanup and the
		// failure looks like a flaky filesystem rather than a race.
		Wait: true,
		Deployment: &provisionerv1.Deployment{
			Id: id, Model: "meta-llama/Llama-3.3-70B-Instruct",
			Image: "vllm/vllm-openai:v0.7.0", EngineArgs: engineArgs,
		},
		ReplicasSpec: []*provisionerv1.ReplicaSpec{{
			Provider: "cap", Replicas: 1,
			Requirements: &provisionerv1.ResourceRequirements{Sku: "a100", GpuCount: cards},
		}},
	}
}

func TestDeployRefusesAPlanTheCardsCannotHold(t *testing.T) {
	// The whole point: budget at 16k, deploy at 32k, and the cards were
	// only ever going to hold the smaller plan. The control plane has
	// every number it needs to catch that (#326).
	p := &capacityProvider{Provider: local.New(), name: "cap", bytes: a100Bytes}
	svc := budgetService(t, p, llama70B)

	_, err := svc.CreateDeployment(context.Background(),
		budgetDeployReq("llama70b", []string{"--max-model-len=32768", "--max-num-seqs=16"}, 2))
	if err == nil {
		t.Fatal("want a refusal for a 70B at 32k x 16 on two 80 GB cards")
	}
	// Before anything was rented. An error after the meter starts is the
	// failure this exists to prevent, not a smaller version of it.
	if got := p.spawnCalls.Load(); got != 0 {
		t.Errorf("provider spawned %d instance(s) before refusing", got)
	}
}

func TestTheRefusalCarriesTheArithmetic(t *testing.T) {
	// "Does not fit" sends an operator back to guess which of four
	// knobs to move. Naming the largest claim is what makes the refusal
	// actionable, and at this context and batch it is the cache rather
	// than the weights everyone reaches for first.
	p := &capacityProvider{Provider: local.New(), name: "cap", bytes: a100Bytes}
	svc := budgetService(t, p, llama70B)

	_, err := svc.CreateDeployment(context.Background(),
		budgetDeployReq("llama70b", []string{"--max-model-len=32768", "--max-num-seqs=16"}, 2))
	if err == nil {
		t.Fatal("want a refusal")
	}
	for _, want := range []string{"per card", "usable", "cache", "--max-model-len"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
}

func TestDeployProceedsWhenThePlanFits(t *testing.T) {
	// Every deploy that fits has to be untouched by this, including the
	// overwhelming majority that name no context length at all.
	p := &capacityProvider{Provider: local.New(), name: "cap", bytes: a100Bytes}
	svc := budgetService(t, p, llama70B)

	if _, err := svc.CreateDeployment(context.Background(),
		budgetDeployReq("small", []string{"--max-model-len=4096", "--max-num-seqs=1"}, 8)); err != nil {
		t.Errorf("a plan that fits was refused: %v", err)
	}
}

func TestDeployIsNotBlockedWhenTheCheckCannotRun(t *testing.T) {
	// Three ways the inputs go missing, and all three have to let the
	// deploy through. A false refusal is worse than the silence this
	// replaces, which is the whole reason the check reads what is
	// already being passed instead of demanding new flags.
	cases := []struct {
		id         string
		name       string
		engineArgs []string
		bytes      int64
		arch       *provisionerv1.ModelArchitecture
	}{
		{"no-context", "no context length in the engine args", []string{"--max-num-seqs=16"}, a100Bytes, llama70B},
		{"no-capacity", "the SKU has no exact capacity", []string{"--max-model-len=131072", "--max-num-seqs=32"}, 0, llama70B},
		{"no-shape", "the hub cannot describe the model", []string{"--max-model-len=131072", "--max-num-seqs=32"}, a100Bytes, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &capacityProvider{Provider: local.New(), name: "cap", bytes: tc.bytes}
			svc := budgetService(t, p, tc.arch)
			if _, err := svc.CreateDeployment(context.Background(),
				budgetDeployReq(tc.id, tc.engineArgs, 1)); err != nil {
				t.Errorf("deploy blocked when the check could not run: %v", err)
			}
		})
	}
}

func TestEngineArgsNobodyModelsAreForwardedUntouched(t *testing.T) {
	// The engine stays opaque. Parsing the flags we recognise must not
	// turn into filtering the ones we do not, or every engine option
	// becomes iplane's business.
	p := &capacityProvider{Provider: local.New(), name: "cap", bytes: a100Bytes}
	svc := budgetService(t, p, llama70B)

	args := []string{"--enable-chunked-prefill", "--swap-space", "8", "--max-model-len=4096", "--served-model-name", "custom"}
	resp, err := svc.CreateDeployment(context.Background(), budgetDeployReq("passthrough", args, 8))
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	got := resp.GetDeployment().GetEngineArgs()
	for _, want := range args {
		var found bool
		for _, g := range got {
			if g == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("engine arg %q did not survive: %v", want, got)
		}
	}
}
