package vast

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// logTailLines is how much of the engine's output a failure carries.
//
// Enough to hold a Python traceback and the lines above it, which is what
// an engine that refused to start actually prints, and short enough to sit
// inside a failure_reason an operator reads in a terminal.
const logTailLines = 60

// instanceLogs returns what the container has printed, or "" when Vast
// cannot say.
//
// Two steps, because Vast does not serve logs inline. A PUT asks it to
// upload the container's output to S3 and hands back a URL; the object
// appears a moment later, so the fetch polls through the 403 that means
// "not written yet" rather than treating it as a refusal. Measured on a
// live instance: 403 on the first attempt, 200 on the second.
//
// **Only useful before teardown.** Asking a destroyed instance returns
// "Error response from daemon: No such container", which is how this was
// discovered: the logs of the run that motivated it were fetched after the
// box was gone, and said nothing (#47).
//
// Errors are swallowed into "" on purpose. This runs on a path that has
// already failed, and a logging problem must never replace the real
// failure with its own.
func (p *Provider) instanceLogs(ctx context.Context, providerID string) string {
	url, err := p.requestLogUpload(ctx, providerID)
	if err != nil || url == "" {
		return ""
	}
	body := p.fetchUploadedLog(ctx, url)
	if body == "" {
		return ""
	}
	return tail(body, logTailLines)
}

// requestLogUpload asks Vast to publish the container's output and returns
// where it will appear.
func (p *Provider) requestLogUpload(ctx context.Context, providerID string) (string, error) {
	payload, err := json.Marshal(map[string]string{"tail": fmt.Sprint(logTailLines)})
	if err != nil {
		return "", err
	}
	req, err := p.client.newReq(http.MethodPut, pathRequestLogs+providerID+"/", nil, payload)
	if err != nil {
		return "", err
	}
	resp, err := p.client.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var out struct {
		Success   bool   `json:"success"`
		ResultURL string `json:"result_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if !out.Success {
		return "", fmt.Errorf("vast declined to publish logs for %s", providerID)
	}
	return out.ResultURL, nil
}

// fetchUploadedLog polls the published URL until the object exists.
//
// The 403 is Vast's S3 bucket saying "not written yet" rather than
// "forbidden", so a single attempt would report no logs for every instance
// whose upload had not landed in the few hundred milliseconds since the
// PUT.
func (p *Provider) fetchUploadedLog(ctx context.Context, url string) string {
	for attempt := range 6 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ""
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return ""
		}
		resp, err := p.client.do(req)
		if err != nil {
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && readErr == nil {
			return string(body)
		}
	}
	return ""
}

// tail keeps the last n non-empty lines, which is where an engine's
// failure always is.
func tail(body string, n int) string {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
