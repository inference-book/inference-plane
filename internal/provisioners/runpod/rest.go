package runpod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/inference-book/inference-plane/internal/provisioners"
)

// Default endpoint and HTTP timeout for the RunPod REST API. The
// endpoint is overrideable via WithBaseURL (httptest in unit tests,
// staging in integration tests).
const (
	DefaultBaseURL     = "https://rest.runpod.io/v1"
	defaultHTTPTimeout = 30 * time.Second
)

// Client is a minimal HTTP client targeted at RunPod's REST endpoint.
// Not a general-purpose REST client -- it knows how to attach RunPod's
// bearer auth and wraps every failure in *provisioners.ProviderError
// so callers can errors.As to the cause and surface the original
// RunPod message (the design doc forbids normalizing provider errors
// into iplane-canonical codes).
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
}

// ClientOption configures the Client at construction time.
type ClientOption func(*Client)

// WithHTTPClient swaps in a custom *http.Client. Tests pass an
// httptest.Server's client; production keeps the default.
func WithHTTPClient(c *http.Client) ClientOption {
	return func(cl *Client) { cl.httpClient = c }
}

// WithBaseURL points the client at a different endpoint. Tests pass
// the httptest.Server URL; integration tests can point at staging.
// The supplied URL is used verbatim -- the caller is responsible for
// including the API version path segment if any.
func WithBaseURL(u string) ClientOption {
	return func(cl *Client) { cl.baseURL = strings.TrimRight(u, "/") }
}

// NewClient builds a RunPod REST client. apiKey is the operator's
// RUNPOD_API_KEY value, attached as a bearer token on every request.
func NewClient(apiKey string, opts ...ClientOption) *Client {
	c := &Client{
		httpClient: &http.Client{Timeout: defaultHTTPTimeout},
		baseURL:    DefaultBaseURL,
		apiKey:     apiKey,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// errorBody is the shape RunPod returns for 4xx/5xx responses. The
// `error` field is the actionable message; we preserve it verbatim
// in the wrapped ProviderError so the operator sees what RunPod said
// rather than a rewritten iplane phrasing.
type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// do is the underlying request helper. op is the iplane operation
// label ("spawn", "terminate", etc.) used for error wrapping. method
// is the HTTP verb; path is the API path AFTER the base URL (e.g.,
// "/pods"). body is JSON-encoded if non-nil; dest is decoded from the
// response body if non-nil. query is appended as a URL query string.
func (c *Client) do(ctx context.Context, op, method, path string, query url.Values, body, dest any) error {
	endpoint, err := url.JoinPath(c.baseURL, path)
	if err != nil {
		return provisioners.NewProviderError(provisioners.ProviderRunPod, op, fmt.Errorf("build url: %w", err), 0)
	}
	if len(query) > 0 {
		endpoint = endpoint + "?" + query.Encode()
	}

	var reqBody io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return provisioners.NewProviderError(provisioners.ProviderRunPod, op, fmt.Errorf("encode request body: %w", err), 0)
		}
		reqBody = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reqBody)
	if err != nil {
		return provisioners.NewProviderError(provisioners.ProviderRunPod, op, err, 0)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return provisioners.NewProviderError(provisioners.ProviderRunPod, op, err, 0)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return provisioners.NewProviderError(provisioners.ProviderRunPod, op, fmt.Errorf("read response: %w", err), resp.StatusCode)
	}

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return provisioners.NewProviderError(provisioners.ProviderRunPod, op,
			fmt.Errorf("%s: %w", parseErrorMessage(respBody), provisioners.ErrNotFound), resp.StatusCode)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return provisioners.NewProviderError(provisioners.ProviderRunPod, op,
			fmt.Errorf("auth failed: %s", parseErrorMessage(respBody)), resp.StatusCode)
	case resp.StatusCode >= 400:
		return provisioners.NewProviderError(provisioners.ProviderRunPod, op,
			errors.New(parseErrorMessage(respBody)), resp.StatusCode)
	}

	if dest != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, dest); err != nil {
			return provisioners.NewProviderError(provisioners.ProviderRunPod, op,
				fmt.Errorf("decode response: %w (body=%s)", err, truncate(respBody, 200)), resp.StatusCode)
		}
	}
	return nil
}

// parseErrorMessage extracts a useful message from RunPod's error
// response shape. RunPod is inconsistent across endpoints -- some
// return `{"error": "..."}`, some `{"message": "..."}`, some plain
// text. We try the common shapes, fall back to the raw body.
func parseErrorMessage(body []byte) string {
	var e errorBody
	if err := json.Unmarshal(body, &e); err == nil {
		if e.Error != "" {
			return e.Error
		}
		if e.Message != "" {
			return e.Message
		}
	}
	return truncate(body, 200)
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
