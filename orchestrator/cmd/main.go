package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"synod/orchestrator/internal"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	projectID := os.Getenv("GCP_PROJECT_ID")
	if projectID == "" {
		slog.Error("GCP_PROJECT_ID is required")
		os.Exit(1)
	}

	store, err := internal.NewStore(ctx, projectID)
	if err != nil {
		slog.Error("failed to create store", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	dispatcher, err := internal.NewDispatcher(ctx)
	if err != nil {
		slog.Error("failed to create dispatcher", "error", err)
		os.Exit(1)
	}
	defer dispatcher.Close()

	planner, err := internal.NewPlanner(ctx)
	if err != nil {
		slog.Error("failed to create planner", "error", err)
		os.Exit(1)
	}

	srv := internal.NewServer(store, dispatcher, planner)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	httpServer := &http.Server{
		Addr:         ":" + port,
		Handler:      srv,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		slog.Info("shutting down")
		shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 10*time.Second)
		defer shutdownCancel()
		httpServer.Shutdown(shutdownCtx)
		cancel()
	}()

	slog.Info("orchestrator starting", "port", port)
	if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}
