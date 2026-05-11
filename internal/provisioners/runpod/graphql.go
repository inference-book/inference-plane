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

// Default endpoint and HTTP timeout for the RunPod GraphQL API. The
// endpoint is overrideable via WithBaseURL (httptest in unit tests,
// staging in integration tests).
const (
	DefaultBaseURL    = "https://api.runpod.io"
	defaultHTTPTimeout = 30 * time.Second
)

// Client is a minimal GraphQL client targeted at RunPod's endpoint.
// Not a general-purpose GraphQL client -- it speaks the {data, errors}
// envelope, knows how to attach RunPod's bearer auth, and wraps every
// failure in *provisioners.ProviderError so callers can errors.As to
// the cause and surface the original RunPod message (the design doc
// forbids normalizing provider errors into iplane-canonical codes).
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
func WithBaseURL(u string) ClientOption {
	return func(cl *Client) { cl.baseURL = strings.TrimRight(u, "/") }
}

// NewClient builds a RunPod GraphQL client. apiKey is the operator's
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

// gqlRequest is the wire shape of a GraphQL request body.
type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// gqlResponse is the wire shape of a GraphQL response envelope.
// Either Data is populated or Errors is non-empty (or both for partial
// successes, but RunPod's mutations are atomic so partials do not
// happen for the operations we care about).
type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []gqlError      `json:"errors,omitempty"`
}

type gqlError struct {
	Message    string         `json:"message"`
	Extensions map[string]any `json:"extensions,omitempty"`
	Path       []any          `json:"path,omitempty"`
}

// do executes a GraphQL request. op is the iplane operation name
// ("spawn", "terminate", "describe", "list") used for error labeling.
// dest is unmarshalled from the `data` field on success; pass nil if
// you do not need the data payload.
//
// HTTP failures (network, 401, 5xx) and GraphQL errors both surface as
// *provisioners.ProviderError. The Cause field preserves the original
// message; HTTP holds the status code where one is available.
func (c *Client) do(ctx context.Context, op, query string, variables map[string]any, dest any) error {
	body, err := json.Marshal(gqlRequest{Query: query, Variables: variables})
	if err != nil {
		return provisioners.NewProviderError(provisioners.ProviderRunPod, op, fmt.Errorf("encode request: %w", err), 0)
	}

	endpoint, err := url.JoinPath(c.baseURL, "graphql")
	if err != nil {
		return provisioners.NewProviderError(provisioners.ProviderRunPod, op, fmt.Errorf("build url: %w", err), 0)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return provisioners.NewProviderError(provisioners.ProviderRunPod, op, err, 0)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

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
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return provisioners.NewProviderError(provisioners.ProviderRunPod, op,
			fmt.Errorf("auth failed: %s", truncate(respBody, 200)), resp.StatusCode)
	case resp.StatusCode >= 500:
		return provisioners.NewProviderError(provisioners.ProviderRunPod, op,
			fmt.Errorf("runpod server error: %s", truncate(respBody, 200)), resp.StatusCode)
	}

	var envelope gqlResponse
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return provisioners.NewProviderError(provisioners.ProviderRunPod, op,
			fmt.Errorf("decode response: %w (body=%s)", err, truncate(respBody, 200)), resp.StatusCode)
	}
	if len(envelope.Errors) > 0 {
		// Preserve the original RunPod message verbatim. Multiple errors
		// get joined; the first is usually the actionable one.
		first := envelope.Errors[0].Message
		cause := errors.New(first)
		if isNotFoundMessage(first) {
			cause = fmt.Errorf("%s: %w", first, provisioners.ErrNotFound)
		}
		return provisioners.NewProviderError(provisioners.ProviderRunPod, op, cause, resp.StatusCode)
	}

	if dest != nil && len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, dest); err != nil {
			return provisioners.NewProviderError(provisioners.ProviderRunPod, op,
				fmt.Errorf("decode data: %w", err), resp.StatusCode)
		}
	}
	return nil
}

// isNotFoundMessage spots the common RunPod phrasings that mean
// "no such pod" so the Provider can return ErrNotFound idempotently.
func isNotFoundMessage(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "not found") ||
		strings.Contains(lower, "no pod") ||
		strings.Contains(lower, "does not exist")
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
