package enginewait

import (
	"context"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// The wait must carry the provider's shell address outward as soon as it
// exists. Nothing else revisits the instance record after the rent call, so
// an address the wait sees and drops is an address nobody ever records, and
// the box reads as having no SSH endpoint while being perfectly reachable.
func TestWaitReportsTheSSHTargetItObserves(t *testing.T) {
	want := &provisionerv1.SshTarget{Host: "ssh2.vast.ai", Port: 20996, User: "root"}
	var got []*provisionerv1.SshTarget

	ticks := 0
	_, err := Wait(context.Background(), Config{
		Timeout:  200 * time.Millisecond,
		Interval: time.Millisecond,
		Ladder: Ladder{
			Ordinal:     func(string) int { return 0 },
			Description: func(p string) string { return p },
		},
		Observe: func(context.Context, string) Observation {
			ticks++
			// Absent on the first tick, assigned afterwards, which is the
			// real shape: Vast populates ssh_host after the contract exists.
			if ticks == 1 {
				return Observation{Phase: "starting"}
			}
			return Observation{Phase: "starting", SSH: want}
		},
		Probe: func(context.Context, string) (bool, string) { return false, "not yet" },
		Emit:  func(u provisioners.DeployStateUpdate) { got = append(got, u.SSH) },
	})
	if err == nil {
		t.Fatal("expected the wait to time out")
	}
	if len(got) < 2 {
		t.Fatalf("only %d updates emitted; cannot show the transition", len(got))
	}
	if got[0] != nil {
		t.Errorf("first update carried %+v, want nil before the address exists", got[0])
	}
	if last := got[len(got)-1]; last.GetHost() != want.Host || last.GetPort() != want.Port {
		t.Errorf("last update carried %v, want %s:%d", last, want.Host, want.Port)
	}
}
