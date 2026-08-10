package cmd

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"
	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
)

// registerLoop is the agent half of the control channel (#204), running
// inside mock-engine so the registration path is exercisable without a GPU.
//
// It is deliberately the whole agent: register, sleep, register again. There
// is no reconnect logic, no backoff state machine, no stream to tend, because
// a lease has none to have. An agent that crashes and restarts just registers
// again, and the control plane reconciles by timestamp rather than by
// tracking a session. That simplicity is a large part of the argument for
// leasing over a held-open stream.
//
// A real agent for a real engine (#204b) reports what the engine tells it
// instead of the fabricated span here, but its loop is this loop.
type registerLoop struct {
	client   provisionerv1connect.EngineRegistryServiceClient
	engine   *provisionerv1.Engine
	assemble time.Duration
	log      *slog.Logger
}

// newRegisterLoop builds the agent. serviceURL is the control plane's public
// address; nodes and cards fabricate a span so a multi-node member is
// renderable without renting one.
func newRegisterLoop(serviceURL, engineID, model, endpoint string, nodes, cards int, assemble time.Duration, log *slog.Logger) *registerLoop {
	span := make([]*provisionerv1.EngineNode, 0, nodes)
	perNode := cards / max(nodes, 1)
	if perNode < 1 {
		perNode = 1
	}
	for i := range nodes {
		span = append(span, &provisionerv1.EngineNode{
			HostId:    engineID + "-node-" + itoa(i),
			Provider:  "mock",
			NodeIndex: int32(i),
			GpuCount:  int32(perNode),
		})
	}
	return &registerLoop{
		client: provisionerv1connect.NewEngineRegistryServiceClient(http.DefaultClient, serviceURL),
		engine: &provisionerv1.Engine{
			Id:       engineID,
			Model:    model,
			Endpoint: endpoint,
			Span:     span,
		},
		assemble: assemble,
		log:      log,
	}
}

// run registers until ctx is cancelled, renewing at the cadence the control
// plane asks for.
//
// The engine reports ASSEMBLING first and flips to SERVING once the assemble
// delay has passed. That models the real thing rather than decorating the
// demo: workers have to find each other and settle ranks before the endpoint
// serves a token, and a probe cannot see that interval because there is no
// endpoint yet to probe. Making it visible here is the point of the state.
//
// A failed registration logs and retries on the next tick. It must not be
// fatal: the control plane being briefly unreachable is not a reason for a
// serving engine to stop serving, and the lease already encodes what happens
// if the outage outlasts it.
func (r *registerLoop) run(ctx context.Context) {
	started := time.Now()
	interval := 5 * time.Second

	for {
		r.engine.State = provisionerv1.EngineState_ENGINE_STATE_ASSEMBLING
		if time.Since(started) >= r.assemble {
			r.engine.State = provisionerv1.EngineState_ENGINE_STATE_SERVING
		}

		resp, err := r.client.RegisterEngine(ctx, connect.NewRequest(
			&provisionerv1.RegisterEngineRequest{Engine: r.engine}))
		switch {
		case err != nil:
			r.log.Warn("engine registration failed", "engine", r.engine.GetId(), "err", err)
		default:
			// The control plane owns the cadence, so detection latency is
			// tunable in one place rather than per agent. Renew at a third of
			// the lease, leaving two chances before expiry.
			if lease := resp.Msg.GetLeaseSeconds(); lease > 0 {
				interval = time.Duration(lease) * time.Second / 3
			}
			r.log.Info("engine registered",
				"engine", r.engine.GetId(),
				"state", r.engine.GetState().String(),
				"lease_seconds", resp.Msg.GetLeaseSeconds())
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// itoa avoids pulling strconv in for one call site.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
