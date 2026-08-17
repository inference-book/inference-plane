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

	// TextConfig is where multimodal models put the language model's
	// shape. A vision-language model's top-level config describes the
	// wrapper, and reading the wrapper's (absent) layer count as the
	// model's would report a KV cost of zero for a model that certainly
	// has one.
	TextConfig *modelConfig `json:"text_config"`
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
		Params:     info.Safetensors.Total,
		Layers:     cfg.Layers,
		KvHeads:    cfg.KVHeads,
		HeadDim:    cfg.HeadDim,
		HiddenSize: cfg.HiddenSize,
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
	if arch.HeadDim == 0 && cfg.AttentionHead > 0 {
		arch.HeadDim = cfg.HiddenSize / cfg.AttentionHead
	}

	if err := vrambudget.ValidateArch(arch); err != nil {
		return nil, fmt.Errorf("model %q config is missing what the budget needs: %w", id, err)
	}
	return &provisionerv1.DescribeModelResponse{Architecture: arch}, nil
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
