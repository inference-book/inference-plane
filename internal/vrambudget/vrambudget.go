// Package vrambudget computes whether a model will fit on the cards you
// are about to rent, before you rent them.
//
// Four things compete for a GPU's memory and a deployment pays for all
// four at once: the model weights, the KV cache, the activation buffers,
// and a band of overhead the runtime holds no matter what. Fit is the sum
// of those four claims against the card, not the weight footprint against
// the card, and a budget that checks only the weights passes deployments
// that then refuse to start.
//
// Everything here is arithmetic over numbers the model publishes and
// numbers the operator chooses. Nothing in this package talks to a
// provider or to Hugging Face; callers supply an Arch and get a Budget.
// That split is deliberate, because the arithmetic is the part worth
// testing exactly and the fetching is the part worth mocking.
//
// # What this is not
//
// An estimate, and it says so where it is weakest. The weight term is
// exact. The KV term is exact for a given context and batch. The
// activation term is a coarse approximation with a stated basis, and the
// overhead term is a flat fraction that stands in for the CUDA context,
// allocator pools, and fragmentation, none of which compute from the
// model's architecture. A deployment that clears the card only by eating
// into the overhead band has not cleared the card.
package vrambudget

import (
	"fmt"
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// OverheadFraction is the band budgeted for everything the runtime holds
// that is not weights, cache, or activations: the CUDA context, the
// allocator pools the engine keeps so it is not re-allocating per
// request, framework buffers, and the fragmentation any pooled allocator
// strands.
//
// It is a flat fraction of the working set rather than a computed figure
// because none of those terms follow from the model's architecture.
// Fifteen percent is the figure the book budgets and it is deliberately
// generous, since this is the term whose overrun does not fail at
// startup. An overcommitted weight term refuses to load, with the
// arithmetic in the log. An overcommitted overhead band surfaces minutes
// into real traffic as an allocation that no longer finds one contiguous
// block.
const OverheadFraction = 0.15

// ActivationFactor scales the per-token activation estimate.
//
// Activations are the scratch memory a forward pass holds for
// intermediate results as a batch flows through the layers, and unlike
// the paged KV cache the memory has to be contiguous, because the
// matrix-multiply kernels want single unbroken blocks. The size follows
// the batch, the context, and the hidden dimension, and the constant in
// front of those depends on engine internals this package deliberately
// does not model.
//
// 1.5 reproduces the ~2 GB the book budgets for a 30B-class model
// serving one sequence at a 128k context. It is the least precise number
// in this file and it is the smallest term, which is the only reason a
// coarse constant is tolerable here.
const ActivationFactor = 1.5

// activationElementBytes is the precision a forward pass computes at.
// Fixed at two rather than following the weight precision, because a
// quantized model dequantizes to half precision for the matrix multiply.
const activationElementBytes = 2

// Arch is the generated provisionerv1.ModelArchitecture.
//
// Aliased rather than redeclared, following the rule the capacity search
// had to learn the hard way: a wire type travels as the generated
// message, with no parallel Go struct to keep in sync. A model's shape
// crosses the wire, because reading it needs the hub credential and that
// lives in the daemon's environment.
//
// The cost of the alias is that Validate cannot be a method on it, which
// is why ValidateArch is a free function. Same shape as CanAnswer and
// DescribePlacement next to their aliased messages.
type Arch = provisionerv1.ModelArchitecture

// ValidateArch reports whether an architecture is usable for a budget.
//
// Absent is unknown rather than zero throughout. A budget computed from a
// zero layer count reports no KV cache, which is a budget that says yes
// to everything, so every field that divides or multiplies the cache is
// checked rather than defaulted.
func ValidateArch(a *Arch) error {
	switch {
	case a == nil:
		return fmt.Errorf("model architecture is required")
	case a.GetParams() <= 0:
		return fmt.Errorf("parameter count must be positive, got %d", a.GetParams())
	case a.GetLayers() <= 0:
		return fmt.Errorf("layer count must be positive, got %d", a.GetLayers())
	case a.GetKvHeads() <= 0:
		return fmt.Errorf("kv-head count must be positive, got %d", a.GetKvHeads())
	case a.GetHeadDim() <= 0:
		return fmt.Errorf("head dimension must be positive, got %d", a.GetHeadDim())
	}
	return nil
}

// Precision is a bytes-per-element choice made at deploy time. It applies
// separately to the weights and to the KV cache, which is why Plan
// carries two of them: an engine can hold the cache at one byte while the
// weights stay at two.
type Precision string

// The precision ladder. Each step down roughly halves the weight term.
const (
	PrecisionFP16 Precision = "fp16"
	PrecisionBF16 Precision = "bf16"
	PrecisionFP8  Precision = "fp8"
	PrecisionINT8 Precision = "int8"
	PrecisionAWQ  Precision = "awq"
	PrecisionGPTQ Precision = "gptq"
)

// ParsePrecision maps an operator-supplied string onto the ladder.
func ParsePrecision(s string) (Precision, error) {
	switch p := Precision(strings.ToLower(strings.TrimSpace(s))); p {
	case PrecisionFP16, PrecisionBF16, PrecisionFP8, PrecisionINT8, PrecisionAWQ, PrecisionGPTQ:
		return p, nil
	case "":
		return "", fmt.Errorf("precision is required")
	default:
		return "", fmt.Errorf("unknown precision %q (want one of fp16, bf16, fp8, int8, awq, gptq)", s)
	}
}

// BytesPerParam is the weight footprint of one parameter.
//
// The four-bit formats are 0.6 rather than the nominal 0.5, and the
// difference decides deployments rather than rounding them. A four-bit
// build does not store bare four-bit weights: it keeps a 16-bit scale and
// offset for every small group, and leaves the embeddings and the output
// head at higher precision. The published four-bit build of a 32B model
// lands near 4.8 effective bits per parameter, so 0.6 bytes is what an
// operator actually pays. Budgeting 0.5 is the difference between a model
// that fits a 24 GB card with cache headroom and one that does not.
func (p Precision) BytesPerParam() float64 {
	switch p {
	case PrecisionFP16, PrecisionBF16:
		return 2
	case PrecisionFP8, PrecisionINT8:
		return 1
	case PrecisionAWQ, PrecisionGPTQ:
		return 0.6
	}
	return 0
}

// BytesPerCacheElement is the per-element cost of a cached key or value.
//
// The cache does not have to be held at the weights' precision, and it
// tolerates lower precision better than the weights do. Anything at or
// below one byte per parameter holds the cache at one byte; everything
// else holds it at two. A four-bit weight format does not imply a
// four-bit cache, which is why AWQ and GPTQ land on one here rather than
// on 0.6: no engine stores the cache at sub-byte precision.
func (p Precision) BytesPerCacheElement() int64 {
	switch p {
	case PrecisionFP16, PrecisionBF16:
		return 2
	case PrecisionFP8, PrecisionINT8, PrecisionAWQ, PrecisionGPTQ:
		return 1
	}
	return 0
}

// Plan is the part of the budget the operator chooses at deploy time.
type Plan struct {
	// Weights is the precision the weights are stored in.
	Weights Precision
	// KVCache is the precision the cache is held at. Empty means "same as
	// the weights", which is the engine default.
	KVCache Precision
	// MaxModelLen is the context window the engine is configured for, in
	// tokens. This is what the cache is sized against, not the model's
	// advertised maximum.
	MaxModelLen int32
	// MaxBatch is the number of sequences held concurrently. Context and
	// batch multiply the same per-token cost, so the cache is linear in
	// each and one fixed pool trades one against the other.
	MaxBatch int32
	// TPSize is how many cards one engine shards across. 0 and 1 both
	// mean a single card.
	TPSize int32
}

// cacheDtype resolves the cache precision, defaulting to the weights'.
func (p Plan) cacheDtype() Precision {
	if p.KVCache == "" {
		return p.Weights
	}
	return p.KVCache
}

// cards normalises TPSize so 0 and 1 both mean one card.
func (p Plan) cards() int32 {
	if p.TPSize < 1 {
		return 1
	}
	return p.TPSize
}

// Validate reports whether the plan is usable for a budget.
func (p Plan) Validate() error {
	if p.Weights.BytesPerParam() == 0 {
		return fmt.Errorf("unknown weight precision %q", p.Weights)
	}
	if p.cacheDtype().BytesPerCacheElement() == 0 {
		return fmt.Errorf("unknown kv-cache precision %q", p.KVCache)
	}
	if p.MaxModelLen <= 0 {
		return fmt.Errorf("max model length must be positive, got %d", p.MaxModelLen)
	}
	if p.MaxBatch <= 0 {
		return fmt.Errorf("max batch must be positive, got %d", p.MaxBatch)
	}
	return nil
}

// Budget is the four claims, per card, in bytes.
//
// Per card rather than per deployment, because the question a budget
// answers is whether one card holds its share. A caller wanting the whole
// engine's footprint multiplies by the card count it planned for.
type Budget struct {
	// Cards is the number of cards the claims below were divided across.
	Cards int32

	WeightBytes     int64
	KVBytes         int64
	ActivationBytes int64
	OverheadBytes   int64

	// KVBytesPerToken is the cache cost of one token across the whole
	// engine, before the card division. Surfaced because it is the number
	// that makes a context-length decision legible: everything about how
	// much traffic the engine holds runs through it.
	KVBytesPerToken int64
}

// WorkingSetBytes is weights plus cache plus activations, the three terms
// that follow from the model and the plan. Overhead is a fraction of it.
func (b Budget) WorkingSetBytes() int64 {
	return b.WeightBytes + b.KVBytes + b.ActivationBytes
}

// TotalBytes is what one card has to hold.
func (b Budget) TotalBytes() int64 {
	return b.WorkingSetBytes() + b.OverheadBytes
}

// Compute returns the per-card budget for a model under a plan.
//
// Tensor parallelism divides the weights, the cache, and the activations
// across the cards, since each card holds a shard of each. Overhead is
// then taken on the per-card working set rather than divided, because a
// CUDA context and an allocator pool exist once per card rather than once
// per engine. The communication buffers tensor parallelism itself needs
// are not modelled and are small relative to the overhead band they fall
// inside.
func Compute(a *Arch, p Plan) (Budget, error) {
	if err := ValidateArch(a); err != nil {
		return Budget{}, err
	}
	if err := p.Validate(); err != nil {
		return Budget{}, err
	}

	cards := p.cards()

	// Weights: parameters times bytes per parameter. The one term that is
	// exact and known before the engine starts.
	weights := float64(a.GetParams()) * p.Weights.BytesPerParam()

	// KV cache: two (a key and a value, stored separately) times the
	// state one token contributes at each layer, summed over layers,
	// times the bytes per element the cache precision sets.
	perToken := 2 * int64(a.GetLayers()) * int64(a.GetKvHeads()) * int64(a.GetHeadDim()) * p.cacheDtype().BytesPerCacheElement()
	kv := float64(perToken) * float64(p.MaxModelLen) * float64(p.MaxBatch)

	// Activations: scratch for a forward pass, following batch, context,
	// and hidden dimension. Always two bytes per element regardless of
	// how the weights are stored, because a quantized model dequantizes
	// to half precision for the matrix multiply. Quantization moves the
	// weight term and leaves this one alone.
	activations := float64(p.MaxBatch) * float64(p.MaxModelLen) * float64(a.GetHiddenSize()) * activationElementBytes * ActivationFactor

	perCard := func(total float64) int64 { return int64(total / float64(cards)) }

	b := Budget{
		Cards:           cards,
		WeightBytes:     perCard(weights),
		KVBytes:         perCard(kv),
		ActivationBytes: perCard(activations),
		KVBytesPerToken: perToken,
	}
	b.OverheadBytes = int64(float64(b.WorkingSetBytes()) * OverheadFraction)
	return b, nil
}

// Verdict is how a budget stands against a card.
type Verdict int

const (
	// Fits means the sum clears the usable memory with the overhead band
	// intact.
	Fits Verdict = iota
	// Tight means it clears only by eating into the overhead band. The
	// engine will very likely start and may fail under load, which is the
	// worst of the three outcomes to discover after renting.
	Tight
	// Overcommitted means it does not clear at all. The engine refuses at
	// startup.
	Overcommitted
)

// String renders a verdict for operator-facing output.
func (v Verdict) String() string {
	switch v {
	case Fits:
		return "fits"
	case Tight:
		return "tight"
	case Overcommitted:
		return "overcommitted"
	}
	return "unknown"
}

// Against reports how the budget stands against a card's usable memory.
//
// usableBytes is the memory the engine is allowed to touch, which is less
// than the card's memory. An engine's memory-utilization setting caps it
// below the physical total on purpose, because the CUDA context and the
// driver's own allocations sit outside the engine's accounting and
// claiming the whole card buys an allocation failure at startup rather
// than extra cache. Callers convert; see UsableBytes.
func (b Budget) Against(usableBytes int64) Verdict {
	switch {
	case b.TotalBytes() <= usableBytes:
		return Fits
	case b.WorkingSetBytes() <= usableBytes:
		return Tight
	default:
		return Overcommitted
	}
}

// DefaultUtilization is the fraction of a card an engine is allowed to
// touch by default, matching vLLM's gpu_memory_utilization.
const DefaultUtilization = 0.9

// UsableBytes converts a card's physical memory into what the engine may
// claim of it.
func UsableBytes(cardBytes int64, utilization float64) int64 {
	if utilization <= 0 || utilization > 1 {
		utilization = DefaultUtilization
	}
	return int64(float64(cardBytes) * utilization)
}

// GB is one decimal gigabyte, the unit providers quote VRAM in.
const GB int64 = 1_000_000_000

// GiB is one binary gibibyte, the unit engines report memory in.
const GiB int64 = 1 << 30

// MinCards returns the fewest cards of the given size that hold the model
// under this plan, along with the resulting per-card budget.
//
// This is the question Plan.TPSize cannot answer on its own: an operator
// choosing hardware knows the model and the card and wants the shape.
// Candidates are powers of two up to maxCards, because tensor parallelism
// in practice is, and a candidate is only considered when the KV head
// count divides evenly by it.
//
// That divisibility rule is load-bearing rather than fussy. When the
// cards outnumber the KV heads, or divide them unevenly, an engine
// replicates KV heads across cards instead of sharding them, so the cache
// term stops shrinking with the card count while the operator's bill
// keeps growing. Counting a saving that would not happen is the failure
// this rules out.
func MinCards(a *Arch, p Plan, cardBytes int64, utilization float64, maxCards int32) (int32, Budget, error) {
	candidates, err := Sweep(a, p, cardBytes, utilization, maxCards)
	if err != nil {
		return 0, Budget{}, err
	}

	var widest Candidate
	for _, c := range candidates {
		if c.SkipReason != "" {
			continue
		}
		if c.Verdict == Fits {
			return c.Cards, c.Budget, nil
		}
		widest = c
	}

	// Report against the widest shape actually considered, not against
	// maxCards, since a card count the KV heads do not divide was never
	// a candidate and quoting its budget would describe a shape this
	// function refused to plan.
	usable := UsableBytes(cardBytes, utilization)
	return 0, widest.Budget, fmt.Errorf("does not fit on %d card(s) of %d GB at this context and precision: needs %.1f GB per card, %.1f GB usable",
		widest.Cards, cardBytes/GB, float64(widest.Budget.TotalBytes())/float64(GB), float64(usable)/float64(GB))
}

// Candidate is one card count a sweep looked at.
//
// A count either got a budget and a verdict, or it was refused before
// any arithmetic ran and SkipReason says why. The two are exclusive:
// when SkipReason is set, Budget and Verdict are zero and mean nothing.
type Candidate struct {
	Cards      int32
	Budget     Budget
	Verdict    Verdict
	SkipReason string
}

// Sweep reports every card count up to maxCards, in ascending order,
// including the counts the shard rule refuses.
//
// MinCards answers the operator's question and throws the rest away.
// Anything explaining the answer needs what it discarded: the counts that
// overran, by how much and in which term, and the counts that were never
// candidates at all. A gap in the sequence reads as an oversight, so a
// refused count is reported with its reason rather than omitted.
//
// The candidate rule is MinCards' and is documented there.
func Sweep(a *Arch, p Plan, cardBytes int64, utilization float64, maxCards int32) ([]Candidate, error) {
	if err := ValidateArch(a); err != nil {
		return nil, err
	}
	if cardBytes <= 0 {
		return nil, fmt.Errorf("card memory must be positive, got %d", cardBytes)
	}
	if maxCards < 1 {
		maxCards = 8
	}
	usable := UsableBytes(cardBytes, utilization)

	var out []Candidate
	for n := int32(1); n <= maxCards; n *= 2 {
		if n > 1 && a.GetKvHeads()%n != 0 {
			out = append(out, Candidate{
				Cards: n,
				SkipReason: fmt.Sprintf("%d kv heads do not divide by %d, so an engine would replicate the cache across cards rather than shard it",
					a.GetKvHeads(), n),
			})
			continue
		}
		try := p
		try.TPSize = n
		b, err := Compute(a, try)
		if err != nil {
			return nil, err
		}
		out = append(out, Candidate{Cards: n, Budget: b, Verdict: b.Against(usable)})
	}
	return out, nil
}
