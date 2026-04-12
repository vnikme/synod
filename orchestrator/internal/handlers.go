package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"cloud.google.com/go/firestore"
	"github.com/google/uuid"
)

// handleRoute error classification:
// - permanentError → return 200 to prevent Cloud Tasks retries
// - other errors   → return 500 so Cloud Tasks retries transient failures

type Server struct {
	orchestrator *OrchestratorAgent
	data         *DataAgent
	analyst      *AnalystAgent
	report       *ReportAgent
	store        *Store
	dispatcher   *Dispatcher
	selfURL      string
	internalAuth func(http.Handler) http.Handler
	mux          *http.ServeMux
}

func NewServer(
	orchestrator *OrchestratorAgent,
	data *DataAgent, analyst *AnalystAgent, report *ReportAgent,
	store *Store, dispatcher *Dispatcher,
	selfURL string, internalAuth func(http.Handler) http.Handler,
) *Server {
	if internalAuth == nil {
		internalAuth = func(next http.Handler) http.Handler { return next }
	}
	s := &Server{
		orchestrator: orchestrator,
		data:         data,
		analyst:      analyst,
		report:       report,
		store:        store,
		dispatcher:   dispatcher,
		selfURL:      selfURL,
		internalAuth: internalAuth,
		mux:          http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("POST /api/v1/tasks", s.handleIngest)
	s.mux.HandleFunc("GET /api/v1/tasks/{jobID}", s.handleStatus)
	s.mux.Handle("POST /internal/route", s.internalAuth(http.HandlerFunc(s.handleRoute)))
	s.mux.Handle("POST /internal/agent/data", s.internalAuth(http.HandlerFunc(s.handleAgentData)))
	s.mux.Handle("POST /internal/agent/analyst", s.internalAuth(http.HandlerFunc(s.handleAgentAnalyst)))
	s.mux.Handle("POST /internal/agent/report", s.internalAuth(http.HandlerFunc(s.handleAgentReport)))
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

// parseAgentPayload extracts and validates job_id + session_id from the request body.
func parseAgentPayload(r *http.Request) (string, string, error) {
	var payload TaskPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		return "", "", fmt.Errorf("invalid payload: %w", err)
	}
	jobID := strings.TrimSpace(payload.JobID)
	sessionID := strings.TrimSpace(payload.SessionID)
	if jobID == "" || sessionID == "" {
		return "", "", fmt.Errorf("job_id and session_id are required")
	}
	return jobID, sessionID, nil
}

// handleAgentExec is the shared pattern for all agent webhook handlers:
// parse payload → read job → run agent → audit → handle error → callback to orchestrator.
func (s *Server) handleAgentExec(w http.ResponseWriter, r *http.Request, agent AgentType, run func(ctx context.Context, job *Job) (TokenUsage, error)) {
	ctx := r.Context()

	jobID, sessionID, err := parseAgentPayload(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	slog.Info("agent webhook: received", "agent", agent, "job_id", jobID)

	// Read current job state
	job, err := s.store.GetJob(ctx, jobID, sessionID)
	if err != nil {
		slog.Error("agent: get job failed", "agent", agent, "job_id", jobID, "error", err)
		http.Error(w, "get job failed", http.StatusInternalServerError)
		return
	}
	if job == nil {
		slog.Warn("agent: job not found (permanent)", "agent", agent, "job_id", jobID)
		w.WriteHeader(http.StatusOK) // ACK — don't retry
		return
	}

	// Guard: only execute if the job is in the expected state.
	// Prevents duplicate Cloud Tasks deliveries from re-running the agent.
	if job.Status != StatusQueued || job.ActiveAgent != agent {
		slog.Warn("agent: stale/duplicate delivery, skipping",
			"agent", agent, "job_id", jobID,
			"status", job.Status, "active_agent", job.ActiveAgent)
		w.WriteHeader(http.StatusOK) // ACK — don't retry
		return
	}

	// Mark in-progress
	if err := s.store.UpdateJob(ctx, jobID, sessionID, []firestore.Update{
		{Path: "status", Value: StatusInProgress},
	}); err != nil {
		slog.Error("agent: update status failed", "agent", agent, "error", err)
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}

	// Execute the agent
	usage, agentErr := run(ctx, job)

	// Audit
	detail := "success"
	if agentErr != nil {
		detail = agentErr.Error()
	}
	if auditErr := s.store.AppendAuditLog(ctx, jobID, sessionID, AuditEntry{
		Agent: agent, Action: "execute", Tokens: usage, Detail: detail,
	}); auditErr != nil {
		slog.Error("audit log failed", "job_id", jobID, "agent", agent, "error", auditErr)
	}

	// Handle agent failure — mark job failed, ACK task
	if agentErr != nil {
		slog.Error("agent failed", "agent", agent, "job_id", jobID, "error", agentErr)
		if failErr := s.store.UpdateJob(ctx, jobID, sessionID, []firestore.Update{
			{Path: "status", Value: StatusFailed},
			{Path: "final_result", Value: fmt.Sprintf("%s agent failed", agent)},
		}); failErr != nil {
			slog.Error("agent: failJob error", "agent", agent, "error", failErr)
		}
		w.WriteHeader(http.StatusOK) // ACK — agent failure is permanent
		return
	}

	// Re-read job to check if agent set a terminal state (e.g., NEEDS_CONTEXT, COMPLETED)
	job, err = s.store.GetJob(ctx, jobID, sessionID)
	if err != nil {
		slog.Error("agent: re-read job failed", "agent", agent, "error", err)
		http.Error(w, "re-read job failed", http.StatusInternalServerError)
		return
	}
	if job.Status == StatusCompleted || job.Status == StatusFailed || job.Status == StatusHITL {
		slog.Info("agent: terminal state", "agent", agent, "job_id", jobID, "status", job.Status)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Callback: enqueue orchestrator for next routing decision.
	// ACK the task regardless — the agent already succeeded and wrote to Firestore.
	// If enqueue fails, the orchestrator won't be notified, but the job state is
	// consistent (IN_PROGRESS). A future resume/retry mechanism can recover.
	if err := s.dispatcher.Enqueue(ctx, s.selfURL+"/internal/route", jobID, sessionID); err != nil {
		slog.Error("agent: enqueue callback failed (agent work is persisted)", "agent", agent, "error", err)
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleAgentData(w http.ResponseWriter, r *http.Request) {
	s.handleAgentExec(w, r, AgentData, func(ctx context.Context, job *Job) (TokenUsage, error) {
		return s.data.Execute(ctx, job, job.AgentInstructions, job.MissingQueries)
	})
}

func (s *Server) handleAgentAnalyst(w http.ResponseWriter, r *http.Request) {
	s.handleAgentExec(w, r, AgentAnalyst, func(ctx context.Context, job *Job) (TokenUsage, error) {
		return s.analyst.Execute(ctx, job, job.AgentInstructions)
	})
}

func (s *Server) handleAgentReport(w http.ResponseWriter, r *http.Request) {
	s.handleAgentExec(w, r, AgentReport, func(ctx context.Context, job *Job) (TokenUsage, error) {
		session, err := s.store.GetSession(ctx, job.SessionID)
		if err != nil {
			return TokenUsage{}, fmt.Errorf("get session: %w", err)
		}
		return s.report.Execute(ctx, job, session, job.AgentInstructions)
	})
}
