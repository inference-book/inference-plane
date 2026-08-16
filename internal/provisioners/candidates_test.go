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

	got, err := svc.CandidatesFrom(context.Background(), provisioners.ProviderLocal,
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

	_, err := svc.CandidatesFrom(context.Background(), "vast", &provisionerv1.ResourceRequirements{})

	if status.Code(err) != codes.NotFound {
		t.Errorf("code = %v, want NotFound for a provider that is not configured", status.Code(err))
	}
}

// Two providers, two spellings, one fact. Vast says "amd64" where Lambda says
// "x86_64", and a caller comparing candidates across them should never have to
// know which vendor said which.
func TestNormalizeArch(t *testing.T) {
	for in, want := range map[string]string{
		"amd64":   provisioners.ArchAMD64,
		"x86_64":  provisioners.ArchAMD64,
		"X86-64":  provisioners.ArchAMD64,
		"arm64":   provisioners.ArchARM64,
		"AArch64": provisioners.ArchARM64,
		" arm64 ": provisioners.ArchARM64,
	} {
		if got := provisioners.NormalizeArch(in); got != want {
			t.Errorf("NormalizeArch(%q) = %q, want %q", in, got, want)
		}
	}
}

// Unreported must stay unreported. Guessing x86 here produces a deploy that
// pulls an image the host cannot run, and the failure surfaces as a container
// that will not start rather than as anything naming the architecture.
func TestNormalizeArchDoesNotGuess(t *testing.T) {
	for _, in := range []string{"", "   ", "riscv64", "unknown"} {
		if got := provisioners.NormalizeArch(in); got != "" {
			t.Errorf("NormalizeArch(%q) = %q, want empty rather than a default", in, got)
		}
	}
}
