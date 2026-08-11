package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/engineagent"
	"github.com/spf13/cobra"
)

var (
	engineAgentServiceURL string
	engineAgentID         string
	engineAgentDeployID   string
	engineAgentModel      string
	engineAgentEndpoint   string
	engineAgentHealthURL  string
	engineAgentProvider   string
	engineAgentHostID     string
	engineAgentNodeIndex  int
	engineAgentInterval   time.Duration
)

// engineAgentCmd runs the agent half of the control channel next to an
// engine on a rented box.
//
// It is an operator-visible surface rather than a hidden dev helper, because
// an operator running their own engine outside iplane's provisioning can
// point this at their control plane and have the member appear in the fleet
// view. That is the same property that makes Engine deliberately not a field
// on Deployment: an engine that announces itself need not have been rented
// by us.
var engineAgentCmd = &cobra.Command{
	Use:   "engine-agent",
	Short: "Register this engine with the control plane and keep its lease alive",
	Long: `Runs next to an engine and registers it with the control plane,
renewing on the cadence the control plane returns. Silence past the lease is
how the control plane learns the engine is gone.

The agent reports ASSEMBLING until the engine's local health endpoint
answers, then SERVING. That interval is invisible to the control plane's
own /health poller, because during assembly there is no reachable endpoint
for it to ask.

Identity is injected rather than discovered: a container cannot see its
provider's machine id from inside, so --host-id and friends come from the
deploy path (or the IPLANE_* env vars it stamps). Card count is read locally
from nvidia-smi.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runEngineAgent(cmd.Context())
	},
}

func init() {
	rootCmd.AddCommand(engineAgentCmd)
	f := engineAgentCmd.Flags()

	// IPLANE_SERVICE_URL by the same spelling the other remote-transport
	// verbs use, so an operator who has already exported it for
	// `iplane deployment` does not learn a second name here.
	f.StringVar(&engineAgentServiceURL, "service-url", os.Getenv(engineagent.EnvServiceURL),
		"control-plane public URL to register with (e.g. https://<tunnel>.trycloudflare.com)")
	f.StringVar(&engineAgentID, "engine-id", os.Getenv(engineagent.EnvEngineID),
		"stable engine id; registering twice with the same id is a renewal, not a second member")
	f.StringVar(&engineAgentDeployID, "deployment-id", os.Getenv(engineagent.EnvDeploymentID),
		"deployment this engine belongs to; empty for an engine the control plane did not provision")
	f.StringVar(&engineAgentModel, "model", os.Getenv(engineagent.EnvModel),
		"model this engine serves")
	f.StringVar(&engineAgentEndpoint, "endpoint", os.Getenv(engineagent.EnvEndpoint),
		"externally reachable OpenAI-compatible endpoint; injected, since a pod cannot know its own proxy URL")
	f.StringVar(&engineAgentHealthURL, "health-url", envOr(engineagent.EnvHealthURL, "http://127.0.0.1:8000/health"),
		"local engine health endpoint separating ASSEMBLING from SERVING")
	f.StringVar(&engineAgentProvider, "provider", os.Getenv(engineagent.EnvProvider),
		"provider that rented this box; qualifies --host-id so two providers' ids cannot collide")
	f.StringVar(&engineAgentHostID, "host-id", os.Getenv(engineagent.EnvHostID),
		"provider machine id for this node (RunPod machineId, Vast machine_id, EC2 instance id). Unreadable from inside the container, so it must be injected")
	f.IntVar(&engineAgentNodeIndex, "node-index", envInt(engineagent.EnvNodeIndex, 0),
		"rank of this node within the engine's group, 0-based")
	f.DurationVar(&engineAgentInterval, "interval", engineagent.DefaultInterval,
		"starting renewal cadence; the control plane's lease replaces it on the first successful registration")

	// Deliberately NOT MarkFlagRequired. Cobra's required check tests
	// whether a flag was *changed*, and on the path that matters most the
	// value arrives as an injected env var rather than an argv token, so
	// marking it required rejects every real deploy. Required-ness is about
	// the value, so it is checked as a value below.
}

func runEngineAgent(ctx context.Context) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if engineAgentServiceURL == "" {
		return fmt.Errorf("no control plane to register with: pass --service-url or set IPLANE_SERVICE_URL")
	}
	if engineAgentID == "" {
		return fmt.Errorf("no engine id: pass --engine-id or set %s", engineagent.EnvEngineID)
	}

	ident := engineagent.Identity{
		EngineID:     engineAgentID,
		DeploymentID: engineAgentDeployID,
		Model:        engineAgentModel,
		Endpoint:     engineAgentEndpoint,
		Provider:     engineAgentProvider,
		HostID:       engineAgentHostID,
		NodeIndex:    int32(engineAgentNodeIndex),
	}

	// Card count is the discovered half of the span. A box without the
	// NVIDIA tooling in its container reports zero and still registers: a
	// legible gap in the fleet view beats a missing member.
	cards := engineagent.CountCards(ctx)
	if cards == 0 {
		log.Warn("no GPUs discovered; registering with a zero-card span",
			"hint", "nvidia-smi absent or exposing no devices inside this container")
	}
	if ident.HostID == "" {
		log.Warn("no host id injected; this member cannot be attributed to a node",
			"env", engineagent.EnvHostID)
	}

	// Link health, and the reason it is composed with the health probe rather
	// than replacing it. HTTPProbe can only ever say serving or not, because a
	// health endpoint answers the question it was built for and a group with a
	// downed link is serving. AnyDegraded lets the link reading downgrade a
	// serving engine without being able to promote a broken one, so assembly
	// never reads as an impairment.
	//
	// On a board with no NVLink, or a container where the NVIDIA tooling is
	// not reachable, the sensor reports no reading and the engine keeps
	// reporting SERVING. Absence of a reading is not evidence of a fault.
	interconnect := engineagent.ReadInterconnect
	if ic := interconnect(ctx); !ic.GetAvailable() {
		log.Info("no interconnect reading available; link health will show as unknown",
			"hint", "expected on a PCIe-only box, or where nvidia-smi is not reachable in this container")
	}

	agent, err := engineagent.New(
		provisionerv1connect.NewEngineRegistryServiceClient(http.DefaultClient, engineAgentServiceURL),
		ident,
		engineagent.WithProbe(engineagent.AnyDegraded(
			engineagent.HTTPProbe(engineAgentHealthURL, 2*time.Second),
			engineagent.InterconnectProbe(interconnect),
		)),
		engineagent.WithInterconnect(interconnect),
		engineagent.WithCards(cards),
		engineagent.WithInterval(engineAgentInterval),
		engineagent.WithLogger(log),
	)
	if err != nil {
		return err
	}

	// SIGTERM is how a container shutdown arrives. Returning cleanly on it
	// stops the renewals, and the lease does the rest: the sweeper moves the
	// member to LOST once it runs out. There is deliberately no goodbye
	// message, because an agent that dies without one has to produce the
	// same outcome anyway.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("engine agent starting",
		"engine", ident.EngineID,
		"deployment", ident.DeploymentID,
		"provider", ident.Provider,
		"host", ident.HostID,
		"cards", cards,
		"control_plane", engineAgentServiceURL)

	agent.Run(ctx)
	return nil
}

// envOr returns the env var's value, or fallback when it is unset or empty.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envInt returns the env var parsed as an int, or fallback when unset or
// unparseable. A malformed value falls back rather than failing: node index
// is display ordering, and refusing to register over it would trade a
// cosmetic problem for an invisible member.
func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
