package vast

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
)

// debugTransport wraps an http.RoundTripper and prints the request and
// response (method, URL, body) to stderr. The Authorization header is
// never logged -- it carries the bearer token. Bodies are buffered so
// the downstream caller still gets a normal io.ReadCloser.
//
// Mirrors runpod's debugTransport; toggled by IPLANE_VAST_DEBUG.
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
	fmt.Fprintf(t.out, "\n→ %s %s\n", req.Method, req.URL)
	if len(reqBody) > 0 {
		fmt.Fprintf(t.out, "  request:\n%s\n", indent(reqBody))
	}

	resp, err := t.inner.RoundTrip(req)
	if err != nil {
		fmt.Fprintf(t.out, "← transport error: %v\n", err)
		return resp, err
	}

	var respBody []byte
	if resp.Body != nil {
		respBody, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
	}
	fmt.Fprintf(t.out, "← %d %s\n", resp.StatusCode, http.StatusText(resp.StatusCode))
	if len(respBody) > 0 {
		fmt.Fprintf(t.out, "  response:\n%s\n", indent(respBody))
	}
	return resp, nil
}

func indent(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	lines := bytes.Split(b, []byte("\n"))
	var out bytes.Buffer
	for i, line := range lines {
		if i == len(lines)-1 && len(line) == 0 {
			continue
		}
		out.WriteString("    ")
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.String()
}
