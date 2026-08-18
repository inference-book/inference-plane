package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/inference-book/inference-plane/internal/backends"
	"github.com/spf13/cobra"
)

var (
	mockEnginePort         int
	mockEngineLatency      time.Duration
	mockEngineRegister     string
	mockEngineID           string
	mockEngineModel        string
	mockEngineNodes        int
	mockEngineCards        int
	mockEngineAssemble     time.Duration
	mockEngineDegradeAfter time.Duration
	mockEngineLinks        int
	mockEngineTokenGap     time.Duration
	mockEngineKVBudget     int
)

// mockEngineCmd runs a standalone OpenAI-compatible mock engine. It is
// dev/CI scaffolding, not an operator surface (hidden), used with the
// external provider to stand up a GPU-free multi-replica deployment
// locally: run N of these on different ports, then
// `iplane deployment deploy <id> --provider external --engine-endpoints
// http://127.0.0.1:9001 http://127.0.0.1:9002`.
var mockEngineCmd = &cobra.Command{
	Use:    "mock-engine",
	Short:  "Run a standalone OpenAI-compatible mock engine (dev/CI harness)",
	Hidden: true,
	Long: `Serves the OpenAI-compatible surface (/v1/chat/completions,
/v1/completions, /health, /v1/models) backed by the in-process mock
engine. Pair with the external provider to build a routable, GPU-free
multi-replica deployment for local demos.

Each request's X-IPlane-Session header is logged, so you can watch the
prefix-affinity router pin a session to one engine.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runMockEngine(cmd.Context(), mockEnginePort)
	},
}

func init() {
	rootCmd.AddCommand(mockEngineCmd)
	mockEngineCmd.Flags().IntVar(&mockEnginePort, "port", 9001, "port to listen on (127.0.0.1)")
	mockEngineCmd.Flags().DurationVar(&mockEngineLatency, "latency", 0,
		"fixed per-request latency; 0 keeps the realistic bimodal-with-tail default. Routing demos set this low (e.g. 3ms) so runs finish fast.")
	mockEngineCmd.Flags().IntVar(&mockEngineKVBudget, "kv-budget-tokens", 0,
		"cap the tokens held across all in-flight sequences, so how many fit depends on how long they are. 0 (default) admits everything, which is what the routing demos expect")
	mockEngineCmd.Flags().DurationVar(&mockEngineTokenGap, "token-latency", 0,
		"pause between streamed content frames, so inter-token latency is measurable without a GPU. 0 (default) emits the whole reply in one burst, which is what every existing demo expects")
	mockEngineCmd.Flags().StringVar(&mockEngineRegister, "register", "",
		"control-plane URL to register with (e.g. http://127.0.0.1:8080); empty disables registration")
	mockEngineCmd.Flags().StringVar(&mockEngineID, "engine-id", "",
		"stable engine id used for registration; defaults to mock-engine-<port>")
	mockEngineCmd.Flags().StringVar(&mockEngineModel, "model", "mock-model",
		"model name reported in registrations")
	mockEngineCmd.Flags().IntVar(&mockEngineNodes, "span-nodes", 1,
		"nodes to report in the registered span (fabricated; lets a multi-node member render without renting one)")
	mockEngineCmd.Flags().IntVar(&mockEngineCards, "span-cards", 1,
		"total GPUs to report across the span")
	mockEngineCmd.Flags().DurationVar(&mockEngineAssemble, "assemble-delay", 0,
		"report ASSEMBLING for this long before flipping to SERVING; models the interval where workers exist but the group has not formed")
	mockEngineCmd.Flags().DurationVar(&mockEngineDegradeAfter, "degrade-after", 0,
		"after this long, report SERVING_DEGRADED while still answering requests normally, so the degraded-not-dead state is demonstrable without breaking real hardware. 0 disables")
	mockEngineCmd.Flags().IntVar(&mockEngineLinks, "links", 0,
		"simulate an NVLink board with this many links, so `iplane fleet status` renders a real LINKS reading. With --degrade-after, one link goes down and the reported state is derived from that reading rather than from a separate clock. 0 (default) models a board with no NVLink, which reports 'no reading' rather than 'zero links up'")
}

// newMockEngineMux builds the OpenAI-compatible handler set backed by the
// given mock backend. Extracted from runMockEngine so tests can exercise
// the handlers without binding a port. label tags log lines (the port in
// the server path).
func newMockEngineMux(be *backends.MockBackend, label string) *http.ServeMux {
	return newMockEngineMuxWithPacing(be, label, 0)
}

// newMockEngineMuxWithPacing is newMockEngineMux with a gap between
// streamed content frames. Separate constructor rather than a parameter
// on the original, so the existing callers keep reading as the thing they
// were and only the sweep harness has to know pacing exists.
func newMockEngineMuxWithPacing(be *backends.MockBackend, label string, tokenGap time.Duration) *http.ServeMux {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	chat := func(w http.ResponseWriter, r *http.Request) {
		var req backends.GenerateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		logger.Info("mock-engine request",
			"label", label,
			"session", r.Header.Get("X-IPlane-Session"),
			"messages", len(req.Messages),
			"model", req.Model)
		resp, err := be.Generate(r.Context(), req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if req.Stream {
			streamChatCompletion(w, &resp, tokenGap)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", chat)
	mux.HandleFunc("POST /v1/completions", chat)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"mock","object":"model"}]}`)
	})
	return mux
}

func runMockEngine(parent context.Context, port int) error {
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var opts []backends.MockOption
	if mockEngineLatency > 0 {
		opts = append(opts, backends.WithLatency(mockEngineLatency, mockEngineLatency))
	}
	if mockEngineKVBudget > 0 {
		opts = append(opts, backends.WithKVBudget(mockEngineKVBudget))
	}
	be := backends.NewMock(fmt.Sprintf("mock-engine:%d", port), opts...)
	mux := newMockEngineMuxWithPacing(be, fmt.Sprintf("%d", port), mockEngineTokenGap)

	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// Agent half of the control channel (#204). Opt-in: without --register
	// this is the same standalone mock engine the routing demos use.
	if mockEngineRegister != "" {
		id := mockEngineID
		if id == "" {
			id = fmt.Sprintf("mock-engine-%d", port)
		}
		agent, err := newMockRegisterAgent(
			mockEngineRegister, id, mockEngineModel,
			fmt.Sprintf("http://%s", addr),
			mockEngineNodes, mockEngineCards, mockEngineLinks, mockEngineAssemble, mockEngineDegradeAfter,
			slog.New(slog.NewTextHandler(os.Stderr, nil)),
		)
		if err != nil {
			return err
		}
		go agent.Run(ctx)
	}

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	fmt.Fprintf(os.Stderr, "iplane mock-engine listening on http://%s\n", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
