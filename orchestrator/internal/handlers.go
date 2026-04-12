package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"cloud.google.com/go/firestore"
	"github.com/google/uuid"
)

const maxHops = 5

type Server struct {
	store      *Store
	dispatcher *Dispatcher
	planner    *Planner
	workerURL  string
	selfURL    string
	mux        *http.ServeMux
}

func NewServer(store *Store, dispatcher *Dispatcher, planner *Planner) *Server {
	s := &Server{
		store:      store,
		dispatcher: dispatcher,
		planner:    planner,
		workerURL:  strings.TrimRight(os.Getenv("WORKER_BASE_URL"), "/"),
		selfURL:    strings.TrimRight(os.Getenv("ORCHESTRATOR_BASE_URL"), "/"),
		mux:        http.NewServeMux(),
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
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
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

	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = uuid.NewString()
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

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

	routeURL := s.selfURL + "/internal/route"
	if err := s.dispatcher.Enqueue(ctx, routeURL, jobID, sessionID); err != nil {
		slog.Error("enqueue route task failed", "error", err)
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
	job, err := s.store.GetJob(ctx, payload.JobID, payload.SessionID)
	if err != nil {
		slog.Error("route: get job failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if job == nil {
		slog.Warn("route: job not found", "job_id", payload.JobID)
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}

	slog.Info("route: processing", "job_id", job.JobID, "status", job.Status, "hop_count", job.HopCount, "active_agent", job.ActiveAgent)

	newHop := job.HopCount + 1
	if newHop > maxHops {
		slog.Warn("circuit breaker triggered", "job_id", job.JobID, "hop_count", newHop)
		_ = s.store.UpdateJob(ctx, job.JobID, job.SessionID, []firestore.Update{
			{Path: "status", Value: StatusHITL},
			{Path: "hop_count", Value: newHop},
			{Path: "active_agent", Value: AgentOrchestrator},
		})
		w.WriteHeader(http.StatusOK)
		return
	}

	switch job.Status {
	case StatusQueued:
		s.routeQueued(ctx, job, newHop)
	case StatusNeedsCtx:
		s.routeNeedsContext(ctx, job, newHop)
	case StatusInProgress:
		s.routeInProgress(ctx, job, newHop)
	case StatusCompleted:
		s.routeCompleted(ctx, job)
	default:
		slog.Warn("route: unhandled status", "status", job.Status)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) routeQueued(ctx context.Context, job *Job, hop int) {
	sess, err := s.store.GetSession(ctx, job.SessionID)
	if err != nil {
		slog.Error("route: get session failed", "error", err)
		return
	}
	var history []ChatMessage
	if sess != nil {
		history = sess.ChatHistory
	}
	plan, err := s.planner.Plan(ctx, job.Prompt, history)
	if err != nil {
		slog.Error("route: planning failed", "error", err)
		s.failJob(ctx, job, "planning failed: "+err.Error())
		return
	}

	updates := []firestore.Update{
		{Path: "hop_count", Value: hop},
		{Path: "missing_queries", Value: plan.ResearchQueries},
	}
	if plan.NeedsResearch {
		updates = append(updates,
			firestore.Update{Path: "status", Value: StatusInProgress},
			firestore.Update{Path: "active_agent", Value: AgentResearcher},
		)
		if err := s.store.UpdateJob(ctx, job.JobID, job.SessionID, updates); err != nil {
			slog.Error("route: update job failed", "error", err)
			return
		}
		if err := s.dispatcher.Enqueue(ctx, s.workerURL+"/internal/agent/researcher", job.JobID, job.SessionID); err != nil {
			slog.Error("route: enqueue researcher failed", "error", err)
		}
	} else if plan.NeedsAnalysis {
		updates = append(updates,
			firestore.Update{Path: "status", Value: StatusInProgress},
			firestore.Update{Path: "active_agent", Value: AgentAnalyst},
		)
		if err := s.store.UpdateJob(ctx, job.JobID, job.SessionID, updates); err != nil {
			slog.Error("route: update job failed", "error", err)
			return
		}
		if err := s.dispatcher.Enqueue(ctx, s.workerURL+"/internal/agent/analyst", job.JobID, job.SessionID); err != nil {
			slog.Error("route: enqueue analyst failed", "error", err)
		}
	} else {
		s.failJob(ctx, job, "planner determined no agents needed")
	}
}

func (s *Server) routeNeedsContext(ctx context.Context, job *Job, hop int) {
	if err := s.store.UpdateJob(ctx, job.JobID, job.SessionID, []firestore.Update{
		{Path: "hop_count", Value: hop},
		{Path: "status", Value: StatusInProgress},
		{Path: "active_agent", Value: AgentResearcher},
	}); err != nil {
		slog.Error("route: update job failed", "error", err)
		return
	}
	if err := s.dispatcher.Enqueue(ctx, s.workerURL+"/internal/agent/researcher", job.JobID, job.SessionID); err != nil {
		slog.Error("route: enqueue researcher failed", "error", err)
	}
}

func (s *Server) routeInProgress(ctx context.Context, job *Job, hop int) {
	switch job.ActiveAgent {
	case AgentResearcher:
		if err := s.store.UpdateJob(ctx, job.JobID, job.SessionID, []firestore.Update{
			{Path: "hop_count", Value: hop},
			{Path: "active_agent", Value: AgentAnalyst},
		}); err != nil {
			slog.Error("route: update job failed", "error", err)
			return
		}
		if err := s.dispatcher.Enqueue(ctx, s.workerURL+"/internal/agent/analyst", job.JobID, job.SessionID); err != nil {
			slog.Error("route: enqueue analyst failed", "error", err)
		}
	case AgentAnalyst:
		slog.Warn("route: analyst callback but not completed", "job_id", job.JobID)
	default:
		slog.Warn("route: unknown agent in progress", "agent", job.ActiveAgent)
	}
}

func (s *Server) routeCompleted(ctx context.Context, job *Job) {
	if job.FinalResult != "" {
		if err := s.store.AppendChatHistory(ctx, job.SessionID,
			ChatMessage{Role: "assistant", Content: job.FinalResult},
		); err != nil {
			slog.Error("route: append final result failed", "error", err)
		}
	}
	slog.Info("job completed", "job_id", job.JobID)
}

func (s *Server) failJob(ctx context.Context, job *Job, reason string) {
	slog.Error("failing job", "job_id", job.JobID, "reason", reason)
	_ = s.store.UpdateJob(ctx, job.JobID, job.SessionID, []firestore.Update{
		{Path: "status", Value: StatusFailed},
		{Path: "final_result", Value: reason},
	})
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
