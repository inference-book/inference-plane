package lambdalabs

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

// DefaultBaseURL is the Lambda Labs Cloud REST base. Verified
// via real-API probe 2026-06. The API is versioned at /api/v1.
const DefaultBaseURL = "https://cloud.lambdalabs.com"

// Versioned endpoint paths. Held as constants so the call sites
// read like the documented endpoints rather than carrying inline
// literals.
const (
	pathInstances             = "/api/v1/instances"
	pathInstanceTypes         = "/api/v1/instance-types"
	pathSSHKeys               = "/api/v1/ssh-keys"
	pathInstanceLaunch        = "/api/v1/instance-operations/launch"
	pathInstanceTerminate     = "/api/v1/instance-operations/terminate"
)

// EnvHTTPDebug enables wire-level debug logging when set to any
// non-empty value. Mirrors runpod's IPLANE_RUNPOD_DEBUG and vast's
// IPLANE_VAST_DEBUG -- one toggle pattern per adapter.
const EnvHTTPDebug = "IPLANE_LAMBDA_DEBUG"

// Client carries the bits every Lambda Labs request needs.
//
// Auth is HTTP Basic with the API key as the username and an
// empty password -- verified via real-API probe. NOT a Bearer
// token (RunPod and Vast both use Bearer; Lambda is the outlier).
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

// WithBaseURL points the client at a different endpoint. Tests
// pass the httptest.Server URL; staging tests point at a staging
// endpoint.
func WithBaseURL(u string) ClientOption {
	return func(cl *Client) { cl.baseURL = strings.TrimRight(u, "/") }
}

// NewClient builds a Lambda Labs REST client. apiKey is the
// operator's LAMBDA_API_KEY, attached via HTTP Basic on every
// request (Basic auth: username = apiKey, password = empty).
//
// When IPLANE_LAMBDA_DEBUG is set, the underlying http.Client is
// wrapped with a debugTransport that prints request/response
// bytes to stderr (sans Authorization header).
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

// newReq builds a JSON-shaped request with Basic auth attached.
// The caller passes it to servicekit's Call/CallVoid, which adds
// the context and performs the round trip.
//
// Auth detail: Basic <base64(apiKey + ":")> with the API key as
// the username and an empty password. SetBasicAuth handles the
// base64 encoding and colon separator.
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
	// Lambda Labs uses HTTP Basic auth with the API key as the
	// username and an empty password (verified via real-API probe).
	req.SetBasicAuth(c.apiKey, "")
	req.Header.Set("Accept", "application/json")
	if bodyBytes != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// callOpts returns the servicekit options to apply.
func (c *Client) callOpts() []skhttp.CallOption {
	if c.httpClient != nil {
		return []skhttp.CallOption{skhttp.WithClient(c.httpClient)}
	}
	return nil
}

// wrapErr translates a servicekit HTTPError into a
// *provisioners.ProviderError with Lambda Labs' error body parsed.
//
// Lambda's error shape (verified via real-API probe):
//
//	{"error": {"code": "...", "message": "...", "suggestion": "..."}}
//
// Common codes seen:
//   - global/object-does-not-exist  -> ErrNotFound mapping
//   - global/invalid-parameters     -> validation failure
//   - global/quota-exceeded         -> account limit
func wrapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var httpErr *skhttp.HTTPError
	if errors.As(err, &httpErr) {
		msg, code := parseErrorBody(httpErr.Body)
		cause := errors.New(msg)
		switch {
		case httpErr.Code == http.StatusNotFound,
			code == "global/object-does-not-exist":
			cause = fmt.Errorf("%s: %w", msg, provisioners.ErrNotFound)
		case httpErr.Code == http.StatusUnauthorized,
			httpErr.Code == http.StatusForbidden:
			cause = fmt.Errorf("auth failed: %s", msg)
		}
		return provisioners.NewProviderError(provisioners.ProviderLambdaLabs, op, cause, httpErr.Code)
	}
	return provisioners.NewProviderError(provisioners.ProviderLambdaLabs, op, err, 0)
}

// errorBody wraps Lambda Labs' error envelope. Verified via probe.
type errorBody struct {
	Error struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		Suggestion string `json:"suggestion"`
	} `json:"error"`
}

// parseErrorBody extracts (message, code) from Lambda's error
// envelope. Prefers the message for human-readable output;
// returns the code for the wrapErr's NotFound mapping. Falls back
// to the raw body for unrecognized shapes (e.g., 5xx HTML from
// the edge).
func parseErrorBody(body []byte) (msg, code string) {
	var env errorBody
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		return env.Error.Message, env.Error.Code
	}
	return strings.TrimSpace(string(body)), ""
}
