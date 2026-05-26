package runpod

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

// Default endpoint for the RunPod REST API. Overrideable via WithBaseURL
// (httptest in unit tests, staging in integration tests).
const DefaultBaseURL = "https://rest.runpod.io/v1"

// Client carries the bits every RunPod request needs (base URL, bearer
// token) plus an optional custom *http.Client for tests. The actual
// HTTP send / read / decode lives in servicekit's http.Call / CallVoid.
// We keep this struct thin: build the request, hand it to servicekit.
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

// WithBaseURL points the client at a different endpoint. Tests pass the
// httptest.Server URL; staging tests point at a staging endpoint.
func WithBaseURL(u string) ClientOption {
	return func(cl *Client) { cl.baseURL = strings.TrimRight(u, "/") }
}

// NewClient builds a RunPod REST client. apiKey is the operator's
// RUNPOD_API_KEY, attached as a bearer token on every request.
//
// When IPLANE_RUNPOD_DEBUG is set, the underlying http.Client is wrapped
// with a debugTransport that prints the request/response bytes to stderr
// (sans Authorization header). The wrap is non-destructive -- we clone
// any caller-supplied *http.Client so re-use elsewhere keeps its
// original transport.
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
// with RunPod's array-shaped error body parsed. Preserves the raw body via
// the wrapped cause so the original message reaches the operator.
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
		return provisioners.NewProviderError(provisioners.ProviderRunPod, op, cause, httpErr.Code)
	}
	return provisioners.NewProviderError(provisioners.ProviderRunPod, op, err, 0)
}

// errorBody is the single-object form of RunPod's error response. The
// array form is handled in parseErrorMessage. `problems` is RunPod's
// per-field validation list (set on 4xx schema rejections); it carries
// the actually-actionable signal while `error` is the generic envelope.
type errorBody struct {
	Error    string   `json:"error"`
	Message  string   `json:"message"`
	Problems []string `json:"problems,omitempty"`
}

// parseErrorMessage extracts a useful message from RunPod's error
// response shape. RunPod is inconsistent across endpoints -- some
// return `{"error": "..."}`, some `{"message": "..."}`, some return
// an array `[{"error": "...", "problems": [...]}]`, some plain text.
// `problems` (when present) carries the actionable per-field validator
// output; we prefer it over the generic envelope. Errors are the
// operator's actionable signal -- never truncate them.
func parseErrorMessage(body []byte) string {
	render := func(e errorBody) string {
		if len(e.Problems) > 0 {
			return strings.Join(e.Problems, "; ")
		}
		if e.Error != "" {
			return e.Error
		}
		return e.Message
	}

	// Single-object form: {"error": "..."} or {"message": "..."}.
	var single errorBody
	if err := json.Unmarshal(body, &single); err == nil {
		if msg := render(single); msg != "" {
			return msg
		}
	}
	// Array form: [{"error": "...", "problems": [...]}, ...]. RunPod's
	// OpenAPI validator uses this for schema rejections.
	var arr []errorBody
	if err := json.Unmarshal(body, &arr); err == nil && len(arr) > 0 {
		parts := make([]string, 0, len(arr))
		for _, e := range arr {
			if msg := render(e); msg != "" {
				parts = append(parts, msg)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "; ")
		}
	}
	return string(body)
}
