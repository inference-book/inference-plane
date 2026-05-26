// Package huggingface implements modelstores.ModelStore against the
// public Hugging Face Hub API. v0.1 uses it for pre-flight validation
// of model specs (catches typos + gated-access errors before paying
// for a pod that fails 3 minutes in). iplane does not download
// weights -- vLLM does that inside the pod from the same model id.
package huggingface

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/inference-book/inference-plane/internal/modelstores"
)

// DefaultBaseURL is the public HF API. Tests inject an httptest server.
const DefaultBaseURL = "https://huggingface.co"

// DefaultTimeout caps each pre-flight call so a degraded HF doesn't
// stall every deploy by 30s. Operators on slow links can bypass with
// --skip-model-validation.
const DefaultTimeout = 5 * time.Second

// hfModelSpec validates the operator-supplied spec shape before any
// network call: `<org>/<name>` with an optional `:<revision>` suffix.
// Matches HF's canonical id format; rejects bare names, paths with
// extra slashes, etc.
//
// We intentionally don't validate the revision sub-pattern -- HF
// accepts any branch/tag/commit-sha string, so anything after the
// colon is opaque to us.
var hfModelSpec = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*\/[a-zA-Z0-9._-]+(:[^/\s]+)?$`)

// Store is the modelstores.ModelStore impl backed by huggingface.co.
//
// Zero value is NOT ready for use; construct via New(). The Token
// field is set from $HF_TOKEN by the CLI wiring; if empty, gated
// models that require auth surface as 401 from HF, which Resolve
// reports as InvalidArgument with a "set HF_TOKEN" hint.
type Store struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New builds a Store with sensible defaults. Pass an empty token if
// the operator didn't set $HF_TOKEN -- gated models will then surface
// the actionable error rather than silently proceeding.
func New(token string) *Store {
	return &Store{
		BaseURL: DefaultBaseURL,
		Token:   token,
		HTTP:    &http.Client{Timeout: DefaultTimeout},
	}
}

// modelInfo captures the subset of huggingface.co/api/models/<id>
// response we care about. The endpoint returns much more (siblings,
// safetensors metadata, etc.) but we don't need any of it for
// validation -- existence + access is enough.
type modelInfo struct {
	ID       string `json:"id"`
	Gated    bool   `json:"gated"`
	Disabled bool   `json:"disabled"`
}

// Resolve validates the spec against HF's model-info endpoint. On
// success returns the spec unchanged plus HF_TOKEN env propagation if
// the operator has it set. Errors map to actionable messages:
//
//   - bad spec shape           -> "spec %q is not <org>/<name>[:rev]"
//   - HF 404                   -> "model not found on huggingface.co (typo?)"
//   - HF 401 (gated, no token) -> "model is gated; set HF_TOKEN"
//   - HF 403 (gated, no perms) -> "model is gated; HF_TOKEN lacks access"
//   - HF disabled flag         -> "model has been disabled by HF"
//   - network / 5xx            -> "HF API unreachable; --skip-model-validation to bypass"
func (s *Store) Resolve(ctx context.Context, spec string) (modelstores.Resolved, error) {
	if spec == "" {
		return modelstores.Resolved{}, errors.New("model spec is required")
	}
	if !hfModelSpec.MatchString(spec) {
		return modelstores.Resolved{}, fmt.Errorf("model spec %q is not a valid HF id (want <org>/<name> with optional :<revision>)", spec)
	}

	// HF API uses the spec without :<revision>; the revision is
	// validated separately by passing ?revision=... but for existence
	// checking we only need the canonical id.
	id := spec
	if i := strings.IndexByte(spec, ':'); i >= 0 {
		id = spec[:i]
	}

	info, err := s.fetchModelInfo(ctx, id)
	if err != nil {
		return modelstores.Resolved{}, err
	}
	if info.Disabled {
		return modelstores.Resolved{}, fmt.Errorf("model %q has been disabled on huggingface.co", id)
	}

	res := modelstores.Resolved{EngineModelArg: spec}
	if s.Token != "" {
		res.EnvOverrides = map[string]string{"HF_TOKEN": s.Token}
	}
	return res, nil
}

// fetchModelInfo issues GET /api/models/<id> against HF and parses
// the response. Auth via Bearer token when s.Token is set. Returns
// an actionable error for each HTTP class.
func (s *Store) fetchModelInfo(ctx context.Context, id string) (*modelInfo, error) {
	url := strings.TrimRight(s.BaseURL, "/") + "/api/models/" + id
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build HF request: %w", err)
	}
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HF API unreachable: %w (use --skip-model-validation to bypass)", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
		var info modelInfo
		if err := json.Unmarshal(body, &info); err != nil {
			return nil, fmt.Errorf("HF returned 200 but body unparseable: %w (body: %s)", err, snippet(body))
		}
		return &info, nil
	case http.StatusUnauthorized:
		// HF treats missing-token-on-gated-model as 401.
		return nil, fmt.Errorf("model %q is gated on huggingface.co; set HF_TOKEN with read access and retry", id)
	case http.StatusForbidden:
		// Token present but lacks access (operator hasn't accepted the
		// model's license, or the token's scope excludes this org).
		return nil, fmt.Errorf("model %q is gated and HF_TOKEN lacks access; accept the model license on huggingface.co/%s, then retry", id, id)
	case http.StatusNotFound:
		return nil, fmt.Errorf("model %q not found on huggingface.co (typo? or unpublished revision)", id)
	default:
		return nil, fmt.Errorf("HF API returned %d %s: %s (use --skip-model-validation to bypass)",
			resp.StatusCode, http.StatusText(resp.StatusCode), snippet(body))
	}
}

// snippet trims a response body to a single readable line for error
// messages. Avoids dumping multi-MB HTML pages when HF returns its
// branded error pages.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	// Collapse newlines so the error stays on one log line.
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
