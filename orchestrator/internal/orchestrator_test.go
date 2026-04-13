package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestCircuitBreaker_TriggersHITL(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}
	gemini := &mockGemini{}
	orch := NewOrchestratorAgent(gemini, store, disp, "http://localhost:8080")

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusQueued,
		ActiveAgent: AgentOrchestrator,
		HopCount:    maxHops, // already at the limit
	})

	err := orch.Execute(context.Background(), "job-1", "sess-1")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	if job.Status != StatusHITL {
		t.Errorf("job.Status = %s, want HITL (circuit breaker)", job.Status)
	}
	if job.AgentInstructions == "" {
		t.Error("expected agent_instructions to contain a message for the user")
	}
}

func TestOrchestrator_TerminalStateSkip(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}
	gemini := &mockGemini{}
	orch := NewOrchestratorAgent(gemini, store, disp, "http://localhost:8080")

	for _, status := range []JobStatus{StatusCompleted, StatusFailed, StatusHITL} {
		t.Run(string(status), func(t *testing.T) {
			seedJob(store, &Job{
				JobID:       "job-" + string(status),
				SessionID:   "sess-1",
				Status:      status,
				ActiveAgent: AgentOrchestrator,
			})
			seedSession(store, &Session{SessionID: "sess-1"})

			err := orch.Execute(context.Background(), "job-"+string(status), "sess-1")
			if err != nil {
				t.Errorf("Execute() should skip terminal state %s, got error: %v", status, err)
			}
		})
	}
}

func TestOrchestrator_ActiveAgentGuard(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}
	gemini := &mockGemini{}
	orch := NewOrchestratorAgent(gemini, store, disp, "http://localhost:8080")

	seedSession(store, &Session{SessionID: "sess-1"})
	// Active agent is data, not orchestrator — should skip
	seedJob(store, &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusQueued,
		ActiveAgent: AgentData,
	})

	err := orch.Execute(context.Background(), "job-1", "sess-1")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// No dispatcher calls (skipped)
	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.enqueued) != 0 {
		t.Errorf("expected no enqueued tasks, got %d", len(disp.enqueued))
	}
}

func TestOrchestrator_JobNotFound(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}
	gemini := &mockGemini{}
	orch := NewOrchestratorAgent(gemini, store, disp, "http://localhost:8080")

	err := orch.Execute(context.Background(), "nonexistent", "sess-1")
	if !IsPermanentError(err) {
		t.Errorf("expected permanentError, got: %v", err)
	}
}

func TestOrchestrator_RouteToData(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}
	gemini := mockGeminiWithJSON(RoutingDecision{
		NextAgent:    "data",
		Reasoning:    "need facts",
		Instructions: "get revenue",
		Queries:      []string{"TSLA revenue 2024"},
	})
	orch := NewOrchestratorAgent(gemini, store, disp, "http://localhost:8080")

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusQueued,
		ActiveAgent: AgentOrchestrator,
	})

	err := orch.Execute(context.Background(), "job-1", "sess-1")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	if job.Status != StatusQueued {
		t.Errorf("job.Status = %s, want QUEUED", job.Status)
	}
	if job.ActiveAgent != AgentData {
		t.Errorf("job.ActiveAgent = %s, want data", job.ActiveAgent)
	}
	if len(job.MissingQueries) != 1 || job.MissingQueries[0] != "TSLA revenue 2024" {
		t.Errorf("job.MissingQueries = %v", job.MissingQueries)
	}

	// Verify dispatcher was called for data agent
	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.enqueued) != 1 {
		t.Fatalf("enqueued = %d, want 1", len(disp.enqueued))
	}
	if disp.enqueued[0].URL != "http://localhost:8080/internal/agent/data" {
		t.Errorf("enqueued URL = %s", disp.enqueued[0].URL)
	}
}

func TestOrchestrator_RouteToAnalyst(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}
	gemini := mockGeminiWithJSON(RoutingDecision{
		NextAgent:    "analyst",
		Reasoning:    "facts collected, need analysis",
		Instructions: "create revenue chart",
	})
	orch := NewOrchestratorAgent(gemini, store, disp, "http://localhost:8080")

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusQueued,
		ActiveAgent: AgentOrchestrator,
	})

	err := orch.Execute(context.Background(), "job-1", "sess-1")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	if job.ActiveAgent != AgentAnalyst {
		t.Errorf("job.ActiveAgent = %s, want analyst", job.ActiveAgent)
	}
}

func TestOrchestrator_RouteToReport(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}
	gemini := mockGeminiWithJSON(RoutingDecision{
		NextAgent:    "report",
		Reasoning:    "analysis complete",
		Instructions: "synthesize findings",
	})
	orch := NewOrchestratorAgent(gemini, store, disp, "http://localhost:8080")

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusQueued,
		ActiveAgent: AgentOrchestrator,
	})

	err := orch.Execute(context.Background(), "job-1", "sess-1")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	if job.ActiveAgent != AgentReport {
		t.Errorf("job.ActiveAgent = %s, want report", job.ActiveAgent)
	}
}

func TestOrchestrator_RouteToComplete(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}
	gemini := mockGeminiWithJSON(RoutingDecision{
		NextAgent: "complete",
		Reasoning: "report is adequate",
	})
	orch := NewOrchestratorAgent(gemini, store, disp, "http://localhost:8080")

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusQueued,
		ActiveAgent: AgentOrchestrator,
		FinalResult: "The final report.",
	})

	err := orch.Execute(context.Background(), "job-1", "sess-1")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	if job.Status != StatusCompleted {
		t.Errorf("job.Status = %s, want COMPLETED", job.Status)
	}
}

func TestOrchestrator_RouteToAskUser(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}
	gemini := mockGeminiWithJSON(RoutingDecision{
		NextAgent:    "ask_user",
		Reasoning:    "ambiguous request",
		Instructions: "Which company do you mean?",
	})
	orch := NewOrchestratorAgent(gemini, store, disp, "http://localhost:8080")

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusQueued,
		ActiveAgent: AgentOrchestrator,
	})

	err := orch.Execute(context.Background(), "job-1", "sess-1")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	if job.Status != StatusHITL {
		t.Errorf("job.Status = %s, want HITL", job.Status)
	}
	if job.AgentInstructions != "Which company do you mean?" {
		t.Errorf("job.AgentInstructions = %q", job.AgentInstructions)
	}
}

func TestOrchestrator_DataNoQueries_Fails(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}
	gemini := mockGeminiWithJSON(RoutingDecision{
		NextAgent:    "data",
		Reasoning:    "need facts",
		Instructions: "get data",
		Queries:      []string{}, // empty queries — should fail
	})
	orch := NewOrchestratorAgent(gemini, store, disp, "http://localhost:8080")

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusQueued,
		ActiveAgent: AgentOrchestrator,
	})

	err := orch.Execute(context.Background(), "job-1", "sess-1")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	if job.Status != StatusFailed {
		t.Errorf("job.Status = %s, want FAILED", job.Status)
	}
}

func TestOrchestrator_UnknownAgent_Fails(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}
	gemini := mockGeminiWithJSON(RoutingDecision{
		NextAgent: "nonexistent",
		Reasoning: "bad decision",
	})
	orch := NewOrchestratorAgent(gemini, store, disp, "http://localhost:8080")

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusQueued,
		ActiveAgent: AgentOrchestrator,
	})

	err := orch.Execute(context.Background(), "job-1", "sess-1")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	if job.Status != StatusFailed {
		t.Errorf("job.Status = %s, want FAILED", job.Status)
	}
}

func TestOrchestrator_EnqueueFailure_Reverts(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{err: fmt.Errorf("cloud tasks unavailable")}
	gemini := mockGeminiWithJSON(RoutingDecision{
		NextAgent:    "analyst",
		Reasoning:    "need analysis",
		Instructions: "analyze data",
	})
	orch := NewOrchestratorAgent(gemini, store, disp, "http://localhost:8080")

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusQueued,
		ActiveAgent: AgentOrchestrator,
	})

	err := orch.Execute(context.Background(), "job-1", "sess-1")
	if err == nil {
		t.Fatal("expected error from enqueue failure")
	}

	// Job should be reverted to QUEUED+orchestrator so a retry can re-route
	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	if job.ActiveAgent != AgentOrchestrator {
		t.Errorf("job.ActiveAgent = %s, want orchestrator (reverted)", job.ActiveAgent)
	}
}

func TestOrchestrator_AuditLog(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}
	gemini := mockGeminiWithJSON(RoutingDecision{
		NextAgent: "complete",
		Reasoning: "done",
	})
	// Give usage so audit accumulates
	gemini.usage = TokenUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}
	orch := NewOrchestratorAgent(gemini, store, disp, "http://localhost:8080")

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusQueued,
		ActiveAgent: AgentOrchestrator,
	})

	orch.Execute(context.Background(), "job-1", "sess-1")

	// Verify audit log was created
	store.mu.Lock()
	entries := store.audit["job-1"]
	store.mu.Unlock()
	if len(entries) == 0 {
		t.Fatal("no audit entries recorded")
	}
	if entries[0].Agent != AgentOrchestrator {
		t.Errorf("audit agent = %s, want orchestrator", entries[0].Agent)
	}
}

func TestIsPermanentError(t *testing.T) {
	if IsPermanentError(fmt.Errorf("regular error")) {
		t.Error("regular error should not be permanent")
	}
	if !IsPermanentError(&permanentError{msg: "perm"}) {
		t.Error("permanentError should be permanent")
	}
	// Wrapped permanent error
	wrapped := fmt.Errorf("wrap: %w", &permanentError{msg: "inner"})
	if !IsPermanentError(wrapped) {
		t.Error("wrapped permanentError should be permanent")
	}
}

func TestAssetSummaries(t *testing.T) {
	assets := []Asset{
		{Type: "chart", Name: "chart_1.png", Content: "base64data"},
		{Type: "analysis_output", Name: "analysis.txt", Content: "Some analysis text here"},
	}
	summaries := assetSummaries(assets)
	if len(summaries) != 2 {
		t.Fatalf("len = %d, want 2", len(summaries))
	}

	// Chart should NOT have content_preview (binary)
	if _, has := summaries[0]["content_preview"]; has {
		t.Error("chart should not have content_preview")
	}

	// Analysis output should have content_preview
	if preview, has := summaries[1]["content_preview"]; !has || preview == "" {
		t.Error("analysis_output should have content_preview")
	}
}

// --- E2E: Full Task Lifecycle ---

func TestE2E_IngestRouteComplete(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}
	gemini := &mockGemini{}
	orch := NewOrchestratorAgent(gemini, store, disp, "http://localhost:8080")

	// Step 1: Simulate ingest — create session + job
	sess := &Session{SessionID: "sess-1", ChatHistory: []ChatMessage{{Role: "user", Content: "What is 2+2?"}}}
	store.CreateSession(context.Background(), sess)
	job := &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusQueued,
		ActiveAgent: AgentOrchestrator,
		Prompt:      "What is 2+2?",
	}
	store.CreateJob(context.Background(), job)

	// Step 2: Orchestrator routes to "complete" (simple computation, report already done)
	store.mu.Lock()
	store.jobs["job-1"].FinalResult = "2+2 = 4"
	store.mu.Unlock()

	gemini.generateJSONFn = func(_ context.Context, _, _ string, out any) (TokenUsage, error) {
		data, _ := json.Marshal(RoutingDecision{NextAgent: "complete", Reasoning: "task is trivial"})
		return TokenUsage{TotalTokens: 5}, json.Unmarshal(data, out)
	}

	err := orch.Execute(context.Background(), "job-1", "sess-1")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Verify final state
	final, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	if final.Status != StatusCompleted {
		t.Errorf("final status = %s, want COMPLETED", final.Status)
	}
}

func TestE2E_HITLCycle(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}
	gemini := &mockGemini{}
	orch := NewOrchestratorAgent(gemini, store, disp, "http://localhost:8080")

	sess := &Session{SessionID: "sess-1", ChatHistory: []ChatMessage{{Role: "user", Content: "Analyze IPO"}}}
	store.CreateSession(context.Background(), sess)
	job := &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusQueued,
		ActiveAgent: AgentOrchestrator,
		Prompt:      "Analyze IPO",
	}
	store.CreateJob(context.Background(), job)

	// Step 1: Orchestrator asks for clarification → HITL
	gemini.generateJSONFn = func(_ context.Context, _, _ string, out any) (TokenUsage, error) {
		data, _ := json.Marshal(RoutingDecision{
			NextAgent:    "ask_user",
			Reasoning:    "which IPO?",
			Instructions: "Which company's IPO?",
		})
		return TokenUsage{TotalTokens: 5}, json.Unmarshal(data, out)
	}

	err := orch.Execute(context.Background(), "job-1", "sess-1")
	if err != nil {
		t.Fatalf("step 1 error = %v", err)
	}

	j, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	if j.Status != StatusHITL {
		t.Fatalf("step 1: status = %s, want HITL", j.Status)
	}

	// Step 2: User replies → resume
	store.ResumeHITLJob(context.Background(), "job-1", "sess-1")
	store.AppendChatHistory(context.Background(), "sess-1", ChatMessage{Role: "user", Content: "I meant OpenAI"})

	// Step 3: Orchestrator routes to "complete" after clarification
	gemini.generateJSONFn = func(_ context.Context, _, _ string, out any) (TokenUsage, error) {
		data, _ := json.Marshal(RoutingDecision{NextAgent: "complete", Reasoning: "clarified"})
		return TokenUsage{TotalTokens: 5}, json.Unmarshal(data, out)
	}

	// Manually set a final result so complete makes sense
	store.mu.Lock()
	store.jobs["job-1"].FinalResult = "OpenAI IPO analysis"
	store.mu.Unlock()

	err = orch.Execute(context.Background(), "job-1", "sess-1")
	if err != nil {
		t.Fatalf("step 3 error = %v", err)
	}

	final, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	if final.Status != StatusCompleted {
		t.Errorf("final status = %s, want COMPLETED", final.Status)
	}
}
