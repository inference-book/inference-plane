package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/engineagent"
)

// newMockRegisterAgent builds the agent mock-engine runs so the registration
// path is exercisable without a GPU.
//
// It is the same agent `iplane engine-agent` runs on a rented box, wired
// differently in exactly two places:
//
//   - the span is fabricated rather than read, so a multi-node member renders
//     without renting a pool (see engineagent.WithSpan on why that seam is
//     the right one and why it currently has one caller)
//   - readiness is a timer rather than a health probe, so the ASSEMBLING
//     window is demonstrable on demand
//
// Everything else, the lease, the cadence, the non-fatal failure handling,
// is shared code rather than a second implementation that drifts.
func newMockRegisterAgent(serviceURL, engineID, model, endpoint string, nodes, cards int, assemble, degradeAfter time.Duration, log *slog.Logger) (*engineagent.Agent, error) {
	span := make([]*provisionerv1.EngineNode, 0, nodes)
	perNode := max(cards/max(nodes, 1), 1)
	for i := range nodes {
		span = append(span, &provisionerv1.EngineNode{
			HostId:    fmt.Sprintf("%s-node-%d", engineID, i),
			Provider:  "mock",
			NodeIndex: int32(i),
			GpuCount:  int32(perNode),
		})
	}

	// The engine reports ASSEMBLING first and flips to SERVING once the
	// delay has passed. That models the real thing rather than decorating
	// the demo: workers have to find each other and settle ranks before the
	// endpoint serves a token, and no control-plane probe can see that
	// interval because there is no endpoint yet to ask.
	started := time.Now()
	ready := func(context.Context) engineagent.Readiness {
		if time.Since(started) >= assemble {
			return engineagent.Ready
		}
		return engineagent.NotReady
	}

	// Stands in for issue 213's link sensor. The real one reads NVLink
	// state and error counters off the cards; this one reads a clock. What
	// matters for the demo is the same either way: the engine keeps serving
	// correct tokens the whole time, and the only thing that changes is a
	// reading nothing else in the system is watching.
	impaired := func(context.Context) bool {
		return degradeAfter > 0 && time.Since(started) >= degradeAfter
	}

	return engineagent.New(
		provisionerv1connect.NewEngineRegistryServiceClient(http.DefaultClient, serviceURL),
		engineagent.Identity{
			EngineID: engineID,
			Model:    model,
			Endpoint: endpoint,
			Provider: "mock",
		},
		engineagent.WithSpan(span),
		engineagent.WithProbe(engineagent.AnyDegraded(ready, impaired)),
		engineagent.WithLogger(log),
	)
}
