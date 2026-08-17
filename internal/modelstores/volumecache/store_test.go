package volumecache

import (
	"context"
	"errors"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/modelstores"
	"google.golang.org/protobuf/proto"
)

// baseStub is a controllable base ModelStore so the tests can assert the
// wrapper preserves the base's EngineModelArg + EnvOverrides and honors
// its errors.
type baseStub struct {
	resolved modelstores.Resolved
	err      error
}

func (b baseStub) Resolve(context.Context, string) (modelstores.Resolved, error) {
	return b.resolved, b.err
}

func TestResolve_StampsMountAndHFHome(t *testing.T) {
	base := baseStub{resolved: modelstores.Resolved{
		EngineModelArg: "Qwen/Qwen2.5-32B-Instruct-AWQ",
		EnvOverrides:   map[string]string{"HF_TOKEN": "hf_x"},
	}}
	s := New(base, modelstores.Mount{VolumeID: "vol-1", MountPath: "/models"})

	got, err := s.Resolve(context.Background(), "Qwen/Qwen2.5-32B-Instruct-AWQ")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// EngineModelArg is unchanged -- the cache hits via HF_HOME redirect,
	// not by rewriting --model to a local path.
	if got.EngineModelArg != "Qwen/Qwen2.5-32B-Instruct-AWQ" {
		t.Errorf("EngineModelArg = %q, want the HF id unchanged", got.EngineModelArg)
	}
	if got.EnvOverrides["HF_TOKEN"] != "hf_x" {
		t.Errorf("base HF_TOKEN dropped: %+v", got.EnvOverrides)
	}
	if got.EnvOverrides["HF_HOME"] != "/models/hf" {
		t.Errorf("HF_HOME = %q, want /models/hf", got.EnvOverrides["HF_HOME"])
	}
	if len(got.Mounts) != 1 || got.Mounts[0].VolumeID != "vol-1" || got.Mounts[0].MountPath != "/models" {
		t.Errorf("Mounts = %+v, want one {vol-1,/models}", got.Mounts)
	}
}

func TestResolve_PropagatesBaseError(t *testing.T) {
	sentinel := errors.New("gated model")
	s := New(baseStub{err: sentinel}, modelstores.Mount{VolumeID: "vol-1", MountPath: "/models"})
	if _, err := s.Resolve(context.Background(), "x"); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want base error propagated", err)
	}
}

func TestResolve_DoesNotMutateBaseEnvMap(t *testing.T) {
	baseEnv := map[string]string{"HF_TOKEN": "hf_x"}
	s := New(baseStub{resolved: modelstores.Resolved{EnvOverrides: baseEnv}},
		modelstores.Mount{VolumeID: "vol-1", MountPath: "/models"})
	if _, err := s.Resolve(context.Background(), "x"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, leaked := baseEnv["HF_HOME"]; leaked {
		t.Error("wrapper wrote HF_HOME into the base's env map; must copy, not mutate")
	}
}

// archBase is a base store that also answers the optional architecture
// capability, standing in for huggingface.Store.
type archBase struct {
	baseStub
	arch *provisionerv1.ModelArchitecture
	saw  string
}

func (a *archBase) Architecture(_ context.Context, req *provisionerv1.DescribeModelRequest) (*provisionerv1.DescribeModelResponse, error) {
	a.saw = req.GetModelSpec()
	return &provisionerv1.DescribeModelResponse{Architecture: a.arch}, nil
}

func TestArchitecture_SurvivesTheWrapper(t *testing.T) {
	// The wrapper adds a mount, and a mount says nothing about a model's
	// shape. Dropping the capability made a warm-cache daemon report a
	// hub-backed store as unable to describe a model, which turned Ch 9's
	// configuration into a switch that disables Ch 12's pre-flight (#324).
	want := &provisionerv1.ModelArchitecture{Params: 32_760_000_000, Layers: 64, KvHeads: 8, HeadDim: 128, HiddenSize: 5120}
	base := &archBase{arch: want}
	s := New(base, modelstores.Mount{VolumeID: "vol-1", MountPath: "/models"})

	src, ok := interface{}(s).(modelstores.ArchitectureSource)
	if !ok {
		t.Fatal("the wrapper does not satisfy ArchitectureSource, so DescribeModel cannot reach the base")
	}
	got, err := src.Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "Qwen/Qwen2.5-32B"})
	if err != nil {
		t.Fatalf("Architecture: %v", err)
	}
	if !proto.Equal(got.GetArchitecture(), want) {
		t.Errorf("Architecture = %+v, want the base's answer unchanged %+v", got.GetArchitecture(), want)
	}
	// Forwarded whole. A wrapper that rewrote the spec would send the hub
	// looking for a model nobody asked about.
	if base.saw != "Qwen/Qwen2.5-32B" {
		t.Errorf("base saw spec %q, want it forwarded unchanged", base.saw)
	}
}

func TestArchitecture_SaysSoWhenTheBaseCannotAnswer(t *testing.T) {
	// Satisfying the interface is what lets the wrapper forward, so the
	// wrapper satisfies it even over a base that has no hub. The sentinel
	// is what keeps that case reporting as capability-absent instead of
	// as a bad model spec.
	s := New(baseStub{}, modelstores.Mount{VolumeID: "vol-1", MountPath: "/models"})

	_, err := s.Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "a/b"})
	if err == nil {
		t.Fatal("want a refusal when the base cannot report a shape")
	}
	if !errors.Is(err, modelstores.ErrNoArchitectureSource) {
		t.Errorf("error does not carry ErrNoArchitectureSource, so the RPC will call it a bad spec: %v", err)
	}
}
