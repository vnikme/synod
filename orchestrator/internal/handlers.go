package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

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
	store        JobStore
	dispatcher   TaskDispatcher
	selfURL      string
	internalAuth func(http.Handler) http.Handler
	mux          *http.ServeMux
	staticFS     fs.FS
}

func NewServer(
	orchestrator *OrchestratorAgent,
	data *DataAgent, analyst *AnalystAgent, report *ReportAgent,
	store JobStore, dispatcher TaskDispatcher,
	selfURL string, internalAuth func(http.Handler) http.Handler,
	staticFS fs.FS,
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
		staticFS:     staticFS,
	}
	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("POST /api/v1/tasks", s.handleIngest)
	s.mux.HandleFunc("GET /api/v1/tasks/{jobID}", s.handleStatus)
	s.mux.HandleFunc("POST /api/v1/tasks/{jobID}/reply", s.handleReply)
	s.mux.Handle("POST /internal/route", s.internalAuth(http.HandlerFunc(s.handleRoute)))
	s.mux.Handle("POST /internal/agent/data", s.internalAuth(http.HandlerFunc(s.handleAgentData)))
	s.mux.Handle("POST /internal/agent/analyst", s.internalAuth(http.HandlerFunc(s.handleAgentAnalyst)))
	s.mux.Handle("POST /internal/agent/report", s.internalAuth(http.HandlerFunc(s.handleAgentReport)))

	// Serve embedded UI — SPA fallback: serve index.html for unmatched GET requests.
	if s.staticFS != nil {
		s.mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			// Don't serve SPA for API/internal paths — return 404 instead.
			if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/internal/") {
				http.NotFound(w, r)
				return
			}
			// Try to serve the exact file first; fall back to index.html for SPA routes.
			if r.URL.Path != "/" {
				f, err := s.staticFS.Open(strings.TrimPrefix(r.URL.Path, "/"))
				if err == nil {
					f.Close()
					http.FileServerFS(s.staticFS).ServeHTTP(w, r)
					return
				}
			}
			// Serve index.html for / and any unknown path (SPA catch-all).
			data, err := fs.ReadFile(s.staticFS, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if _, err := w.Write(data); err != nil {
				slog.Error("failed to write SPA fallback response", "path", r.URL.Path, "error", err)
			}
		})
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB

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

func (s *Server) handleReply(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	jobID := r.PathValue("jobID")
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB

	var req ReplyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if strings.TrimSpace(req.Message) == "" || strings.TrimSpace(req.SessionID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message and session_id are required"})
		return
	}
	sessionID := strings.TrimSpace(req.SessionID)

	// Verify job exists and is in HITL state before mutating session.
	job, err := s.store.GetJob(ctx, jobID, sessionID)
	if err != nil {
		slog.Error("reply: get job failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if job == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job not found"})
		return
	}
	if job.Status != StatusHITL {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "job is not awaiting user input"})
		return
	}

	// Atomic resume: CAS HITL → QUEUED+orchestrator, reset hop_count, clear final_result.
	// Only one concurrent reply succeeds; others get the appropriate error.
	_, result, err := s.store.ResumeHITLJob(ctx, jobID, sessionID)
	if err != nil {
		slog.Error("reply: resume job failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	switch result {
	case ResumeNotFound:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job not found"})
		return
	case ResumeNotHITL:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "job is not awaiting user input"})
		return
	}

	// Append user reply to chat history AFTER the CAS succeeds, so only one
	// racing reply ends up in chat_history.
	if err := s.store.AppendChatHistory(ctx, sessionID, ChatMessage{Role: "user", Content: req.Message}); err != nil {
		slog.Error("reply: append chat history failed", "error", err)
		// Roll back to HITL so the user can retry.
		if rbErr := s.store.UpdateJob(ctx, jobID, sessionID, []firestore.Update{
			{Path: "status", Value: StatusHITL},
			{Path: "active_agent", Value: AgentOrchestrator},
			{Path: "agent_instructions", Value: job.AgentInstructions},
		}); rbErr != nil {
			slog.Error("reply: rollback to HITL failed", "error", rbErr, "job_id", jobID)
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Enqueue orchestrator. On failure, roll back to HITL so the client can retry safely.
	if err := s.dispatcher.Enqueue(ctx, s.selfURL+"/internal/route", jobID, sessionID); err != nil {
		slog.Error("reply: enqueue route failed", "error", err)
		if rollbackErr := s.store.UpdateJob(ctx, jobID, sessionID, []firestore.Update{
			{Path: "status", Value: StatusHITL},
			{Path: "active_agent", Value: AgentOrchestrator},
			{Path: "agent_instructions", Value: job.AgentInstructions},
			{Path: "final_result", Value: "Your reply was received, but resuming the job failed. Please retry."},
		}); rollbackErr != nil {
			slog.Error("reply: rollback to HITL failed", "error", rollbackErr, "job_id", jobID)
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	slog.Info("reply: resumed job", "job_id", jobID, "session_id", sessionID)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "resumed"})
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

	slog.Info("route: received", "job_id", jobID, "session_id", sessionID)

	if err := s.orchestrator.Execute(ctx, jobID, sessionID); err != nil {
		if IsPermanentError(err) {
			// Non-retryable: ACK the task so Cloud Tasks stops retrying.
			slog.Warn("orchestrator permanent error (not retrying)", "job_id", jobID, "session_id", sessionID, "error", err)
			w.WriteHeader(http.StatusOK)
			return
		}
		slog.Error("orchestrator execution failed", "job_id", jobID, "session_id", sessionID, "error", err)
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
// parse payload → claim job → run agent → audit → callback to orchestrator.
//
// CRITICAL INVARIANT: After ClaimQueuedJob succeeds (CAS QUEUED→IN_PROGRESS),
// this handler MUST return HTTP 200. Returning 5xx would trigger a Cloud Tasks
// retry, but the retry's ClaimQueuedJob would see IN_PROGRESS (not QUEUED) and
// silently ACK as a stale delivery — leaving the job stuck IN_PROGRESS forever.
// All post-claim errors are handled best-effort with loud logging.
func (s *Server) handleAgentExec(w http.ResponseWriter, r *http.Request, agent AgentType, run func(ctx context.Context, job *Job) (TokenUsage, error)) {
	ctx := r.Context()

	jobID, sessionID, err := parseAgentPayload(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	slog.Info("agent webhook: received", "agent", agent, "job_id", jobID, "session_id", sessionID)

	// Atomic claim: compare-and-swap QUEUED+agent → IN_PROGRESS.
	// Prevents duplicate Cloud Tasks deliveries from both executing the agent.
	job, err := s.store.ClaimQueuedJob(ctx, jobID, sessionID, agent)
	if err != nil {
		slog.Error("agent: claim job failed", "agent", agent, "job_id", jobID, "session_id", sessionID, "error", err)
		http.Error(w, "claim job failed", http.StatusInternalServerError)
		return
	}
	if job == nil {
		slog.Warn("agent: stale/duplicate delivery, skipping",
			"agent", agent, "job_id", jobID, "session_id", sessionID)
		w.WriteHeader(http.StatusOK) // ACK — don't retry
		return
	}

	// ---------------------------------------------------------------
	// POINT OF NO RETURN — always ACK (HTTP 200) after this line.
	//
	// The job has been claimed (CAS QUEUED→IN_PROGRESS). Returning a
	// 5xx would trigger a Cloud Tasks retry, but the retry's
	// ClaimQueuedJob would see IN_PROGRESS (not QUEUED) and silently
	// ACK as a stale delivery — leaving the job stuck IN_PROGRESS
	// permanently with no recovery path.
	//
	// All subsequent Firestore/enqueue errors are handled best-effort
	// with CRITICAL-level logging for operator alerting.
	// ---------------------------------------------------------------

	// Panic safety net: if the agent (or any code below) panics, we must
	// still return HTTP 200 rather than letting net/http recover and
	// return 500. Without this, a panic leaves the job stuck IN_PROGRESS.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("CRITICAL: panic in agent execution after claim — job may be stuck IN_PROGRESS, recovery sweep will handle",
				"agent", agent, "job_id", jobID, "session_id", sessionID, "panic", r)
			w.WriteHeader(http.StatusOK)
		}
	}()

	// Per-agent timeout — defense-in-depth against hung HTTP calls or
	// LLM API stalls. Cloud Tasks has its own timeout, but this catches
	// hangs earlier with a clear error message.
	agentTimeout := agentExecTimeout(agent)
	agentCtx, agentCancel := context.WithTimeout(ctx, agentTimeout)
	defer agentCancel()

	// Execute the agent
	usage, agentErr := run(agentCtx, job)

	// Audit
	detail := "success"
	if agentErr != nil {
		detail = agentErr.Error()
	}
	if auditErr := s.store.AppendAuditLog(ctx, jobID, sessionID, AuditEntry{
		Agent: agent, Action: "execute", Tokens: usage, Detail: detail,
	}); auditErr != nil {
		slog.Error("audit log failed", "job_id", jobID, "session_id", sessionID, "agent", agent, "error", auditErr)
	}

	// Handle agent failure — mark job FAILED, ACK task.
	if agentErr != nil {
		slog.Error("agent execution failed", "agent", agent, "job_id", jobID, "session_id", sessionID, "error", agentErr)
		errMsg := agentErr.Error()
		if len(errMsg) > 200 {
			errMsg = errMsg[:200] + "…"
		}
		if failErr := s.store.UpdateJob(ctx, jobID, sessionID, []firestore.Update{
			{Path: "status", Value: StatusFailed},
			{Path: "final_result", Value: fmt.Sprintf("%s agent failed: %s", agent, errMsg)},
		}); failErr != nil {
			slog.Error("CRITICAL: failed to mark job as FAILED after agent error, job stuck IN_PROGRESS — recovery sweep will handle",
				"agent", agent, "job_id", jobID, "session_id", sessionID,
				"store_error", failErr, "agent_error", agentErr)
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	// Agent succeeded — enqueue orchestrator for next routing decision.
	//
	// Agents MUST NOT set terminal states (COMPLETED/FAILED/HITL) — only the
	// orchestrator manages status transitions. The job is IN_PROGRESS here,
	// so transitioning to QUEUED+orchestrator is always correct.
	//
	// Transition first: if the subsequent Enqueue fails, the job will be
	// QUEUED+orchestrator with no Cloud Task scheduled. The recovery sweep
	// only targets IN_PROGRESS jobs, so this case requires manual re-enqueue
	// (e.g., POST /internal/route with the job_id and session_id).
	if err := s.store.UpdateJob(ctx, jobID, sessionID, []firestore.Update{
		{Path: "status", Value: StatusQueued},
		{Path: "active_agent", Value: AgentOrchestrator},
	}); err != nil {
		slog.Error("CRITICAL: callback transition failed after successful agent execution, job stuck IN_PROGRESS — recovery sweep will handle",
			"agent", agent, "job_id", jobID, "session_id", sessionID, "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := s.dispatcher.Enqueue(ctx, s.selfURL+"/internal/route", jobID, sessionID); err != nil {
		slog.Error("CRITICAL: enqueue callback failed, job is QUEUED+orchestrator but no Cloud Task scheduled — manual re-enqueue required",
			"agent", agent, "job_id", jobID, "session_id", sessionID, "error", err)
	}

	w.WriteHeader(http.StatusOK)
}

// agentExecTimeout returns the maximum execution time for each agent type.
// These are defense-in-depth against hung HTTP calls or LLM API stalls.
//
// Sizing rationale for analyst: up to maxCodeRetries (3) attempts, each
// consisting of an LLM code-gen call (~30s) plus a sandbox HTTP call (240s
// client timeout, 120s code execution). Worst case ≈ 3×150s = 7.5min, so
// 10min provides headroom without being excessive.
func agentExecTimeout(agent AgentType) time.Duration {
	switch agent {
	case AgentData:
		return 2 * time.Minute // Multiple concurrent HTTP calls + LLM extraction
	case AgentAnalyst:
		return 10 * time.Minute // Up to 3 sandbox retries (120s each) + LLM code gen
	case AgentReport:
		return 90 * time.Second // Single LLM call
	default:
		return 3 * time.Minute
	}
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
