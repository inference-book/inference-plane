# vrambudget notes

Lore for `internal/vrambudget`. The package doc says what the arithmetic
is; this says why it is shaped this way and what got it wrong before.

## The four claims, and which one decides

Fit is weights + KV cache + activations + a flat 15% overhead band
against the card, never the weight footprint against the card. `Verdict`
is `fits | tight | overcommitted`, and `tight` means it clears only by
eating into the overhead band, which is the worst thing to discover after
renting.

**`largest term` is the load-bearing output.** Past a few thousand tokens
at any real batch the cache outweighs the weights, so quantizing harder
barely moves the card count and the context or the concurrency has to
give instead.

## Held versus read: the two parameter counts

A mixture-of-experts model holds every expert resident and reads only the
activated ones per step. `Budget.ActiveParams` and `ActiveWeightBytes`
carry the second number; the four memory claims are unchanged by
sparsity, because an unpicked expert still occupies memory.

`ActiveParams` **subtracts** the unpicked experts from the published
total rather than summing the pieces that are read. Attention,
embeddings, the dense-layer feed-forward and the shared experts are all
read every step and all already inside the published total, so summing
would mean modelling four more shapes to arrive at a number the
subtraction gets for free.

The routed-expert formula is checkable rather than asserted:

```
moe_layers x num_experts x 3 x routed_expert_hidden_size x moe_intermediate_size
92 x 896 x 3 x 3584 x 3072 = 2,722,740,830,208
```

which is Kimi K3's published eight-bit tensor count to the parameter.
`ffnMatrices = 3` is the one architectural assumption baked in (gate, up,
down), and the `resident >= params` guard is the only thing standing
between it and a wrong answer on an ungated feed-forward.

**Zero active means unknown, never dense.** A draft fell through to the
total when the share would not compute and reported a sparse model as
reading all of itself, which is the one answer that is certainly wrong.
Four ways to get zero: no activated count, no expert width, an activated
count above the expert count, or a share computing to more than the whole
model.

## Two cache shapes, and they differ by more than an order of magnitude

Per-head caching is `2 x layers x kv_heads x head_dim`. Compressed-latent
attention caches **one** latent per token per layer plus the
uncompressed position-carrying remainder, so `layers x (kv_lora_rank +
qk_rope_head_dim)`: no factor of two, since the one latent serves as
both, and no head count, since not scaling with heads is the point.

Checked against a published figure: DeepSeek-V3 at 61 layers of 512 + 64
is 70,272 bytes per token at bf16, the number that model is quoted at.

Hybrids pay cache only on the layers that have a growing one.
`full_attention_layers` records that where it is a real restriction; Kimi
K3 is 24 attention layers among 93, and counting all 93 prices its cache
at nearly four times what it costs.

Getting this wrong was worth 40x on GLM-5.2 and 95x on K3 (#362). Before
the fix `model describe` reported a single 128k GLM-5.2 sequence as
costing 502.5 GB against a real 11.8.

### The latent is replicated across cards, not sharded

Each tensor-parallel rank reconstructs the heads it owns from the whole
latent, so each rank holds the whole latent. Adding cards buys weight
headroom and no cache headroom. It is the tradeoff compressed attention
makes, and it is why engines grow a separate data-parallel attention mode
to escape it.

**This is the least-verified claim in the package.** It is a reading of
how MLA works under TP rather than something pinned to a published
figure, and both `Compute`'s per-card division and `MaxSessions` depend
on it. The GLM measurement run (#358) is the first thing that tests it.

For the same reason `Sweep`'s shard-divisibility refusal does not apply
to a latent model: that rule describes a sharding that is not happening,
and refusing a card count on head-divisibility grounds would withhold a
shape for a saving that was never on offer.

## MinCards, Sweep, and why counts get refused

Powers of two only, and only where the KV heads divide evenly. Otherwise
an engine replicates KV heads rather than sharding them, the cache stops
shrinking with the card count, and the bill keeps growing. `Sweep`
reports the refusals with reasons rather than leaving gaps, because a gap
in the sequence reads as an oversight.

## MaxSessions is the inverse, and the two must agree at the boundary

`Compute` says whether a named batch fits; `MaxSessions` says what batch
fits. Same inequality from opposite ends, so an off-by-one is a shape the
pre-flight blesses and the engine refuses. Pinned from both directions
across three cache shapes, three contexts and two card counts.

Activations are solved for **inside** the per-session divisor rather than
held fixed, because they follow the batch exactly as the cache does and
holding them constant overstates the answer at exactly the long contexts
where it matters.

## The precision ladder is calibrated, not nominal

Four-bit is 0.6 rather than 0.5 because a four-bit build keeps group
scales and leaves embeddings and the output head higher; a published 32B
four-bit build lands near 4.8 effective bits.

`mxfp4` is a separate rung at **0.57**, not because the format differs
much but because *what fraction of the model stays unquantized* differs.
On a frontier sparse model the routed experts are so much of the
parameter count that the remainder barely moves the average. Two
published checkpoints measure it: `openai/gpt-oss-120b` at 65.25 GB over
116.83B (0.5585) and `moonshotai/Kimi-K3` at 1560.86 GB over 2779.93B
(0.5615). The rung sits **above** both rather than between them.

That direction is the rule: a filter should be generous so it does not
reject, a budget conservative so it does not promise a fit that OOMs on
arrival. Independently confirmed on the fp8 rung, where `zai-org/GLM-5.2-FP8`
measures 1.003 bytes per parameter.

`ActiveWeightBytes` uses a flat rate, the same simplification
`WeightBytes` makes. A mixed-precision checkpoint holds attention and
embeddings above the experts' precision, so a step that is mostly
attention costs more than this says.

## Reading a checkpoint's real footprint

Two Hugging Face endpoints answer this and neither needs a download:

```
/api/models/<id>                          -> safetensors.total, and the
                                             per-dtype parameter split
/<id>/resolve/main/model.safetensors.index.json
                                          -> metadata.total_size, exact bytes
```

The dtype split is what confirmed K3's expert share; `total_size` is what
calibrated the mxfp4 rung. Prefer them over any figure derived from a
model's name.

## The deploy reads its plan out of the engine args (#326)

`provisioners.EnginePlan` recognises `--quantization`, `--kv-cache-dtype`,
`--max-model-len` and `--max-num-seqs` wherever they appear in
`--engine-args`, builds a `Plan`, and `budgetCheck` refuses an
`overcommitted` shape before renting. Parsed rather than promoted to
typed deploy flags, because typed flags fork the vocabulary for every
engine that spells them differently and put the control plane in the
business of knowing engine CLIs.

**Every input is optional and a missing one skips the check**, logging
why. A false refusal is worse than the silence it replaces. `tight` warns
and proceeds there, unlike `model budget`'s exit code, because nothing
has been rented at the point the command runs and something has by the
point this does.

## Where the shape comes from

Over `DescribeModel`, never from the CLI host, since a gated model's
config needs the daemon's `HF_TOKEN`. A decorator dropping the
architecture capability disabled the read on every warm-cache daemon
(#324); see [docs/modelstore.md](../../docs/modelstore.md) on why
forwarding an optional capability needs a sentinel and not just a method.

The hub-read side of this, including which config field names real model
families actually use, is in
[../modelstores/huggingface/NOTES.md](../modelstores/huggingface/NOTES.md).

## The layer count is not the expert-layer count

`config.num_hidden_layers` does not count the multi-token-prediction
block, and the checkpoint does. GLM-5.2 publishes 78 layers and ships
tensors for layer indices 0 through 78, where that last one carries a full
256-expert stack. HF's safetensors accounting counts those experts in the
parameter total.

So sizing the routed-expert share from `num_hidden_layers - dense` leaves
one layer of unpicked experts nowhere, and since the activated figure is
computed by subtracting the resident share from the total, they land in
it. GLM-5.2 read 51.2B active against a checkpoint that says 41.8B, an
overstatement of a quarter, on the model Part IV rehearses with.

`moeLayers` adds `mtp_layers` back on. Two things about that choice:

- The MTP experts are counted as **resident**, which is unambiguous. They
  are on the cards whether or not speculative decoding runs.
- They are also counted as **read**, at the same top-k as any other layer,
  which is true only when the engine actually runs multi-token prediction.
  vLLM's is opt-in. Counting them errs high on the activated figure by one
  layer's picked experts, which is the safe direction for a budget and
  worth revisiting if a run ever cares about the difference.

**Why this survived #340.** The formula was validated against Kimi K3,
which publishes `num_nextn_predict_layers: 0`. K2 does too. DeepSeek-V3,
GLM-4.5 and GLM-5.2 all publish 1, so the one model the arithmetic was
checked against was the one model the error could not show up on. Found by
#350, whose acceptance was to compare against a published config.

## A pre-quantized checkpoint is not a smaller model

The parameter count this package reads is HF's element accounting, which
for an already-quantized repo counts packed elements rather than
parameters. `nvidia/GLM-5.2-NVFP4` reports 381B against the base model's
753B, and `QuantTrio/GLM-5.2-Int4-Int8Mix` reports 785B, more than the
bf16 original, because scales and zero-points are elements too. Applying a
precision to either is quantizing twice.

This package still has no guard, and deliberately does not need one. The
check sits at the operator surfaces, where `iplane model budget` refuses
and `model describe` prints no ladder (#382). `Compute` stays arithmetic
over whatever it is handed, so a caller passing a packed architecture in
gets a packed answer out, and a new caller wants the same
`GetQuantization()` check those two commands make.

## Two sharding dimensions, and they divide different parts of the model

`Plan` carries `TPSize` and `EPSize` because tensor parallelism and expert
parallelism do not shard the same thing. Tensor ranks each hold a slice of
every weight. Expert ranks each hold *whole experts* and a full copy of
everything else, so a plan running `tp=1` with the width carried by data
parallelism holds one eighth of the routed experts and a whole copy of the
attention, the embeddings and the dense layers, on every card.

`Compute` splits the weight term accordingly: `RoutedExpertParams` over
`EPSize`, the remainder over `TPSize`. `EPSize` at zero falls back to the
tensor width, so every plan that asked for no expert parallelism computes
what it always did.

The size of the error this fixes, GLM-5.2 at mxfp4 on eight 80 GB cards:

| plan | per card | verdict |
| --- | --- | --- |
| `tp=8` | 65.3 GB | fits |
| `tp=1, ep=8`, before | 65.3 GB | fits, wrongly |
| `tp=1, ep=8`, after | 77.3 GB | tight, against 77.3 usable |

The corrected figure lands on the boundary almost exactly, which is worth
knowing before reading much into either side of it.

### The cache under data parallelism is still wrong, in the safe direction

`LatentCache` replicates the cache on every card, and that is right under
*tensor* parallelism, where every rank works the same sequence and needs
that sequence's whole latent. Under *data* parallelism the ranks work
different sequences, so each holds only its own share and the per-card
cache should divide by the data-parallel width.

Left alone deliberately. The error overstates the cache, so it refuses
plans that would have fit, where the weight error understated and rented
hardware that then runs out of memory. A budget that is wrong should be
wrong in the direction that costs nothing. Worth fixing when a real run
makes the refusals bite; the reasoning is here so nobody has to derive it
twice.

### None of this has met hardware

The split follows vLLM's documented behaviour (`docs/serving/expert_parallel_deployment.md`,
v0.26.0): expert layers shard across all EP ranks, attention replicates
across data-parallel ranks when TP is 1 and shards when it is greater. The
mixed case (`tp=2, ep=8`, so `dp=4`) follows from the same rule and has
never been checked against a running engine. Same standing as the
MLA-replication claim above: a reading, not a measurement.

