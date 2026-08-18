package huggingface

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestArchitectureReadsTheTrainedContextWindow(t *testing.T) {
	// No budget term reads this. It is read so that a caller choosing a
	// context length can start from the model's own answer, because the
	// alternative default is a house number that is wrong for every
	// model whose window is not the one somebody picked.
	f := newArchFixture(t)
	f.infoBody = anchorInfo
	f.configBody = `{"num_hidden_layers":64,"num_attention_heads":40,"num_key_value_heads":8,
		"hidden_size":5120,"head_dim":128,"max_position_embeddings":131072}`

	got, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "Qwen/Qwen2.5-32B"})
	if err != nil {
		t.Fatal(err)
	}
	if n := got.GetArchitecture().GetMaxPositionEmbeddings(); n != 131_072 {
		t.Errorf("max position embeddings = %d, want 131072", n)
	}
}

func TestArchitectureStillAnswersWithoutAContextWindow(t *testing.T) {
	// The window is not one of the things a budget cannot proceed
	// without, so requiring it would refuse models the arithmetic can
	// price perfectly well. Absent has to mean "ask the operator", not
	// "this model is unreadable".
	f := newArchFixture(t)
	f.infoBody, f.configBody = anchorInfo, anchorConfig

	got, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "Qwen/Qwen2.5-32B"})
	if err != nil {
		t.Fatalf("a config without max_position_embeddings was refused: %v", err)
	}
	if n := got.GetArchitecture().GetMaxPositionEmbeddings(); n != 0 {
		t.Errorf("max position embeddings = %d, want 0 for a config that does not publish it", n)
	}
}

func TestArchitectureTakesTheContextWindowFromTheLanguageModel(t *testing.T) {
	// A multimodal wrapper carries its own window, and it is not the one
	// the KV cache is sized against. Taking the wrapper's would quote a
	// context the language model cannot actually serve.
	f := newArchFixture(t)
	f.infoBody = anchorInfo
	f.configBody = `{"architectures":["Qwen2VLForConditionalGeneration"],"max_position_embeddings":4096,
		"text_config":{"num_hidden_layers":80,"num_attention_heads":64,"num_key_value_heads":8,
		"hidden_size":8192,"head_dim":128,"max_position_embeddings":32768}}`

	got, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "Qwen/Qwen2-VL-72B"})
	if err != nil {
		t.Fatal(err)
	}
	if n := got.GetArchitecture().GetMaxPositionEmbeddings(); n != 32_768 {
		t.Errorf("max position embeddings = %d, want the text_config's 32768", n)
	}
}

// loadTestdataConfig reads a config.json captured from the hub. These are
// the published files trimmed to the keys the budget reads, kept whole
// rather than hand-written because what is under test is which spelling
// a real model family actually uses. A hand-written fixture would only
// re-assert the spelling the implementation already assumes.
func loadTestdataConfig(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The expert-count field has three spellings on the hub and the spelling
// follows the model family. Reading one of them would report the other
// two families as dense, which is a wrong answer rather than a missing
// one: a dense reading of a sparse model says every parameter is read on
// every step.
func TestArchitectureReadsEveryExpertCountSpelling(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fixture  string
		spelling string
		experts  int32
	}{
		{"GLM 5.2", "glm-5.2.json", "n_routed_experts", 256},
		{"GLM 4.5", "glm-4.5.json", "n_routed_experts", 160},
		{"Kimi K3", "kimi-k3.json", "num_experts", 896},
		{"Qwen3 MoE", "qwen3-moe.json", "num_experts", 128},
		{"Qwen2 MoE", "qwen2-moe.json", "num_experts", 64},
		{"Mixtral", "mixtral.json", "num_local_experts", 8},
		{"GPT-OSS", "gpt-oss.json", "num_local_experts", 128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArchFixture(t)
			f.infoBody = `{"id":"org/model","safetensors":{"total":1000000000}}`
			f.configBody = loadTestdataConfig(t, tc.fixture)

			got, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "org/model"})
			if err != nil {
				t.Fatal(err)
			}
			if n := got.GetArchitecture().GetNumExperts(); n != tc.experts {
				t.Errorf("experts via %s = %d, want %d", tc.spelling, n, tc.experts)
			}
		})
	}
}

// Kimi K3 spells the activated count num_experts_per_token where every
// other family spells it num_experts_per_tok. It is the flagship target
// of the cost-curve work, and reading zero activated experts from it
// would make a 2.8T model look like it decodes all 2.8T every step.
func TestArchitectureReadsEveryActivatedExpertSpelling(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture string
		active  int32
	}{
		{"Kimi K3 says num_experts_per_token", "kimi-k3.json", 16},
		{"GLM 5.2 says num_experts_per_tok", "glm-5.2.json", 8},
		{"GPT-OSS says both and they agree", "gpt-oss.json", 4},
		{"Mixtral", "mixtral.json", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArchFixture(t)
			f.infoBody = `{"id":"org/model","safetensors":{"total":1000000000}}`
			f.configBody = loadTestdataConfig(t, tc.fixture)

			got, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "org/model"})
			if err != nil {
				t.Fatal(err)
			}
			if n := got.GetArchitecture().GetNumExpertsPerTok(); n != tc.active {
				t.Errorf("experts per token = %d, want %d", n, tc.active)
			}
		})
	}
}

func TestArchitectureReadsBothSharedExpertSpellings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture string
		shared  int32
	}{
		{"GLM 5.2 says n_shared_experts", "glm-5.2.json", 1},
		{"Kimi K3 says num_shared_experts", "kimi-k3.json", 2},
		{"Mixtral has none", "mixtral.json", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArchFixture(t)
			f.infoBody = `{"id":"org/model","safetensors":{"total":1000000000}}`
			f.configBody = loadTestdataConfig(t, tc.fixture)

			got, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "org/model"})
			if err != nil {
				t.Fatal(err)
			}
			if n := got.GetArchitecture().GetSharedExperts(); n != tc.shared {
				t.Errorf("shared experts = %d, want %d", n, tc.shared)
			}
		})
	}
}

// Qwen2-MoE has exactly one shared expert and publishes only its width,
// never a count. Inferring the count from the width would be a guess, and
// this file's other derivation (head_dim from hidden_size) is exact. The
// width is what the active-parameter term will want, and it can be read
// when there is a term reading it.
func TestArchitectureLeavesTheSharedCountUnknownWhenOnlyItsWidthIsPublished(t *testing.T) {
	f := newArchFixture(t)
	f.infoBody = `{"id":"Qwen/Qwen2-57B-A14B","safetensors":{"total":57408658944}}`
	f.configBody = loadTestdataConfig(t, "qwen2-moe.json")

	got, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "Qwen/Qwen2-57B-A14B"})
	if err != nil {
		t.Fatal(err)
	}
	if n := got.GetArchitecture().GetSharedExperts(); n != 0 {
		t.Errorf("shared experts = %d, want 0 (unknown); the config states a width and no count", n)
	}
}

// Mixtral and GPT-OSS have no dense feed-forward beside the experts, so
// intermediate_size is the expert's width. The families that have both
// state the expert width separately, and taking intermediate_size from
// them would report the dense layer's width as the expert's.
func TestArchitectureReadsTheExpertWidthFromWhicheverFieldStatesIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture string
		width   int32
	}{
		{"GLM 5.2 states both, expert width wins", "glm-5.2.json", 2048},
		{"Kimi K3 states both", "kimi-k3.json", 3072},
		{"Mixtral states only intermediate_size", "mixtral.json", 14336},
		{"GPT-OSS states only intermediate_size", "gpt-oss.json", 2880},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArchFixture(t)
			f.infoBody = `{"id":"org/model","safetensors":{"total":1000000000}}`
			f.configBody = loadTestdataConfig(t, tc.fixture)

			got, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "org/model"})
			if err != nil {
				t.Fatal(err)
			}
			if n := got.GetArchitecture().GetMoeIntermediateSize(); n != tc.width {
				t.Errorf("expert width = %d, want %d", n, tc.width)
			}
		})
	}
}

func TestArchitectureReadsTheDenseLayerPrefix(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture string
		dense   int32
	}{
		{"GLM 5.2 makes its first three layers dense", "glm-5.2.json", 3},
		{"Kimi K3 makes its first layer dense", "kimi-k3.json", 1},
		{"Mixtral has no dense prefix", "mixtral.json", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArchFixture(t)
			f.infoBody = `{"id":"org/model","safetensors":{"total":1000000000}}`
			f.configBody = loadTestdataConfig(t, tc.fixture)

			got, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "org/model"})
			if err != nil {
				t.Fatal(err)
			}
			if n := got.GetArchitecture().GetDenseLayers(); n != tc.dense {
				t.Errorf("dense layers = %d, want %d", n, tc.dense)
			}
		})
	}
}

// A dense model has no expert fields to report, and zero is the answer
// rather than an absent reading. Asserted field by field because a
// partial read is the failure mode that would not show up as an error.
func TestArchitectureReportsADenseModelAsHavingNoExperts(t *testing.T) {
	f := newArchFixture(t)
	f.infoBody = `{"id":"Qwen/Qwen2.5-32B","safetensors":{"total":32760000000}}`
	f.configBody = loadTestdataConfig(t, "qwen2.5-32b.json")

	got, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "Qwen/Qwen2.5-32B"})
	if err != nil {
		t.Fatal(err)
	}
	a := got.GetArchitecture()
	for _, f := range []struct {
		name string
		got  int32
	}{
		{"num_experts", a.GetNumExperts()},
		{"num_experts_per_tok", a.GetNumExpertsPerTok()},
		{"moe_intermediate_size", a.GetMoeIntermediateSize()},
		{"shared_experts", a.GetSharedExperts()},
		{"dense_layers", a.GetDenseLayers()},
	} {
		if f.got != 0 {
			t.Errorf("%s = %d on a dense model, want 0", f.name, f.got)
		}
	}
	// The dense read is unchanged, which is the compatibility claim.
	if err := vrambudget.ValidateArch(a); err != nil {
		t.Errorf("dense model no longer validates: %v", err)
	}
}

// A dense model states intermediate_size too, and it is the plain
// feed-forward width rather than an expert's. Reading it as an expert
// width would make every dense model look sparse to anything keying on
// the field being set.
func TestArchitectureDoesNotReadAnExpertWidthOffADenseModel(t *testing.T) {
	f := newArchFixture(t)
	f.infoBody = anchorInfo
	f.configBody = `{"num_hidden_layers":64,"num_attention_heads":40,"num_key_value_heads":8,"hidden_size":5120,"head_dim":128,"intermediate_size":27648,"first_k_dense_replace":0}`

	got, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "Qwen/Qwen2.5-32B"})
	if err != nil {
		t.Fatal(err)
	}
	if n := got.GetArchitecture().GetMoeIntermediateSize(); n != 0 {
		t.Errorf("expert width = %d on a dense model, want 0", n)
	}
}

// The two models Part IV is built around, read end to end against their
// published configs. This is the acceptance criterion, and it is a
// separate test from the spelling matrix because these two numbers are
// the ones the chapters quote.
func TestArchitectureReportsThePartFourTargets(t *testing.T) {
	for _, tc := range []struct {
		spec    string
		fixture string
		info    string
		want    *provisionerv1.ModelArchitecture
	}{
		{
			spec:    "zai-org/GLM-5.2",
			fixture: "glm-5.2.json",
			info:    `{"id":"zai-org/GLM-5.2","safetensors":{"total":753329940480}}`,
			want: &provisionerv1.ModelArchitecture{
				Params: 753329940480, Layers: 78, KvHeads: 64, HeadDim: 192,
				HiddenSize: 6144, MaxPositionEmbeddings: 1048576,
				NumExperts: 256, NumExpertsPerTok: 8, MoeIntermediateSize: 2048,
				SharedExperts: 1, DenseLayers: 3, RoutedExpertHiddenSize: 6144,
			},
		},
		{
			spec:    "moonshotai/Kimi-K3",
			fixture: "kimi-k3.json",
			info:    `{"id":"moonshotai/Kimi-K3","safetensors":{"total":2779931837184}}`,
			want: &provisionerv1.ModelArchitecture{
				Params: 2779931837184, Layers: 93, KvHeads: 96, HeadDim: 74,
				HiddenSize: 7168, MaxPositionEmbeddings: 1048576,
				NumExperts: 896, NumExpertsPerTok: 16, MoeIntermediateSize: 3072,
				SharedExperts: 2, DenseLayers: 1, RoutedExpertHiddenSize: 3584,
			},
		},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			f := newArchFixture(t)
			f.infoBody = tc.info
			f.configBody = loadTestdataConfig(t, tc.fixture)

			got, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: tc.spec})
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(got.GetArchitecture(), tc.want) {
				t.Errorf("architecture mismatch\n got %v\nwant %v", got.GetArchitecture(), tc.want)
			}
		})
	}
}

// A model that runs its experts narrower than itself publishes the width,
// and reading it as the model's own computes an expert stack larger than
// the whole model. Everything else states nothing and means "full width",
// which is stored as the hidden size rather than left at zero, since
// every reader of the field multiplies by it.
func TestArchitectureReadsTheExpertWidthAModelProjectsDownTo(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture string
		want    int32
	}{
		{"Kimi K3 projects down to 3584", "kimi-k3.json", 3584},
		{"GLM 5.2 runs its experts at full width", "glm-5.2.json", 6144},
		{"Mixtral runs its experts at full width", "mixtral.json", 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArchFixture(t)
			f.infoBody = `{"id":"org/model","safetensors":{"total":1000000000}}`
			f.configBody = loadTestdataConfig(t, tc.fixture)

			got, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "org/model"})
			if err != nil {
				t.Fatal(err)
			}
			if n := got.GetArchitecture().GetRoutedExpertHiddenSize(); n != tc.want {
				t.Errorf("routed expert hidden size = %d, want %d", n, tc.want)
			}
		})
	}
}

// A dense model states no expert width and gets none, rather than
// inheriting its hidden size for an expert stack it does not have.
func TestArchitectureLeavesTheExpertWidthUnsetOnADenseModel(t *testing.T) {
	f := newArchFixture(t)
	f.infoBody = anchorInfo
	f.configBody = loadTestdataConfig(t, "qwen2.5-32b.json")

	got, err := f.store("").Architecture(context.Background(), &provisionerv1.DescribeModelRequest{ModelSpec: "Qwen/Qwen2.5-32B"})
	if err != nil {
		t.Fatal(err)
	}
	if n := got.GetArchitecture().GetRoutedExpertHiddenSize(); n != 0 {
		t.Errorf("routed expert hidden size = %d on a dense model, want 0", n)
	}
}
