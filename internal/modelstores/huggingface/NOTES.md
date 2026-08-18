# huggingface store notes

Lore for `internal/modelstores/huggingface`. Two things live here: the
model-existence pre-flight (`store.go`) and the architecture read behind
`DescribeModel` (`architecture.go`).

## The hub has no convention, and the spelling follows the family

Every field below is read through aliases. This is not defensiveness: the
two models Part IV is built around disagree with each other, and reading
only the canonical name reports GLM-5.2 as dense and Kimi K3 as
activating no experts. Both are wrong answers rather than missing ones.

| Concept | Spellings, and who uses them |
|---|---|
| routed expert count | `n_routed_experts` (DeepSeek, GLM) · `num_experts` (Qwen, Kimi K3) · `num_local_experts` (Mixtral, Llama 4, GPT-OSS) |
| activated per token | `num_experts_per_tok` (everything else) · `num_experts_per_token` (Kimi K3) · `experts_per_token` (GPT-OSS, alongside the standard one) |
| shared experts | `n_shared_experts` (DeepSeek, GLM, Kimi K2) · `num_shared_experts` (Kimi K3) · size-only via `shared_expert_intermediate_size` (Qwen2-MoE) · absent (Mixtral, Qwen3, GPT-OSS) |
| expert FFN width | `moe_intermediate_size` (most) · `intermediate_size` (Mixtral, GPT-OSS, which have no dense feed-forward beside the experts) |

`firstNonZero` takes the first stated one, which is safe because a family
states one and the rest decode to zero. Where two are stated they have so
far agreed (GPT-OSS states both activation spellings, both 4).

Fixtures for all of these are real captured `config.json` files under
`testdata/`, trimmed to the keys the budget reads. Hand-written fixtures
would only re-assert the spelling the implementation already assumes.

## The expert count gates everything below it

`resolveExperts` returns early when no expert count is found. Every field
after it has a dense meaning as well as a sparse one, most importantly
`intermediate_size`, which on a dense model is the ordinary feed-forward
width. Reading it unconditionally would make every dense model look
sparse to a caller keying on the field being set.

## Absent means unknown, and the derivations are exact or absent

- **No `num_key_value_heads`** means no grouped-query attention, so it is
  as many as there are attention heads. Never zero.
- **No `head_dim`** is derived as `hidden_size / num_attention_heads`,
  which is exact by construction, **except on a latent-cache model**. The
  division always produces a number; on Kimi K3 it produced 74, which is
  neither of the head dimensions that model publishes nor anything the
  engine allocates. A published head dimension is kept either way; only
  the invention is dropped.
- **No `routed_expert_hidden_size`** means the experts run at the model's
  own width, which is what almost every family does. Stored as the hidden
  size rather than left at zero, because every reader multiplies by it.
- **Qwen2-MoE's shared-expert count stays zero.** It has exactly one and
  publishes only `shared_expert_intermediate_size`, a width with no
  count. Inferring the count from the width would be a guess where the
  other derivation here is exact.
- **No safetensors parameter count is a refusal**, not a guess. The
  obvious guess is to read a size out of the model's name, and a name is
  right often enough to be trusted and wrong exactly where a budget
  matters: on repackaged and merged models whose names describe their
  ancestry rather than their weights.

## Multimodal models nest, and hybrids list their layers

A vision-language model describes the language model under `text_config`,
and the wrapper's absent layer count would report a KV cost of zero. The
unwrap triggers on the nested config carrying the layers. Kimi K3 arrives
this way (`KimiK3ForConditionalGeneration` wrapping `KimiLinearForCausalLM`).

`linear_attn_config.full_attn_layers` names the layers that are ordinary
attention; the rest are linear-attention layers whose state does not grow
with the sequence. Recorded only when it is a real restriction, since a
list naming every layer says the same thing as no list and one meaning
for absent is worth keeping.

## Gated models answer `"auto"`, not `true`

`gatedFlag.UnmarshalJSON` exists because HF sends `"auto"` or `"manual"`
where a bool is expected, and decoding as bool failed the whole response
for every gated model.

## What consumes this

`internal/vrambudget` and nothing else, through the `Arch` type alias.
The arithmetic and its calibration are in
[../../vrambudget/NOTES.md](../../vrambudget/NOTES.md).
