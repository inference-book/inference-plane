package huggingface

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/modelstores"
	"github.com/inference-book/inference-plane/internal/vrambudget"
)

// modelConfig is the subset of a model's config.json that decides how
// much memory the KV cache costs per token. The file carries far more;
// none of the rest bears on the budget.
type modelConfig struct {
	Layers        int32 `json:"num_hidden_layers"`
	AttentionHead int32 `json:"num_attention_heads"`
	KVHeads       int32 `json:"num_key_value_heads"`
	HiddenSize    int32 `json:"hidden_size"`
	HeadDim       int32 `json:"head_dim"`

	// MaxPositionEmbeddings is the trained context window. It bears on no
	// budget term; it is read so a caller choosing a context length has
	// the model's own answer available instead of a house number.
	MaxPositionEmbeddings int32 `json:"max_position_embeddings"`

	// TextConfig is where multimodal models put the language model's
	// shape. A vision-language model's top-level config describes the
	// wrapper, and reading the wrapper's (absent) layer count as the
	// model's would report a KV cost of zero for a model that certainly
	// has one.
	TextConfig *modelConfig `json:"text_config"`

	// The expert fields below are read through several spellings each,
	// because the hub has no convention for them and the spelling follows
	// the model family. Every alias is listed with the families that use
	// it, so a reader can check the set against a config rather than
	// against this file's guess about one.

	// Routed expert count. DeepSeek and GLM say n_routed_experts, Qwen
	// and Kimi K3 say num_experts, Mixtral and Llama 4 and GPT-OSS say
	// num_local_experts.
	NRoutedExperts  int32 `json:"n_routed_experts"`
	NumExperts      int32 `json:"num_experts"`
	NumLocalExperts int32 `json:"num_local_experts"`

	// Activated experts per token. Every family says num_experts_per_tok
	// except Kimi K3, which spells it out. GPT-OSS states both and they
	// agree.
	NumExpertsPerTok   int32 `json:"num_experts_per_tok"`
	NumExpertsPerToken int32 `json:"num_experts_per_token"`
	ExpertsPerToken    int32 `json:"experts_per_token"`

	// Shared experts, the ones every token passes through. DeepSeek, GLM
	// and Kimi K2 say n_shared_experts; Kimi K3 says num_shared_experts.
	// Qwen2-MoE publishes only shared_expert_intermediate_size, a width
	// with no count, and is deliberately not read here (see
	// resolveExperts).
	NSharedExperts   int32 `json:"n_shared_experts"`
	NumSharedExperts int32 `json:"num_shared_experts"`

	// Expert feed-forward width. MoEIntermediateSize is the expert's own
	// width where a model has dense layers alongside the experts;
	// IntermediateSize is the dense width in that case and the expert
	// width in models that have no dense feed-forward at all.
	MoEIntermediateSize int32 `json:"moe_intermediate_size"`
	IntermediateSize    int32 `json:"intermediate_size"`

	// Leading dense layers before the mixture-of-experts stack starts.
	FirstKDenseReplace int32 `json:"first_k_dense_replace"`

	// Width a routed expert operates at, where the model projects down
	// before the expert stack. Absent means the experts run at the
	// model's own hidden size, which is what almost every family does.
	RoutedExpertHiddenSize int32 `json:"routed_expert_hidden_size"`

	// The compressed-latent attention fields. A model publishing
	// KVLoraRank caches one latent per token per layer instead of a key
	// and a value per head, and QKRopeHeadDim is the uncompressed
	// position-carrying remainder stored beside it.
	KVLoraRank    int32 `json:"kv_lora_rank"`
	QKRopeHeadDim int32 `json:"qk_rope_head_dim"`

	// LinearAttn is where a hybrid model says which of its layers are
	// ordinary attention. The rest are linear-attention layers whose
	// state does not grow with the sequence, so they cost nothing per
	// token and must not be counted.
	LinearAttn *linearAttnConfig `json:"linear_attn_config"`
}

// linearAttnConfig is the hybrid-attention layer split. Only the list of
// full-attention layers is read; the complementary list is derivable and
// the rest of the block describes the linear layers' own shape, which
// costs no cache.
type linearAttnConfig struct {
	FullAttnLayers []int32 `json:"full_attn_layers"`
}

// safetensorsInfo is HF's published tensor accounting, and the only
// parameter count on the Hub that is a measurement rather than a claim.
type safetensorsInfo struct {
	Total int64 `json:"total"`
}

// Architecture reports the part of a model fixed at training time, for
// the VRAM budget.
//
// Two calls: the model-info endpoint for the parameter count, and
// config.json for the shape. Both are needed and neither substitutes for
// the other, since the parameter count does not appear in config.json and
// the layer and head counts do not appear in model info.
//
// The parameter count comes from HF's safetensors accounting, which is
// derived from the uploaded tensors rather than stated by the uploader. A
// model that publishes no such accounting gets a refusal rather than a
// guess. The obvious guess is to read a size out of the model's name, and
// a name is a label the uploader chose: it is right often enough to be
// trusted and wrong exactly where a budget matters, on the repackaged and
// merged models whose names describe their ancestry rather than their
// weights.
func (s *Store) Architecture(ctx context.Context, req *provisionerv1.DescribeModelRequest) (*provisionerv1.DescribeModelResponse, error) {
	spec := req.GetModelSpec()
	if spec == "" {
		return nil, fmt.Errorf("model spec is required")
	}
	if !hfModelSpec.MatchString(spec) {
		return nil, fmt.Errorf("model spec %q is not a valid HF id (want <org>/<name> with optional :<revision>)", spec)
	}

	id, revision := spec, "main"
	if i := strings.IndexByte(spec, ':'); i >= 0 {
		id, revision = spec[:i], spec[i+1:]
	}

	info, err := s.fetchModelInfo(ctx, id)
	if err != nil {
		return nil, err
	}
	if info.Disabled {
		return nil, fmt.Errorf("model %q has been disabled on huggingface.co", id)
	}
	if info.Safetensors == nil || info.Safetensors.Total <= 0 {
		return nil, fmt.Errorf("model %q publishes no safetensors parameter count, so its weight footprint cannot be computed; pass the budget inputs explicitly or deploy without the pre-flight check", id)
	}

	cfg, err := s.fetchConfig(ctx, id, revision)
	if err != nil {
		return nil, err
	}
	// A multimodal model describes the language model one level down.
	// Take that one when it is the one carrying the layers.
	if cfg.TextConfig != nil && cfg.TextConfig.Layers > 0 {
		cfg = cfg.TextConfig
	}

	arch := &provisionerv1.ModelArchitecture{
		Params:                info.Safetensors.Total,
		Layers:                cfg.Layers,
		KvHeads:               cfg.KVHeads,
		HeadDim:               cfg.HeadDim,
		HiddenSize:            cfg.HiddenSize,
		MaxPositionEmbeddings: cfg.MaxPositionEmbeddings,
	}

	// A model without grouped-query attention states no separate KV head
	// count, because every attention head keeps its own key and value.
	// Absent means "as many as there are attention heads", never zero.
	if arch.KvHeads == 0 {
		arch.KvHeads = cfg.AttentionHead
	}
	// head_dim is optional and derivable. Deriving it is exact rather
	// than an estimate, since the hidden dimension is by construction the
	// head dimension times the attention-head count.
	//
	// Not derived for a model that caches a compressed latent. The
	// division is arithmetic that will always produce a number, and on a
	// latent-cache model that number describes nothing: the model has no
	// per-head key and value to size. Kimi K3 came out at 74, which is
	// neither its qk_nope_head_dim of 128 nor its v_head_dim of 128 nor
	// anything the engine allocates.
	if arch.HeadDim == 0 && cfg.AttentionHead > 0 && cfg.KVLoraRank == 0 {
		arch.HeadDim = cfg.HiddenSize / cfg.AttentionHead
	}

	resolveExperts(arch, cfg)

	resolveAttention(arch, cfg)

	if err := vrambudget.ValidateArch(arch); err != nil {
		return nil, fmt.Errorf("model %q config is missing what the budget needs: %w", id, err)
	}
	return &provisionerv1.DescribeModelResponse{Architecture: arch}, nil
}

// resolveExperts fills in the mixture-of-experts shape, or leaves it at
// zero for a dense model.
//
// Zero here is a shape rather than a missing reading. A dense model has
// no experts to count, so every field stays zero and no budget term
// changes, which is what keeps this additive for every model the tool
// handled before.
//
// The expert count gates the rest. Nothing else is read off a model that
// states no experts, because the remaining fields all have dense
// meanings: intermediate_size is a plain feed-forward width on a dense
// model, and reading it as an expert's would make every dense model look
// sparse to a caller keying on the field being set.
func resolveExperts(arch *provisionerv1.ModelArchitecture, cfg *modelConfig) {
	experts := firstNonZero(cfg.NRoutedExperts, cfg.NumExperts, cfg.NumLocalExperts)
	if experts <= 0 {
		return
	}

	arch.NumExperts = experts
	arch.NumExpertsPerTok = firstNonZero(cfg.NumExpertsPerTok, cfg.NumExpertsPerToken, cfg.ExpertsPerToken)
	// Absent is unknown and stays zero. Qwen2-MoE has exactly one shared
	// expert and publishes only its width, and inferring a count from a
	// width would be a guess where this file's other derivation (head_dim
	// from hidden_size) is exact. The width is what an active-parameter
	// term will want, and it can be read when there is a term reading it.
	arch.SharedExperts = firstNonZero(cfg.NSharedExperts, cfg.NumSharedExperts)
	// A model with no dense feed-forward beside its experts states one
	// width and it is the expert's. A model with both states the expert
	// width separately, so the plain field is its dense layers' and is
	// only reached when no expert-specific width exists.
	arch.MoeIntermediateSize = firstNonZero(cfg.MoEIntermediateSize, cfg.IntermediateSize)
	arch.DenseLayers = cfg.FirstKDenseReplace
	// Absent means the experts run at the model's own width. Left at the
	// hidden size rather than at zero, because every caller of this field
	// multiplies by it and zero would silently price the expert stack at
	// nothing.
	arch.RoutedExpertHiddenSize = firstNonZero(cfg.RoutedExpertHiddenSize, cfg.HiddenSize)
}

// resolveAttention records how the model caches, which decides the shape
// of the cache term rather than only its size.
//
// Absent throughout means the ordinary case: no latent, so a key and a
// value per head; no hybrid split, so every layer pays.
func resolveAttention(arch *provisionerv1.ModelArchitecture, cfg *modelConfig) {
	arch.KvLoraRank = cfg.KVLoraRank
	arch.QkRopeHeadDim = cfg.QKRopeHeadDim
	if cfg.LinearAttn == nil {
		return
	}
	// Recorded only when it is a real restriction. A list naming every
	// layer says the same thing as no list, and storing it that way keeps
	// one meaning for absent.
	if n := int32(len(cfg.LinearAttn.FullAttnLayers)); n > 0 && n < cfg.Layers {
		arch.FullAttentionLayers = n
	}
}

// firstNonZero returns the first stated value among a field's spellings.
// Zero means the config did not state it under that name, which is what
// makes ordering the aliases safe: a family states one of them and the
// rest decode to zero.
func firstNonZero(vals ...int32) int32 {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

// fetchConfig retrieves a model's config.json at a revision.
//
// The resolve endpoint redirects to a CDN, which the default client
// follows. A 404 here after model info succeeded means the repository
// exists and carries no config.json, which is a different problem from a
// missing model and gets a different message.
func (s *Store) fetchConfig(ctx context.Context, id, revision string) (*modelConfig, error) {
	url := fmt.Sprintf("%s/%s/resolve/%s/config.json", strings.TrimRight(s.BaseURL, "/"), id, revision)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build config request: %w", err)
	}
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("huggingface.co unreachable while reading %s config: %w", id, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s config: %w", id, err)
	}

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("model %q has no config.json at revision %q, so its architecture cannot be read", id, revision)
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("model %q config is gated; set HF_TOKEN with access to read it", id)
	case resp.StatusCode >= 300:
		return nil, fmt.Errorf("huggingface.co returned %d reading %s config: %s", resp.StatusCode, id, snippet(body))
	}

	var cfg modelConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s config.json: %w", id, err)
	}
	return &cfg, nil
}

// Ensure Store satisfies the optional capability.
var _ modelstores.ArchitectureSource = (*Store)(nil)
