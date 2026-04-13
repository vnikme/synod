package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
)

func setupTestServer(t *testing.T) (*Server, *mockStore, *mockDispatcher, *mockGemini) {
	t.Helper()
	store := newMockStore()
	disp := &mockDispatcher{}
	gemini := &mockGemini{}

	orch := NewOrchestratorAgent(gemini, store, disp, "http://localhost:8080")
	data := NewDataAgent(gemini, store, "test-ua")
	analyst := &AnalystAgent{
		gemini:     gemini,
		store:      store,
		sandboxURL: "http://sandbox:8080",
		http:       &http.Client{Timeout: 10 * time.Second},
	}
	report := NewReportAgent(gemini, store)

	srv := NewServer(orch, data, analyst, report, store, disp, "http://localhost:8080", nil, nil)
	return srv, store, disp, gemini
}

// --- Health ---

func TestHealthEndpoint(t *testing.T) {
	srv, _, _, _ := setupTestServer(t)
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var body map[string]string
	json.NewDecoder(w.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("body = %v, want status=ok", body)
	}
}

// --- Ingest ---

func TestIngest_Success(t *testing.T) {
	srv, store, disp, _ := setupTestServer(t)

	body := `{"prompt": "Analyze TSLA stock"}`
	req := httptest.NewRequest("POST", "/api/v1/tasks", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	var resp IngestResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.JobID == "" || resp.SessionID == "" {
		t.Fatalf("missing job_id or session_id: %+v", resp)
	}

	// Verify job was created
	job, _ := store.GetJob(context.Background(), resp.JobID, resp.SessionID)
	if job == nil {
		t.Fatal("job not found in store")
	}
	if job.Status != StatusQueued {
		t.Errorf("job.Status = %s, want %s", job.Status, StatusQueued)
	}
	if job.ActiveAgent != AgentOrchestrator {
		t.Errorf("job.ActiveAgent = %s, want %s", job.ActiveAgent, AgentOrchestrator)
	}
	if job.Prompt != "Analyze TSLA stock" {
		t.Errorf("job.Prompt = %q", job.Prompt)
	}

	// Verify session was created with user message
	sess, _ := store.GetSession(context.Background(), resp.SessionID)
	if sess == nil {
		t.Fatal("session not found")
	}
	if len(sess.ChatHistory) != 1 || sess.ChatHistory[0].Content != "Analyze TSLA stock" {
		t.Errorf("chat_history = %v", sess.ChatHistory)
	}

	// Verify dispatcher was called
	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.enqueued) != 1 {
		t.Fatalf("enqueued = %d, want 1", len(disp.enqueued))
	}
	if disp.enqueued[0].URL != "http://localhost:8080/internal/route" {
		t.Errorf("enqueued URL = %s", disp.enqueued[0].URL)
	}
}

func TestIngest_ExistingSession(t *testing.T) {
	srv, store, _, _ := setupTestServer(t)

	sessionID := "550e8400-e29b-41d4-a716-446655440000"
	seedSession(store, &Session{
		SessionID:   sessionID,
		ChatHistory: []ChatMessage{{Role: "user", Content: "previous message"}},
	})

	body := `{"prompt": "Follow up question", "session_id": "` + sessionID + `"}`
	req := httptest.NewRequest("POST", "/api/v1/tasks", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
	}

	// Verify chat history was appended (not replaced)
	sess, _ := store.GetSession(context.Background(), sessionID)
	if len(sess.ChatHistory) != 2 {
		t.Errorf("chat_history length = %d, want 2", len(sess.ChatHistory))
	}
}

func TestIngest_EmptyPrompt(t *testing.T) {
	srv, _, _, _ := setupTestServer(t)

	body := `{"prompt": ""}`
	req := httptest.NewRequest("POST", "/api/v1/tasks", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestIngest_WhitespaceOnlyPrompt(t *testing.T) {
	srv, _, _, _ := setupTestServer(t)

	body := `{"prompt": "   "}`
	req := httptest.NewRequest("POST", "/api/v1/tasks", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestIngest_InvalidJSON(t *testing.T) {
	srv, _, _, _ := setupTestServer(t)

	req := httptest.NewRequest("POST", "/api/v1/tasks", bytes.NewBufferString("not json"))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestIngest_InvalidSessionID(t *testing.T) {
	srv, _, _, _ := setupTestServer(t)

	body := `{"prompt": "hello", "session_id": "not-a-uuid"}`
	req := httptest.NewRequest("POST", "/api/v1/tasks", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// --- Status ---

func TestStatus_Success(t *testing.T) {
	srv, store, _, _ := setupTestServer(t)

	seedJob(store, &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusInProgress,
		ActiveAgent: AgentData,
		Prompt:      "Test prompt",
	})

	req := httptest.NewRequest("GET", "/api/v1/tasks/job-1?session_id=sess-1", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var job Job
	json.NewDecoder(w.Body).Decode(&job)
	if job.Status != StatusInProgress {
		t.Errorf("job.Status = %s, want IN_PROGRESS", job.Status)
	}
}

func TestStatus_NotFound(t *testing.T) {
	srv, _, _, _ := setupTestServer(t)

	req := httptest.NewRequest("GET", "/api/v1/tasks/nonexistent?session_id=sess-1", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestStatus_SessionIsolation(t *testing.T) {
	srv, store, _, _ := setupTestServer(t)

	seedJob(store, &Job{
		JobID:     "job-1",
		SessionID: "sess-1",
		Status:    StatusQueued,
	})

	// Access with different session ID — should be not found
	req := httptest.NewRequest("GET", "/api/v1/tasks/job-1?session_id=other-session", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (session isolation)", w.Code, http.StatusNotFound)
	}
}

func TestStatus_MissingParams(t *testing.T) {
	srv, _, _, _ := setupTestServer(t)

	req := httptest.NewRequest("GET", "/api/v1/tasks/job-1", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// --- Reply ---

func TestReply_Success(t *testing.T) {
	srv, store, disp, _ := setupTestServer(t)

	seedSession(store, &Session{
		SessionID:   "sess-1",
		ChatHistory: []ChatMessage{{Role: "user", Content: "original prompt"}},
	})
	seedJob(store, &Job{
		JobID:             "job-1",
		SessionID:         "sess-1",
		Status:            StatusHITL,
		ActiveAgent:       AgentOrchestrator,
		AgentInstructions: "Please clarify",
	})

	body := `{"session_id": "sess-1", "message": "I meant OpenAI"}`
	req := httptest.NewRequest("POST", "/api/v1/tasks/job-1/reply", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	// Verify job transitioned to QUEUED
	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	if job.Status != StatusQueued {
		t.Errorf("job.Status = %s, want QUEUED", job.Status)
	}
	if job.HopCount != 0 {
		t.Errorf("job.HopCount = %d, want 0 (reset)", job.HopCount)
	}

	// Verify chat history was appended
	sess, _ := store.GetSession(context.Background(), "sess-1")
	if len(sess.ChatHistory) != 2 {
		t.Errorf("chat_history length = %d, want 2", len(sess.ChatHistory))
	}

	// Verify orchestrator was enqueued
	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.enqueued) != 1 {
		t.Fatalf("enqueued = %d, want 1", len(disp.enqueued))
	}
}

func TestReply_NotHITL(t *testing.T) {
	srv, store, _, _ := setupTestServer(t)

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:     "job-1",
		SessionID: "sess-1",
		Status:    StatusInProgress,
	})

	body := `{"session_id": "sess-1", "message": "reply"}`
	req := httptest.NewRequest("POST", "/api/v1/tasks/job-1/reply", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
	}
}

func TestReply_NotFound(t *testing.T) {
	srv, _, _, _ := setupTestServer(t)

	body := `{"session_id": "sess-1", "message": "reply"}`
	req := httptest.NewRequest("POST", "/api/v1/tasks/nonexistent/reply", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestReply_EmptyMessage(t *testing.T) {
	srv, _, _, _ := setupTestServer(t)

	body := `{"session_id": "sess-1", "message": ""}`
	req := httptest.NewRequest("POST", "/api/v1/tasks/job-1/reply", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// --- Route (internal webhook) ---

func TestRoute_Success(t *testing.T) {
	srv, store, _, gemini := setupTestServer(t)

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusQueued,
		ActiveAgent: AgentOrchestrator,
		FinalResult: "A great report.",
	})

	// Mock LLM returns "complete" decision
	gemini.generateJSONFn = func(_ context.Context, _, _ string, out any) (TokenUsage, error) {
		data, _ := json.Marshal(RoutingDecision{
			NextAgent: "complete",
			Reasoning: "report is adequate",
		})
		return TokenUsage{TotalTokens: 10}, json.Unmarshal(data, out)
	}

	payload := `{"job_id": "job-1", "session_id": "sess-1"}`
	req := httptest.NewRequest("POST", "/internal/route", bytes.NewBufferString(payload))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Verify job is completed
	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	if job.Status != StatusCompleted {
		t.Errorf("job.Status = %s, want COMPLETED", job.Status)
	}
}

func TestRoute_InvalidPayload(t *testing.T) {
	srv, _, _, _ := setupTestServer(t)

	req := httptest.NewRequest("POST", "/internal/route", bytes.NewBufferString("not json"))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// --- Agent Webhooks ---

func TestAgentWebhook_DuplicateDelivery(t *testing.T) {
	srv, store, _, _ := setupTestServer(t)

	seedSession(store, &Session{SessionID: "sess-1"})
	// Job is already IN_PROGRESS (claimed by a previous delivery)
	seedJob(store, &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusInProgress,
		ActiveAgent: AgentData,
	})

	payload := `{"job_id": "job-1", "session_id": "sess-1"}`
	req := httptest.NewRequest("POST", "/internal/agent/data", bytes.NewBufferString(payload))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	// Should ACK without executing (stale delivery)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (ACK stale delivery)", w.Code, http.StatusOK)
	}

	// Job should still be IN_PROGRESS (not modified)
	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	if job.Status != StatusInProgress {
		t.Errorf("job.Status = %s, want IN_PROGRESS (unchanged)", job.Status)
	}
}

func TestAgentWebhook_InvalidPayload(t *testing.T) {
	srv, _, _, _ := setupTestServer(t)

	req := httptest.NewRequest("POST", "/internal/agent/data", bytes.NewBufferString("bad"))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// Regression test: previously, a transient Firestore error during post-execution
// re-read returned HTTP 500, triggering a Cloud Tasks retry. But the retry's
// ClaimQueuedJob saw IN_PROGRESS (not QUEUED) and ACKed as stale — leaving the
// job stuck IN_PROGRESS permanently. The fix removes the re-read and ensures
// HTTP 200 is always returned after the agent claims the job.
func TestAgentWebhook_CallbackTransitionFails_Returns200(t *testing.T) {
	srv, store, _, gemini := setupTestServer(t)

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusQueued,
		ActiveAgent: AgentReport,
		Prompt:      "test",
	})

	gemini.textResponse = "A report."
	gemini.usage = TokenUsage{TotalTokens: 10}

	// Make the SECOND UpdateJob call fail (first = report agent writing
	// final_result, second = handleAgentExec callback transition).
	callCount := 0
	store.updateJobFn = func(_ context.Context, jobID, sessionID string, updates []firestore.Update) error {
		callCount++
		if callCount == 2 {
			return fmt.Errorf("simulated Firestore unavailable")
		}
		// Default behavior for other calls.
		store.mu.Lock()
		defer store.mu.Unlock()
		job, ok := store.jobs[jobID]
		if !ok || job.SessionID != sessionID {
			return fmt.Errorf("job %s not found for session %s", jobID, sessionID)
		}
		applyMockUpdates(job, updates)
		job.UpdatedAt = time.Now()
		return nil
	}

	payload := `{"job_id": "job-1", "session_id": "sess-1"}`
	req := httptest.NewRequest("POST", "/internal/agent/report", bytes.NewBufferString(payload))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	// MUST return 200 — returning 500 would cause a stuck job (the whole bug)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d — must ACK after agent execution to prevent stuck job", w.Code, http.StatusOK)
	}
}

func TestAgentWebhook_AgentFails_FailJobFails_Returns200(t *testing.T) {
	srv, store, _, gemini := setupTestServer(t)

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusQueued,
		ActiveAgent: AgentReport,
		Prompt:      "test",
	})

	// Make the LLM fail (so the report agent returns error)
	gemini.err = fmt.Errorf("LLM API unavailable")

	// Also make UpdateJob fail (so failJob can't mark FAILED)
	store.updateJobErr = fmt.Errorf("Firestore unavailable")

	payload := `{"job_id": "job-1", "session_id": "sess-1"}`
	req := httptest.NewRequest("POST", "/internal/agent/report", bytes.NewBufferString(payload))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	// MUST return 200 even when both agent AND failJob fail
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d — must ACK after claim to prevent stuck retry loop", w.Code, http.StatusOK)
	}
}

func TestAgentWebhook_SuccessEnqueuesOrchestrator(t *testing.T) {
	srv, store, disp, gemini := setupTestServer(t)

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusQueued,
		ActiveAgent: AgentReport,
		Prompt:      "test",
	})

	gemini.textResponse = "A report."
	gemini.usage = TokenUsage{TotalTokens: 10}

	payload := `{"job_id": "job-1", "session_id": "sess-1"}`
	req := httptest.NewRequest("POST", "/internal/agent/report", bytes.NewBufferString(payload))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Verify job transitioned to QUEUED+orchestrator
	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	if job.Status != StatusQueued {
		t.Errorf("job.Status = %s, want QUEUED", job.Status)
	}
	if job.ActiveAgent != AgentOrchestrator {
		t.Errorf("job.ActiveAgent = %s, want orchestrator", job.ActiveAgent)
	}

	// Verify orchestrator callback was enqueued
	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.enqueued) != 1 {
		t.Fatalf("enqueued = %d, want 1", len(disp.enqueued))
	}
	if disp.enqueued[0].URL != "http://localhost:8080/internal/route" {
		t.Errorf("enqueued URL = %s", disp.enqueued[0].URL)
	}
}

// --- SPA Fallback ---

func TestSPA_APIPathReturns404(t *testing.T) {
	srv, _, _, _ := setupTestServer(t)

	// /api/* paths that don't match a route should 404, not serve HTML
	req := httptest.NewRequest("GET", "/api/nonexistent", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestSPA_InternalPathReturns404(t *testing.T) {
	srv, _, _, _ := setupTestServer(t)

	req := httptest.NewRequest("GET", "/internal/nonexistent", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}
