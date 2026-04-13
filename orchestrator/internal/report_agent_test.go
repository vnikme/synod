package internal

import (
	"context"
	"testing"
)

func TestReportAgent_Success(t *testing.T) {
	store := newMockStore()
	gemini := &mockGemini{
		textResponse: "# Executive Summary\n\nTesla revenue grew 15% YoY to $96.77B in 2023.\n\n# Key Findings\n- Revenue: $96.77B",
		usage:        TokenUsage{PromptTokens: 200, CompletionTokens: 100, TotalTokens: 300},
	}
	agent := NewReportAgent(gemini, store)

	seedSession(store, &Session{
		SessionID:   "sess-1",
		ChatHistory: []ChatMessage{{Role: "user", Content: "Analyze Tesla"}},
	})
	seedJob(store, &Job{
		JobID:     "job-1",
		SessionID: "sess-1",
		Status:    StatusInProgress,
		Prompt:    "Analyze Tesla revenue",
		CollectedFacts: []Fact{
			{Key: "Revenue 2023", Value: "$96.77B", Source: "SEC EDGAR"},
		},
		GeneratedAssets: []Asset{
			{Type: "chart", Name: "chart_1.png", Content: "base64"},
			{Type: "analysis_output", Name: "analysis.txt", Content: "Revenue grew 15%"},
		},
	})

	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	sess, _ := store.GetSession(context.Background(), "sess-1")

	usage, err := agent.Execute(context.Background(), job, sess, "Write comprehensive report")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if usage.TotalTokens != 300 {
		t.Errorf("usage.TotalTokens = %d, want 300", usage.TotalTokens)
	}

	// Verify report was written to final_result
	updated, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	if updated.FinalResult == "" {
		t.Error("expected final_result to be populated")
	}
	if updated.LastAgentSummary == "" {
		t.Error("expected last_agent_summary to be populated")
	}

	// Note: chat_history append is now done by the orchestrator's "complete"
	// handler, not the report agent. This prevents duplicate reports when the
	// orchestrator sends the report back for revision.
	updatedSess, _ := store.GetSession(context.Background(), "sess-1")
	if len(updatedSess.ChatHistory) != 1 {
		t.Errorf("chat_history length = %d, want 1 (report agent should NOT append)", len(updatedSess.ChatHistory))
	}
}

func TestReportAgent_LLMError(t *testing.T) {
	store := newMockStore()
	gemini := &mockGemini{
		err: context.DeadlineExceeded,
	}
	agent := NewReportAgent(gemini, store)

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:     "job-1",
		SessionID: "sess-1",
		Status:    StatusInProgress,
	})

	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	sess, _ := store.GetSession(context.Background(), "sess-1")

	_, err := agent.Execute(context.Background(), job, sess, "write report")
	if err == nil {
		t.Fatal("expected error on LLM failure")
	}
}

func TestReportAgent_NilSession(t *testing.T) {
	store := newMockStore()
	gemini := &mockGemini{
		textResponse: "Report without session context.",
		usage:        TokenUsage{TotalTokens: 50},
	}
	agent := NewReportAgent(gemini, store)

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:     "job-1",
		SessionID: "sess-1",
		Status:    StatusInProgress,
		Prompt:    "Test prompt",
	})

	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")

	// Execute with nil session — should not panic
	usage, err := agent.Execute(context.Background(), job, nil, "write report")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if usage.TotalTokens != 50 {
		t.Errorf("usage.TotalTokens = %d, want 50", usage.TotalTokens)
	}
}

func TestReportAgent_DoesNotSetTerminalState(t *testing.T) {
	store := newMockStore()
	gemini := &mockGemini{
		textResponse: "A report.",
		usage:        TokenUsage{TotalTokens: 10},
	}
	agent := NewReportAgent(gemini, store)

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:     "job-1",
		SessionID: "sess-1",
		Status:    StatusInProgress,
		Prompt:    "Test",
	})

	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	sess, _ := store.GetSession(context.Background(), "sess-1")
	agent.Execute(context.Background(), job, sess, "report")

	// Report agent should NOT set terminal state — orchestrator decides
	updated, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	if updated.Status == StatusCompleted || updated.Status == StatusFailed {
		t.Errorf("report agent should not set terminal state, got %s", updated.Status)
	}
}
