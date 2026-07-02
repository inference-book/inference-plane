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

	"github.com/inference-book/inference-plane/internal/backends"
	"github.com/spf13/cobra"
)

var mockEnginePort int

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
}

// newMockEngineMux builds the OpenAI-compatible handler set backed by the
// given mock backend. Extracted from runMockEngine so tests can exercise
// the handlers without binding a port. label tags log lines (the port in
// the server path).
func newMockEngineMux(be *backends.MockBackend, label string) *http.ServeMux {
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

	be := backends.NewMock(fmt.Sprintf("mock-engine:%d", port))
	mux := newMockEngineMux(be, fmt.Sprintf("%d", port))

	addr := fmt.Sprintf("127.0.0.1:%d", port)
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
