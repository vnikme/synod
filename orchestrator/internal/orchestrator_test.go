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
		JobID:         "job-1",
		SessionID:     "sess-1",
		Status:        StatusQueued,
		ActiveAgent:   AgentOrchestrator,
		CollectedFacts: []Fact{{Key: "revenue", Value: "$1B", Source: "web"}},
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
		JobID:         "job-1",
		SessionID:     "sess-1",
		Status:        StatusQueued,
		ActiveAgent:   AgentOrchestrator,
		CollectedFacts: []Fact{{Key: "data", Value: "some data", Source: "web"}},
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

func TestOrchestrator_DataNoQueries_RedirectsToAskUser(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}
	gemini := mockGeminiWithJSON(RoutingDecision{
		NextAgent:    "data",
		Reasoning:    "need facts",
		Instructions: "get data",
		Queries:      []string{}, // empty queries — guardrail redirects to ask_user
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
		t.Errorf("job.Status = %s, want HITL (guardrail redirected data-no-queries to ask_user)", job.Status)
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

// --- validateDecision tests ---

func TestValidateDecision_DataNoQueries_RedirectsToAskUser(t *testing.T) {
	d := &RoutingDecision{NextAgent: "data", Reasoning: "need data", Instructions: "fetch info"}
	job := &Job{JobID: "j1"}

	result := validateDecision(d, job)
	if result.NextAgent != "ask_user" {
		t.Errorf("NextAgent = %s, want ask_user", result.NextAgent)
	}
}

func TestValidateDecision_DataWithQueries_PassesThrough(t *testing.T) {
	d := &RoutingDecision{NextAgent: "data", Queries: []string{"q1"}, Reasoning: "search"}
	job := &Job{JobID: "j1"}

	result := validateDecision(d, job)
	if result.NextAgent != "data" {
		t.Errorf("NextAgent = %s, want data", result.NextAgent)
	}
}

func TestValidateDecision_AnalystNoFacts_RedirectsToData(t *testing.T) {
	d := &RoutingDecision{NextAgent: "analyst", Reasoning: "analyze", Instructions: "run analysis"}
	job := &Job{JobID: "j1", Prompt: "test prompt"}

	result := validateDecision(d, job)
	if result.NextAgent != "data" {
		t.Errorf("NextAgent = %s, want data", result.NextAgent)
	}
	if len(result.Queries) == 0 {
		t.Error("expected queries to include the job prompt as fallback")
	}
}

func TestValidateDecision_AnalystWithFacts_PassesThrough(t *testing.T) {
	d := &RoutingDecision{NextAgent: "analyst", Reasoning: "analyze"}
	job := &Job{JobID: "j1", CollectedFacts: []Fact{{Key: "k", Value: "v"}}}

	result := validateDecision(d, job)
	if result.NextAgent != "analyst" {
		t.Errorf("NextAgent = %s, want analyst", result.NextAgent)
	}
}

func TestValidateDecision_AnalystWithAssets_PassesThrough(t *testing.T) {
	d := &RoutingDecision{NextAgent: "analyst", Reasoning: "re-analyze"}
	job := &Job{JobID: "j1", GeneratedAssets: []Asset{{Type: "chart", Name: "c.png"}}}

	result := validateDecision(d, job)
	if result.NextAgent != "analyst" {
		t.Errorf("NextAgent = %s, want analyst", result.NextAgent)
	}
}

func TestValidateDecision_ReportNoContent_RedirectsToAskUser(t *testing.T) {
	d := &RoutingDecision{NextAgent: "report", Reasoning: "synthesize"}
	job := &Job{JobID: "j1"}

	result := validateDecision(d, job)
	if result.NextAgent != "ask_user" {
		t.Errorf("NextAgent = %s, want ask_user", result.NextAgent)
	}
}

func TestValidateDecision_ReportWithFacts_PassesThrough(t *testing.T) {
	d := &RoutingDecision{NextAgent: "report", Reasoning: "synthesize"}
	job := &Job{JobID: "j1", CollectedFacts: []Fact{{Key: "k", Value: "v"}}}

	result := validateDecision(d, job)
	if result.NextAgent != "report" {
		t.Errorf("NextAgent = %s, want report", result.NextAgent)
	}
}

func TestValidateDecision_CompleteNoReport_RedirectsToReport(t *testing.T) {
	d := &RoutingDecision{NextAgent: "complete", Reasoning: "done"}
	job := &Job{JobID: "j1"}

	result := validateDecision(d, job)
	if result.NextAgent != "report" {
		t.Errorf("NextAgent = %s, want report", result.NextAgent)
	}
}

func TestValidateDecision_CompleteWithReport_PassesThrough(t *testing.T) {
	d := &RoutingDecision{NextAgent: "complete", Reasoning: "done"}
	job := &Job{JobID: "j1", FinalResult: "A report"}

	result := validateDecision(d, job)
	if result.NextAgent != "complete" {
		t.Errorf("NextAgent = %s, want complete", result.NextAgent)
	}
}

func TestValidateDecision_AskUser_AlwaysPassesThrough(t *testing.T) {
	d := &RoutingDecision{NextAgent: "ask_user", Instructions: "clarify?"}
	job := &Job{JobID: "j1"}

	result := validateDecision(d, job)
	if result.NextAgent != "ask_user" {
		t.Errorf("NextAgent = %s, want ask_user", result.NextAgent)
	}
}

// --- compactFacts tests ---

func TestCompactFacts_Empty(t *testing.T) {
	result := compactFacts(nil)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
	result = compactFacts([]Fact{})
	if len(result) != 0 {
		t.Errorf("expected empty, got %v", result)
	}
}

func TestCompactFacts_Deduplication(t *testing.T) {
	facts := []Fact{
		{Key: "revenue", Value: "old-value", Source: "web"},
		{Key: "profit", Value: "100", Source: "sec"},
		{Key: "revenue", Value: "new-value", Source: "web"},
	}
	result := compactFacts(facts)
	if len(result) != 2 {
		t.Fatalf("len = %d, want 2 (deduplicated)", len(result))
	}
	// "revenue" should have the newer value
	for _, f := range result {
		if f.Key == "revenue" && f.Value != "new-value" {
			t.Errorf("revenue value = %q, want 'new-value' (last wins)", f.Value)
		}
	}
}

func TestCompactFacts_CapsAtLimit(t *testing.T) {
	facts := make([]Fact, maxFactsForLLM+10)
	for i := range facts {
		facts[i] = Fact{Key: fmt.Sprintf("key-%d", i), Value: "v", Source: "web"}
	}
	result := compactFacts(facts)
	if len(result) != maxFactsForLLM {
		t.Errorf("len = %d, want %d (capped)", len(result), maxFactsForLLM)
	}
}

func TestCompactFacts_TruncatesValues(t *testing.T) {
	longValue := ""
	for i := 0; i < maxFactValueRunes+50; i++ {
		longValue += "x"
	}
	facts := []Fact{{Key: "k", Value: longValue, Source: "web"}}
	result := compactFacts(facts)
	if len(result) != 1 {
		t.Fatalf("len = %d, want 1", len(result))
	}
	// Value should be truncated + "…"
	runes := []rune(result[0].Value)
	if len(runes) > maxFactValueRunes+1 { // +1 for "…"
		t.Errorf("value runes = %d, want <= %d", len(runes), maxFactValueRunes+1)
	}
}

func TestCompactFacts_PreservesSourceAndKey(t *testing.T) {
	facts := []Fact{{Key: "mykey", Value: "myval", Source: "sec_edgar"}}
	result := compactFacts(facts)
	if result[0].Key != "mykey" || result[0].Source != "sec_edgar" {
		t.Errorf("key/source mangled: %+v", result[0])
	}
}

// --- compactChatHistory tests ---

func TestCompactChatHistory_UnderLimit(t *testing.T) {
	msgs := []ChatMessage{{Role: "user", Content: "hello"}}
	result := compactChatHistory(msgs)
	if len(result) != 1 {
		t.Errorf("len = %d, want 1", len(result))
	}
}

func TestCompactChatHistory_ExactlyAtLimit(t *testing.T) {
	msgs := make([]ChatMessage, maxChatHistoryForLLM)
	for i := range msgs {
		msgs[i] = ChatMessage{Role: "user", Content: fmt.Sprintf("msg-%d", i)}
	}
	result := compactChatHistory(msgs)
	if len(result) != maxChatHistoryForLLM {
		t.Errorf("len = %d, want %d", len(result), maxChatHistoryForLLM)
	}
}

func TestCompactChatHistory_OverLimit_KeepsFirstAndTail(t *testing.T) {
	total := maxChatHistoryForLLM + 10
	msgs := make([]ChatMessage, total)
	for i := range msgs {
		msgs[i] = ChatMessage{Role: "user", Content: fmt.Sprintf("msg-%d", i)}
	}

	result := compactChatHistory(msgs)
	if len(result) != maxChatHistoryForLLM {
		t.Fatalf("len = %d, want %d", len(result), maxChatHistoryForLLM)
	}

	// First message is always preserved
	if result[0].Content != "msg-0" {
		t.Errorf("first message = %q, want 'msg-0'", result[0].Content)
	}

	// Last message is the tail
	lastIdx := total - 1
	if result[len(result)-1].Content != fmt.Sprintf("msg-%d", lastIdx) {
		t.Errorf("last message = %q, want 'msg-%d'", result[len(result)-1].Content, lastIdx)
	}

	// Messages in the middle are skipped (tail starts after the gap)
	tailStart := total - (maxChatHistoryForLLM - 1)
	if result[1].Content != fmt.Sprintf("msg-%d", tailStart) {
		t.Errorf("second message = %q, want 'msg-%d' (tail start)", result[1].Content, tailStart)
	}
}

// --- Complete appends chat history ---

func TestOrchestrator_Complete_AppendsChatHistory(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}
	gemini := mockGeminiWithJSON(RoutingDecision{
		NextAgent: "complete",
		Reasoning: "report ready",
	})
	orch := NewOrchestratorAgent(gemini, store, disp, "http://localhost:8080")

	seedSession(store, &Session{
		SessionID:   "sess-1",
		ChatHistory: []ChatMessage{{Role: "user", Content: "Analyze something"}},
	})
	seedJob(store, &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusQueued,
		ActiveAgent: AgentOrchestrator,
		FinalResult: "The final report content.",
	})

	err := orch.Execute(context.Background(), "job-1", "sess-1")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Verify report was appended to chat history
	sess, _ := store.GetSession(context.Background(), "sess-1")
	if len(sess.ChatHistory) != 2 {
		t.Fatalf("chat_history length = %d, want 2", len(sess.ChatHistory))
	}
	last := sess.ChatHistory[1]
	if last.Role != "assistant" || last.Content != "The final report content." {
		t.Errorf("last chat message = %+v, want assistant report", last)
	}
}

func TestOrchestrator_Complete_EmptyResult_NoAppend(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}
	// LLM says "complete" but validateDecision redirects to "report"
	// because FinalResult is empty.
	gemini := mockGeminiWithJSON(RoutingDecision{
		NextAgent: "complete",
		Reasoning: "done",
	})
	orch := NewOrchestratorAgent(gemini, store, disp, "http://localhost:8080")

	seedSession(store, &Session{
		SessionID:   "sess-1",
		ChatHistory: []ChatMessage{{Role: "user", Content: "hello"}},
	})
	seedJob(store, &Job{
		JobID:          "job-1",
		SessionID:      "sess-1",
		Status:         StatusQueued,
		ActiveAgent:    AgentOrchestrator,
		CollectedFacts: []Fact{{Key: "k", Value: "v"}},
		// No FinalResult — guardrail redirects to report
	})

	err := orch.Execute(context.Background(), "job-1", "sess-1")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Should NOT have appended anything (redirected to report, not complete)
	sess, _ := store.GetSession(context.Background(), "sess-1")
	if len(sess.ChatHistory) != 1 {
		t.Errorf("chat_history length = %d, want 1 (no report appended)", len(sess.ChatHistory))
	}
}
