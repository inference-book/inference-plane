// Command controlplane is the v0.1 entrypoint.
//
// Loads config from YAML + environment, wires up the OpenTelemetry SDK,
// constructs the configured Backend, and starts the HTTP API server.
//
// See the book chapter "Building Control Plane v0.1" for the design
// rationale behind each abstraction.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	skhttp "github.com/panyam/servicekit/http"

	"github.com/inference-book/inference-plane/internal/config"
	"github.com/inference-book/inference-plane/internal/server"
	"github.com/inference-book/inference-plane/internal/telemetry"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(os.Getenv("CP_CONFIG_PATH"))
	if err != nil {
		logger.Error("config load failed", "err", err)
		os.Exit(1)
	}

	shutdownTel, err := telemetry.Init(context.Background(), cfg.Telemetry)
	if err != nil {
		logger.Error("telemetry init failed", "err", err)
		os.Exit(1)
	}

	srv, err := server.New(cfg, logger)
	if err != nil {
		logger.Error("server build failed", "err", err)
		os.Exit(1)
	}

	logger.Info("control plane listening", "addr", cfg.Server.Addr)

	// Graceful shutdown lifecycle (signals -> pre-drain callbacks -> drain).
	// Telemetry flush runs as an OnShutdown callback so traces and metrics
	// have a chance to export before the process exits.
	err = skhttp.ListenAndServeGraceful(srv.HTTP(),
		skhttp.WithDrainTimeout(time.Duration(cfg.Server.ShutdownSec)*time.Second),
		skhttp.WithOnShutdown(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdownTel(ctx); err != nil {
				logger.Error("telemetry shutdown failed", "err", err)
			}
		}),
	)
	if err != nil {
		logger.Error("server exited with error", "err", err)
		os.Exit(1)
	}
	logger.Info("control plane stopped")
}
