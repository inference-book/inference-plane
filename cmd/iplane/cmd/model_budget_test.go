package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/modelstores"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
	"github.com/inference-book/inference-plane/internal/vrambudget"
)

// llama70B is the shape the chapter's worked example is computed from:
// 70.6B parameters over 80 layers, 8 KV heads, head dimension 128,
// hidden size 8192, trained to 128k. Every figure in the golden output
// below traces to these numbers, so an arithmetic change that still
// passes is a change the chapter has to make too.
var llama70B = &provisionerv1.ModelArchitecture{
	Params:                70_600_000_000,
	Layers:                80,
	KvHeads:               8,
	HeadDim:               128,
	HiddenSize:            8192,
	MaxPositionEmbeddings: 131_072,
}

// budgetStore is a model store that answers the architecture capability
// from a fixture, standing in for the hub.
type budgetStore struct {
	arch *provisionerv1.ModelArchitecture
	saw  string
}

func (b *budgetStore) Resolve(_ context.Context, spec string) (modelstores.Resolved, error) {
	return modelstores.Resolved{EngineModelArg: spec}, nil
}

func (b *budgetStore) Architecture(_ context.Context, req *provisionerv1.DescribeModelRequest) (*provisionerv1.DescribeModelResponse, error) {
	b.saw = req.GetModelSpec()
	return &provisionerv1.DescribeModelResponse{Architecture: b.arch}, nil
}

// runBudget drives the real command over the real wire, the way an
// operator with a daemon does, and returns stdout with any error.
func runBudget(t *testing.T, arch *provisionerv1.ModelArchitecture, args ...string) (string, *budgetStore, error) {
	t.Helper()
	resetBudgetFlags()

	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	ms := &budgetStore{arch: arch}
	svc := provisioners.New(nil, store, "default", provisioners.WithModelStore(ms))

	mux := http.NewServeMux()
	path, handler := provisionerv1connect.NewProvisionerServiceHandler(provisioners.NewConnectProvisionerAdapter(svc))
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs(append([]string{"model", "--service-url", server.URL, "budget"}, args...))
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})

	err = rootCmd.Execute()
	return buf.String(), ms, err
}

// resetBudgetFlags restores the package-level flag state cobra parses
// into, so a test never inherits the previous one's flags.
func resetBudgetFlags() {
	budgetQuantization = "fp16"
	budgetKVQuantization = ""
	budgetMaxModelLen = 0
	budgetMaxBatch = 1
	budgetTP = 0
	budgetVRAMGB = 0
	budgetUtilization = 0.9
	budgetMaxCards = 8
	budgetRevision = "main"
	budgetOutput = outputTable
	budgetSessionsAt = ""
	budgetExpertParallel = false
	instanceServiceURL = ""
	modelBudgetCmd.Flags().Lookup("revision").Changed = false
}

func TestModelBudgetPrintsTheChaptersWorkedExample(t *testing.T) {
	// Compared whole rather than by substring. The chapter reproduces
	// this listing, so the test's job is to fail on any drift in the
	// arithmetic or the layout, not to confirm a few words survived.
	out, _, err := runBudget(t, llama70B,
		"meta-llama/Llama-3.3-70B-Instruct",
		"--quantization", "fp8", "--max-model-len", "32768", "--max-batch", "16", "--vram-gb", "80")
	if err != nil {
		t.Fatalf("budget: %v", err)
	}

	want := strings.Join([]string{
		"meta-llama/Llama-3.3-70B-Instruct  70.6B params  80 layers  8 kv-heads  head-dim 128",
		"plan   weights fp8, cache fp8, 32768 tokens x 16 sequences",
		// 85.9 GB rather than 80: the vendor's "80GB" label is a binary
		// count and the card holds 80 GiB. Reading it as decimal took
		// seven percent off every card before the arithmetic began
		// (#323). The verdicts below did not move, which is why the
		// chapter's conclusions survive the correction.
		"card   80 GB (80 GiB = 85.9 GB) at 0.90 utilization = 77.3 GB usable",
		"",
		"   cards   weights     cache   activation   overhead      total   verdict",
		// 194.8 rather than the 194.7 the ticket's listing prints: the
		// four terms sum to 194.791 GB, and the ticket's own columns add
		// up to 194.8 too. Reported on #320 so the chapter matches.
		"       1   70.6 GB   85.9 GB      12.9 GB    25.4 GB   194.8 GB   overcommitted",
		"       2   35.3 GB   42.9 GB       6.4 GB    12.7 GB    97.4 GB   overcommitted",
		"       4   17.6 GB   21.5 GB       3.2 GB     6.4 GB    48.7 GB   fits",
		"       8    8.8 GB   10.7 GB       1.6 GB     3.2 GB    24.3 GB   fits",
		"",
		"fewest cards that fit: 4",
		"largest term: cache, 51% of the working set",
		"",
	}, "\n")
	if out != want {
		t.Errorf("output drifted.\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

func TestModelBudgetShowsEveryTermForEveryCardCount(t *testing.T) {
	// A verdict without its terms tells an operator they were wrong
	// without telling them which claim overran, which is the one thing
	// they need in order to decide what to give up.
	out, _, err := runBudget(t, llama70B,
		"meta-llama/Llama-3.3-70B-Instruct",
		"--quantization", "fp8", "--max-model-len", "32768", "--max-batch", "16", "--vram-gb", "80")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"weights", "cache", "activation", "overhead", "total", "verdict"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q column", want)
		}
	}
	// The line the chapter turns on: at this context and batch the cache
	// is the largest claim, so quantizing the weights further barely
	// moves the card count.
	if !strings.Contains(out, "largest term: cache") {
		t.Errorf("largest term is not reported as the cache:\n%s", out)
	}
}

func TestModelBudgetNamesTheCardCountsItSkips(t *testing.T) {
	// A gap in the sequence reads as an oversight. The reason 4 and 8
	// are not candidates is exactly why the operator should stop asking
	// for them.
	narrow := &provisionerv1.ModelArchitecture{
		Params: 7_000_000_000, Layers: 32, KvHeads: 2, HeadDim: 128, HiddenSize: 4096,
	}
	out, _, err := runBudget(t, narrow, "org/narrow-7b",
		"--max-model-len", "8192", "--vram-gb", "80")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"skipped 4 cards", "skipped 8 cards", "replicate"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
}

func TestModelBudgetAgreesWithItselfAcrossTheTwoModes(t *testing.T) {
	// --tp pins one shape and the sweep walks the ladder. They are one
	// calculation, so a row they share has to carry identical numbers or
	// the command contradicts itself between invocations.
	args := []string{"meta-llama/Llama-3.3-70B-Instruct",
		"--quantization", "fp8", "--max-model-len", "32768", "--max-batch", "16", "--vram-gb", "80"}

	sweep, _, err := runBudget(t, llama70B, args...)
	if err != nil {
		t.Fatal(err)
	}
	pinned, _, err := runBudget(t, llama70B, append(append([]string{}, args...), "--tp", "4")...)
	if err != nil {
		t.Fatal(err)
	}

	row := budgetRowFor(t, sweep, "4")
	if !strings.Contains(pinned, "17.6 GB") || !strings.Contains(pinned, "48.7 GB") {
		t.Fatalf("--tp 4 did not report the four-card shape:\n%s", pinned)
	}
	for _, field := range strings.Fields(row) {
		if !strings.Contains(pinned, field) {
			t.Errorf("--tp 4 is missing %q, which the sweep's four-card row carries.\nsweep row: %s\npinned:\n%s", field, row, pinned)
		}
	}
}

// budgetRowFor returns the table row for a card count.
func budgetRowFor(t *testing.T, out, cards string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 1 && fields[0] == cards {
			return line
		}
	}
	t.Fatalf("no row for %s cards in:\n%s", cards, out)
	return ""
}

func TestModelBudgetExitsNonZeroWhenNothingFits(t *testing.T) {
	// The exit code is what makes this usable as a pre-flight gate in
	// front of a deploy, which is the whole reason for computing the
	// budget before renting rather than after.
	out, _, err := runBudget(t, llama70B, "meta-llama/Llama-3.3-70B-Instruct",
		"--max-model-len", "131072", "--max-batch", "32", "--vram-gb", "24")
	if err == nil {
		t.Fatal("want a non-zero exit when no shape fits")
	}
	var ec exitCoder
	if !errors.As(err, &ec) || ec.ExitCode() != 1 {
		t.Errorf("error does not carry exit code 1: %v", err)
	}
	if msg := err.Error(); msg != "" {
		t.Errorf("error carries message %q; the table already said it, and a stderr copy reads as a second failure", msg)
	}
	// The table still has to print. An operator who is told only "no"
	// cannot see how far off they were.
	if !strings.Contains(out, "fewest cards that fit: none") || !strings.Contains(out, "overcommitted") {
		t.Errorf("refusal printed no table:\n%s", out)
	}
}

func TestModelBudgetQuantizesTheCacheIndependentlyOfTheWeights(t *testing.T) {
	// An engine can hold the cache at one byte with the weights at two.
	// Collapsing the two flags would make the cache term, which is the
	// one that usually decides the deploy, unreachable on its own.
	both, _, err := runBudget(t, llama70B, "meta-llama/Llama-3.3-70B-Instruct",
		"--quantization", "fp16", "--max-model-len", "32768", "--max-batch", "16",
		"--vram-gb", "80", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	split, _, err := runBudget(t, llama70B, "meta-llama/Llama-3.3-70B-Instruct",
		"--quantization", "fp16", "--kv-cache-quantization", "fp8",
		"--max-model-len", "32768", "--max-batch", "16", "--vram-gb", "80", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}

	a, b := decodeBudget(t, both), decodeBudget(t, split)
	if a.Candidates[0].WeightBytes != b.Candidates[0].WeightBytes {
		t.Errorf("the cache flag moved the weights: %d then %d", a.Candidates[0].WeightBytes, b.Candidates[0].WeightBytes)
	}
	if want := a.Candidates[0].KVBytes / 2; b.Candidates[0].KVBytes != want {
		t.Errorf("fp8 cache = %d bytes, want half of fp16's %d", b.Candidates[0].KVBytes, a.Candidates[0].KVBytes)
	}
}

func decodeBudget(t *testing.T, out string) budgetJSON {
	t.Helper()
	var doc budgetJSON
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decode --output json: %v\n%s", err, out)
	}
	return doc
}

func TestModelBudgetTakesTheContextLengthFromTheModel(t *testing.T) {
	// The default is the model's own trained window rather than a house
	// number, because a house number is wrong for every model whose
	// window is not the one somebody picked.
	out, _, err := runBudget(t, llama70B, "meta-llama/Llama-3.3-70B-Instruct", "--vram-gb", "80")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "131072 tokens x 1 sequences") {
		t.Errorf("did not default to the model's 128k window:\n%s", out)
	}
}

func TestModelBudgetAsksForAContextLengthWhenTheModelPublishesNone(t *testing.T) {
	// Guessing here would be a budget for a context the operator never
	// asked about, reported as confidently as one they did.
	silent := &provisionerv1.ModelArchitecture{
		Params: 7_000_000_000, Layers: 32, KvHeads: 8, HeadDim: 128, HiddenSize: 4096,
	}
	_, _, err := runBudget(t, silent, "org/silent-7b", "--vram-gb", "80")
	if err == nil {
		t.Fatal("want a refusal when there is no window to default to")
	}
	if !strings.Contains(err.Error(), "--max-model-len") {
		t.Errorf("error does not name the flag that fixes it: %v", err)
	}
}

func TestModelBudgetRefusesAnOutOfRangeUtilization(t *testing.T) {
	// UsableBytes silently falls back to the default for anything
	// outside (0, 1]. A library correcting a caller is defensible; a
	// command answering a different question than the operator typed,
	// with the same authority, is not.
	_, _, err := runBudget(t, llama70B, "meta-llama/Llama-3.3-70B-Instruct",
		"--vram-gb", "80", "--gpu-memory-utilization", "90")
	if err == nil {
		t.Fatal("want a refusal for a utilization of 90")
	}
	if !strings.Contains(err.Error(), "gpu-memory-utilization") {
		t.Errorf("error does not name the flag: %v", err)
	}
}

func TestModelBudgetRequiresACard(t *testing.T) {
	_, _, err := runBudget(t, llama70B, "meta-llama/Llama-3.3-70B-Instruct")
	if err == nil || !strings.Contains(err.Error(), "--vram-gb") {
		t.Fatalf("want a refusal naming --vram-gb, got %v", err)
	}
}

func TestModelBudgetFoldsTheRevisionIntoTheSpec(t *testing.T) {
	// --revision rides the spec grammar the hub read already parses, so
	// the request needs no field of its own.
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"flag", []string{"org/m", "--revision", "abc123"}, "org/m:abc123"},
		{"inline", []string{"org/m:abc123"}, "org/m:abc123"},
		{"neither", []string{"org/m"}, "org/m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ms, err := runBudget(t, llama70B, append(tc.args, "--vram-gb", "80")...)
			if err != nil {
				t.Fatal(err)
			}
			if ms.saw != tc.want {
				t.Errorf("asked the hub for %q, want %q", ms.saw, tc.want)
			}
		})
	}
}

func TestModelBudgetRefusesTwoRevisionsThatDisagree(t *testing.T) {
	// Deciding which one wins would be a silent choice about which
	// weights got budgeted, and both spellings look deliberate.
	_, _, err := runBudget(t, llama70B, "org/m:abc123", "--revision", "def456", "--vram-gb", "80")
	if err == nil {
		t.Fatal("want a refusal when the spec and the flag pin different revisions")
	}
	if !strings.Contains(err.Error(), "abc123") || !strings.Contains(err.Error(), "def456") {
		t.Errorf("error does not show both revisions: %v", err)
	}
}

func TestModelBudgetReadsTheCardLabelAsBinary(t *testing.T) {
	// A vendor's "80GB" is 80 GiB, which is 85.9 decimal GB. Reading the
	// label as decimal understated every card by seven percent, and the
	// error runs in the direction that refuses a shape which fits.
	out, _, err := runBudget(t, llama70B, "meta-llama/Llama-3.3-70B-Instruct", "--vram-gb", "80")
	if err != nil && !strings.Contains(out, "cards") {
		t.Fatal(err)
	}
	if !strings.Contains(out, "80 GiB = 85.9 GB") {
		t.Errorf("the card line does not show what the label was read as:\n%s", out)
	}
	if !strings.Contains(out, "77.3 GB usable") {
		t.Errorf("usable memory is not 0.9 of 85.9 GB:\n%s", out)
	}
}

// glm52Budget is GLM-5.2's published shape: a compressed latent cache on
// every one of its 78 layers, which is what makes the card columns below
// behave differently from a dense model's, and a 256-expert stack on 76
// of them, which is what an expert-parallel plan divides.
//
// The expert fields change nothing on the tensor axis, where the routed
// share and the rest divide by the same card count, and the goldens above
// are the proof of it.
var glm52Budget = &provisionerv1.ModelArchitecture{
	Params: 753_329_940_480, Layers: 78, HiddenSize: 6144,
	MaxPositionEmbeddings: 1_048_576,
	KvLoraRank:            512, QkRopeHeadDim: 64,
	DenseLayers: 3, MtpLayers: 1,
	NumExperts: 256, NumExpertsPerTok: 8, SharedExperts: 1,
	MoeIntermediateSize: 2048,
}

// The wall, whole. Concurrency collapsing down the page is the chapter's
// central result, and it is held as a golden because a figure gets drawn
// from these numbers.
func TestModelBudgetShowsConcurrencyCollapsingWithContext(t *testing.T) {
	out, _, err := runBudget(t, glm52Budget, "zai-org/GLM-5.2",
		"--vram-gb", "80", "--quantization", "fp8", "--sessions-at", "8k,128k,1M", "--max-cards", "32")
	if err != nil {
		t.Fatal(err)
	}

	want := `zai-org/GLM-5.2  753.3B params  78 layers
plan   weights fp8, cache fp8
card   80 GB (80 GiB = 85.9 GB) at 0.90 utilization = 77.3 GB usable

   context   1 card   2 cards   4 cards   8 cards   16 cards   32 cards   cache/session
       8k:        -         -         -         -         53        117   0.4 GB
     128k:        -         -         -         -          3          7   5.9 GB
       1M:        -         -         -         -          -          -   47.1 GB

cache/session is per card at 32 cards.
this model caches a compressed latent, which is replicated on every card rather than
sharded, so cards buy room for weights and none for cache.
`
	if out != want {
		t.Errorf("sessions table\n got:\n%s\nwant:\n%s", out, want)
	}
}

// The 128k row, by hand, against the arithmetic rather than against the
// implementation. 78 layers of a 512 + 64 latent at one byte over 131072
// tokens is 5,888,802,816 bytes of cache per session; the activation
// scratch adds 75,497,472 per card across 32; and the room left after
// 753.33B fp8 weights on a 77.3 GB card, with the overhead band intact,
// divides by that into seven.
func TestModelBudgetSessionCountMatchesTheHandComputedArithmetic(t *testing.T) {
	usable := float64(vrambudget.UsableBytes(80*vrambudget.GiB, 0.9))
	room := usable/(1+vrambudget.OverheadFraction) - 753_329_940_480.0/32
	cache := 78.0 * (512 + 64) * 1 * 131072
	activation := 131072.0 * 6144 * 2 * vrambudget.ActivationFactor / 32
	want := int64(room / (cache + activation))

	if want != 7 {
		t.Fatalf("the hand computation itself gives %d, so the test is wrong before the code is", want)
	}
	out, _, err := runBudget(t, glm52Budget, "zai-org/GLM-5.2",
		"--vram-gb", "80", "--quantization", "fp8", "--sessions-at", "128k", "--max-cards", "32")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "     128k:        -         -         -         -          3          7   5.9 GB") {
		t.Errorf("want a 128k row ending in 7 sessions on 32 cards, got:\n%s", out)
	}
}

// A model whose cache shards gains far more from cards than one whose
// latent is replicated, and the table is where an operator sees that.
func TestModelBudgetShowsCardsBuyingConcurrencyWhenTheCacheShards(t *testing.T) {
	out, _, err := runBudget(t, llama70B, "meta-llama/Llama-3.3-70B-Instruct",
		"--vram-gb", "80", "--quantization", "fp8", "--sessions-at", "8k", "--max-cards", "8")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "replicated on every card") {
		t.Errorf("named the latent caveat for a per-head model:\n%s", out)
	}
	if !strings.Contains(out, "       8k:        -        41       128       302") {
		t.Errorf("want concurrency multiplying across the card ladder, got:\n%s", out)
	}
}

func TestModelBudgetReadsTheContextLadderTheWayOperatorsWriteIt(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []int32
	}{
		{"8k", []int32{8192}},
		{"8k,128k,1M", []int32{8192, 131072, 1048576}},
		{"4096", []int32{4096}},
		{" 8K , 1m ", []int32{8192, 1048576}},
		{"", nil},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseContextLadder(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestModelBudgetRefusesAContextLadderItCannotRead(t *testing.T) {
	for _, in := range []string{"8g", "abc", "0", "-1", "8k,"} {
		t.Run(in, func(t *testing.T) {
			got, err := parseContextLadder(in)
			if in == "8k," {
				// A trailing comma is a typo with an obvious reading,
				// not an error worth stopping for.
				if err != nil || len(got) != 1 {
					t.Errorf("got %v, %v; want the one length it names", got, err)
				}
				return
			}
			if err == nil {
				t.Errorf("accepted %q as a context length", in)
			}
		})
	}
}

// The exit status is the pre-flight gate, and it has to mean the same
// thing in this mode: zero when some shape works, one when none does.
func TestModelBudgetExitsNonZeroWhenNoShapeHoldsASession(t *testing.T) {
	_, _, err := runBudget(t, glm52Budget, "zai-org/GLM-5.2",
		"--vram-gb", "80", "--quantization", "fp16", "--sessions-at", "1M", "--max-cards", "2")
	if err == nil {
		t.Fatal("exited zero when nothing holds a session")
	}
}

// The concurrency question names its own context lengths, so a model
// that publishes no trained window must not be refused for lacking one.
// That model is exactly the case where an operator names the ladder.
func TestModelBudgetSessionsDoNotNeedTheModelToPublishAWindow(t *testing.T) {
	noWindow := &provisionerv1.ModelArchitecture{
		Params: 70_600_000_000, Layers: 80, KvHeads: 8, HeadDim: 128, HiddenSize: 8192,
	}
	out, _, err := runBudget(t, noWindow, "org/model",
		"--vram-gb", "80", "--quantization", "fp8", "--sessions-at", "8k", "--max-cards", "8")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "8k:") {
		t.Errorf("want an 8k row, got:\n%s", out)
	}
}

// Both modes have to agree, since one is a table drawn from the other's
// numbers and a figure gets drawn from whichever the reader ran.
func TestModelBudgetSessionsAgreesAcrossTheTwoOutputModes(t *testing.T) {
	out, _, err := runBudget(t, glm52Budget, "zai-org/GLM-5.2",
		"--vram-gb", "80", "--quantization", "fp8", "--sessions-at", "8k,128k", "--max-cards", "32", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	var got sessionsJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("parse json: %v\n%s", err, out)
	}
	if !got.LatentCache {
		t.Error("json did not record the latent cache the table names in prose")
	}
	if len(got.CardCounts) != 6 || got.CardCounts[5] != 32 {
		t.Fatalf("card counts = %v, want the powers of two up to 32", got.CardCounts)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(got.Rows))
	}
	if n := got.Rows[0].Sessions[5]; n != 117 {
		t.Errorf("8k on 32 cards = %d, want the 117 the table prints", n)
	}
	if n := got.Rows[1].Sessions[5]; n != 7 {
		t.Errorf("128k on 32 cards = %d, want the 7 the table prints", n)
	}
	if got.Rows[1].CacheBytesPerSession != 5_888_802_816 {
		t.Errorf("cache per session = %d, want 5888802816", got.Rows[1].CacheBytesPerSession)
	}
}

// The plan #387 exists for. `deployment deploy --ep 8 --tp 1` sizes a
// GLM-5.2 rental correctly and this command could not express the same
// shape, so an operator checking the rental by hand read the tensor row
// and saw twelve gigabytes per card that the deploy path knew about.
//
// Held whole rather than by row: the note under the table is what tells a
// reader that a row here is a card count and `--ep` there is a degree,
// and a number nobody can translate is not an answer.
func TestModelBudgetSizesAnExpertParallelPlan(t *testing.T) {
	out, _, err := runBudget(t, glm52Budget, "zai-org/GLM-5.2",
		"--quantization", "mxfp4", "--max-model-len", "8192", "--max-batch", "8",
		"--tp", "1", "--expert-parallel", "--vram-gb", "80")
	// Exit 1, because the widest row is tight rather than fitting: it
	// clears only by eating the overhead band. That is the answer rather
	// than a failure of the command.
	var ec exitCoder
	if !errors.As(err, &ec) || ec.ExitCode() != 1 {
		t.Fatalf("expected the exit gate to refuse a plan whose best row is only tight, got %v", err)
	}

	want := strings.Join([]string{
		"zai-org/GLM-5.2  753.3B params  78 layers  0 kv-heads  head-dim 0",
		"plan   weights mxfp4, cache mxfp4, 8192 tokens x 8 sequences, experts across every card, tensor width 1",
		"card   80 GB (80 GiB = 85.9 GB) at 0.90 utilization = 77.3 GB usable",
		"",
		"   cards    weights    cache   activation   overhead      total   verdict",
		// The routed experts divide by the row and the attention, the
		// embeddings and the dense layers do not, so the weight column
		// falls far more slowly than the tensor table's does.
		"       1   429.4 GB   2.9 GB       1.2 GB    65.0 GB   498.6 GB   overcommitted",
		"       2   220.1 GB   2.9 GB       1.2 GB    33.6 GB   257.9 GB   overcommitted",
		"       4   115.4 GB   2.9 GB       1.2 GB    17.9 GB   137.5 GB   overcommitted",
		// 77.3 against 77.3 usable. The boundary almost exactly, which is
		// worth knowing before reading confidence into either side of it.
		"       8    63.1 GB   2.9 GB       1.2 GB    10.1 GB    77.3 GB   tight",
		"",
		"each row spreads the routed experts across all of its cards: 8 cards is --ep 8 --tp 1, so 8 data-parallel rank(s) each holding a whole copy of the attention, the embeddings and the dense layers.",
		"",
		"fewest cards that fit: none within --max-cards 8",
		"largest term: weights, 99% of the working set",
		"",
	}, "\n")
	if out != want {
		t.Errorf("output drifted.\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

// The same eight cards under the two arrangements, which is the gap the
// ticket measures: twelve gigabytes per card, and the difference between
// a verdict of fits and a verdict of tight.
func TestModelBudgetExpertParallelIsNotTheTensorRow(t *testing.T) {
	args := []string{"zai-org/GLM-5.2", "--quantization", "mxfp4", "--max-model-len", "8192",
		"--max-batch", "8", "--vram-gb", "80", "--output", outputJSON}
	tensor, _, err := runBudget(t, glm52Budget, args...)
	if err != nil {
		t.Fatal(err)
	}
	expert, _, _ := runBudget(t, glm52Budget, append(args, "--tp", "1", "--expert-parallel")...)

	tensorRow := budgetRowByCards(t, decodeBudget(t, tensor), 8)
	expertRow := budgetRowByCards(t, decodeBudget(t, expert), 8)
	if tensorRow.TotalBytes >= expertRow.TotalBytes {
		t.Errorf("expert parallelism replicates what tensor parallelism shards, so it must cost more per card: tensor %d, expert %d",
			tensorRow.TotalBytes, expertRow.TotalBytes)
	}
	if tensorRow.Verdict != "fits" || expertRow.Verdict != "tight" {
		t.Errorf("verdicts drifted: tensor %q, expert %q (want fits and tight)", tensorRow.Verdict, expertRow.Verdict)
	}
}

// The agreement the ticket asks for, checked against the deploy path
// itself rather than against a figure copied out of it. `CreateDeployment`
// reads its plan back out of the engine arguments and the parallelism
// message, so that is what this builds, and the two have to land on the
// same byte count for the same shape.
func TestModelBudgetExpertParallelAgreesWithTheDeployPath(t *testing.T) {
	out, _, _ := runBudget(t, glm52Budget, "zai-org/GLM-5.2",
		"--quantization", "mxfp4", "--max-model-len", "8192", "--max-batch", "8",
		"--tp", "1", "--expert-parallel", "--vram-gb", "80", "--output", outputJSON)
	row := budgetRowByCards(t, decodeBudget(t, out), 8)

	plan, usable := provisioners.EnginePlan(
		[]string{"--quantization", "mxfp4", "--max-model-len", "8192", "--max-num-seqs", "8"},
		&provisionerv1.Parallelism{TensorParallelSize: 1, ExpertParallelSize: 8})
	if !usable {
		t.Fatal("the deploy path could not read a plan out of these arguments")
	}
	deploy, err := vrambudget.Compute(glm52Budget, plan)
	if err != nil {
		t.Fatal(err)
	}
	if deploy.TotalBytes() != row.TotalBytes {
		t.Errorf("the budget and the deploy path disagree about the same plan: budget %d, deploy %d",
			row.TotalBytes, deploy.TotalBytes())
	}
}

// A row narrower than the tensor width is not a shape, and neither is one
// the width does not divide: there is no whole number of data-parallel
// ranks either way. Reported as a skip with its reason, the way the
// tensor sweep reports a card count the KV heads do not divide.
func TestModelBudgetSkipsExpertRowsTheTensorWidthDoesNotDivide(t *testing.T) {
	out, _, _ := runBudget(t, glm52Budget, "zai-org/GLM-5.2",
		"--quantization", "mxfp4", "--max-model-len", "8192", "--max-batch", "8",
		"--tp", "2", "--expert-parallel", "--vram-gb", "80")

	if !strings.Contains(out, "skipped 1 cards: a tensor width of 2 does not divide a 1-card row") {
		t.Errorf("no skip line for the row narrower than the tensor width:\n%s", out)
	}
	// And it is a skip rather than a row with numbers in it: a budget for
	// a shape no engine would run reads as a plan.
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[0] == "1" {
			t.Errorf("the skipped row was budgeted anyway: %q", line)
		}
	}
}

// The concurrency question under the same plan, by hand rather than
// against the implementation. Eight cards at tp=1 hold 63.1 GB of weights
// each, which leaves 4.13 GB of the 77.3 GB card once the overhead band
// is kept intact, and a session at 8k costs 0.368 GB of latent cache plus
// 0.151 GB of activation scratch. That divides into seven.
//
// Wrong before #387 in the expensive direction: the weight term divided
// the whole model by eight, understating it by 9.4 GB per card, and the
// table reported room for thirty-two sessions that the hardware does not
// have.
func TestModelBudgetExpertParallelSessionsMatchTheHandComputedArithmetic(t *testing.T) {
	out, _, err := runBudget(t, glm52Budget, "zai-org/GLM-5.2",
		"--quantization", "mxfp4", "--sessions-at", "8k", "--tp", "1",
		"--expert-parallel", "--vram-gb", "80", "--output", outputJSON)
	if err != nil {
		t.Fatal(err)
	}
	var doc sessionsJSON
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if len(doc.Rows) != 1 || len(doc.Rows[0].Sessions) != len(doc.CardCounts) {
		t.Fatalf("unexpected shape: %+v", doc)
	}
	if got := doc.Rows[0].Sessions[len(doc.CardCounts)-1]; got != 7 {
		t.Errorf("sessions on %d cards = %d, want 7", doc.CardCounts[len(doc.CardCounts)-1], got)
	}
}

// Under --expert-parallel, --tp is a width the engine has to shard a
// layer across rather than a row to look up, so the answer for a width no
// engine shards across is the same refusal the tensor sweep gives.
func TestModelBudgetRefusesAnExpertParallelTensorWidthNoEngineShardsAcross(t *testing.T) {
	_, _, err := runBudget(t, glm52Budget, "zai-org/GLM-5.2",
		"--quantization", "mxfp4", "--max-model-len", "8192", "--max-batch", "8",
		"--tp", "3", "--expert-parallel", "--vram-gb", "80")
	if err == nil || !strings.Contains(err.Error(), "powers of two") {
		t.Errorf("want the powers-of-two refusal for --tp 3, got %v", err)
	}
	// An unset --tp is a width of one rather than a width nobody named,
	// so it must not be caught by the same rule.
	if _, _, err := runBudget(t, glm52Budget, "zai-org/GLM-5.2",
		"--quantization", "mxfp4", "--max-model-len", "8192", "--max-batch", "8",
		"--expert-parallel", "--vram-gb", "80"); err != nil {
		var ec exitCoder
		if !errors.As(err, &ec) {
			t.Errorf("--expert-parallel without --tp was refused: %v", err)
		}
	}
}

// budgetRowByCards returns the JSON row for a card count.
func budgetRowByCards(t *testing.T, doc budgetJSON, cards int32) budgetRowJSON {
	t.Helper()
	for _, row := range doc.Candidates {
		if row.Cards == cards {
			return row
		}
	}
	t.Fatalf("no row for %d cards in %+v", cards, doc.Candidates)
	return budgetRowJSON{}
}
