package internal

import (
	"context"

	"cloud.google.com/go/firestore"
)

// JobStore abstracts the persistent storage layer for sessions and jobs.
type JobStore interface {
	CreateSession(ctx context.Context, sess *Session) error
	GetSession(ctx context.Context, sessionID string) (*Session, error)
	AppendChatHistory(ctx context.Context, sessionID string, msg ChatMessage) error
	CreateJob(ctx context.Context, job *Job) error
	GetJob(ctx context.Context, jobID, sessionID string) (*Job, error)
	UpdateJob(ctx context.Context, jobID, sessionID string, updates []firestore.Update) error
	IncrementHopCount(ctx context.Context, jobID, sessionID string) (int, error)
	ClaimQueuedJob(ctx context.Context, jobID, sessionID string, agent AgentType) (*Job, error)
	ResumeHITLJob(ctx context.Context, jobID, sessionID string) (*Job, ResumeResult, error)
	AppendAuditLog(ctx context.Context, jobID, sessionID string, entry AuditEntry) error
}

// TaskDispatcher abstracts the async task queue for enqueuing Cloud Tasks.
type TaskDispatcher interface {
	Enqueue(ctx context.Context, targetURL, jobID, sessionID string) error
}

// LLMClient abstracts the LLM interaction layer.
type LLMClient interface {
	GenerateJSON(ctx context.Context, system, prompt string, out any) (TokenUsage, error)
	GenerateText(ctx context.Context, system, prompt string) (string, TokenUsage, error)
	SearchWeb(ctx context.Context, query string) ([]SearchResult, string, TokenUsage, error)
}
