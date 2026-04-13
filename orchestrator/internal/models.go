package internal

import (
	"time"
	"unicode/utf8"
)

// truncateRunes truncates s to at most maxRunes runes, appending "…" if truncated.
func truncateRunes(s string, maxRunes int) string {
	byteIdx := 0
	for i := 0; i < maxRunes && byteIdx < len(s); i++ {
		_, size := utf8.DecodeRuneInString(s[byteIdx:])
		byteIdx += size
	}
	if byteIdx >= len(s) {
		return s
	}
	return s[:byteIdx] + "…"
}

// --- Enumerations ---

type JobStatus string

const (
	StatusQueued     JobStatus = "QUEUED"
	StatusInProgress JobStatus = "IN_PROGRESS"
	StatusHITL       JobStatus = "HITL"
	StatusCompleted  JobStatus = "COMPLETED"
	StatusFailed     JobStatus = "FAILED"
)

type AgentType string

const (
	AgentOrchestrator AgentType = "orchestrator"
	AgentData         AgentType = "data"
	AgentAnalyst      AgentType = "analyst"
	AgentReport       AgentType = "report"
)

// --- Domain Models ---

type ChatMessage struct {
	Role    string `json:"role" firestore:"role"`
	Content string `json:"content" firestore:"content"`
}

type Fact struct {
	Key    string `json:"key" firestore:"key"`
	Value  string `json:"value" firestore:"value"`
	Source string `json:"source" firestore:"source"`
}

type Asset struct {
	Type    string `json:"type" firestore:"type"`
	Name    string `json:"name" firestore:"name"`
	Content string `json:"content" firestore:"content"`
}

type TaskPlan struct {
	ResearchQueries      []string `json:"research_queries" firestore:"research_queries"`
	AnalysisInstructions string   `json:"analysis_instructions" firestore:"analysis_instructions"`
	NeedsResearch        bool     `json:"needs_research" firestore:"needs_research"`
	NeedsAnalysis        bool     `json:"needs_analysis" firestore:"needs_analysis"`
}

type Job struct {
	JobID             string    `json:"job_id" firestore:"job_id"`
	SessionID         string    `json:"session_id" firestore:"session_id"`
	Status            JobStatus `json:"status" firestore:"status"`
	ActiveAgent       AgentType `json:"active_agent" firestore:"active_agent"`
	HopCount          int       `json:"hop_count" firestore:"hop_count"`
	Prompt            string    `json:"prompt" firestore:"prompt"`
	Plan              *TaskPlan `json:"plan,omitempty" firestore:"plan,omitempty"`
	CollectedFacts    []Fact    `json:"collected_facts" firestore:"collected_facts"`
	GeneratedAssets   []Asset   `json:"generated_assets" firestore:"generated_assets"`
	MissingQueries    []string  `json:"missing_queries" firestore:"missing_queries"`
	AgentInstructions string    `json:"agent_instructions" firestore:"agent_instructions"`
	LastAgentSummary  string    `json:"last_agent_summary" firestore:"last_agent_summary"`
	FinalResult       string    `json:"final_result" firestore:"final_result"`
	TokenUsage        TokenUsage `json:"token_usage" firestore:"token_usage"`
	CreatedAt         time.Time `json:"created_at" firestore:"created_at"`
	UpdatedAt         time.Time `json:"updated_at" firestore:"updated_at"`
}

type Session struct {
	SessionID   string        `json:"session_id" firestore:"session_id"`
	ChatHistory []ChatMessage `json:"chat_history" firestore:"chat_history"`
}

// --- API Types ---

type IngestRequest struct {
	Prompt    string `json:"prompt"`
	SessionID string `json:"session_id,omitempty"`
}

type ReplyRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"session_id"`
}

type IngestResponse struct {
	JobID     string `json:"job_id"`
	SessionID string `json:"session_id"`
}

type TaskPayload struct {
	JobID     string `json:"job_id"`
	SessionID string `json:"session_id"`
}

// --- Inter-service Types ---

type SandboxRequest struct {
	Code string `json:"code"`
}

type SandboxResponse struct {
	Success bool               `json:"success"`
	Stdout  string             `json:"stdout"`
	Error   string             `json:"error,omitempty"`
	Charts  []string           `json:"charts"`
	Timings map[string]float64 `json:"timings,omitempty"`
}

// --- Token & Audit Tracking ---

type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens" firestore:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens" firestore:"completion_tokens"`
	TotalTokens      int `json:"total_tokens" firestore:"total_tokens"`
}

func (t TokenUsage) Add(other TokenUsage) TokenUsage {
	return TokenUsage{
		PromptTokens:     t.PromptTokens + other.PromptTokens,
		CompletionTokens: t.CompletionTokens + other.CompletionTokens,
		TotalTokens:      t.TotalTokens + other.TotalTokens,
	}
}

type AuditEntry struct {
	Timestamp time.Time  `json:"timestamp" firestore:"timestamp"`
	Agent     AgentType  `json:"agent" firestore:"agent"`
	Action    string     `json:"action" firestore:"action"`
	Tokens    TokenUsage `json:"tokens" firestore:"tokens"`
	Detail    string     `json:"detail,omitempty" firestore:"detail,omitempty"`
}
