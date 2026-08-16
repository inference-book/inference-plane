package vrambudget

import (
	"math"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
)

// anchor is Qwen2.5-32B, the model the book works its budget against:
// 32.76B parameters, 64 layers, 40 attention heads but only 8 KV heads,
// head dimension 128, hidden size 5120.
//
// Every expected figure in this file traces back to that worked example,
// so a change to the arithmetic that still passes these tests is a change
// the chapter also has to make.
var anchor = &Arch{
	Params:     32_760_000_000,
	Layers:     64,
	KvHeads:    8,
	HeadDim:    128,
	HiddenSize: 5120,
}

func gb(b int64) float64 { return float64(b) / float64(GB) }

// closeTo reports whether got is within tol GB of want.
func closeTo(got int64, wantGB, tolGB float64) bool {
	return math.Abs(gb(got)-wantGB) <= tolGB
}

func TestKVBytesPerTokenMatchesTheWorkedExample(t *testing.T) {
	// 2 x 64 layers x 8 kv heads x 128 head dim x 2 bytes = 262,144, or
	// 256 KiB of cache for one token at half precision.
	b, err := Compute(anchor, Plan{Weights: PrecisionFP16, MaxModelLen: 1, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := b.KVBytesPerToken, int64(262_144); got != want {
		t.Errorf("KV bytes per token = %d, want %d", got, want)
	}
}

func TestOneLongConversationCostsThirtyTwoGiB(t *testing.T) {
	// The anchor's 128k window, one sequence: 32 GiB of cache, which is
	// more than the model's own 4-bit weights.
	b, err := Compute(anchor, Plan{Weights: PrecisionFP16, MaxModelLen: 131_072, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := b.KVBytes, 32*GiB; got != want {
		t.Errorf("KV bytes = %d (%.1f GiB), want %d (32 GiB)", got, float64(got)/float64(GiB), want)
	}
}

func TestTheAnchorDoesNotFitAnEightyGigabyteCard(t *testing.T) {
	// The book's headline: one conversation at a context length users
	// expect overflows the biggest single card there is, with no large
	// batch to blame.
	b, err := Compute(anchor, Plan{Weights: PrecisionFP16, MaxModelLen: 131_072, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}

	if !closeTo(b.WeightBytes, 65.5, 0.1) {
		t.Errorf("weights = %.1f GB, want ~65.5", gb(b.WeightBytes))
	}
	if !closeTo(b.ActivationBytes, 2.0, 0.2) {
		t.Errorf("activations = %.1f GB, want ~2", gb(b.ActivationBytes))
	}
	if !closeTo(b.OverheadBytes, 15.3, 0.5) {
		t.Errorf("overhead = %.1f GB, want ~15.3", gb(b.OverheadBytes))
	}

	if v := b.Against(UsableBytes(80*GB, DefaultUtilization)); v != Overcommitted {
		t.Errorf("verdict against an 80 GB card = %v, want overcommitted (total %.1f GB)", v, gb(b.TotalBytes()))
	}
}

func TestQuantizingWeightsDoesNotShrinkTheCache(t *testing.T) {
	// Quantization is a knob on the weight term. Shrinking the weights
	// while leaving the cache alone is the whole reason the cache becomes
	// the binding constraint at long context, so a change that quietly
	// scaled the cache by the weight precision would erase the lesson.
	half, err := Compute(anchor, Plan{Weights: PrecisionFP16, KVCache: PrecisionFP16, MaxModelLen: 131_072, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	quant, err := Compute(anchor, Plan{Weights: PrecisionAWQ, KVCache: PrecisionFP16, MaxModelLen: 131_072, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	if quant.KVBytes != half.KVBytes {
		t.Errorf("KV bytes moved with the weight precision: %d -> %d", half.KVBytes, quant.KVBytes)
	}
	if quant.WeightBytes >= half.WeightBytes {
		t.Errorf("weights did not shrink: %d -> %d", half.WeightBytes, quant.WeightBytes)
	}
}

func TestFourBitWeightsAreBudgetedAboveTheNominalHalfByte(t *testing.T) {
	// A four-bit build keeps 16-bit scales per group and leaves the
	// embeddings and output head higher, landing near 4.8 effective bits.
	// Budgeting a clean 0.5 is the difference between a model that fits a
	// 24 GB card with cache headroom and one that does not, so the
	// distinction is asserted rather than left to the constant.
	for _, p := range []Precision{PrecisionAWQ, PrecisionGPTQ} {
		if got := p.BytesPerParam(); got != 0.6 {
			t.Errorf("%s bytes per param = %v, want 0.6", p, got)
		}
	}
	b, err := Compute(anchor, Plan{Weights: PrecisionAWQ, MaxModelLen: 1, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	// The published four-bit build of the anchor is about 19.3 GB.
	if !closeTo(b.WeightBytes, 19.7, 0.5) {
		t.Errorf("4-bit weights = %.1f GB, want ~19.7 (the published build is ~19.3)", gb(b.WeightBytes))
	}
	if closeTo(b.WeightBytes, 16.4, 0.5) {
		t.Errorf("4-bit weights budgeted at the nominal half byte (%.1f GB); the real build is larger", gb(b.WeightBytes))
	}
}

func TestActivationsDoNotShrinkWithTheWeightPrecision(t *testing.T) {
	// A quantized model dequantizes to half precision for the matrix
	// multiply, so the forward pass holds the same scratch either way.
	// Scaling this term by the weight precision would credit four-bit
	// weights with a saving on a buffer they do not touch, and it would
	// hide behind the anchor's fp16 case, where the two are the same
	// number.
	base := Plan{Weights: PrecisionFP16, KVCache: PrecisionFP16, MaxModelLen: 8192, MaxBatch: 4}
	half, err := Compute(anchor, base)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []Precision{PrecisionFP8, PrecisionINT8, PrecisionAWQ, PrecisionGPTQ} {
		p := base
		p.Weights = q
		b, err := Compute(anchor, p)
		if err != nil {
			t.Fatal(err)
		}
		if b.ActivationBytes != half.ActivationBytes {
			t.Errorf("%s activations = %d, want %d (unchanged from half precision)", q, b.ActivationBytes, half.ActivationBytes)
		}
	}
}

func TestFP8CacheHalvesThePerTokenCost(t *testing.T) {
	// Holding keys and values at one byte instead of two halves the term
	// that grows fastest, so the same pool holds twice the tokens.
	half, err := Compute(anchor, Plan{Weights: PrecisionFP16, MaxModelLen: 131_072, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	eight, err := Compute(anchor, Plan{Weights: PrecisionFP16, KVCache: PrecisionFP8, MaxModelLen: 131_072, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := eight.KVBytes*2, half.KVBytes; got != want {
		t.Errorf("fp8 cache = %d, want exactly half of %d", eight.KVBytes, half.KVBytes)
	}
	if eight.WeightBytes != half.WeightBytes {
		t.Errorf("cache precision moved the weight term: %d -> %d", half.WeightBytes, eight.WeightBytes)
	}
}

func TestCacheIsLinearInContextAndInBatch(t *testing.T) {
	// Context length and batch size multiply the same per-token cost, so
	// one fixed pool trades one against the other. Doubling either has to
	// do the same thing to the cache.
	base := Plan{Weights: PrecisionFP16, MaxModelLen: 8192, MaxBatch: 4}
	b0, err := Compute(anchor, base)
	if err != nil {
		t.Fatal(err)
	}
	longer := base
	longer.MaxModelLen *= 2
	b1, err := Compute(anchor, longer)
	if err != nil {
		t.Fatal(err)
	}
	wider := base
	wider.MaxBatch *= 2
	b2, err := Compute(anchor, wider)
	if err != nil {
		t.Fatal(err)
	}
	if b1.KVBytes != 2*b0.KVBytes {
		t.Errorf("doubling context: KV %d -> %d, want exactly double", b0.KVBytes, b1.KVBytes)
	}
	if b2.KVBytes != b1.KVBytes {
		t.Errorf("doubling batch gave %d but doubling context gave %d; they must be interchangeable", b2.KVBytes, b1.KVBytes)
	}
}

func TestOverheadIsTakenOnTheWorkingSetNotTheCard(t *testing.T) {
	// A fraction of the card would make the overhead band independent of
	// what the deployment actually holds, which is the opposite of what
	// it stands for.
	b, err := Compute(anchor, Plan{Weights: PrecisionFP16, MaxModelLen: 8192, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	want := int64(float64(b.WorkingSetBytes()) * OverheadFraction)
	if b.OverheadBytes != want {
		t.Errorf("overhead = %d, want %d (%.0f%% of the working set)", b.OverheadBytes, want, OverheadFraction*100)
	}
}

func TestTightIsDistinctFromFitsAndFromOvercommitted(t *testing.T) {
	// A deployment that clears the card only by eating the overhead band
	// is the worst of the three to discover after renting: it starts, and
	// it fails minutes into real traffic. Collapsing it into either
	// neighbour loses the only verdict worth acting on.
	b, err := Compute(anchor, Plan{Weights: PrecisionFP16, MaxModelLen: 8192, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	if v := b.Against(b.TotalBytes()); v != Fits {
		t.Errorf("exactly enough for the total = %v, want fits", v)
	}
	if v := b.Against(b.TotalBytes() - 1); v != Tight {
		t.Errorf("one byte into the overhead band = %v, want tight", v)
	}
	if v := b.Against(b.WorkingSetBytes()); v != Tight {
		t.Errorf("exactly the working set = %v, want tight", v)
	}
	if v := b.Against(b.WorkingSetBytes() - 1); v != Overcommitted {
		t.Errorf("one byte under the working set = %v, want overcommitted", v)
	}
}

func TestUtilizationCapsWhatTheEngineMayClaim(t *testing.T) {
	// Claiming the whole card buys an allocation failure at startup
	// rather than extra cache, because the CUDA context and the driver's
	// allocations sit outside the engine's accounting.
	if got, want := UsableBytes(80*GB, 0.9), int64(72*GB); got != want {
		t.Errorf("usable = %d, want %d", got, want)
	}
	// Out-of-range utilizations fall back rather than producing a card
	// larger than itself or one with no memory at all.
	for _, u := range []float64{0, -1, 1.5} {
		if got, want := UsableBytes(80*GB, u), UsableBytes(80*GB, DefaultUtilization); got != want {
			t.Errorf("utilization %v: usable = %d, want the default %d", u, got, want)
		}
	}
}

func TestTensorParallelismDividesTheClaimsAndNotTheOverhead(t *testing.T) {
	// Each card holds a shard of the weights, the cache, and the
	// activations. A CUDA context and an allocator pool exist once per
	// card, so the overhead band is taken per card rather than split.
	one, err := Compute(anchor, Plan{Weights: PrecisionFP16, MaxModelLen: 131_072, MaxBatch: 1, TPSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	four, err := Compute(anchor, Plan{Weights: PrecisionFP16, MaxModelLen: 131_072, MaxBatch: 1, TPSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := four.WeightBytes, one.WeightBytes/4; got != want {
		t.Errorf("weights per card = %d, want %d", got, want)
	}
	if got, want := four.KVBytes, one.KVBytes/4; got != want {
		t.Errorf("KV per card = %d, want %d", got, want)
	}
	if got, want := four.OverheadBytes, int64(float64(four.WorkingSetBytes())*OverheadFraction); got != want {
		t.Errorf("overhead per card = %d, want %d", got, want)
	}
	if four.OverheadBytes >= one.OverheadBytes {
		t.Errorf("overhead did not follow the per-card working set down: %d -> %d", one.OverheadBytes, four.OverheadBytes)
	}
}

func TestZeroAndOneCardsMeanTheSameThing(t *testing.T) {
	p := Plan{Weights: PrecisionFP16, MaxModelLen: 4096, MaxBatch: 1}
	unset, err := Compute(anchor, p)
	if err != nil {
		t.Fatal(err)
	}
	p.TPSize = 1
	single, err := Compute(anchor, p)
	if err != nil {
		t.Fatal(err)
	}
	if unset != single {
		t.Errorf("TPSize 0 gave %+v, TPSize 1 gave %+v", unset, single)
	}
	if unset.Cards != 1 {
		t.Errorf("Cards = %d, want 1", unset.Cards)
	}
}

func TestMinCardsFindsTheShapeThatHoldsTheAnchor(t *testing.T) {
	// The anchor at 128k does not fit one 80 GB card. It is the sort of
	// question an operator asks before choosing hardware, and the answer
	// is a card count rather than a yes or no.
	p := Plan{Weights: PrecisionFP16, MaxModelLen: 131_072, MaxBatch: 1}
	n, b, err := MinCards(anchor, p, 80*GB, DefaultUtilization, 8)
	if err != nil {
		t.Fatalf("MinCards: %v", err)
	}
	if n != 2 {
		t.Errorf("min cards = %d, want 2 (per-card total %.1f GB)", n, gb(b.TotalBytes()))
	}
	if b.Cards != n {
		t.Errorf("budget reports %d cards, MinCards returned %d", b.Cards, n)
	}
	if v := b.Against(UsableBytes(80*GB, DefaultUtilization)); v != Fits {
		t.Errorf("returned budget does not fit: %v", v)
	}
}

func TestMinCardsReturnsOneWhenOneIsEnough(t *testing.T) {
	// A short context is the case the whole chapter is contrasted
	// against, and a calculator that always reaches for more cards would
	// sell hardware nobody needs.
	p := Plan{Weights: PrecisionAWQ, MaxModelLen: 4096, MaxBatch: 1}
	n, _, err := MinCards(anchor, p, 80*GB, DefaultUtilization, 8)
	if err != nil {
		t.Fatalf("MinCards: %v", err)
	}
	if n != 1 {
		t.Errorf("min cards = %d, want 1", n)
	}
}

func TestMinCardsWillNotCountOnAShardThatWouldNotHappen(t *testing.T) {
	// Two KV heads cannot shard across four cards. An engine replicates
	// them instead, so the cache term stops shrinking while the bill
	// keeps growing, and a candidate that assumed the saving would be
	// planning a shape that does not exist.
	narrow := proto.Clone(anchor).(*Arch)
	narrow.KvHeads = 2
	p := Plan{Weights: PrecisionFP16, MaxModelLen: 131_072, MaxBatch: 8}
	n, _, err := MinCards(narrow, p, 80*GB, DefaultUtilization, 8)
	if err == nil {
		t.Fatalf("MinCards returned %d cards; want a refusal, since only 1 and 2 shard 2 KV heads", n)
	}
	if !strings.Contains(err.Error(), "2 card") {
		t.Errorf("error reports a shape that was never a candidate: %v", err)
	}
}

func TestMinCardsRefusesWhenNoShapeFits(t *testing.T) {
	p := Plan{Weights: PrecisionFP16, MaxModelLen: 1_000_000, MaxBatch: 32}
	_, b, err := MinCards(anchor, p, 24*GB, DefaultUtilization, 8)
	if err == nil {
		t.Fatal("want a refusal for a million-token context on 24 GB cards")
	}
	// The refusal has to carry the arithmetic. "It does not fit" sends an
	// operator to guess at which term to move.
	for _, want := range []string{"GB per card", "usable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if b.TotalBytes() <= 0 {
		t.Error("refusal returned an empty budget; the caller cannot show what overflowed")
	}
}

func TestParsePrecisionAcceptsTheLadderAndRejectsTheRest(t *testing.T) {
	for _, in := range []string{"fp16", "BF16", " fp8 ", "int8", "awq", "GPTQ"} {
		if _, err := ParsePrecision(in); err != nil {
			t.Errorf("ParsePrecision(%q): %v", in, err)
		}
	}
	for _, in := range []string{"", "int4", "fp4", "float16", "nf4"} {
		if _, err := ParsePrecision(in); err == nil {
			t.Errorf("ParsePrecision(%q) accepted an unknown precision", in)
		}
	}
}

func TestComputeRejectsIncompleteInputs(t *testing.T) {
	good := Plan{Weights: PrecisionFP16, MaxModelLen: 4096, MaxBatch: 1}
	cases := []struct {
		name string
		a    *Arch
		p    Plan
	}{
		{"no params", &Arch{Layers: 1, KvHeads: 1, HeadDim: 1}, good},
		{"no layers", &Arch{Params: 1, KvHeads: 1, HeadDim: 1}, good},
		{"no kv heads", &Arch{Params: 1, Layers: 1, HeadDim: 1}, good},
		{"no head dim", &Arch{Params: 1, Layers: 1, KvHeads: 1}, good},
		{"unknown weight precision", anchor, Plan{Weights: "int4", MaxModelLen: 4096, MaxBatch: 1}},
		{"unknown cache precision", anchor, Plan{Weights: PrecisionFP16, KVCache: "int4", MaxModelLen: 4096, MaxBatch: 1}},
		{"no context", anchor, Plan{Weights: PrecisionFP16, MaxBatch: 1}},
		{"no batch", anchor, Plan{Weights: PrecisionFP16, MaxModelLen: 4096}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Compute(tc.a, tc.p); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestVerdictStringsAreOperatorFacing(t *testing.T) {
	for v, want := range map[Verdict]string{Fits: "fits", Tight: "tight", Overcommitted: "overcommitted"} {
		if got := v.String(); got != want {
			t.Errorf("Verdict(%d).String() = %q, want %q", v, got, want)
		}
	}
}
