package huggingface

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/vrambudget"
	"google.golang.org/protobuf/proto"
)

// archFixture serves both endpoints Architecture needs: the model-info
// endpoint for the parameter count and the resolve endpoint for
// config.json. Splitting them matters, because the two failure modes are
// different and the messages have to be too.
type archFixture struct {
	srv *httptest.Server

	infoStatus, configStatus int
	infoBody, configBody     string

	infoHits   int
	configPath string
	configAuth string
}

func newArchFixture(t *testing.T) *archFixture {
	t.Helper()
	f := &archFixture{
		infoStatus:   http.StatusOK,
		configStatus: http.StatusOK,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			f.infoHits++
			w.WriteHeader(f.infoStatus)
			_, _ = io.WriteString(w, f.infoBody)
			return
		}
		f.configPath = r.URL.Path
		f.configAuth = r.Header.Get("Authorization")
		w.WriteHeader(f.configStatus)
		_, _ = io.WriteString(w, f.configBody)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *archFixture) store(token string) *Store {
	s := New(token)
	s.BaseURL = f.srv.URL
	return s
}

// The anchor's real published shape: 32.76B parameters over 64 layers,
// 40 attention heads but only 8 KV heads, head dimension 128.
const anchorInfo = `{"id":"Qwen/Qwen2.5-32B","safetensors":{"total":32760000000}}`
const anchorConfig = `{"num_hidden_layers":64,"num_attention_heads":40,"num_key_value_heads":8,"hidden_size":5120,"head_dim":128}`

func TestArchitectureReadsTheShapeAndTheParameterCount(t *testing.T) {
	f := newArchFixture(t)
	f.infoBody, f.configBody = anchorInfo, anchorConfig

	got, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "Qwen/Qwen2.5-32B"})
	if err != nil {
		t.Fatal(err)
	}
	want := &provisionerv1.ModelArchitecture{Params: 32_760_000_000, Layers: 64, KvHeads: 8, HeadDim: 128, HiddenSize: 5120}
	if !proto.Equal(got.GetArchitecture(), want) {
		t.Errorf("Architecture = %+v, want %+v", got.GetArchitecture(), want)
	}
}

func TestArchitectureDerivesHeadDimWhenTheConfigOmitsIt(t *testing.T) {
	// head_dim is optional and the hidden dimension is by construction
	// the head dimension times the attention-head count, so deriving it
	// is exact rather than an estimate.
	f := newArchFixture(t)
	f.infoBody = anchorInfo
	f.configBody = `{"num_hidden_layers":64,"num_attention_heads":40,"num_key_value_heads":8,"hidden_size":5120}`

	got, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "Qwen/Qwen2.5-32B"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetArchitecture().GetHeadDim() != 128 {
		t.Errorf("HeadDim = %d, want 128 (5120 / 40)", got.GetArchitecture().GetHeadDim())
	}
}

func TestArchitectureTreatsAbsentKVHeadsAsNoGroupedQueryAttention(t *testing.T) {
	// A model without GQA states no separate KV head count, because every
	// attention head keeps its own key and value. Reading that absence as
	// zero would report a model with no cache at all.
	f := newArchFixture(t)
	f.infoBody = anchorInfo
	f.configBody = `{"num_hidden_layers":32,"num_attention_heads":32,"hidden_size":4096,"head_dim":128}`

	got, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "meta-llama/Llama-2-7b-hf"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetArchitecture().GetKvHeads() != 32 {
		t.Errorf("KvHeads = %d, want 32 (as many as attention heads)", got.GetArchitecture().GetKvHeads())
	}
}

func TestArchitectureReadsTheLanguageModelOfAMultimodalConfig(t *testing.T) {
	// A vision-language model's top-level config describes the wrapper.
	// Reading the wrapper's absent layer count as the model's would
	// report a KV cost of zero for a model that certainly has one.
	f := newArchFixture(t)
	f.infoBody = anchorInfo
	f.configBody = `{"architectures":["Qwen2VLForConditionalGeneration"],
		"text_config":{"num_hidden_layers":80,"num_attention_heads":64,"num_key_value_heads":8,"hidden_size":8192,"head_dim":128}}`

	got, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "Qwen/Qwen2-VL-72B"})
	if err != nil {
		t.Fatal(err)
	}
	if a := got.GetArchitecture(); a.GetLayers() != 80 || a.GetKvHeads() != 8 || a.GetHiddenSize() != 8192 {
		t.Errorf("Architecture = %+v, want the text_config shape (80 layers, 8 kv heads, hidden 8192)", got.GetArchitecture())
	}
}

func TestArchitectureRefusesRatherThanGuessingTheParameterCount(t *testing.T) {
	// The obvious guess is to read a size out of the name. A name is a
	// label the uploader chose, right often enough to be trusted and
	// wrong exactly where a budget matters.
	f := newArchFixture(t)
	f.infoBody = `{"id":"someone/Frankenmerge-70B"}`
	f.configBody = anchorConfig

	_, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "someone/Frankenmerge-70B"})
	if err == nil {
		t.Fatal("want a refusal when the model publishes no parameter count")
	}
	if !strings.Contains(err.Error(), "parameter count") {
		t.Errorf("error does not say what is missing: %v", err)
	}
}

func TestArchitectureRefusesAZeroParameterCount(t *testing.T) {
	f := newArchFixture(t)
	f.infoBody = `{"id":"a/b","safetensors":{"total":0}}`
	f.configBody = anchorConfig

	_, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "a/b"})
	if err == nil {
		t.Fatal("want a refusal for a zero parameter count")
	}
	// A repository publishing a total of zero is in the same position as
	// one publishing nothing, and deserves the same actionable message
	// rather than falling through to the generic shape validation.
	if !strings.Contains(err.Error(), "safetensors") {
		t.Errorf("error %q does not name the missing accounting", err)
	}
}

func TestArchitectureUsesTheRequestedRevision(t *testing.T) {
	f := newArchFixture(t)
	f.infoBody, f.configBody = anchorInfo, anchorConfig

	if _, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "Qwen/Qwen2.5-32B:a1b2c3d"}); err != nil {
		t.Fatal(err)
	}
	if want := "/Qwen/Qwen2.5-32B/resolve/a1b2c3d/config.json"; f.configPath != want {
		t.Errorf("config path = %q, want %q", f.configPath, want)
	}
}

func TestArchitectureDefaultsToMainWhenNoRevisionIsGiven(t *testing.T) {
	f := newArchFixture(t)
	f.infoBody, f.configBody = anchorInfo, anchorConfig

	if _, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "Qwen/Qwen2.5-32B"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.configPath, "/resolve/main/") {
		t.Errorf("config path = %q, want the main revision", f.configPath)
	}
}

func TestArchitectureSendsTheTokenToTheConfigEndpoint(t *testing.T) {
	// A gated model's config.json is gated too, so a token that reaches
	// only the model-info call reads as a missing config on exactly the
	// models an operator most needs sized.
	f := newArchFixture(t)
	f.infoBody, f.configBody = anchorInfo, anchorConfig

	if _, err := f.store("hf_secret").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "Qwen/Qwen2.5-32B"}); err != nil {
		t.Fatal(err)
	}
	if f.configAuth != "Bearer hf_secret" {
		t.Errorf("config Authorization = %q, want the bearer token", f.configAuth)
	}
}

func TestArchitectureDistinguishesAMissingConfigFromAGatedOne(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   string
	}{
		{"missing", http.StatusNotFound, "no config.json"},
		{"gated", http.StatusUnauthorized, "HF_TOKEN"},
		{"forbidden", http.StatusForbidden, "HF_TOKEN"},
		{"server error", http.StatusInternalServerError, "returned 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newArchFixture(t)
			f.infoBody = anchorInfo
			f.configStatus, f.configBody = tc.status, "nope"

			_, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "a/b"})
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestArchitectureReportsAConfigThatIsMissingWhatTheBudgetNeeds(t *testing.T) {
	// A config that parses and carries no layers would otherwise produce
	// an Arch that computes a KV cost of zero, which is a budget that
	// says yes to everything.
	f := newArchFixture(t)
	f.infoBody = anchorInfo
	f.configBody = `{"hidden_size":5120}`

	_, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "a/b"})
	if err == nil {
		t.Fatal("want an error for a config with no layers")
	}
	if !strings.Contains(err.Error(), "layer") {
		t.Errorf("error does not name the missing field: %v", err)
	}
}

func TestArchitectureRejectsUnparseableConfig(t *testing.T) {
	f := newArchFixture(t)
	f.infoBody = anchorInfo
	f.configBody = `<!doctype html><html>rate limited</html>`

	if _, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "a/b"}); err == nil {
		t.Fatal("want an error when config.json is not JSON")
	}
}

func TestArchitectureValidatesTheSpecBeforeAnyCall(t *testing.T) {
	f := newArchFixture(t)
	for _, spec := range []string{"", "bare-name", "too/many/slashes", "a/b c"} {
		if _, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: spec}); err == nil {
			t.Errorf("Architecture(%q) accepted an invalid spec", spec)
		}
	}
	// Asserting only that an error came back cannot tell a local
	// rejection from one the hub made, since an unparseable response
	// errors too. The claim is that nothing was sent at all.
	if f.infoHits != 0 || f.configPath != "" {
		t.Errorf("an invalid spec reached the network: %d model-info calls, config path %q", f.infoHits, f.configPath)
	}
}

func TestArchitectureRefusesADisabledModel(t *testing.T) {
	f := newArchFixture(t)
	f.infoBody = `{"id":"a/b","disabled":true,"safetensors":{"total":1000}}`
	f.configBody = anchorConfig

	if _, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "a/b"}); err == nil {
		t.Fatal("want a refusal for a disabled model")
	}
}

func TestArchitectureFeedsAUsableBudget(t *testing.T) {
	// The point of the fetch. What comes back has to be something
	// Compute accepts, and the seam between the two packages is exactly
	// where a plausible-looking Arch with a zero field would go unnoticed.
	f := newArchFixture(t)
	f.infoBody, f.configBody = anchorInfo, anchorConfig

	resp, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "Qwen/Qwen2.5-32B"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := vrambudget.Compute(resp.GetArchitecture(), vrambudget.Plan{
		Weights: vrambudget.PrecisionFP16, MaxModelLen: 131_072, MaxBatch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := b.KVBytesPerToken, int64(262_144); got != want {
		t.Errorf("KV bytes per token = %d, want %d", got, want)
	}
	if v := b.Against(vrambudget.UsableBytes(80*vrambudget.GB, vrambudget.DefaultUtilization)); v != vrambudget.Overcommitted {
		t.Errorf("verdict = %v, want overcommitted", v)
	}
}
