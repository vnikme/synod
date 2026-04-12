package internal

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Store struct {
	client *firestore.Client
}

func NewStore(ctx context.Context, projectID string) (*Store, error) {
	client, err := firestore.NewClient(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("firestore.NewClient: %w", err)
	}
	return &Store{client: client}, nil
}

func (s *Store) Close() error {
	return s.client.Close()
}

// --- Sessions ---

func (s *Store) CreateSession(ctx context.Context, sess *Session) error {
	_, err := s.client.Collection("sessions").Doc(sess.SessionID).Set(ctx, sess)
	return err
}

func (s *Store) GetSession(ctx context.Context, sessionID string) (*Session, error) {
	doc, err := s.client.Collection("sessions").Doc(sessionID).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, nil
		}
		return nil, err
	}
	var sess Session
	if err := doc.DataTo(&sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

func (s *Store) AppendChatHistory(ctx context.Context, sessionID string, msg ChatMessage) error {
	ref := s.client.Collection("sessions").Doc(sessionID)
	return s.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		doc, err := tx.Get(ref)
		if err != nil {
			return err
		}
		var sess Session
		if err := doc.DataTo(&sess); err != nil {
			return err
		}
		sess.ChatHistory = append(sess.ChatHistory, msg)
		return tx.Update(ref, []firestore.Update{
			{Path: "chat_history", Value: sess.ChatHistory},
		})
	})
}

// --- Jobs ---

func (s *Store) CreateJob(ctx context.Context, job *Job) error {
	job.CreatedAt = time.Now()
	job.UpdatedAt = job.CreatedAt
	_, err := s.client.Collection("jobs").Doc(job.JobID).Set(ctx, job)
	return err
}

func (s *Store) GetJob(ctx context.Context, jobID, sessionID string) (*Job, error) {
	doc, err := s.client.Collection("jobs").Doc(jobID).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, nil
		}
		return nil, err
	}
	var job Job
	if err := doc.DataTo(&job); err != nil {
		return nil, err
	}
	if job.SessionID != sessionID {
		return nil, nil // session isolation
	}
	return &job, nil
}

func (s *Store) UpdateJob(ctx context.Context, jobID, sessionID string, updates []firestore.Update) error {
	ref := s.client.Collection("jobs").Doc(jobID)
	return s.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		doc, err := tx.Get(ref)
		if err != nil {
			return err
		}
		var job Job
		if err := doc.DataTo(&job); err != nil {
			return err
		}
		if job.SessionID != sessionID {
			return fmt.Errorf("job %s not found for session %s", jobID, sessionID)
		}
		updates = append(updates, firestore.Update{Path: "updated_at", Value: time.Now()})
		return tx.Update(ref, updates)
	})
}

// IncrementHopCount atomically increments the hop count and returns the new value.
// This prevents race conditions from concurrent Cloud Tasks deliveries.
func (s *Store) IncrementHopCount(ctx context.Context, jobID, sessionID string) (int, error) {
	ref := s.client.Collection("jobs").Doc(jobID)
	var newHop int
	err := s.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		doc, err := tx.Get(ref)
		if err != nil {
			return err
		}
		var job Job
		if err := doc.DataTo(&job); err != nil {
			return err
		}
		if job.SessionID != sessionID {
			return fmt.Errorf("job %s not found for session %s", jobID, sessionID)
		}
		newHop = job.HopCount + 1
		return tx.Update(ref, []firestore.Update{
			{Path: "hop_count", Value: newHop},
			{Path: "active_agent", Value: AgentOrchestrator},
			{Path: "updated_at", Value: time.Now()},
		})
	})
	return newHop, err
}

// ClaimQueuedJob atomically transitions a job from QUEUED with the expected
// active_agent to IN_PROGRESS. Returns the job if the claim succeeds, or nil
// if the precondition failed (stale/duplicate delivery). This is a
// compare-and-swap to prevent duplicate Cloud Tasks deliveries from both
// executing the same agent.
func (s *Store) ClaimQueuedJob(ctx context.Context, jobID, sessionID string, agent AgentType) (*Job, error) {
	ref := s.client.Collection("jobs").Doc(jobID)
	var claimed *Job
	err := s.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		claimed = nil // reset on retry
		doc, err := tx.Get(ref)
		if err != nil {
			return err
		}
		var job Job
		if err := doc.DataTo(&job); err != nil {
			return err
		}
		if job.SessionID != sessionID {
			return fmt.Errorf("job %s not found for session %s", jobID, sessionID)
		}
		if job.Status != StatusQueued || job.ActiveAgent != agent {
			// Precondition failed — another delivery already claimed this job
			return nil
		}
		if err := tx.Update(ref, []firestore.Update{
			{Path: "status", Value: StatusInProgress},
			{Path: "updated_at", Value: time.Now()},
		}); err != nil {
			return err
		}
		job.Status = StatusInProgress
		claimed = &job
		return nil
	})
	return claimed, err
}

// --- Audit Log ---

// AppendAuditLog writes an audit entry to the jobs/{jobID}/audit subcollection
// and atomically accumulates token_usage on the job document (when tokens > 0).
// All writes happen inside a single transaction for consistency and session isolation.
func (s *Store) AppendAuditLog(ctx context.Context, jobID, sessionID string, entry AuditEntry) error {
	entry.Timestamp = time.Now()

	jobRef := s.client.Collection("jobs").Doc(jobID)
	auditRef := jobRef.Collection("audit").NewDoc()

	return s.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		doc, err := tx.Get(jobRef)
		if err != nil {
			return err
		}
		var job Job
		if err := doc.DataTo(&job); err != nil {
			return err
		}
		if job.SessionID != sessionID {
			return fmt.Errorf("job %s not found for session %s", jobID, sessionID)
		}

		// Write audit entry
		if err := tx.Set(auditRef, entry); err != nil {
			return err
		}

		// Accumulate token usage if any
		if entry.Tokens.TotalTokens > 0 {
			newUsage := job.TokenUsage.Add(entry.Tokens)
			return tx.Update(jobRef, []firestore.Update{
				{Path: "token_usage", Value: newUsage},
				{Path: "updated_at", Value: time.Now()},
			})
		}
		return nil
	})
}
