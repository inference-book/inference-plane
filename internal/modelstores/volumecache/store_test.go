package volumecache

import (
	"context"
	"errors"
	"testing"

	"github.com/inference-book/inference-plane/internal/modelstores"
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
