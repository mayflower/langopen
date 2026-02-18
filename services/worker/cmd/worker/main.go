package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"langopen.dev/worker/internal/engine"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg := engine.Config{
		PostgresDSN: os.Getenv("POSTGRES_DSN"),
		RedisAddr:   os.Getenv("REDIS_ADDR"),
		QueueKey:    envOrDefault("RUN_WAKEUP_LIST", "langopen:runs:wakeup"),
	}

	worker, err := engine.New(cfg, logger)
	if err != nil {
		logger.Error("worker_init_failed", "error", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go runMetricsServer(logger, envOrDefault("WORKER_METRICS_ADDR", ":9091"))

	if err := worker.Run(ctx); err != nil {
		logger.Error("worker_failed", "error", err)
		os.Exit(1)
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func runMetricsServer(logger *slog.Logger, addr string) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	if err := http.ListenAndServe(addr, mux); err != nil && err != http.ErrServerClosed {
		logger.Error("worker_metrics_server_failed", "addr", addr, "error", err)
	}
}
