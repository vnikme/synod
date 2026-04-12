package internal

import "time"

type JobStatus string

const (
	StatusQueued      JobStatus = "QUEUED"
	StatusInProgress  JobStatus = "IN_PROGRESS"
	StatusNeedsCtx    JobStatus = "NEEDS_CONTEXT"
	StatusHITL        JobStatus = "HITL"
	StatusCompleted   JobStatus = "COMPLETED"
	StatusFailed      JobStatus = "FAILED"
)

type AgentType string

const (
	AgentOrchestrator AgentType = "orchestrator"
	AgentResearcher   AgentType = "researcher"
	AgentAnalyst      AgentType = "analyst"
)

type ChatMessage struct {
	Role    string `firestore:"role"    json:"role"`
	Content string `firestore:"content" json:"content"`
}

type Session struct {
	SessionID   string        `firestore:"session_id"   json:"session_id"`
	ChatHistory []ChatMessage `firestore:"chat_history" json:"chat_history"`
	CreatedAt   time.Time     `firestore:"created_at"   json:"created_at"`
	UpdatedAt   time.Time     `firestore:"updated_at"   json:"updated_at"`
}

type Fact struct {
	Key    string `firestore:"key"    json:"key"`
	Value  string `firestore:"value"  json:"value"`
	Source string `firestore:"source" json:"source"`
}

type Asset struct {
	Type string `firestore:"type" json:"type"`
	Data string `firestore:"data" json:"data"`
}

type Job struct {
	JobID           string    `firestore:"job_id"           json:"job_id"`
	SessionID       string    `firestore:"session_id"       json:"session_id"`
	Status          JobStatus `firestore:"status"           json:"status"`
	ActiveAgent     AgentType `firestore:"active_agent"     json:"active_agent"`
	HopCount        int       `firestore:"hop_count"        json:"hop_count"`
	Prompt          string    `firestore:"prompt"           json:"prompt"`
	CollectedFacts  []Fact    `firestore:"collected_facts"  json:"collected_facts"`
	GeneratedAssets []Asset   `firestore:"generated_assets" json:"generated_assets"`
	MissingQueries  []string  `firestore:"missing_queries"  json:"missing_queries"`
	FinalResult     string    `firestore:"final_result"     json:"final_result"`
	CreatedAt       time.Time `firestore:"created_at"       json:"created_at"`
	UpdatedAt       time.Time `firestore:"updated_at"       json:"updated_at"`
}

type TaskPayload struct {
	JobID     string `json:"job_id"`
	SessionID string `json:"session_id"`
}

type IngestRequest struct {
	SessionID string `json:"session_id,omitempty"`
	Prompt    string `json:"prompt"`
}

type IngestResponse struct {
	JobID     string `json:"job_id"`
	SessionID string `json:"session_id"`
}

type TaskPlan struct {
	ResearchQueries      []string `json:"research_queries"`
	AnalysisInstructions string   `json:"analysis_instructions"`
	NeedsResearch        bool     `json:"needs_research"`
	NeedsAnalysis        bool     `json:"needs_analysis"`
}
