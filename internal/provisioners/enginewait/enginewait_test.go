package enginewait

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inference-book/inference-plane/internal/provisioners"
)

// A three-rung ladder standing in for a provider's own.
const (
	scheduling = "test:scheduling"
	imagePull  = "test:image-pull"
	engineInit = "engine:init"
)

func ordinal(p string) int {
	switch p {
	case scheduling:
		return 1
	case imagePull:
		return 2
	case engineInit:
		return 3
	}
	return 0
}

func describeRung(p string) string { return "at " + p }

type harness struct {
	mu       sync.Mutex
	observes int
	probes   int
	phases   []string
}

func (h *harness) cfg(obs []Observation, probeOK func(n int) bool) Config {
	return Config{
		Timeout:  2 * time.Second,
		Interval: 5 * time.Millisecond,
		Ladder:   Ladder{Ordinal: ordinal, Description: describeRung},
		Observe: func(context.Context, string) Observation {
			h.mu.Lock()
			defer h.mu.Unlock()
			i := h.observes
			h.observes++
			if i >= len(obs) {
				i = len(obs) - 1
			}
			return obs[i]
		},
		Probe: func(context.Context, string) (bool, string) {
			h.mu.Lock()
			n := h.probes
			h.probes++
			h.mu.Unlock()
			return probeOK(n), "not yet"
		},
		Emit: func(u provisioners.DeployStateUpdate) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.phases = append(h.phases, u.Phase)
		},
	}
}

func never(int) bool { return false }

func TestAFatalObservationStopsImmediately(t *testing.T) {
	// The reason this package exists. A provider that can tell a
	// container will never run stops the wait instead of billing the
	// rest of the timeout, and hoisting the loop is what makes that
	// reachable from every provider rather than the one it was written
	// in (#268).
	h := &harness{}
	c := h.cfg([]Observation{
		{Phase: scheduling},
		{Fatal: errors.New("contract 42 will not start: OCI runtime create failed")},
	}, never)

	start := time.Now()
	_, err := Wait(context.Background(), c)
	if err == nil {
		t.Fatal("want the fatal observation to end the wait")
	}
	if !strings.Contains(err.Error(), "will not start") {
		t.Errorf("the provider's own words were lost: %v", err)
	}
	// The point is not that it errored; it is that it errored early.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %s to give up on a host that had already failed", elapsed)
	}
}

func TestTheLadderOnlyClimbs(t *testing.T) {
	// A phase change opens and closes a bucket in the deploy phase
	// histogram, so a rung that rewound on a flaky read would record two
	// short image-pulls where there was one long one.
	h := &harness{}
	c := h.cfg([]Observation{
		{Phase: imagePull},
		{Phase: scheduling}, // a lagging read
		{Phase: ""},         // an unparseable one
		{Phase: engineInit},
	}, never)
	c.Timeout = 60 * time.Millisecond

	_, _ = Wait(context.Background(), c)

	h.mu.Lock()
	defer h.mu.Unlock()
	low := 99
	for _, p := range h.phases {
		if o := ordinal(p); o < low {
			low = o
		}
	}
	if low < ordinal(imagePull) {
		t.Errorf("the ladder fell below image-pull after reaching it: %v", h.phases)
	}
}

func TestAKnownEndpointIsProbedBeforeTheProviderIsAsked(t *testing.T) {
	// A provider whose endpoint is fixed at rent time should not pay for
	// a status read on a tick where the engine is already answering,
	// which is every tick of a warm redeploy. Seeding Endpoint is what
	// expresses that difference.
	h := &harness{}
	c := h.cfg([]Observation{{Phase: scheduling}}, func(int) bool { return true })
	c.Endpoint = "http://engine:8000"

	got, err := Wait(context.Background(), c)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got != "http://engine:8000" {
		t.Errorf("endpoint = %q", got)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.observes != 0 {
		t.Errorf("asked the provider %d time(s) for a pod that was already serving", h.observes)
	}
}

func TestADiscoveredEndpointIsProbedOnTheSameTick(t *testing.T) {
	// A provider that has to discover its endpoint should not then wait
	// a whole interval before trying it. The engine may already be up by
	// the time the address appears.
	h := &harness{}
	c := h.cfg([]Observation{{Endpoint: "http://found:8000", Phase: engineInit}},
		func(int) bool { return true })
	c.Interval = 5 * time.Second // would dominate if the probe waited a tick

	start := time.Now()
	got, err := Wait(context.Background(), c)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got != "http://found:8000" {
		t.Errorf("endpoint = %q", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited %s after discovering the endpoint before probing it", elapsed)
	}
}

func TestTheProvidersOwnWordsBeatTheProbes(t *testing.T) {
	// A host saying "Retrying" is telling the operator something the
	// refusal cannot. When both are available the host wins.
	h := &harness{}
	c := h.cfg([]Observation{{Endpoint: "http://e:8000", Phase: imagePull, Detail: "Retrying in 5 seconds"}}, never)
	c.Timeout = 30 * time.Millisecond

	var msgs []string
	base := c.Emit
	c.Emit = func(u provisioners.DeployStateUpdate) {
		msgs = append(msgs, u.ProgressMessage)
		base(u)
	}
	_, _ = Wait(context.Background(), c)

	if len(msgs) == 0 {
		t.Fatal("no progress emitted")
	}
	if !strings.Contains(msgs[0], "Retrying") {
		t.Errorf("host detail lost: %q", msgs[0])
	}
	if strings.Contains(msgs[0], "not yet") {
		t.Errorf("the probe's answer displaced the host's: %q", msgs[0])
	}
}

func TestATimeoutAndACancellationReadDifferently(t *testing.T) {
	// One means the box took too long and the other means the operator
	// stopped waiting. An operator reading a failed deploy needs to know
	// which, and they lead to different next steps.
	h := &harness{}
	c := h.cfg([]Observation{{Phase: imagePull}}, never)
	c.Timeout = 30 * time.Millisecond
	if _, err := Wait(context.Background(), c); err == nil || !strings.Contains(err.Error(), "within 30ms") {
		t.Errorf("timeout error does not name the budget: %v", err)
	}

	h2 := &harness{}
	c2 := h2.cfg([]Observation{{Phase: imagePull}}, never)
	c2.Timeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err := Wait(ctx, c2)
	if err == nil || !strings.Contains(err.Error(), "caller stopped waiting") {
		t.Errorf("cancellation reads as a timeout: %v", err)
	}
}

func TestTheFailureNamesTheRungItDiedOn(t *testing.T) {
	// A long timeout that says only "did not answer" leaves an operator
	// unable to tell a host that never finished pulling from an engine
	// that could not load the model.
	h := &harness{}
	c := h.cfg([]Observation{{Phase: imagePull}}, never)
	c.Timeout = 30 * time.Millisecond

	_, err := Wait(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), imagePull) {
		t.Errorf("failure does not name the rung: %v", err)
	}
}
