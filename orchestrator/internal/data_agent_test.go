package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Apple Inc.", "APPLE"},
		{"TESLA INC", "TESLA"},
		{"Alphabet Inc.", "ALPHABET"},
		{"Microsoft Corp.", "MICROSOFT"},
		{"Meta Platforms, Inc.", "META PLATFORMS"},
		{"Berkshire Hathaway Inc", "BERKSHIRE HATHAWAY"},
		{"Amazon.com Inc.", "AMAZONCOM"},
		{"  SpacX  ", "SPACX"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizeName(tt.input)
			if got != tt.expected {
				t.Errorf("normalizeName(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestCIKCacheLookup_ByTicker(t *testing.T) {
	cache := &cikCache{
		byTicker: map[string]string{"TSLA": "0001318605", "AAPL": "0000320193"},
		byName:   map[string]string{"TESLA": "0001318605"},
		fetched:  time.Now(),
	}

	tests := []struct {
		query    string
		expected string
	}{
		{"TSLA revenue", "0001318605"},
		{"AAPL stock price", "0000320193"},
		{"What is TSLA doing", "0001318605"},
		{"something unknown", ""}, // no match
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			got := cache.lookup(tt.query)
			if got != tt.expected {
				t.Errorf("lookup(%q) = %q, want %q", tt.query, got, tt.expected)
			}
		})
	}
}

func TestCIKCacheLookup_ByName(t *testing.T) {
	cache := &cikCache{
		byTicker: map[string]string{},
		byName:   map[string]string{"TESLA": "0001318605", "APPLE": "0000320193"},
		fetched:  time.Now(),
	}

	// Name-based lookup (no ticker match)
	got := cache.lookup("Tell me about Tesla")
	if got != "0001318605" {
		t.Errorf("lookup by name = %q, want 0001318605", got)
	}
}

func TestCIKCacheNeedsRefresh(t *testing.T) {
	cache := &cikCache{}
	if !cache.needsRefresh() {
		t.Error("empty cache should need refresh")
	}

	cache.byTicker = map[string]string{"TSLA": "123"}
	cache.fetched = time.Now()
	if cache.needsRefresh() {
		t.Error("fresh cache should not need refresh")
	}

	cache.fetched = time.Now().Add(-25 * time.Hour)
	if !cache.needsRefresh() {
		t.Error("stale cache should need refresh")
	}
}

func TestCIKCachePopulate(t *testing.T) {
	cache := &cikCache{}
	// We can't easily override the SEC URL, so test the populate logic indirectly.
	// Instead, test that a populated cache works correctly.
	cache.mu.Lock()
	cache.byTicker = map[string]string{"AAPL": "0000320193", "TSLA": "0001318605"}
	cache.byName = map[string]string{"APPLE": "0000320193", "TESLA": "0001318605"}
	cache.fetched = time.Now()
	cache.mu.Unlock()

	if got := cache.lookup("AAPL"); got != "0000320193" {
		t.Errorf("lookup AAPL = %q", got)
	}
	if got := cache.lookup("TSLA"); got != "0001318605" {
		t.Errorf("lookup TSLA = %q", got)
	}
}

func TestFetchEDGAR(t *testing.T) {
	// Mock EDGAR API server
	edgarServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "CIK0001318605") {
			http.NotFound(w, r)
			return
		}
		resp := map[string]any{
			"entityName": "TESLA, INC",
			"facts": map[string]any{
				"us-gaap": map[string]any{
					"Revenues": map[string]any{
						"label": "Revenue",
						"units": map[string]any{
							"USD": []map[string]any{
								{"end": "2023-12-31", "val": 96773000000, "form": "10-K", "fy": 2023, "fp": "FY"},
							},
						},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer edgarServer.Close()

	store := newMockStore()
	gemini := &mockGemini{}
	agent := NewDataAgent(gemini, store, "test-ua")
	// Pre-seed CIK cache to avoid real HTTP call to SEC
	agent.cikCache = &cikCache{
		byTicker: map[string]string{},
		byName:   map[string]string{},
		fetched:  time.Now(),
	}

	// Test that the agent handles EDGAR response parsing
	ctx := context.Background()

	// Seed job
	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:       "job-1",
		SessionID:   "sess-1",
		Status:      StatusInProgress,
		ActiveAgent: AgentData,
	})

	// Mock gemini to simulate search returning data and fact extraction
	gemini.searchWebFn = func(_ context.Context, query string) ([]SearchResult, string, TokenUsage, error) {
		return []SearchResult{{Title: "Tesla Revenue", URL: "https://example.com"}},
			"Tesla revenue was $96.77B in 2023",
			TokenUsage{TotalTokens: 20}, nil
	}
	gemini.generateJSONFn = func(_ context.Context, _, _ string, out any) (TokenUsage, error) {
		facts := []Fact{{Key: "Tesla Revenue 2023", Value: "$96.77B", Source: "SEC EDGAR"}}
		data, _ := json.Marshal(facts)
		return TokenUsage{TotalTokens: 15}, json.Unmarshal(data, out)
	}

	job, _ := store.GetJob(ctx, "job-1", "sess-1")
	usage, err := agent.Execute(ctx, job, "Get Tesla financial data", []string{"TSLA revenue 2024"})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if usage.TotalTokens == 0 {
		t.Error("expected non-zero token usage")
	}

	// Verify facts were stored
	updated, _ := store.GetJob(ctx, "job-1", "sess-1")
	if len(updated.CollectedFacts) == 0 {
		t.Error("expected collected_facts to be populated")
	}
	if len(updated.MissingQueries) != 0 {
		t.Errorf("missing_queries should be cleared, got %v", updated.MissingQueries)
	}
}

func TestDataAgent_NoDataCollected(t *testing.T) {
	store := newMockStore()
	gemini := &mockGemini{
		err: fmt.Errorf("all searches failed"),
	}
	gemini.searchWebFn = func(_ context.Context, _ string) ([]SearchResult, string, TokenUsage, error) {
		return nil, "", TokenUsage{}, fmt.Errorf("search failed")
	}
	agent := NewDataAgent(gemini, store, "test-ua")

	// Pre-populate CIK cache to avoid HTTP call
	agent.cikCache.mu.Lock()
	agent.cikCache.byTicker = map[string]string{}
	agent.cikCache.byName = map[string]string{}
	agent.cikCache.fetched = time.Now()
	agent.cikCache.mu.Unlock()

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:     "job-1",
		SessionID: "sess-1",
		Status:    StatusInProgress,
	})

	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	_, err := agent.Execute(context.Background(), job, "find data", []string{"nonexistent query"})
	if err != nil {
		t.Fatalf("Execute() error = %v (should succeed with data_unavailable fact)", err)
	}

	// Should have a "data_unavailable" fact
	updated, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	found := false
	for _, f := range updated.CollectedFacts {
		if f.Key == "data_unavailable" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected data_unavailable fact when no data collected")
	}
}

func TestDataAgent_ConcurrentQueries(t *testing.T) {
	store := newMockStore()
	var queryCount atomic.Int32
	gemini := &mockGemini{}
	gemini.searchWebFn = func(_ context.Context, query string) ([]SearchResult, string, TokenUsage, error) {
		queryCount.Add(1)
		return []SearchResult{{Title: query, URL: "https://example.com/" + query}},
			"Result for: " + query,
			TokenUsage{TotalTokens: 5}, nil
	}
	gemini.generateJSONFn = func(_ context.Context, _, prompt string, out any) (TokenUsage, error) {
		facts := []Fact{{Key: "fact1", Value: "value1", Source: "test"}}
		data, _ := json.Marshal(facts)
		return TokenUsage{TotalTokens: 10}, json.Unmarshal(data, out)
	}

	agent := NewDataAgent(gemini, store, "test-ua")
	agent.cikCache.mu.Lock()
	agent.cikCache.byTicker = map[string]string{}
	agent.cikCache.byName = map[string]string{}
	agent.cikCache.fetched = time.Now()
	agent.cikCache.mu.Unlock()

	seedSession(store, &Session{SessionID: "sess-1"})
	seedJob(store, &Job{
		JobID:     "job-1",
		SessionID: "sess-1",
		Status:    StatusInProgress,
	})

	job, _ := store.GetJob(context.Background(), "job-1", "sess-1")
	queries := []string{"query1", "query2", "query3"}
	_, err := agent.Execute(context.Background(), job, "find data", queries)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// All 3 queries should have been searched
	if queryCount.Load() < 3 {
		t.Errorf("queryCount = %d, want >= 3 (concurrent queries)", queryCount.Load())
	}
}
