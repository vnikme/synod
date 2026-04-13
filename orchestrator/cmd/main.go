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
	"synod/orchestrator/ui"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Validate critical env vars
	for _, v := range []string{
		"GCP_PROJECT_ID", "GEMINI_API_KEY", "ORCHESTRATOR_BASE_URL", "SANDBOX_URL",
		"CLOUD_TASKS_LOCATION", "CLOUD_TASKS_QUEUE", "SERVICE_ACCOUNT_EMAIL",
	} {
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

	selfURL := strings.TrimRight(os.Getenv("ORCHESTRATOR_BASE_URL"), "/")
	sandboxURL := strings.TrimRight(os.Getenv("SANDBOX_URL"), "/")

	// Gemini LLM client
	gemini, err := internal.NewGeminiClient(ctx, os.Getenv("GEMINI_API_KEY"), os.Getenv("LLM_MODEL"))
	if err != nil {
		slog.Error("gemini init failed", "error", err)
		os.Exit(1)
	}

	// Cloud Tasks dispatcher
	dispatcher, err := internal.NewDispatcher(ctx, selfURL)
	if err != nil {
		slog.Error("dispatcher init failed", "error", err)
		os.Exit(1)
	}
	defer dispatcher.Close()

	// Initialize agents
	dataAgent := internal.NewDataAgent(gemini, store,
		os.Getenv("GOOGLE_CSE_API_KEY"),
		os.Getenv("GOOGLE_CSE_CX"),
		os.Getenv("SEC_EDGAR_USER_AGENT"),
	)
	analystAgent, err := internal.NewAnalystAgent(ctx, gemini, store, sandboxURL)
	if err != nil {
		slog.Error("analyst agent init failed", "error", err)
		os.Exit(1)
	}
	reportAgent := internal.NewReportAgent(gemini, store)

	// Orchestration agent — no longer holds sub-agents (async dispatch via Cloud Tasks)
	orchestrator := internal.NewOrchestratorAgent(
		gemini, store, dispatcher,
		selfURL,
	)

	// HTTP server — receives agent webhook callbacks
	internalAuth := internal.OIDCAuthMiddleware(selfURL, os.Getenv("SERVICE_ACCOUNT_EMAIL"))
	server := internal.NewServer(orchestrator, dataAgent, analystAgent, reportAgent, store, dispatcher, selfURL, internalAuth, ui.StaticFS)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      server,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 300 * time.Second,
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
