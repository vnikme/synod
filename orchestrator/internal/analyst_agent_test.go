package internal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStripCodeFences(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"no fences", "print('hello')", "print('hello')"},
		{"python fences", "```python\nprint('hello')\n```", "print('hello')"},
		{"generic fences", "```\nprint('hello')\n```", "print('hello')"},
		{"leading whitespace", "  ```python\nprint('hello')\n```  ", "print('hello')"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripCodeFences(tt.input)
			if got != tt.expected {
				t.Errorf("stripCodeFences(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestAnalystAgent_Success(t *testing.T) {
	store := newMockStore()

	// Mock sandbox server
	sandbox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(SandboxResponse{
			Success: true,
			Stdout:  "Revenue: $96.77B",
			Charts:  []string{"base64chart1"},
		})
	}))
	defer sandbox.Close()

	gemini := &mockGemini{
		textResponse: "print('Revenue: $96.77B')",
		usage:        TokenUsage{PromptTokens: 50, CompletionTokens: 30, TotalTokens: 80},
	}
	agent := &AnalystAgent{
		gemini:     gemini,
		store:      store,
		sandboxURL: sandbox.URL,
		http:       &http.Client{Timeout: 10 * time.Second},
	}

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:     "job-1",
		SessionID: "sess-1",
		Status:    StatusInProgress,
		Prompt:    "Analyze Tesla revenue",
		CollectedFacts: []Fact{
			{Key: "Revenue 2023", Value: "$96.77B", Source: "SEC"},
		},
	})

	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	usage, err := agent.Execute(context.Background(), job, "Create revenue analysis")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if usage.TotalTokens == 0 {
		t.Error("expected non-zero token usage")
	}

	updated, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	if len(updated.GeneratedAssets) == 0 {
		t.Fatal("expected generated_assets to be populated")
	}

	// Should have both chart and analysis_output assets
	hasChart, hasOutput := false, false
	for _, a := range updated.GeneratedAssets {
		if a.Type == "chart" {
			hasChart = true
		}
		if a.Type == "analysis_output" {
			hasOutput = true
		}
	}
	if !hasChart {
		t.Error("expected a chart asset")
	}
	if !hasOutput {
		t.Error("expected an analysis_output asset")
	}
}

func TestAnalystAgent_SandboxRetry(t *testing.T) {
	store := newMockStore()

	callCount := 0
	sandbox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount <= 2 {
			// First two attempts fail
			json.NewEncoder(w).Encode(SandboxResponse{
				Success: false,
				Error:   "NameError: name 'df' is not defined",
			})
			return
		}
		// Third attempt succeeds
		json.NewEncoder(w).Encode(SandboxResponse{
			Success: true,
			Stdout:  "Fixed output",
		})
	}))
	defer sandbox.Close()

	textCallCount := 0
	gemini := &mockGemini{}
	gemini.generateTextFn = func(_ context.Context, _, _ string) (string, TokenUsage, error) {
		textCallCount++
		return "print('code attempt')", TokenUsage{TotalTokens: 10}, nil
	}

	agent := &AnalystAgent{
		gemini:     gemini,
		store:      store,
		sandboxURL: sandbox.URL,
		http:       &http.Client{Timeout: 10 * time.Second},
	}

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:     "job-1",
		SessionID: "sess-1",
		Status:    StatusInProgress,
		Prompt:    "analyze data",
	})

	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	_, err := agent.Execute(context.Background(), job, "analyze")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Should have retried: 3 sandbox calls, 3 LLM calls
	if callCount != 3 {
		t.Errorf("sandbox calls = %d, want 3", callCount)
	}
	if textCallCount != 3 {
		t.Errorf("LLM calls = %d, want 3", textCallCount)
	}
}

func TestAnalystAgent_AllRetriesFail(t *testing.T) {
	store := newMockStore()

	sandbox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(SandboxResponse{
			Success: false,
			Error:   "persistent error",
		})
	}))
	defer sandbox.Close()

	gemini := &mockGemini{textResponse: "bad code"}
	agent := &AnalystAgent{
		gemini:     gemini,
		store:      store,
		sandboxURL: sandbox.URL,
		http:       &http.Client{Timeout: 10 * time.Second},
	}

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:     "job-1",
		SessionID: "sess-1",
		Status:    StatusInProgress,
	})

	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	_, err := agent.Execute(context.Background(), job, "analyze")
	if err == nil {
		t.Fatal("expected error after all retries fail")
	}
	if !strings.Contains(err.Error(), "code execution failed") {
		t.Errorf("error = %q, want to contain 'code execution failed'", err.Error())
	}
}

func TestAnalystAgent_SandboxHTTPError(t *testing.T) {
	store := newMockStore()

	sandbox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer sandbox.Close()

	gemini := &mockGemini{textResponse: "print('hello')"}
	agent := &AnalystAgent{
		gemini:     gemini,
		store:      store,
		sandboxURL: sandbox.URL,
		http:       &http.Client{Timeout: 10 * time.Second},
	}

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:     "job-1",
		SessionID: "sess-1",
		Status:    StatusInProgress,
	})

	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	_, err := agent.Execute(context.Background(), job, "analyze")
	if err == nil {
		t.Fatal("expected error on sandbox HTTP failure")
	}
}


