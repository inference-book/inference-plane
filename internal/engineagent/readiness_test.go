package engineagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// readinessForStatus runs HTTPProbe against a server returning status.
func readinessForStatus(t *testing.T, status int) Readiness {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	defer srv.Close()
	return HTTPProbe(srv.URL, time.Second)(t.Context())
}

// The state the chapter exists for. Before this seam it was unreachable:
// the enum and the renderer existed, and nothing could produce it.
func TestDegradedReadinessReportsServingDegraded(t *testing.T) {
	f := &fakeRegistrar{}
	a, _ := New(f, testIdentity(),
		WithProbe(func(context.Context) Readiness { return Degraded }),
		WithLogger(quietLogger()))

	a.registerOnce(t.Context())

	if got := f.sent()[0].GetState(); got != provisionerv1.EngineState_ENGINE_STATE_SERVING_DEGRADED {
		t.Errorf("state = %v, want SERVING_DEGRADED", got)
	}
}

func TestReadinessMapsOntoEngineState(t *testing.T) {
	for _, tc := range []struct {
		readiness Readiness
		want      provisionerv1.EngineState
	}{
		{NotReady, provisionerv1.EngineState_ENGINE_STATE_ASSEMBLING},
		{Ready, provisionerv1.EngineState_ENGINE_STATE_SERVING},
		{Degraded, provisionerv1.EngineState_ENGINE_STATE_SERVING_DEGRADED},
	} {
		t.Run(tc.readiness.String(), func(t *testing.T) {
			f := &fakeRegistrar{}
			a, _ := New(f, testIdentity(),
				WithProbe(func(context.Context) Readiness { return tc.readiness }),
				WithLogger(quietLogger()))

			a.registerOnce(t.Context())

			if got := f.sent()[0].GetState(); got != tc.want {
				t.Errorf("state = %v, want %v", got, tc.want)
			}
		})
	}
}

// Degraded is not latched. A link that recovers stops being reported without
// anything having to clear a flag, the same way a fallen-over engine stops
// reporting serving.
func TestDegradedClearsWhenTheImpairmentDoes(t *testing.T) {
	impaired := true
	f := &fakeRegistrar{}
	a, _ := New(f, testIdentity(),
		WithProbe(AnyDegraded(
			func(context.Context) Readiness { return Ready },
			func(context.Context) bool { return impaired },
		)),
		WithLogger(quietLogger()))

	a.registerOnce(t.Context())
	impaired = false
	a.registerOnce(t.Context())

	sent := f.sent()
	if sent[0].GetState() != provisionerv1.EngineState_ENGINE_STATE_SERVING_DEGRADED {
		t.Errorf("first state = %v, want SERVING_DEGRADED", sent[0].GetState())
	}
	if sent[1].GetState() != provisionerv1.EngineState_ENGINE_STATE_SERVING {
		t.Errorf("second state = %v, want SERVING once the impairment cleared", sent[1].GetState())
	}
}

// A group that has not formed is not a degraded group. Reporting the milder
// state during startup would make ordinary assembly look like a fault.
func TestNotReadyBeatsDegraded(t *testing.T) {
	probe := AnyDegraded(
		func(context.Context) Readiness { return NotReady },
		func(context.Context) bool { return true },
	)

	if got := probe(t.Context()); got != NotReady {
		t.Errorf("readiness = %v, want not-ready to win over degraded", got)
	}
}

// The impairment sensor is consulted only when the engine is serving, so a
// reading that is expensive or unavailable during startup is never taken then.
func TestImpairmentSensorNotConsultedBeforeReady(t *testing.T) {
	var consulted bool
	probe := AnyDegraded(
		func(context.Context) Readiness { return NotReady },
		func(context.Context) bool { consulted = true; return false },
	)

	probe(t.Context())

	if consulted {
		t.Error("impairment sensor was consulted while the group had not formed")
	}
}

// Issue 213's acceptance criterion: a pool with no sensor reports
// "not available" rather than a false healthy or a hard error. Absence of a
// reading must not read as degraded, or the state is useless.
func TestNoImpairmentSensorIsReadyNotDegraded(t *testing.T) {
	probe := AnyDegraded(func(context.Context) Readiness { return Ready }, nil)

	if got := probe(t.Context()); got != Ready {
		t.Errorf("readiness = %v, want ready when no impairment sensor exists", got)
	}
}

// A health endpoint answers whether the engine is serving, and a degraded
// group is serving. It has no vocabulary for impairment and must not invent
// one.
func TestHTTPProbeNeverReportsDegraded(t *testing.T) {
	for _, status := range []int{200, 204, 503, 500} {
		if got := readinessForStatus(t, status); got == Degraded {
			t.Errorf("HTTPProbe returned Degraded on %d", status)
		}
	}
}

func TestReadinessString(t *testing.T) {
	for _, tc := range []struct {
		r    Readiness
		want string
	}{
		{NotReady, "not-ready"},
		{Ready, "ready"},
		{Degraded, "degraded"},
	} {
		if got := tc.r.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}
