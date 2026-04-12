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

// --- Audit Log ---

// AppendAuditLog writes an audit entry to the jobs/{jobID}/audit subcollection
// and atomically accumulates token_usage on the job document.
func (s *Store) AppendAuditLog(ctx context.Context, jobID, sessionID string, entry AuditEntry) error {
	entry.Timestamp = time.Now()

	// Write audit entry to subcollection (outside transaction — append-only, no contention)
	_, _, err := s.client.Collection("jobs").Doc(jobID).Collection("audit").Add(ctx, entry)
	if err != nil {
		return fmt.Errorf("audit log write: %w", err)
	}

	// Atomically accumulate token usage on the job document
	if entry.Tokens.TotalTokens > 0 {
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
			newUsage := job.TokenUsage.Add(entry.Tokens)
			return tx.Update(ref, []firestore.Update{
				{Path: "token_usage", Value: newUsage},
				{Path: "updated_at", Value: time.Now()},
			})
		})
	}
	return nil
}
