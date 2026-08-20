package vrambudget_test

import (
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/vrambudget"
)

// The weight terms of GLM-5.2 at fp16, exactly. 76 expert layers of 256
// experts, each 3 x 6144 x 2048 parameters.
const (
	glmRoutedParams    int64 = 76 * 256 * 3 * 6144 * 2048
	glmNonRoutedParams int64 = 753_329_940_480 - glmRoutedParams
)

func fp16Plan(tp, ep int32) vrambudget.Plan {
	return vrambudget.Plan{
		Weights: vrambudget.PrecisionFP16, MaxModelLen: 8192, MaxBatch: 8,
		TPSize: tp, EPSize: ep,
	}
}

// TestExpertParallelReplicatesWhatItDoesNotShard is the defect. Under
// tp=1 with the width carried by data parallelism, the routed experts
// shard across the eight ranks and everything else is replicated on every
// one of them. Dividing the whole weight by eight understates the
// attention, the embeddings and the dense layers eightfold.
func TestExpertParallelReplicatesWhatItDoesNotShard(t *testing.T) {
	b, err := vrambudget.Compute(glm52(), fp16Plan(1, 8))
	if err != nil {
		t.Fatal(err)
	}

	want := glmRoutedParams*2/8 + glmNonRoutedParams*2
	if b.WeightBytes != want {
		t.Errorf("WeightBytes = %d (%.1f GB), want %d (%.1f GB)",
			b.WeightBytes, float64(b.WeightBytes)/1e9, want, float64(want)/1e9)
	}
}

// TestTensorParallelAtFullWidthIsUnchanged: when the tensor split covers
// every card there is no data-parallel replication, so every term divides
// by the card count exactly as before.
func TestTensorParallelAtFullWidthIsUnchanged(t *testing.T) {
	b, err := vrambudget.Compute(glm52(), fp16Plan(8, 8))
	if err != nil {
		t.Fatal(err)
	}

	want := int64(753_329_940_480) * 2 / 8
	if b.WeightBytes != want {
		t.Errorf("WeightBytes = %d, want %d (the whole model over eight cards)", b.WeightBytes, want)
	}
}

// TestAnUnsetExpertSizeBehavesExactlyAsBefore is the compatibility proof.
// Every deployment that does not ask for expert parallelism computes the
// number it computed yesterday.
func TestAnUnsetExpertSizeBehavesExactlyAsBefore(t *testing.T) {
	withEP, err := vrambudget.Compute(glm52(), fp16Plan(8, 0))
	if err != nil {
		t.Fatal(err)
	}
	// The same plan expressed without the field at all.
	legacy, err := vrambudget.Compute(glm52(), vrambudget.Plan{
		Weights: vrambudget.PrecisionFP16, MaxModelLen: 8192, MaxBatch: 8, TPSize: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if withEP.WeightBytes != legacy.WeightBytes {
		t.Errorf("EPSize=0 changed the answer: %d vs %d", withEP.WeightBytes, legacy.WeightBytes)
	}
	// And the absolute figure, not only that the two agree. Comparing two
	// plans that are equivalent by construction would pass even if the
	// unset case fell back to the wrong divisor, which is exactly what a
	// mutation of expertCards' fallback does.
	want := int64(753_329_940_480) * 2 / 8
	if legacy.WeightBytes != want {
		t.Errorf("WeightBytes = %d, want %d (the whole model over the tensor width)", legacy.WeightBytes, want)
	}
}

// TestADenseModelIgnoresTheExpertSize: no routed experts means nothing to
// shard differently, so the split collapses back to one division.
func TestADenseModelIgnoresTheExpertSize(t *testing.T) {
	dense := &provisionerv1.ModelArchitecture{
		Params: 70_600_000_000, Layers: 80, KvHeads: 8, HeadDim: 128,
		HiddenSize: 8192, MaxPositionEmbeddings: 131_072,
	}
	sharded, err := vrambudget.Compute(dense, fp16Plan(4, 8))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := vrambudget.Compute(dense, fp16Plan(4, 0))
	if err != nil {
		t.Fatal(err)
	}
	if sharded.WeightBytes != plain.WeightBytes {
		t.Errorf("EPSize changed a dense model's weights: %d vs %d", sharded.WeightBytes, plain.WeightBytes)
	}
	if want := int64(70_600_000_000) * 2 / 4; plain.WeightBytes != want {
		t.Errorf("WeightBytes = %d, want %d", plain.WeightBytes, want)
	}
}

// sparseMoE is a small non-latent mixture of experts, for the rules that
// need a cache that shards and an expert count that does not divide by
// every power of two. Six experts and six KV heads are chosen for exactly
// that: neither divides by four.
func sparseMoE() *provisionerv1.ModelArchitecture {
	return &provisionerv1.ModelArchitecture{
		Params: 100_000_000_000, Layers: 32, KvHeads: 6, HeadDim: 128,
		HiddenSize: 4096, MaxPositionEmbeddings: 32_768,
		NumExperts: 6, NumExpertsPerTok: 2, MoeIntermediateSize: 1024,
	}
}

// The row means the expert width here, and the tensor width comes from
// the plan and stays put. Checked against the arithmetic rather than
// against another call: the whole failure #387 describes is two surfaces
// that each looked self-consistent.
func TestSweepExpertParallelPutsTheExpertWidthOnTheRow(t *testing.T) {
	got, err := vrambudget.SweepExpertParallel(glm52(), fp16Plan(1, 0), 80*vrambudget.GiB, 0.9, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("want rows for 1, 2, 4 and 8 cards, got %d", len(got))
	}
	for _, c := range got {
		want := glmRoutedParams*2/int64(c.Cards) + glmNonRoutedParams*2
		if c.Budget.WeightBytes != want {
			t.Errorf("%d cards: WeightBytes = %d, want %d (the experts over the row, everything else replicated)",
				c.Cards, c.Budget.WeightBytes, want)
		}
		// The tensor width is one on every row, so the cache and the
		// activations never divide. A row that quietly sharded them would
		// still produce a plausible table.
		if c.Budget.Cards != 1 {
			t.Errorf("%d cards: budget computed at tensor width %d, want 1", c.Cards, c.Budget.Cards)
		}
	}
}

// A row the tensor width does not divide has no whole number of
// data-parallel ranks, so it is not a shape. That covers a row narrower
// than the width too, which is the same answer for the same reason.
func TestSweepExpertParallelSkipsRowsTheTensorWidthDoesNotDivide(t *testing.T) {
	got, err := vrambudget.SweepExpertParallel(glm52(), fp16Plan(4, 0), 80*vrambudget.GiB, 0.9, 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		skipped := c.SkipReason != ""
		if want := c.Cards%4 != 0; skipped != want {
			t.Errorf("%d cards: skipped = %v, want %v (reason %q)", c.Cards, skipped, want, c.SkipReason)
		}
	}
}

// The deploy path refuses an expert degree the expert count does not
// divide, because an uneven split budgets the average while one card
// carries the maximum. A budget that ranked such a row would recommend
// the shape the deploy then refuses.
func TestSweepExpertParallelSkipsAnUnevenExpertSplit(t *testing.T) {
	got, err := vrambudget.SweepExpertParallel(sparseMoE(), fp16Plan(1, 0), 80*vrambudget.GiB, 0.9, 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		skipped := c.SkipReason != ""
		if want := 6%c.Cards != 0; skipped != want {
			t.Errorf("%d cards against 6 experts: skipped = %v, want %v (reason %q)", c.Cards, skipped, want, c.SkipReason)
		}
	}
}

// The tensor width is the same on every row here, so a width the KV heads
// do not divide is a fact about the plan. Refused once rather than
// repeated as a skip line under every row.
func TestSweepExpertParallelRefusesATensorWidthTheKVHeadsDoNotDivide(t *testing.T) {
	_, err := vrambudget.SweepExpertParallel(sparseMoE(), fp16Plan(4, 0), 80*vrambudget.GiB, 0.9, 8)
	if err == nil {
		t.Fatal("want a refusal: 6 kv heads do not divide by a tensor width of 4")
	}
	// And a width they do divide is not refused, so the rule is the head
	// count rather than the width being greater than one.
	if _, err := vrambudget.SweepExpertParallel(sparseMoE(), fp16Plan(2, 0), 80*vrambudget.GiB, 0.9, 8); err != nil {
		t.Errorf("a tensor width of 2 divides 6 kv heads and must be allowed: %v", err)
	}
	// A latent cache is replicated whatever the head count says, so the
	// rule must not reach a model whose cache does not shard at all.
	if _, err := vrambudget.SweepExpertParallel(glm52(), fp16Plan(4, 0), 80*vrambudget.GiB, 0.9, 8); err != nil {
		t.Errorf("a latent cache shards by nothing, so the head rule does not apply: %v", err)
	}
}

// MaxSessions and Compute have to price the weights the same way, since
// one asks whether a batch fits beside them and the other asks what is
// left once they are placed. They did not under expert parallelism:
// MaxSessions divided the whole model by the tensor width, which on a
// tp=1 plan is no division at all.
func TestMaxSessionsPricesTheWeightsTheWayComputeDoes(t *testing.T) {
	plan := fp16Plan(1, 8)
	plan.Weights = vrambudget.PrecisionMXFP4
	plan.MaxModelLen = 8192

	got, err := vrambudget.MaxSessions(glm52(), plan, 80*vrambudget.GiB, 0.9)
	if err != nil {
		t.Fatal(err)
	}

	// By hand: 63.10 GB of weights per card against 77.31 GB usable
	// leaves 4.13 GB with the overhead band intact, and a session at 8k
	// costs 368,050,176 bytes of latent cache plus 150,994,944 of
	// activation scratch.
	weights := float64(glmNonRoutedParams)*0.57 + float64(glmRoutedParams)*0.57/8
	usable := float64(vrambudget.UsableBytes(80*vrambudget.GiB, 0.9))
	room := usable/1.15 - weights
	// 78 layers of a 512 + 64 latent at one byte over 8192 tokens, plus
	// 8192 tokens of a 6144-wide hidden state at two bytes, scaled by the
	// activation factor. Neither divides, because the tensor width is one.
	cache := float64(78 * (512 + 64) * 8192)
	activation := 8192 * 6144 * 2 * vrambudget.ActivationFactor
	want := int64(room / (cache + activation))
	if want != 7 {
		t.Fatalf("the hand arithmetic itself drifted: %d", want)
	}
	if got != want {
		t.Errorf("MaxSessions = %d, want %d", got, want)
	}
}
