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

func TestSweepReportsEveryCandidateInOrder(t *testing.T) {
	// A caller explaining an answer needs the counts that failed, not
	// only the one that worked. The anchor's 8 KV heads shard across the
	// whole power-of-two ladder, so nothing here is refused and the
	// sequence has to arrive whole.
	p := Plan{Weights: PrecisionFP16, MaxModelLen: 131_072, MaxBatch: 1}
	got, err := Sweep(anchor, p, 80*GB, DefaultUtilization, 8)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	want := []int32{1, 2, 4, 8}
	if len(got) != len(want) {
		t.Fatalf("Sweep returned %d candidates, want %d: %+v", len(got), len(want), got)
	}
	for i, c := range got {
		if c.Cards != want[i] {
			t.Errorf("candidate %d is %d cards, want %d", i, c.Cards, want[i])
		}
		if c.SkipReason != "" {
			t.Errorf("%d cards was refused (%s); 8 kv heads shard across it", c.Cards, c.SkipReason)
		}
		if c.Budget.Cards != c.Cards {
			t.Errorf("candidate %d carries a budget for %d cards", c.Cards, c.Budget.Cards)
		}
	}
	if got[0].Verdict != Overcommitted {
		t.Errorf("one card = %v, want overcommitted at 128k", got[0].Verdict)
	}
	if got[1].Verdict != Fits {
		t.Errorf("two cards = %v, want fits", got[1].Verdict)
	}
}

func TestSweepNamesTheCountsItRefusesRatherThanOmittingThem(t *testing.T) {
	// A missing row reads as an oversight. The reason a count is not a
	// candidate is the same reason the operator should stop asking for
	// it, so dropping the row silently loses the lesson along with the
	// number.
	narrow := proto.Clone(anchor).(*Arch)
	narrow.KvHeads = 2
	p := Plan{Weights: PrecisionFP16, MaxModelLen: 131_072, MaxBatch: 8}
	got, err := Sweep(narrow, p, 80*GB, DefaultUtilization, 8)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("Sweep returned %d rows, want 4 (1, 2, 4, 8): %+v", len(got), got)
	}
	for _, c := range got[:2] {
		if c.SkipReason != "" {
			t.Errorf("%d cards refused, but 2 kv heads shard across it", c.Cards)
		}
	}
	for _, c := range got[2:] {
		if c.SkipReason == "" {
			t.Fatalf("%d cards was planned, but 2 kv heads do not divide by it", c.Cards)
		}
		if !strings.Contains(c.SkipReason, "replicate") {
			t.Errorf("%d cards: reason %q does not say what the engine would do instead", c.Cards, c.SkipReason)
		}
		if c.Budget != (Budget{}) {
			t.Errorf("%d cards was refused but carries a budget: %+v", c.Cards, c.Budget)
		}
	}
}

func TestMinCardsAnswersWithTheFirstShapeSweepAccepts(t *testing.T) {
	// The two are one calculation seen at two altitudes. Letting them
	// drift would put a command's table and its headline in
	// disagreement, which is worse than either being wrong alone.
	plans := []Plan{
		{Weights: PrecisionFP16, MaxModelLen: 131_072, MaxBatch: 1},
		{Weights: PrecisionAWQ, MaxModelLen: 4096, MaxBatch: 1},
		{Weights: PrecisionFP8, KVCache: PrecisionFP16, MaxModelLen: 32_768, MaxBatch: 16},
	}
	for _, p := range plans {
		candidates, err := Sweep(anchor, p, 80*GB, DefaultUtilization, 8)
		if err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		var wantCards int32
		var wantBudget Budget
		for _, c := range candidates {
			if c.SkipReason == "" && c.Verdict == Fits {
				wantCards, wantBudget = c.Cards, c.Budget
				break
			}
		}

		n, b, err := MinCards(anchor, p, 80*GB, DefaultUtilization, 8)
		if wantCards == 0 {
			if err == nil {
				t.Errorf("%+v: MinCards returned %d cards, but Sweep accepted none", p, n)
			}
			continue
		}
		if err != nil {
			t.Errorf("%+v: MinCards refused, but Sweep accepted %d cards: %v", p, wantCards, err)
			continue
		}
		if n != wantCards || b != wantBudget {
			t.Errorf("%+v: MinCards = %d cards %+v, Sweep's first fit = %d cards %+v", p, n, b, wantCards, wantBudget)
		}
	}
}

// The four memory claims do not move when a model turns out to be
// sparse. Every expert is resident whether or not the router picks it,
// so a sparse model occupies exactly what the dense model of the same
// parameter count occupies. Only the per-step reporting differs, which
// is the whole distinction this package now draws.
func TestComputeChargesASparseModelForEveryExpert(t *testing.T) {
	dense := &Arch{Params: 753_329_940_480, Layers: 78, KvHeads: 64, HeadDim: 192, HiddenSize: 6144}
	sparse := &Arch{
		Params: 753_329_940_480, Layers: 78, KvHeads: 64, HeadDim: 192, HiddenSize: 6144,
		NumExperts: 256, NumExpertsPerTok: 8, MoeIntermediateSize: 2048, SharedExperts: 1, DenseLayers: 3,
	}
	plan := Plan{Weights: PrecisionFP8, MaxModelLen: 32768, MaxBatch: 16}

	a, err := Compute(dense, plan)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Compute(sparse, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, term := range []struct {
		name    string
		a, bVal int64
	}{
		{"weights", a.WeightBytes, b.WeightBytes},
		{"cache", a.KVBytes, b.KVBytes},
		{"activations", a.ActivationBytes, b.ActivationBytes},
		{"overhead", a.OverheadBytes, b.OverheadBytes},
	} {
		if term.a != term.bVal {
			t.Errorf("%s moved: dense %d, sparse %d", term.name, term.a, term.bVal)
		}
	}
	if a.ActiveParams != dense.GetParams() {
		t.Errorf("dense active = %d, want the full parameter count %d", a.ActiveParams, dense.GetParams())
	}
	if b.ActiveParams >= a.ActiveParams {
		t.Errorf("sparse active = %d, want well under the dense %d", b.ActiveParams, a.ActiveParams)
	}
}

// A sparse model validates on the same fields a dense one does. The
// expert count is not among them: a model that states no experts is
// dense, which is a shape rather than an incomplete config.
func TestValidateArchDoesNotRequireAnExpertCount(t *testing.T) {
	if err := ValidateArch(&Arch{Params: 1, Layers: 1, KvHeads: 1, HeadDim: 1}); err != nil {
		t.Errorf("dense arch rejected: %v", err)
	}
}

// kimiK3 is the published shape of the model Part IV is aimed at: 2.78T
// parameters over 93 layers, 896 routed experts of which 16 activate,
// two shared experts, one dense layer, and an expert stack that runs at
// 3584 rather than the model's own 7168.
var kimiK3 = &Arch{
	Params: 2_779_931_837_184, Layers: 93, KvHeads: 96, HeadDim: 74, HiddenSize: 7168,
	NumExperts: 896, NumExpertsPerTok: 16, MoeIntermediateSize: 3072,
	SharedExperts: 2, DenseLayers: 1, RoutedExpertHiddenSize: 3584,
}

// The expert-share arithmetic is checkable against the model's own
// published tensor accounting, which is a stronger test than a golden
// number. Hugging Face reports K3 as 2,722,740,830,208 parameters in
// eight-bit tensors and the rest in bf16, and the eight-bit tensors are
// the quantized routed experts. Computing that count from the shape
// alone lands on it exactly.
func TestRoutedExpertParamsMatchesThePublishedTensorAccounting(t *testing.T) {
	const publishedU8 = 2_722_740_830_208
	if got := RoutedExpertParams(kimiK3); got != publishedU8 {
		t.Errorf("routed expert params = %d, want %d (K3's published eight-bit tensor count)", got, publishedU8)
	}
}

// The number the whole part turns on: a 2.78T model that reads about a
// hundred billion parameters per step.
func TestActiveParamsSeparatesWhatIsHeldFromWhatIsRead(t *testing.T) {
	active := ActiveParams(kimiK3)
	if active != 105_811_378_944 {
		t.Fatalf("active params = %d, want 105811378944", active)
	}
	if ratio := float64(kimiK3.GetParams()) / float64(active); ratio < 25 || ratio > 27 {
		t.Errorf("held/read ratio = %.1f, want about 26", ratio)
	}
}

// Resident and per-step at the precision the model actually ships in.
// Both are checkable against the published checkpoint: 1560.86 GB on
// disk, which is what the resident figure has to land near.
func TestBudgetReportsResidentAndPerStepForKimiK3(t *testing.T) {
	b, err := Compute(kimiK3, Plan{Weights: PrecisionMXFP4, MaxModelLen: 8192, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	if resident := float64(b.WeightBytes) / float64(GB); resident < 1561 || resident > 1620 {
		t.Errorf("resident = %.0f GB, want just above the published 1561 GB", resident)
	}
	if step := float64(b.ActiveWeightBytes) / float64(GB); step < 55 || step > 65 {
		t.Errorf("per step = %.0f GB, want about 59 GB", step)
	}
	if b.ActiveWeightBytes >= b.WeightBytes/20 {
		t.Errorf("per step %d is not far enough below resident %d", b.ActiveWeightBytes, b.WeightBytes)
	}
}

// A dense model reads all of itself, so the two figures are the same
// number and the sparse distinction costs it nothing.
func TestActiveParamsIsTheWholeModelWhenItIsDense(t *testing.T) {
	dense := &Arch{Params: 70_600_000_000, Layers: 80, KvHeads: 8, HeadDim: 128, HiddenSize: 8192}
	if got := ActiveParams(dense); got != dense.GetParams() {
		t.Errorf("active = %d, want %d", got, dense.GetParams())
	}
	if got := RoutedExpertParams(dense); got != 0 {
		t.Errorf("routed expert params = %d on a dense model, want 0", got)
	}
}

// Unknown rather than a plausible-looking figure, in each of the three
// ways the share can fail to compute. A wrong active count is worse than
// none, because every use of it is an argument that the number is small.
func TestActiveParamsRefusesRatherThanGuessing(t *testing.T) {
	base := func() *Arch {
		return &Arch{
			Params: 753_329_940_480, Layers: 78, KvHeads: 64, HeadDim: 192, HiddenSize: 6144,
			NumExperts: 256, NumExpertsPerTok: 8, MoeIntermediateSize: 2048, DenseLayers: 3,
		}
	}
	t.Run("no activated count published", func(t *testing.T) {
		a := base()
		a.NumExpertsPerTok = 0
		if got := ActiveParams(a); got != 0 {
			t.Errorf("active = %d, want 0", got)
		}
	})
	t.Run("activated count exceeds the expert count", func(t *testing.T) {
		a := base()
		a.NumExpertsPerTok = 300
		if got := ActiveParams(a); got != 0 {
			t.Errorf("active = %d, want 0", got)
		}
	})
	t.Run("computed expert share exceeds the whole model", func(t *testing.T) {
		a := base()
		// K3's width read at the model's own, which is the mistake that
		// makes the expert stack come out twice the size of the model.
		a.Params = 100_000_000
		if got := ActiveParams(a); got != 0 {
			t.Errorf("active = %d, want 0", got)
		}
	})
}

// The published four-bit checkpoints are the calibration for the rung,
// so the rung has to reproduce them.
func TestMXFP4ReproducesThePublishedCheckpointFootprints(t *testing.T) {
	for _, tc := range []struct {
		name        string
		params      int64
		publishedGB float64
	}{
		{"openai/gpt-oss-120b", 116_829_156_672, 65.25},
		{"moonshotai/Kimi-K3", 2_779_931_837_184, 1560.86},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := float64(tc.params) * PrecisionMXFP4.BytesPerParam() / float64(GB)
			if diff := (got - tc.publishedGB) / tc.publishedGB; diff < 0 || diff > 0.03 {
				t.Errorf("budgeted %.2f GB against a published %.2f GB (%.1f%% off; want 0 to 3%% over)",
					got, tc.publishedGB, diff*100)
			}
		})
	}
}

// deepSeekV3 is the anchor for the compressed-cache arithmetic, because
// its per-token cache cost is a published figure rather than something
// this package gets to decide: 61 layers of a 512-wide latent plus a
// 64-wide uncompressed remainder.
var deepSeekV3 = &Arch{
	Params: 671_026_419_200, Layers: 61, HiddenSize: 7168,
	KvLoraRank: 512, QkRopeHeadDim: 64,
}

// The number DeepSeek-V3 is published against. Getting this right is the
// difference between a cache term that is exact and one that was over by
// a factor of forty.
func TestLatentCacheMatchesThePublishedPerTokenCost(t *testing.T) {
	if got := KVBytesPerToken(deepSeekV3, PrecisionBF16); got != 70_272 {
		t.Errorf("per token = %d bytes, want 70272", got)
	}
}

// The old term reads a key and a value for every head at every layer.
// Applied to a latent-cache model it computes a cache the engine never
// allocates, and the gap is the whole of #362.
func TestLatentCacheIsNotTheHeadCountArithmetic(t *testing.T) {
	glm := &Arch{
		Params: 753_329_940_480, Layers: 78, HiddenSize: 6144,
		KvHeads: 64, HeadDim: 192, KvLoraRank: 512, QkRopeHeadDim: 64,
	}
	latent := KVBytesPerToken(glm, PrecisionBF16)
	perHead := 2 * int64(glm.GetLayers()) * int64(glm.GetKvHeads()) * int64(glm.GetHeadDim()) * 2

	if latent != 89_856 {
		t.Errorf("per token = %d, want 89856 (78 layers x 576 x 2)", latent)
	}
	if ratio := perHead / latent; ratio < 40 {
		t.Errorf("per-head arithmetic is only %dx the real cost; the bug was ~43x", ratio)
	}
}

// A hybrid pays cache only on the layers that have a growing one. The
// rest hold a fixed-size state, so counting all 93 of K3's layers prices
// its cache at nearly four times what it costs.
func TestHybridCachesOnlyOnItsAttentionLayers(t *testing.T) {
	k3 := &Arch{
		Params: 2_779_931_837_184, Layers: 93, HiddenSize: 7168,
		KvLoraRank: 512, QkRopeHeadDim: 64, FullAttentionLayers: 24,
	}
	if n := CachingLayers(k3); n != 24 {
		t.Fatalf("caching layers = %d, want 24", n)
	}
	if got := KVBytesPerToken(k3, PrecisionBF16); got != 27_648 {
		t.Errorf("per token = %d, want 27648 (24 layers x 576 x 2)", got)
	}
}

// Absent means every layer, so a model that is not a hybrid is unaffected
// and a list naming all the layers says nothing a missing list does not.
func TestCachingLayersIsEveryLayerUnlessTheModelSaysOtherwise(t *testing.T) {
	for _, tc := range []struct {
		name       string
		full, want int32
	}{
		{"no hybrid split published", 0, 61},
		{"a split naming every layer", 61, 61},
		{"a real split", 24, 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Arch{Layers: 61, KvLoraRank: 512, QkRopeHeadDim: 64, FullAttentionLayers: tc.full}
			if got := CachingLayers(a); got != tc.want {
				t.Errorf("caching layers = %d, want %d", got, tc.want)
			}
		})
	}
}

// Adding cards buys weight headroom and no cache headroom, because every
// rank reconstructs its heads from the whole latent and so holds the
// whole latent. Dividing it would promise a saving the engine does not
// deliver.
func TestLatentCacheIsReplicatedRatherThanShardedAcrossCards(t *testing.T) {
	plan := Plan{Weights: PrecisionFP8, MaxModelLen: 32768, MaxBatch: 8}
	one, err := Compute(deepSeekV3, plan)
	if err != nil {
		t.Fatal(err)
	}
	plan.TPSize = 8
	eight, err := Compute(deepSeekV3, plan)
	if err != nil {
		t.Fatal(err)
	}
	if eight.KVBytes != one.KVBytes {
		t.Errorf("cache per card went from %d to %d across eight cards; a latent is replicated", one.KVBytes, eight.KVBytes)
	}
	if eight.WeightBytes*8 != one.WeightBytes {
		t.Errorf("weights did not shard: %d across eight cards against %d on one", eight.WeightBytes, one.WeightBytes)
	}
}

// The per-head cache still shards, which is the behaviour every dense
// model in the existing tests depends on.
func TestPerHeadCacheStillShardsAcrossCards(t *testing.T) {
	dense := &Arch{Params: 70_600_000_000, Layers: 80, KvHeads: 8, HeadDim: 128, HiddenSize: 8192}
	plan := Plan{Weights: PrecisionFP8, MaxModelLen: 32768, MaxBatch: 8}
	one, err := Compute(dense, plan)
	if err != nil {
		t.Fatal(err)
	}
	plan.TPSize = 8
	eight, err := Compute(dense, plan)
	if err != nil {
		t.Fatal(err)
	}
	if eight.KVBytes*8 != one.KVBytes {
		t.Errorf("cache per card = %d across eight, want an eighth of %d", eight.KVBytes, one.KVBytes)
	}
}

// The shard-divisibility refusal describes how a per-head cache shards.
// A latent cache does not shard on any card count, so refusing one for
// head-divisibility withholds a shape for a saving that was never on
// offer.
func TestSweepDoesNotApplyTheShardRuleToALatentCache(t *testing.T) {
	odd := &Arch{Params: 671_026_419_200, Layers: 61, HiddenSize: 7168, KvLoraRank: 512, QkRopeHeadDim: 64, KvHeads: 3}
	got, err := Sweep(odd, Plan{Weights: PrecisionFP8, MaxModelLen: 8192, MaxBatch: 1}, 80*GB, 0.9, 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if c.SkipReason != "" {
			t.Errorf("%d cards refused on a latent cache: %s", c.Cards, c.SkipReason)
		}
	}
}

// Which fields a model has to publish depends on how it caches. A latent
// model has no per-head key and value, so requiring a head count off it
// would reject a model this package can price exactly.
func TestValidateArchAsksForTheFieldsTheCacheShapeActuallyUses(t *testing.T) {
	t.Run("latent model needs no per-head figures", func(t *testing.T) {
		if err := ValidateArch(&Arch{Params: 1, Layers: 1, KvLoraRank: 512, QkRopeHeadDim: 64}); err != nil {
			t.Errorf("rejected: %v", err)
		}
	})
	t.Run("per-head model still needs them", func(t *testing.T) {
		if err := ValidateArch(&Arch{Params: 1, Layers: 1, KvHeads: 8}); err == nil {
			t.Error("accepted a per-head model with no head dimension")
		}
		if err := ValidateArch(&Arch{Params: 1, Layers: 1, HeadDim: 128}); err == nil {
			t.Error("accepted a per-head model with no kv-head count")
		}
	})
}
