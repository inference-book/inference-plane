package vast

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/inference-book/inference-plane/internal/provisioners"
	skhttp "github.com/panyam/servicekit/http"
)

// DefaultBaseURL is the Vast.ai REST base. Vast versions some endpoints
// at /api/v0 (bundles, asks, DELETE /instances) and others at /api/v1
// (paginated list /instances/). We carry both paths verbatim on the
// callsites so the wire shape stays debuggable from curl. WithBaseURL
// overrides this in tests; the test base is the httptest.Server URL
// (no trailing slash) and the same per-callsite paths apply.
const DefaultBaseURL = "https://console.vast.ai"

// Versioned path segments. Held as constants so the call sites read
// like the documented endpoints rather than carrying inline literals.
const (
	pathBundles      = "/api/v0/bundles/"
	pathAskPrefix    = "/api/v0/asks/"
	pathInstancesV0  = "/api/v0/instances/"
	pathInstancesV1  = "/api/v1/instances/"
)

// EnvHTTPDebug enables the wire-level debug transport when set to any
// non-empty value. Mirrors runpod's IPLANE_RUNPOD_DEBUG -- the
// equivalent toggle for the Vast.ai adapter.
const EnvHTTPDebug = "IPLANE_VAST_DEBUG"

// Client carries the bits every Vast.ai request needs (base URL,
// bearer token) plus an optional custom *http.Client for tests. The
// actual HTTP send / read / decode lives in servicekit's http.Call /
// CallVoid. Thin by design -- mirrors runpod's Client almost
// one-for-one.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client // when nil, servicekit's DefaultHttpClient is used
}

// ClientOption configures the Client at construction time.
type ClientOption func(*Client)

// WithHTTPClient swaps in a custom *http.Client. Tests pass an
// httptest.Server's client; production keeps the default.
func WithHTTPClient(c *http.Client) ClientOption {
	return func(cl *Client) { cl.httpClient = c }
}

// WithBaseURL points the client at a different endpoint. Tests pass
// the httptest.Server URL; staging tests point at a staging endpoint.
func WithBaseURL(u string) ClientOption {
	return func(cl *Client) { cl.baseURL = strings.TrimRight(u, "/") }
}

// NewClient builds a Vast.ai REST client. apiKey is the operator's
// VAST_API_KEY, attached as a bearer token on every request.
//
// When IPLANE_VAST_DEBUG is set, the underlying http.Client is wrapped
// with a debugTransport that prints the request/response bytes to
// stderr (sans Authorization header). The wrap is non-destructive --
// we clone any caller-supplied *http.Client so re-use elsewhere keeps
// its original transport.
func NewClient(apiKey string, opts ...ClientOption) *Client {
	c := &Client{
		baseURL: DefaultBaseURL,
		apiKey:  apiKey,
	}
	for _, opt := range opts {
		opt(c)
	}
	if os.Getenv(EnvHTTPDebug) != "" {
		base := c.httpClient
		if base == nil {
			base = http.DefaultClient
		}
		cloned := *base
		cloned.Transport = newDebugTransport(cloned.Transport)
		c.httpClient = &cloned
	}
	return c
}

// newReq builds a JSON-shaped request with bearer auth attached. The
// caller passes it to servicekit's Call/CallVoid, which adds the
// context and performs the round trip.
//
// path is the full server-relative path including its API-version
// prefix (e.g. "/api/v0/bundles/" or "/api/v1/instances/"). Vast.ai
// mixes v0 and v1 across endpoints, so we don't smuggle the prefix
// into a single config knob.
func (c *Client) newReq(method, path string, query url.Values, body any) (*http.Request, error) {
	endpoint, err := url.JoinPath(c.baseURL, path)
	if err != nil {
		return nil, fmt.Errorf("build url: %w", err)
	}
	if len(query) > 0 {
		endpoint = endpoint + "?" + query.Encode()
	}
	var bodyBytes []byte
	if body != nil {
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request body: %w", err)
		}
	}
	req, err := skhttp.NewBytesRequest(method, endpoint, bodyBytes)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if bodyBytes != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// callOpts returns the servicekit options to apply. Tests inject a
// custom *http.Client; production lets servicekit's secure default
// apply (TLS verification on).
func (c *Client) callOpts() []skhttp.CallOption {
	if c.httpClient != nil {
		return []skhttp.CallOption{skhttp.WithClient(c.httpClient)}
	}
	return nil
}

// wrapErr translates a servicekit HTTPError into a *provisioners.ProviderError
// with Vast.ai's error body parsed. Preserves the raw body via the
// wrapped cause so the original message reaches the operator.
//
// Vast.ai's error shapes observed:
//   - {"error": "not_found", "msg": "Instance not found"}
//   - {"success": false, "msg": "..."}
//   - {"error": "invalid_request", "msg": "..."}
//   - plain text on 5xx fronting from the edge.
//
// 404 is the not-found signal across endpoints. 410 ("offer gone")
// also surfaces during rent races -- we surface it verbatim because
// the operator's actionable next step is to re-search.
func wrapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var httpErr *skhttp.HTTPError
	if errors.As(err, &httpErr) {
		msg := parseErrorMessage(httpErr.Body)
		cause := errors.New(msg)
		switch httpErr.Code {
		case http.StatusNotFound:
			cause = fmt.Errorf("%s: %w", msg, provisioners.ErrNotFound)
		case http.StatusUnauthorized, http.StatusForbidden:
			cause = fmt.Errorf("auth failed: %s", msg)
		}
		return provisioners.NewProviderError(provisioners.ProviderVast, op, cause, httpErr.Code)
	}
	return provisioners.NewProviderError(provisioners.ProviderVast, op, err, 0)
}

// errorBody is the common Vast.ai error shape. Vast inconsistently
// uses `error` (a short code like "not_found") and `msg` (the
// human-readable detail) across endpoints; we render whichever is
// populated.
type errorBody struct {
	Error   string `json:"error"`
	Msg     string `json:"msg"`
	Message string `json:"message"`
	Success *bool  `json:"success,omitempty"`
}

// parseErrorMessage extracts a useful message from Vast.ai's response.
// Prefers `msg` over `error` because `msg` carries the operator-
// actionable string ("Instance not found", "invalid instance_id")
// while `error` is the short type tag. Falls back to the raw body for
// unrecognized shapes so 5xx edge HTML still surfaces.
func parseErrorMessage(body []byte) string {
	var single errorBody
	if err := json.Unmarshal(body, &single); err == nil {
		switch {
		case single.Msg != "":
			return single.Msg
		case single.Message != "":
			return single.Message
		case single.Error != "":
			return single.Error
		}
	}
	return strings.TrimSpace(string(body))
}
