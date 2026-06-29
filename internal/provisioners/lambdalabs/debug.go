package lambdalabs

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
)

// debugTransport wraps an http.RoundTripper and prints the request
// and response (method, URL, body) to stderr. The Authorization
// header is never logged -- it carries the operator's API key.
// Bodies are buffered so the downstream caller still gets a normal
// io.ReadCloser.
//
// Mirrors runpod's and vast's debugTransport; toggled by
// IPLANE_LAMBDA_DEBUG.
type debugTransport struct {
	inner http.RoundTripper
	out   io.Writer
}

func newDebugTransport(inner http.RoundTripper) *debugTransport {
	if inner == nil {
		inner = http.DefaultTransport
	}
	return &debugTransport{inner: inner, out: os.Stderr}
}

func (t *debugTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var reqBody []byte
	if req.Body != nil {
		reqBody, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}
	fmt.Fprintf(t.out, "\n→ %s %s\n", req.Method, req.URL.String())
	if len(reqBody) > 0 {
		fmt.Fprintf(t.out, "  request:\n    %s\n", reqBody)
	}

	resp, err := t.inner.RoundTrip(req)
	if err != nil {
		fmt.Fprintf(t.out, "  error: %v\n", err)
		return nil, err
	}

	respBody, _ := io.ReadAll(resp.Body)
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	fmt.Fprintf(t.out, "← %d %s\n", resp.StatusCode, resp.Status)
	if len(respBody) > 0 {
		fmt.Fprintf(t.out, "  response:\n    %s\n", respBody)
	}
	return resp, nil
}
