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
