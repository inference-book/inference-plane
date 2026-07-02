package router

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/router/policy"
)

// TestRouter_PrefixAffinity_HeaderRoutesStickily is the end-to-end proof
// of the X-IPlane-Session header -> context -> PrefixAffinity policy ->
// sticky-forwarding wiring. Three engines back one deployment; requests
// bearing the same session land on the same engine every time, and three
// distinct sessions spread across all three engines. Fails under
// round-robin (the same-session assertion breaks), so it genuinely
// exercises affinity, not the default path.
func TestRouter_PrefixAffinity_HeaderRoutesStickily(t *testing.T) {
	const replicas = 3
	engines := make([]*httptest.Server, replicas)
	ids := make([]string, replicas)
	endpoints := make([]string, replicas)
	for i := range replicas {
		marker := fmt.Sprintf("engine-%d", i)
		engines[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"`+marker+`"}}]}`)
		}))
		defer engines[i].Close()
		ids[i] = fmt.Sprintf("r%d", i)
		endpoints[i] = engines[i].URL
	}

	r := New(&fakeDeploymentClient{describe: func(_ *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
		return &provisionerv1.DescribeDeploymentResponse{
			Deployment: &provisionerv1.Deployment{
				Id:              "aff",
				State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				InstanceIds:     ids,
				EngineEndpoints: endpoints,
				EngineEndpoint:  endpoints[0], // satisfy the singular-endpoint readiness gate
			},
		}, nil
	}}, nil, WithRoutingPolicy(policy.NewPrefixAffinity(0)))

	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	hit := func(session string) string {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/aff/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(SessionHeader, session)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
		}
		for i := range replicas {
			if strings.Contains(string(body), fmt.Sprintf("engine-%d", i)) {
				return fmt.Sprintf("engine-%d", i)
			}
		}
		t.Fatalf("response identified no engine; body=%s", body)
		return ""
	}

	// One session sticks across turns.
	sessionA := hit("sess-A")
	for turn := range 4 {
		if got := hit("sess-A"); got != sessionA {
			t.Errorf("sess-A turn %d hit %s, want sticky %s", turn+2, got, sessionA)
		}
	}

	// Three distinct sessions place via round-robin fallback, spreading
	// across all three replicas -- stickiness isn't "everything to one".
	spread := map[string]struct{}{}
	for _, s := range []string{"sess-A", "sess-B", "sess-C"} {
		spread[hit(s)] = struct{}{}
	}
	if len(spread) != replicas {
		t.Errorf("3 sessions covered %d engines, want %d (%v)", len(spread), replicas, spread)
	}
}
