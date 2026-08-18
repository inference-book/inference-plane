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
	}
	// How the cache is shaped decides which fields have to be present.
	// A model caching a compressed latent has no per-head key and value
	// to size, so requiring a head count off it would reject a model
	// whose cache this package can price exactly.
	if a.GetKvLoraRank() > 0 {
		return nil
	}
	switch {
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
	// PrecisionMXFP4 is the block-scaled four-bit format the frontier
	// sparse models ship in. Kept apart from awq/gptq because it measures
	// cheaper per parameter; see BytesPerParam.
	PrecisionMXFP4 Precision = "mxfp4"
)

// ParsePrecision maps an operator-supplied string onto the ladder.
func ParsePrecision(s string) (Precision, error) {
	switch p := Precision(strings.ToLower(strings.TrimSpace(s))); p {
	case PrecisionFP16, PrecisionBF16, PrecisionFP8, PrecisionINT8, PrecisionAWQ, PrecisionGPTQ, PrecisionMXFP4:
		return p, nil
	case "":
		return "", fmt.Errorf("precision is required")
	default:
		return "", fmt.Errorf("unknown precision %q (want one of fp16, bf16, fp8, int8, awq, gptq, mxfp4)", s)
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
//
// MXFP4 is 0.56 rather than 0.6, and the gap is not a rounding
// preference. Both figures are dominated by the parameters a four-bit
// build leaves at higher precision, and what share of the model that is
// depends on the model. On a 32B dense build the embeddings and the
// output head are a big enough share to pull the average to 4.8 bits. On
// a frontier sparse model the routed experts are so much of the parameter
// count that the unquantized remainder barely moves it. Two published
// checkpoints measure it: openai/gpt-oss-120b is 65.25 GB over 116.83B
// parameters (0.5585) and moonshotai/Kimi-K3 is 1560.86 GB over 2779.93B
// (0.5615). The rung sits above both rather than between them, because
// the two error directions are not symmetric: a budget that
// under-promises refuses a shape that would have fit, and one that
// over-promises buys an out-of-memory error after the rental starts.
func (p Precision) BytesPerParam() float64 {
	switch p {
	case PrecisionFP16, PrecisionBF16:
		return 2
	case PrecisionFP8, PrecisionINT8:
		return 1
	case PrecisionAWQ, PrecisionGPTQ:
		return 0.6
	case PrecisionMXFP4:
		return 0.57
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
	case PrecisionFP8, PrecisionINT8, PrecisionAWQ, PrecisionGPTQ, PrecisionMXFP4:
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

	// ActiveParams is the parameters read to decode one token, across the
	// whole engine and before the card division, matching
	// KVBytesPerToken. On a dense model it equals the parameter count,
	// because a dense model reads all of itself every step. On a sparse
	// one it is far smaller, and the difference is what lets a 2.8T model
	// decode at the speed of a 100B one while being billed for the memory
	// of a 2.8T one.
	//
	// Zero means the share could not be worked out, never that nothing is
	// read. See ActiveParams for the cases that produce it.
	ActiveParams int64

	// ActiveWeightBytes is ActiveParams at the plan's weight precision:
	// the weight traffic one decode step costs. Zero when ActiveParams is.
	//
	// A flat rate per parameter, the same simplification WeightBytes
	// makes. A mixed-precision checkpoint holds the attention and the
	// embeddings above the experts' precision, so a step that is mostly
	// attention costs more than this says.
	ActiveWeightBytes int64
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

// ffnMatrices is how many weight matrices one feed-forward expert holds.
//
// Three, for the gated activations every current sparse model uses: a
// gate projection and an up projection into the expert's intermediate
// width, and a down projection back out. An architecture using an
// ungated two-matrix feed-forward would overcount here by half, which is
// one of the things the sanity check in ActiveParams catches.
const ffnMatrices = 3

// moeLayers is the layers that actually hold experts.
//
// Sparse models routinely make the first layer or three dense, and those
// layers hold an ordinary feed-forward rather than an expert stack.
//
// Multi-token-prediction layers are added back on. They sit past
// num_hidden_layers and carry a full expert stack that the published
// parameter total counts, so leaving them out sizes the routed share one
// layer short and strands that layer's unpicked experts in the activated
// figure. GLM-5.2 read 51.2B active against a checkpoint that says 41.8B
// until this was added.
//
// The error survived #340 because the formula was checked against Kimi
// K3, which publishes no such layer. DeepSeek-V3, GLM-4.5 and GLM-5.2 all
// publish one.
func moeLayers(a *Arch) int32 {
	n := a.GetLayers() - a.GetDenseLayers() + a.GetMtpLayers()
	if n < 0 {
		return 0
	}
	return n
}

// expertParams is the parameter count of one routed expert.
//
// The width is the expert's own where the model publishes one, since a
// model that projects down before the expert stack runs its experts
// narrower than itself.
func expertParams(a *Arch) int64 {
	width := int64(a.GetRoutedExpertHiddenSize())
	if width <= 0 {
		width = int64(a.GetHiddenSize())
	}
	return ffnMatrices * width * int64(a.GetMoeIntermediateSize())
}

// RoutedExpertParams is the parameters held in routed experts: resident
// on the cards at all times, and read only when the router picks them.
//
// Zero for a dense model, and zero for a sparse model that publishes too
// little of its shape to compute the share.
func RoutedExpertParams(a *Arch) int64 {
	if a.GetNumExperts() <= 0 {
		return 0
	}
	layers, per := moeLayers(a), expertParams(a)
	if layers <= 0 || per <= 0 {
		return 0
	}
	return int64(layers) * int64(a.GetNumExperts()) * per
}

// ActiveParams is the parameters read to decode one token.
//
// Everything the model holds, less the routed experts the router did not
// pick. Subtracting rather than adding up the pieces is deliberate: the
// attention weights, the embeddings, the dense layers' feed-forward and
// the shared experts are all read every step and all already counted in
// the published parameter total, so naming them individually would mean
// modelling four more shapes to arrive at a number the subtraction gets
// for free.
//
// Zero means unknown, and every way to get there is a sparse model whose
// share will not compute. It publishes no activated count, so there is
// nothing to apportion. Its activated count exceeds its expert count,
// which is a config to distrust rather than arithmetic to attempt. It
// publishes no expert width, so the stack has no size. Or the computed
// share comes out at or above the whole model, which means the shape
// assumed here is not this model's, most likely an ungated feed-forward
// or a width stated somewhere this does not read. Reporting a
// dense-looking figure in any of those cases would be worse than
// reporting none, since every use of this number is an argument that it
// is much smaller than the total.
func ActiveParams(a *Arch) int64 {
	total := a.GetParams()
	if total <= 0 {
		return 0
	}
	if a.GetNumExperts() <= 0 {
		// Dense, so every parameter is read every step.
		return total
	}
	// Sparse from here on. A sparse model whose share will not compute is
	// unknown rather than dense: falling through to the total would
	// report a mixture-of-experts model as reading all of itself, which
	// is the one answer that is certainly wrong.
	resident := RoutedExpertParams(a)
	if resident <= 0 {
		return 0
	}
	active := a.GetNumExpertsPerTok()
	if active <= 0 || active > a.GetNumExperts() {
		return 0
	}
	if resident >= total {
		return 0
	}
	return total - resident + int64(moeLayers(a))*int64(active)*expertParams(a)
}

// CachingLayers is the layers whose cache grows with the sequence.
//
// Every layer, unless the model is a hybrid that says otherwise. A
// hybrid's linear-attention layers hold a state of fixed size however
// long the sequence runs, so they contribute nothing per token and
// counting them prices the cache at a multiple of what it costs.
func CachingLayers(a *Arch) int32 {
	if n := a.GetFullAttentionLayers(); n > 0 && n < a.GetLayers() {
		return n
	}
	return a.GetLayers()
}

// LatentCache reports whether the model caches a compressed latent
// rather than a key and a value per attention head.
func LatentCache(a *Arch) bool { return a.GetKvLoraRank() > 0 }

// KVBytesPerToken is the cache cost of one token, across the whole
// engine.
//
// Two shapes, and which one a model uses moves the figure by more than an
// order of magnitude rather than by a few percent.
//
// The ordinary shape stores a key and a value separately for every
// key-value head at every layer, hence the two.
//
// The compressed shape stores one latent per token per layer, from which
// every head's key and value are reconstructed on the fly, plus the
// position-carrying part of the key which cannot be compressed and rides
// uncompressed beside it. There is no factor of two, because the one
// latent serves as both, and no head count, because the whole point of
// the design is that the cache stops scaling with heads. DeepSeek-V3 at
// 61 layers of 512 + 64 elements works out to 70,272 bytes per token at
// bf16, which is the figure that model is published against.
func KVBytesPerToken(a *Arch, cache Precision) int64 {
	layers := int64(CachingLayers(a))
	if LatentCache(a) {
		return layers * (int64(a.GetKvLoraRank()) + int64(a.GetQkRopeHeadDim())) * cache.BytesPerCacheElement()
	}
	return 2 * layers * int64(a.GetKvHeads()) * int64(a.GetHeadDim()) * cache.BytesPerCacheElement()
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

	// KV cache: the state one token contributes at each caching layer,
	// summed over those layers, times the bytes per element the cache
	// precision sets.
	perToken := KVBytesPerToken(a, p.cacheDtype())
	kv := float64(perToken) * float64(p.MaxModelLen) * float64(p.MaxBatch)

	// Activations: scratch for a forward pass, following batch, context,
	// and hidden dimension. Always two bytes per element regardless of
	// how the weights are stored, because a quantized model dequantizes
	// to half precision for the matrix multiply. Quantization moves the
	// weight term and leaves this one alone.
	activations := float64(p.MaxBatch) * float64(p.MaxModelLen) * float64(a.GetHiddenSize()) * activationElementBytes * ActivationFactor

	perCard := func(total float64) int64 { return int64(total / float64(cards)) }

	// A latent cache is replicated on every card rather than sharded
	// across them. Each rank reconstructs all of the heads it computes
	// from the whole latent, so each rank needs the whole latent, and
	// adding cards buys weight headroom without buying any cache
	// headroom. This is the tradeoff compressed attention makes and it is
	// the reason engines grow a separate data-parallel attention mode to
	// escape it.
	kvPerCard := perCard(kv)
	if LatentCache(a) {
		kvPerCard = int64(kv)
	}

	b := Budget{
		Cards:           cards,
		WeightBytes:     perCard(weights),
		KVBytes:         kvPerCard,
		ActivationBytes: perCard(activations),
		KVBytesPerToken: perToken,
		ActiveParams:    ActiveParams(a),
	}
	b.ActiveWeightBytes = int64(float64(b.ActiveParams) * p.Weights.BytesPerParam())
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

// MaxSessions is how many sequences of the plan's context length fit
// concurrently on the plan's cards, once the weights are placed.
//
// The inverse of Compute, and the question an operator actually has.
// Compute answers "does this batch fit"; this answers "what batch fits",
// which is the number that decides whether a deployment is economic. The
// weights are a fixed cost paid once and the cache is a cost paid per
// session, so what is left after the weights, divided by what a session
// costs, is the concurrency the hardware supports. Everything about the
// cost per token follows from it, because the fixed cost of reading the
// weights splits across however many sessions are in flight.
//
// The plan's MaxBatch is ignored, since it is the answer rather than an
// input. Zero means the weights alone do not leave room for one session,
// which is a different failure from a small answer and reads that way
// in any table quoting it.
//
// Activations are solved for alongside the cache rather than held
// constant, because they follow the batch too. Treating them as fixed
// overstates the answer at exactly the long contexts where the answer
// matters.
func MaxSessions(a *Arch, p Plan, cardBytes int64, utilization float64) (int64, error) {
	if err := ValidateArch(a); err != nil {
		return 0, err
	}
	probe := p
	probe.MaxBatch = 1
	if err := probe.Validate(); err != nil {
		return 0, err
	}
	if cardBytes <= 0 {
		return 0, fmt.Errorf("card memory must be positive, got %d", cardBytes)
	}

	cards := p.cards()
	usable := float64(UsableBytes(cardBytes, utilization))

	// The overhead band is a fraction of the working set, so a card holds
	// (1 + OverheadFraction) times whatever the three real terms come to.
	// Dividing it out here is what keeps the answer on the same side of
	// the line Against calls Fits.
	room := usable/(1+OverheadFraction) - float64(a.GetParams())*p.Weights.BytesPerParam()/float64(cards)
	if room <= 0 {
		return 0, nil
	}

	// Per session, per card. A latent cache is replicated rather than
	// sharded, for the reason Compute gives, so more cards buy weight
	// room and no cache room. Activations shard either way.
	cache := float64(KVBytesPerToken(a, p.cacheDtype())) * float64(p.MaxModelLen)
	if !LatentCache(a) {
		cache /= float64(cards)
	}
	activation := float64(p.MaxModelLen) * float64(a.GetHiddenSize()) * activationElementBytes * ActivationFactor / float64(cards)

	perSession := cache + activation
	if perSession <= 0 {
		return 0, fmt.Errorf("a session costs nothing at this shape, which cannot be right")
	}
	return int64(room / perSession), nil
}

// SessionCost is what one sequence of the given context length costs per
// card, split into the cache and the activation scratch that follow it.
//
// Reported beside a session count because the count alone does not say
// which term ran out, and past a few thousand tokens the answer is
// almost always the cache.
func SessionCost(a *Arch, p Plan, contextLen int32) (cache, activation int64) {
	cards := p.cards()
	c := float64(KVBytesPerToken(a, p.cacheDtype())) * float64(contextLen)
	if !LatentCache(a) {
		c /= float64(cards)
	}
	act := float64(contextLen) * float64(a.GetHiddenSize()) * activationElementBytes * ActivationFactor / float64(cards)
	return int64(c), int64(act)
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
		// The rule below is about how the cache shards, so it does not
		// apply to a model whose cache does not shard at all. A latent
		// cache is replicated on every card whatever the head count
		// divides by, and refusing a card count on head-divisibility
		// grounds would refuse a shape for a saving that was never on
		// offer either way.
		if n > 1 && !LatentCache(a) && a.GetKvHeads()%n != 0 {
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
