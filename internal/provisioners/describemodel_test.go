package provisioners_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/modelstores"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// archStore is a model store that also answers the optional
// architecture capability.
type archStore struct {
	arch *provisionerv1.ModelArchitecture
	err  error
	sawy string
}

func (a *archStore) Resolve(_ context.Context, spec string) (modelstores.Resolved, error) {
	return modelstores.Resolved{EngineModelArg: spec}, nil
}

func (a *archStore) Architecture(_ context.Context, req *provisionerv1.DescribeModelRequest) (*provisionerv1.DescribeModelResponse, error) {
	a.sawy = req.GetModelSpec()
	if a.err != nil {
		return nil, a.err
	}
	return &provisionerv1.DescribeModelResponse{Architecture: a.arch}, nil
}

func newSvcWithModelStore(t *testing.T, ms modelstores.ModelStore) *provisioners.Service {
	t.Helper()
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	return provisioners.New(nil, store, "default", provisioners.WithModelStore(ms))
}

func codeOf(t *testing.T, err error) codes.Code {
	t.Helper()
	if err == nil {
		t.Fatal("want an error")
	}
	var se interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &se) {
		t.Fatalf("error carries no gRPC status: %v", err)
	}
	return se.GRPCStatus().Code()
}

func TestDescribeModel_ReturnsWhatTheStoreReports(t *testing.T) {
	ms := &archStore{arch: &provisionerv1.ModelArchitecture{
		Params: 32_760_000_000, Layers: 64, KvHeads: 8, HeadDim: 128, HiddenSize: 5120,
	}}
	svc := newSvcWithModelStore(t, ms)

	resp, err := svc.DescribeModel(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "Qwen/Qwen2.5-32B"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.GetArchitecture().GetLayers(); got != 64 {
		t.Errorf("layers = %d, want 64", got)
	}
	if ms.sawy != "Qwen/Qwen2.5-32B" {
		t.Errorf("store saw spec %q, want the requested one", ms.sawy)
	}
}

func TestDescribeModel_UnimplementedWhenTheStoreCannotAnswer(t *testing.T) {
	// A pass-through store has no hub behind it. Unimplemented says so.
	// The alternative an implementation drifts towards is returning an
	// empty architecture, which is a model with no layers, which computes
	// a KV cache of zero and therefore fits any card ever made.
	svc := newSvcWithModelStore(t, modelstores.Passthrough{})

	_, err := svc.DescribeModel(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "a/b"})
	if got := codeOf(t, err); got != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", got)
	}
	// The message has to say why, since "unimplemented" on a verb that
	// exists reads as a bug rather than as a configuration.
	if !strings.Contains(err.Error(), "pass-through") {
		t.Errorf("error does not explain the configuration: %v", err)
	}
}

func TestDescribeModel_RequiresASpec(t *testing.T) {
	svc := newSvcWithModelStore(t, &archStore{})
	_, err := svc.DescribeModel(context.Background(), &provisionerv1.DescribeModelRequest{})
	if got := codeOf(t, err); got != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", got)
	}
}

func TestDescribeModel_StoreFailuresAreTheCallersToFix(t *testing.T) {
	// Everything the store can fail on here is either the spec or what
	// the hub publishes for it. Internal would tell an operator to open a
	// bug against iplane for a model they misspelled.
	ms := &archStore{err: errors.New("model \"a/b\" publishes no safetensors parameter count")}
	svc := newSvcWithModelStore(t, ms)

	_, err := svc.DescribeModel(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "a/b"})
	if got := codeOf(t, err); got != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", got)
	}
	if !strings.Contains(err.Error(), "safetensors") {
		t.Errorf("the store's reason did not survive: %v", err)
	}
}

func TestDescribeModel_TakesNoStateLock(t *testing.T) {
	// The verb has to answer while a control plane is running, which is
	// when an operator is most likely to be sizing something. Refusing
	// then is the defect that shipped in `model ls` (#307), and the shape
	// of it is a read that reaches for the lock it does not need.
	dir := t.TempDir()
	held, err := file.Open(dir, "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	release, err := held.LockForLifetime()
	if err != nil {
		t.Fatalf("LockForLifetime: %v", err)
	}
	defer release()

	reader, err := file.Open(dir, "default")
	if err != nil {
		t.Fatalf("second file.Open: %v", err)
	}
	svc := provisioners.New(nil, reader, "default", provisioners.WithModelStore(
		&archStore{arch: &provisionerv1.ModelArchitecture{Params: 1e9, Layers: 32, KvHeads: 8, HeadDim: 128, HiddenSize: 4096}},
	))

	if _, err := svc.DescribeModel(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "a/b"}); err != nil {
		t.Errorf("DescribeModel refused while the state lock was held: %v", err)
	}
}
