package vast

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/inference-book/inference-plane/internal/provisioners"
)

// contractServer serves /api/v0/instances/<id>/ and publishes the SSH
// endpoint only after the configured number of polls, which is what Vast
// does: the rent call returns before the machine answers.
type contractServer struct {
	polls        int
	publishAfter int
	host         string
	port         int
}

func (c *contractServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.polls++
	body := map[string]any{"id": 1, "actual_status": "running"}
	if c.polls > c.publishAfter {
		body["ssh_host"] = c.host
		body["ssh_port"] = c.port
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"instances": body})
}

func waiterProvider(t *testing.T, srv http.Handler, probe func(context.Context, string, int32) error) *Provider {
	t.Helper()
	p, _ := newTestProvider(t, srv)
	WithSSHReadyWait(2*time.Second, time.Millisecond)(p)
	WithSSHProbe(probe)(p)
	return p
}

// The whole point: return only once the endpoint exists, rather than
// immediately against a record that has none.
func TestWaitForSSHReadyPollsUntilPublished(t *testing.T) {
	cs := &contractServer{publishAfter: 3, host: "ssh8.vast.ai", port: 15526}
	p := waiterProvider(t, cs, func(context.Context, string, int32) error { return nil })

	target, err := p.WaitForSSHReady(t.Context(), "1")
	if err != nil {
		t.Fatalf("WaitForSSHReady: %v", err)
	}
	if target.GetHost() != "ssh8.vast.ai" || target.GetPort() != 15526 {
		t.Errorf("target = %v", target)
	}
	if cs.polls <= 3 {
		t.Errorf("returned after %d polls; it should have waited for publication", cs.polls)
	}
}

// Vast publishes the endpoint a few seconds before sshd answers on it, so
// returning on the record alone hands the caller an address that refuses the
// next connection.
func TestWaitForSSHReadyRequiresThePortToAnswer(t *testing.T) {
	cs := &contractServer{host: "ssh8.vast.ai", port: 15526}
	var probes int
	p := waiterProvider(t, cs, func(context.Context, string, int32) error {
		probes++
		if probes < 3 {
			return context.DeadlineExceeded
		}
		return nil
	})

	if _, err := p.WaitForSSHReady(t.Context(), "1"); err != nil {
		t.Fatalf("WaitForSSHReady: %v", err)
	}
	if probes < 3 {
		t.Errorf("probes = %d; it returned before the port answered", probes)
	}
}

func TestWaitForSSHReadyTimesOut(t *testing.T) {
	cs := &contractServer{publishAfter: 1 << 30} // never publishes
	p, _ := newTestProvider(t, cs)
	WithSSHReadyWait(30*time.Millisecond, time.Millisecond)(p)

	_, err := p.WaitForSSHReady(t.Context(), "1")
	if err == nil {
		t.Fatal("want a timeout error when the endpoint never appears")
	}
	if !strings.Contains(err.Error(), "ssh endpoint") {
		t.Errorf("error = %v, want it to name what was being waited for", err)
	}
}

func TestWaitForSSHReadyRejectsANonContractID(t *testing.T) {
	p, _ := newTestProvider(t, &contractServer{})

	if _, err := p.WaitForSSHReady(t.Context(), "not-a-number"); err == nil {
		t.Error("want an error for a provider id that is not a vast contract id")
	}
	if _, err := p.WaitForSSHReady(t.Context(), ""); err == nil {
		t.Error("want an error for an empty provider id")
	}
}

// The Service only waits when the adapter satisfies the capability, so
// signature drift would silently restore the race.
var _ provisioners.SSHReadyWaiter = (*Provider)(nil)
