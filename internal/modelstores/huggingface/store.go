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
	ID       string    `json:"id"`
	Gated    gatedFlag `json:"gated"`
	Disabled bool      `json:"disabled"`

	// Safetensors is HF's tensor accounting, present only on
	// repositories that publish weights in that format. Read by
	// Architecture for the parameter count; validation ignores it.
	Safetensors *safetensorsInfo `json:"safetensors"`
}

// gatedFlag decodes HF's `gated` field, which is not a boolean on the
// wire even though it reads like one.
//
// An ungated repository sends `false`. A gated one sends the string
// "auto" or "manual", naming how access is granted rather than whether
// it is needed. Decoding it as a Go bool fails the whole response, so
// every gated model came back as "HF returned 200 but body unparseable"
// rather than as a gated model, and the pre-flight check that exists to
// give an actionable answer gave the least actionable one there is.
//
// Found by pointing the new describe verb at Llama-3.3-70B, which is
// both gated and the obvious candidate for a 70B deployment. The
// existing tests all fixture `"gated": false`, which is the one shape
// that works.
type gatedFlag bool

func (g *gatedFlag) UnmarshalJSON(b []byte) error {
	var asBool bool
	if err := json.Unmarshal(b, &asBool); err == nil {
		*g = gatedFlag(asBool)
		return nil
	}
	var asString string
	if err := json.Unmarshal(b, &asString); err != nil {
		return fmt.Errorf("gated: want a bool or a string, got %s", snippet(b))
	}
	// Any named gating mode means gated. Matching the known strings
	// exactly would turn a mode HF adds later into a parse failure, and
	// failing the whole lookup over a field nothing reads is how this
	// broke the first time.
	*g = gatedFlag(asString != "" && asString != "false")
	return nil
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
		// HF answers 401 for a gated model AND for one that does not
		// exist, because telling an anonymous caller which is which would
		// leak the existence of private repositories. So this cannot be
		// narrowed to "gated" without being wrong for every typo, and
		// naming only the gated case sends an operator to go and accept a
		// license for a model they misspelled.
		return nil, fmt.Errorf("model %q is gated, or does not exist; huggingface.co answers 401 for both when no token is sent. "+
			"set HF_TOKEN with read access and retry, and check the spelling if that does not help", id)
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
