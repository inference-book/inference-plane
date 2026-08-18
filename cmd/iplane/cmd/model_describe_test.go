package cmd

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// glm52 is GLM-5.2's published shape, read from its config.json. It is
// the sparse counterpart to llama70B: same fields, plus the five that
// only a mixture-of-experts model states.
var glm52 = &provisionerv1.ModelArchitecture{
	Params:                753_329_940_480,
	Layers:                78,
	KvHeads:               64,
	HeadDim:               192,
	HiddenSize:            6144,
	MaxPositionEmbeddings: 1_048_576,
	NumExperts:            256,
	NumExpertsPerTok:      8,
	MoeIntermediateSize:   2048,
	SharedExperts:         1,
	DenseLayers:           3,
}

// runDescribe drives the real command over the real wire, matching how
// runBudget does it, because both verbs answer through the daemon rather
// than from the CLI host.
func runDescribe(t *testing.T, arch *provisionerv1.ModelArchitecture, spec string) (string, error) {
	t.Helper()
	instanceServiceURL = ""

	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New(nil, store, "default", provisioners.WithModelStore(&budgetStore{arch: arch}))

	mux := http.NewServeMux()
	path, handler := provisionerv1connect.NewProvisionerServiceHandler(provisioners.NewConnectProvisionerAdapter(svc))
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs([]string{"model", "--service-url", server.URL, "describe", spec})
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
		instanceServiceURL = ""
	})

	err = rootCmd.Execute()
	return buf.String(), err
}

// The dense output is the compatibility claim of the expert work: a model
// with no experts prints exactly what it printed before the expert fields
// existed. Held as a whole-output golden rather than a spot check,
// because the failure mode is an expert row appearing where there is
// nothing to report.
func TestModelDescribePrintsADenseModelUnchanged(t *testing.T) {
	got, err := runDescribe(t, llama70B, "meta-llama/Llama-3.3-70B-Instruct")
	if err != nil {
		t.Fatal(err)
	}

	want := `meta-llama/Llama-3.3-70B-Instruct

  parameters      70.60 B
  layers          80
  kv heads        8
  head dim        128
  hidden size     8192
  context window  131072 tokens

  weights   141.2 GB fp16   70.6 GB fp8   42.4 GB 4-bit
  kv cache  320.0 KiB per token at fp16, 160.0 KiB at fp8
            8k: 2.7 GB   32k: 10.7 GB   128k: 42.9 GB   (one sequence)
`
	if got != want {
		t.Errorf("dense output changed\n got:\n%s\nwant:\n%s", got, want)
	}
}

// The sparse block, whole. Every derived row below is the same arithmetic
// the dense case runs, so a change to one of those rows here is a change
// to the budget rather than to this command.
func TestModelDescribeReportsTheExpertShape(t *testing.T) {
	got, err := runDescribe(t, glm52, "zai-org/GLM-5.2")
	if err != nil {
		t.Fatal(err)
	}

	want := `zai-org/GLM-5.2

  parameters      753.33 B
  layers          78
  kv heads        64
  head dim        192
  hidden size     6144
  context window  1048576 tokens

  experts       256 routed, 8 active per token, 1 shared
  expert width  2048
  dense layers  3 of 78

  weights   1506.7 GB fp16   753.3 GB fp8   452.0 GB 4-bit
  kv cache  3744.0 KiB per token at fp16, 1872.0 KiB at fp8
            8k: 31.4 GB   32k: 125.6 GB   128k: 502.5 GB   (one sequence)
`
	if got != want {
		t.Errorf("sparse output\n got:\n%s\nwant:\n%s", got, want)
	}
}

// A sparse model that states a count and nothing else still reports the
// count. Absent is unknown throughout, so the row shrinks to what the
// config actually said rather than printing zeros as if they were
// readings.
func TestModelDescribeOmitsTheExpertFactsAModelDoesNotPublish(t *testing.T) {
	sparse := &provisionerv1.ModelArchitecture{
		Params:     46_700_000_000,
		Layers:     32,
		KvHeads:    8,
		HeadDim:    128,
		HiddenSize: 4096,
		NumExperts: 8,
	}

	got, err := runDescribe(t, sparse, "org/sparse")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains([]byte(got), []byte("experts  8 routed\n")) {
		t.Errorf("want a bare routed count, got:\n%s", got)
	}
	for _, unwanted := range []string{"active per token", "shared", "expert width", "dense layers"} {
		if bytes.Contains([]byte(got), []byte(unwanted)) {
			t.Errorf("printed %q for a config that does not state it:\n%s", unwanted, got)
		}
	}
}
