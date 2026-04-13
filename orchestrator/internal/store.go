package internal

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
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
			if status.Code(err) == codes.NotFound {
				return nil // job doesn't exist — return nil, nil (ACK)
			}
			return err
		}
		var job Job
		if err := doc.DataTo(&job); err != nil {
			return err
		}
		if job.SessionID != sessionID {
			return nil // session mismatch — treat as not-found (ACK)
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

// ResumeResult distinguishes the outcomes of ResumeHITLJob.
type ResumeResult int

const (
	ResumeNotFound  ResumeResult = iota // job missing or session mismatch
	ResumeNotHITL                       // job exists but not in HITL state
	ResumeSucceeded                     // transition succeeded
)

// ResumeHITLJob atomically transitions a job from HITL to QUEUED+orchestrator,
// resets hop_count and clears final_result. Returns (job, ResumeSucceeded) on
// success, (nil, ResumeNotFound) if the job is missing/session mismatch, or
// (nil, ResumeNotHITL) if the job exists but is not in HITL state.
func (s *Store) ResumeHITLJob(ctx context.Context, jobID, sessionID string) (*Job, ResumeResult, error) {
	ref := s.client.Collection("jobs").Doc(jobID)
	var resumed *Job
	result := ResumeNotFound
	err := s.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		resumed = nil
		result = ResumeNotFound
		doc, err := tx.Get(ref)
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return nil
			}
			return err
		}
		var job Job
		if err := doc.DataTo(&job); err != nil {
			return err
		}
		if job.SessionID != sessionID {
			return nil
		}
		if job.Status != StatusHITL {
			result = ResumeNotHITL
			return nil
		}
		if err := tx.Update(ref, []firestore.Update{
			{Path: "status", Value: StatusQueued},
			{Path: "active_agent", Value: AgentOrchestrator},
			{Path: "final_result", Value: ""},
			{Path: "agent_instructions", Value: ""},
			{Path: "hop_count", Value: 0},
			{Path: "updated_at", Value: time.Now()},
		}); err != nil {
			return err
		}
		job.Status = StatusQueued
		job.ActiveAgent = AgentOrchestrator
		job.HopCount = 0
		job.FinalResult = ""
		job.AgentInstructions = ""
		resumed = &job
		result = ResumeSucceeded
		return nil
	})
	return resumed, result, err
}

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

// FindStaleJobs queries for jobs in the given status with updated_at before
// the cutoff time. Used by the recovery sweep to find stuck jobs.
func (s *Store) FindStaleJobs(ctx context.Context, jobStatus JobStatus, olderThan time.Time) ([]*Job, error) {
	iter := s.client.Collection("jobs").
		Where("status", "==", string(jobStatus)).
		Where("updated_at", "<", olderThan).
		OrderBy("updated_at", firestore.Asc).
		Limit(50). // Cap per-sweep to avoid Firestore read spikes
		Documents(ctx)
	defer iter.Stop()

	var jobs []*Job
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return jobs, fmt.Errorf("FindStaleJobs: iterator error: %w", err)
		}
		var job Job
		if err := doc.DataTo(&job); err != nil {
			slog.Error("FindStaleJobs: unmarshal failed", "doc_id", doc.Ref.ID, "error", err)
			continue
		}
		jobs = append(jobs, &job)
	}
	return jobs, nil
}

// RecoverStaleJob atomically transitions a job from IN_PROGRESS to
// QUEUED+orchestrator via CAS. The olderThan cutoff is re-validated inside
// the transaction to ensure the job hasn't been updated since the sweep
// query (prevents recovering actively-progressing jobs). Returns true if
// the transition succeeded, false if the preconditions no longer hold.
func (s *Store) RecoverStaleJob(ctx context.Context, jobID, sessionID string, olderThan time.Time) (bool, error) {
	ref := s.client.Collection("jobs").Doc(jobID)
	var recovered bool
	err := s.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		recovered = false
		doc, err := tx.Get(ref)
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return nil
			}
			return err
		}
		var job Job
		if err := doc.DataTo(&job); err != nil {
			return err
		}
		if job.SessionID != sessionID {
			return nil
		}
		if job.Status != StatusInProgress {
			return nil // already moved — nothing to do
		}
		if !job.UpdatedAt.Before(olderThan) {
			return nil // job was updated since the sweep query — still active
		}
		if err := tx.Update(ref, []firestore.Update{
			{Path: "status", Value: StatusQueued},
			{Path: "active_agent", Value: AgentOrchestrator},
			{Path: "last_agent_summary", Value: "Job recovered by automatic sweep after stale IN_PROGRESS state."},
			{Path: "updated_at", Value: time.Now()},
		}); err != nil {
			return err
		}
		recovered = true
		return nil
	})
	return recovered, err
}
