package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
)

// --- Mock Store (in-memory JobStore) ---

type mockStore struct {
	mu       sync.Mutex
	sessions map[string]*Session
	jobs     map[string]*Job
	audit    map[string][]AuditEntry

	// Error injection — set any of these to make the corresponding method fail.
	createSessionErr error
	getSessionErr    error
	appendChatErr    error
	createJobErr     error
	getJobErr        error
	updateJobErr     error
	incrementHopErr  error
	claimJobErr      error
	resumeHITLErr    error
	appendAuditErr   error
}

func newMockStore() *mockStore {
	return &mockStore{
		sessions: make(map[string]*Session),
		jobs:     make(map[string]*Job),
		audit:    make(map[string][]AuditEntry),
	}
}

func (m *mockStore) CreateSession(_ context.Context, sess *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createSessionErr != nil {
		return m.createSessionErr
	}
	cp := *sess
	cp.ChatHistory = append([]ChatMessage{}, sess.ChatHistory...)
	m.sessions[sess.SessionID] = &cp
	return nil
}

func (m *mockStore) GetSession(_ context.Context, sessionID string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getSessionErr != nil {
		return nil, m.getSessionErr
	}
	sess, ok := m.sessions[sessionID]
	if !ok {
		return nil, nil
	}
	cp := *sess
	cp.ChatHistory = append([]ChatMessage{}, sess.ChatHistory...)
	return &cp, nil
}

func (m *mockStore) AppendChatHistory(_ context.Context, sessionID string, msg ChatMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.appendChatErr != nil {
		return m.appendChatErr
	}
	sess, ok := m.sessions[sessionID]
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	sess.ChatHistory = append(sess.ChatHistory, msg)
	return nil
}

func (m *mockStore) CreateJob(_ context.Context, job *Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createJobErr != nil {
		return m.createJobErr
	}
	cp := *job
	cp.CreatedAt = time.Now()
	cp.UpdatedAt = cp.CreatedAt
	cp.CollectedFacts = append([]Fact{}, job.CollectedFacts...)
	cp.GeneratedAssets = append([]Asset{}, job.GeneratedAssets...)
	cp.MissingQueries = append([]string{}, job.MissingQueries...)
	m.jobs[job.JobID] = &cp
	return nil
}

func (m *mockStore) GetJob(_ context.Context, jobID, sessionID string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getJobErr != nil {
		return nil, m.getJobErr
	}
	job, ok := m.jobs[jobID]
	if !ok {
		return nil, nil
	}
	if job.SessionID != sessionID {
		return nil, nil // session isolation
	}
	cp := *job
	return &cp, nil
}

func (m *mockStore) UpdateJob(_ context.Context, jobID, sessionID string, updates []firestore.Update) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.updateJobErr != nil {
		return m.updateJobErr
	}
	job, ok := m.jobs[jobID]
	if !ok || job.SessionID != sessionID {
		return fmt.Errorf("job %s not found for session %s", jobID, sessionID)
	}
	applyMockUpdates(job, updates)
	job.UpdatedAt = time.Now()
	return nil
}

func applyMockUpdates(job *Job, updates []firestore.Update) {
	for _, u := range updates {
		switch u.Path {
		case "status":
			job.Status = u.Value.(JobStatus)
		case "active_agent":
			job.ActiveAgent = u.Value.(AgentType)
		case "final_result":
			job.FinalResult = u.Value.(string)
		case "agent_instructions":
			job.AgentInstructions = u.Value.(string)
		case "missing_queries":
			job.MissingQueries = u.Value.([]string)
		case "collected_facts":
			job.CollectedFacts = u.Value.([]Fact)
		case "generated_assets":
			job.GeneratedAssets = u.Value.([]Asset)
		case "last_agent_summary":
			job.LastAgentSummary = u.Value.(string)
		case "hop_count":
			job.HopCount = u.Value.(int)
		}
	}
}

func (m *mockStore) IncrementHopCount(_ context.Context, jobID, sessionID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.incrementHopErr != nil {
		return 0, m.incrementHopErr
	}
	job, ok := m.jobs[jobID]
	if !ok || job.SessionID != sessionID {
		return 0, fmt.Errorf("job %s not found for session %s", jobID, sessionID)
	}
	job.HopCount++
	job.UpdatedAt = time.Now()
	return job.HopCount, nil
}

func (m *mockStore) ClaimQueuedJob(_ context.Context, jobID, sessionID string, agent AgentType) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.claimJobErr != nil {
		return nil, m.claimJobErr
	}
	job, ok := m.jobs[jobID]
	if !ok || job.SessionID != sessionID {
		return nil, nil
	}
	if job.Status != StatusQueued || job.ActiveAgent != agent {
		return nil, nil // precondition failed
	}
	job.Status = StatusInProgress
	job.UpdatedAt = time.Now()
	cp := *job
	return &cp, nil
}

func (m *mockStore) ResumeHITLJob(_ context.Context, jobID, sessionID string) (*Job, ResumeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.resumeHITLErr != nil {
		return nil, ResumeNotFound, m.resumeHITLErr
	}
	job, ok := m.jobs[jobID]
	if !ok || job.SessionID != sessionID {
		return nil, ResumeNotFound, nil
	}
	if job.Status != StatusHITL {
		return nil, ResumeNotHITL, nil
	}
	job.Status = StatusQueued
	job.ActiveAgent = AgentOrchestrator
	job.HopCount = 0
	job.FinalResult = ""
	job.AgentInstructions = ""
	job.UpdatedAt = time.Now()
	cp := *job
	return &cp, ResumeSucceeded, nil
}

func (m *mockStore) AppendAuditLog(_ context.Context, jobID, sessionID string, entry AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.appendAuditErr != nil {
		return m.appendAuditErr
	}
	job, ok := m.jobs[jobID]
	if !ok || job.SessionID != sessionID {
		return fmt.Errorf("job %s not found for session %s", jobID, sessionID)
	}
	entry.Timestamp = time.Now()
	m.audit[jobID] = append(m.audit[jobID], entry)
	if entry.Tokens.TotalTokens > 0 {
		job.TokenUsage = job.TokenUsage.Add(entry.Tokens)
	}
	return nil
}

// --- Mock Dispatcher ---

type enqueuedTask struct {
	URL       string
	JobID     string
	SessionID string
}

type mockDispatcher struct {
	mu       sync.Mutex
	enqueued []enqueuedTask
	err      error
}

func (m *mockDispatcher) Enqueue(_ context.Context, targetURL, jobID, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.enqueued = append(m.enqueued, enqueuedTask{URL: targetURL, JobID: jobID, SessionID: sessionID})
	return nil
}

// --- Mock LLM Client ---

type mockGemini struct {
	// Function overrides — set these for per-test control.
	generateJSONFn func(ctx context.Context, system, prompt string, out any) (TokenUsage, error)
	generateTextFn func(ctx context.Context, system, prompt string) (string, TokenUsage, error)
	searchWebFn    func(ctx context.Context, query string) ([]SearchResult, string, TokenUsage, error)

	// Default return values (used when function overrides are nil).
	textResponse  string
	searchResults []SearchResult
	searchSummary string
	usage         TokenUsage
	err           error
}

func (m *mockGemini) GenerateJSON(ctx context.Context, system, prompt string, out any) (TokenUsage, error) {
	if m.generateJSONFn != nil {
		return m.generateJSONFn(ctx, system, prompt, out)
	}
	return m.usage, m.err
}

func (m *mockGemini) GenerateText(ctx context.Context, system, prompt string) (string, TokenUsage, error) {
	if m.generateTextFn != nil {
		return m.generateTextFn(ctx, system, prompt)
	}
	return m.textResponse, m.usage, m.err
}

func (m *mockGemini) SearchWeb(ctx context.Context, query string) ([]SearchResult, string, TokenUsage, error) {
	if m.searchWebFn != nil {
		return m.searchWebFn(ctx, query)
	}
	return m.searchResults, m.searchSummary, m.usage, m.err
}

// --- Test Helpers ---

// seedJob creates a job in the mock store and returns it.
func seedJob(store *mockStore, job *Job) {
	store.mu.Lock()
	defer store.mu.Unlock()
	cp := *job
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now()
	}
	cp.UpdatedAt = cp.CreatedAt
	store.jobs[cp.JobID] = &cp
}

// seedSession creates a session in the mock store.
func seedSession(store *mockStore, sess *Session) {
	store.mu.Lock()
	defer store.mu.Unlock()
	cp := *sess
	cp.ChatHistory = append([]ChatMessage{}, sess.ChatHistory...)
	store.sessions[cp.SessionID] = &cp
}

// mockGeminiWithJSON creates a mockGemini that returns the given value from GenerateJSON.
func mockGeminiWithJSON(v any) *mockGemini {
	return &mockGemini{
		generateJSONFn: func(_ context.Context, _, _ string, out any) (TokenUsage, error) {
			data, _ := json.Marshal(v)
			return TokenUsage{TotalTokens: 10}, json.Unmarshal(data, out)
		},
	}
}
