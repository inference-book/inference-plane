package provisioners_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/local"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

func candidateService(t *testing.T) *provisioners.Service {
	t.Helper()
	store, err := file.Open(t.TempDir(), "test-operator")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return provisioners.New([]provisioners.Provider{local.New()}, store, "test-operator")
}

// "This provider cannot answer" and "this provider has no capacity" are
// different answers, and an empty list would collapse them. An operator told
// their requirements matched nothing would go widen the requirements, when the
// truth is that nobody looked.
func TestListCandidatesUnimplementedRatherThanEmpty(t *testing.T) {
	svc := candidateService(t)

	got, err := svc.ListCandidates(context.Background(), provisioners.ProviderLocal,
		&provisionerv1.ResourceRequirements{MinVramGb: 80})

	if err == nil {
		t.Fatalf("got %v and no error, want Unimplemented for a provider that cannot answer", got)
	}
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", status.Code(err))
	}
}

// A provider that was never configured is a different failure again, and one
// the operator fixes by exporting an API key rather than by rewriting the
// query.
func TestListCandidatesNotFoundForUnconfiguredProvider(t *testing.T) {
	svc := candidateService(t)

	_, err := svc.ListCandidates(context.Background(), "vast", &provisionerv1.ResourceRequirements{})

	if status.Code(err) != codes.NotFound {
		t.Errorf("code = %v, want NotFound for a provider that is not configured", status.Code(err))
	}
}
