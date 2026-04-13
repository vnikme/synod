package internal

import "testing"

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		max      int
		expected string
	}{
		{"empty", "", 10, ""},
		{"short", "hello", 10, "hello"},
		{"exact", "hello", 5, "hello"},
		{"over", "hello world", 5, "hello…"},
		{"unicode", "héllo wörld", 5, "héllo…"},
		{"zero_max", "hello", 0, "…"},
		{"single_char", "h", 1, "h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateRunes(tt.input, tt.max)
			if got != tt.expected {
				t.Errorf("truncateRunes(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.expected)
			}
		})
	}
}

func TestTokenUsageAdd(t *testing.T) {
	a := TokenUsage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30}
	b := TokenUsage{PromptTokens: 5, CompletionTokens: 15, TotalTokens: 20}
	got := a.Add(b)
	expected := TokenUsage{PromptTokens: 15, CompletionTokens: 35, TotalTokens: 50}
	if got != expected {
		t.Errorf("Add() = %+v, want %+v", got, expected)
	}

	// Add zero
	zero := TokenUsage{}
	if got := a.Add(zero); got != a {
		t.Errorf("Add(zero) = %+v, want %+v", got, a)
	}

	// Commutativity
	if a.Add(b) != b.Add(a) {
		t.Error("Add is not commutative")
	}
}

func TestJobStatusConstants(t *testing.T) {
	// Verify enum values match expected strings for API contracts.
	if StatusQueued != "QUEUED" {
		t.Errorf("StatusQueued = %q", StatusQueued)
	}
	if StatusCompleted != "COMPLETED" {
		t.Errorf("StatusCompleted = %q", StatusCompleted)
	}
	if StatusHITL != "HITL" {
		t.Errorf("StatusHITL = %q", StatusHITL)
	}
	if StatusFailed != "FAILED" {
		t.Errorf("StatusFailed = %q", StatusFailed)
	}
}

func TestAgentTypeConstants(t *testing.T) {
	if AgentOrchestrator != "orchestrator" {
		t.Errorf("AgentOrchestrator = %q", AgentOrchestrator)
	}
	if AgentData != "data" {
		t.Errorf("AgentData = %q", AgentData)
	}
	if AgentAnalyst != "analyst" {
		t.Errorf("AgentAnalyst = %q", AgentAnalyst)
	}
	if AgentReport != "report" {
		t.Errorf("AgentReport = %q", AgentReport)
	}
}
