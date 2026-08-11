package vast

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/inference-book/inference-plane/internal/provisioners"
)

// The capability is opt-in by type assertion, so losing the method is a silent
// regression rather than a compile error at the call site.
var _ provisioners.FailureReporter = (*Provider)(nil)

func instanceServer(t *testing.T, body map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"instances": body})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTerminalFailureReportsTheHostsOwnWords(t *testing.T) {
	srv := instanceServer(t, map[string]any{
		"id": 1, "actual_status": "created", "cur_state": "stopped",
		"status_msg": msgBrokenCDI,
	})
	p, _ := newTestProvider(t, nil)
	p.client = NewClient("k", WithBaseURL(srv.URL))

	failed, reason := p.TerminalFailure(context.Background(), "1")

	if !failed {
		t.Fatal("a container that could not start was not reported as terminal")
	}
	if reason == "" {
		t.Error("no reason; the operator gets a failure with no evidence")
	}
}

func TestTerminalFailureIgnoresAHostStillPulling(t *testing.T) {
	srv := instanceServer(t, map[string]any{
		"id": 1, "actual_status": "loading", "cur_state": "stopped",
		"status_msg": "0d30c18b26f8: Retrying in 5 seconds",
	})
	p, _ := newTestProvider(t, nil)
	p.client = NewClient("k", WithBaseURL(srv.URL))

	if failed, reason := p.TerminalFailure(context.Background(), "1"); failed {
		t.Errorf("aborted a host that was still pulling: %s", reason)
	}
}

// A provider API that is briefly unreachable says nothing about the instance
// behind it. Vast's control API was observed going slow in bursts and
// recovering mid-deploy, so this must not read as a dead host.
func TestTerminalFailureSwallowsATransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	p, _ := newTestProvider(t, nil)
	p.client = NewClient("k", WithBaseURL(srv.URL))

	if failed, _ := p.TerminalFailure(context.Background(), "1"); failed {
		t.Error("a failing control API was reported as a dead instance")
	}
}

// Vast keys instances by an integer contract id. A record whose provider id
// was never stamped must not send garbage to the API.
func TestTerminalFailureIgnoresANonNumericID(t *testing.T) {
	p, _ := newTestProvider(t, nil)
	if failed, _ := p.TerminalFailure(context.Background(), "not-a-contract"); failed {
		t.Error("reported a failure for an id that cannot name a Vast instance")
	}
}
