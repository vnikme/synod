package internal

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// handleRoute error classification:
// - permanentError → return 200 to prevent Cloud Tasks retries
// - other errors   → return 500 so Cloud Tasks retries transient failures

type Server struct {
	orchestrator *OrchestratorAgent
	store        *Store
	dispatcher   *Dispatcher
	selfURL      string
	mux          *http.ServeMux
}

func NewServer(orchestrator *OrchestratorAgent, store *Store, dispatcher *Dispatcher, selfURL string) *Server {
	s := &Server{
		orchestrator: orchestrator,
		store:        store,
		dispatcher:   dispatcher,
		selfURL:      selfURL,
		mux:          http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("POST /api/v1/tasks", s.handleIngest)
	s.mux.HandleFunc("GET /api/v1/tasks/{jobID}", s.handleStatus)
	s.mux.HandleFunc("POST /internal/route", s.handleRoute)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req IngestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt is required"})
		return
	}

	// Session management
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = uuid.NewString()
	} else if _, err := uuid.Parse(sessionID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session_id must be a valid UUID"})
		return
	}
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		slog.Error("get session failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if sess == nil {
		sess = &Session{SessionID: sessionID, ChatHistory: []ChatMessage{}}
		if err := s.store.CreateSession(ctx, sess); err != nil {
			slog.Error("create session failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}
	if err := s.store.AppendChatHistory(ctx, sessionID, ChatMessage{Role: "user", Content: req.Prompt}); err != nil {
		slog.Error("append chat history failed", "error", err)
	}

	// Create job
	jobID := uuid.NewString()
	job := &Job{
		JobID:       jobID,
		SessionID:   sessionID,
		Status:      StatusQueued,
		ActiveAgent: AgentOrchestrator,
		Prompt:      req.Prompt,
	}
	if err := s.store.CreateJob(ctx, job); err != nil {
		slog.Error("create job failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Enqueue first orchestration step
	routeURL := s.selfURL + "/internal/route"
	if err := s.dispatcher.Enqueue(ctx, routeURL, jobID, sessionID); err != nil {
		slog.Error("enqueue route failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	slog.Info("task ingested", "job_id", jobID, "session_id", sessionID)
	writeJSON(w, http.StatusAccepted, IngestResponse{JobID: jobID, SessionID: sessionID})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	jobID := r.PathValue("jobID")
	sessionID := r.URL.Query().Get("session_id")
	if jobID == "" || sessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "job_id and session_id are required"})
		return
	}
	job, err := s.store.GetJob(ctx, jobID, sessionID)
	if err != nil {
		slog.Error("get job failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if job == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job not found"})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleRoute(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var payload TaskPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		slog.Error("invalid route payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	jobID := strings.TrimSpace(payload.JobID)
	sessionID := strings.TrimSpace(payload.SessionID)
	if jobID == "" || sessionID == "" {
		http.Error(w, "job_id and session_id are required", http.StatusBadRequest)
		return
	}

	slog.Info("route: received", "job_id", jobID)

	if err := s.orchestrator.Execute(ctx, jobID, sessionID); err != nil {
		if IsPermanentError(err) {
			// Non-retryable: ACK the task so Cloud Tasks stops retrying.
			slog.Warn("orchestrator permanent error (not retrying)", "job_id", jobID, "error", err)
			w.WriteHeader(http.StatusOK)
			return
		}
		slog.Error("orchestrator execution failed", "job_id", jobID, "error", err)
		// Return 500 so Cloud Tasks retries on transient errors.
		// Agent-level failures are handled internally (failJob marks FAILED and returns nil).
		http.Error(w, "orchestrator error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
