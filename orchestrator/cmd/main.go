package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"synod/orchestrator/internal"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Validate critical env vars
	for _, v := range []string{"GCP_PROJECT_ID", "GEMINI_API_KEY", "ORCHESTRATOR_BASE_URL", "SANDBOX_URL"} {
		if os.Getenv(v) == "" {
			slog.Error("missing required environment variable", "var", v)
			os.Exit(1)
		}
	}

	// Firestore
	store, err := internal.NewStore(ctx, os.Getenv("GCP_PROJECT_ID"))
	if err != nil {
		slog.Error("store init failed", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	// Gemini LLM client
	gemini, err := internal.NewGeminiClient(ctx, os.Getenv("GEMINI_API_KEY"), os.Getenv("LLM_MODEL"))
	if err != nil {
		slog.Error("gemini init failed", "error", err)
		os.Exit(1)
	}

	// Cloud Tasks dispatcher
	dispatcher, err := internal.NewDispatcher(ctx)
	if err != nil {
		slog.Error("dispatcher init failed", "error", err)
		os.Exit(1)
	}
	defer dispatcher.Close()

	selfURL := strings.TrimRight(os.Getenv("ORCHESTRATOR_BASE_URL"), "/")
	sandboxURL := strings.TrimRight(os.Getenv("SANDBOX_URL"), "/")

	// Initialize agents
	dataAgent := internal.NewDataAgent(gemini, store,
		os.Getenv("GOOGLE_CSE_API_KEY"),
		os.Getenv("GOOGLE_CSE_CX"),
		os.Getenv("SEC_EDGAR_USER_AGENT"),
	)
	analystAgent := internal.NewAnalystAgent(gemini, store, sandboxURL)
	reportAgent := internal.NewReportAgent(gemini, store)

	// Orchestration agent — holds all sub-agents
	orchestrator := internal.NewOrchestratorAgent(
		gemini, store, dispatcher,
		dataAgent, analystAgent, reportAgent,
		selfURL,
	)

	// HTTP server
	server := internal.NewServer(orchestrator, store, dispatcher, selfURL)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      server,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh
		slog.Info("shutting down", "signal", sig)
		cancel()
		shutdownCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown error", "error", err)
		}
	}()

	slog.Info("starting server", "port", port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
	slog.Info("server stopped")
}
