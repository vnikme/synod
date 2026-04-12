package internal

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	collSessions = "sessions"
	collJobs     = "jobs"
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

func (s *Store) CreateSession(ctx context.Context, sess *Session) error {
	sess.CreatedAt = time.Now()
	sess.UpdatedAt = sess.CreatedAt
	_, err := s.client.Collection(collSessions).Doc(sess.SessionID).Set(ctx, sess)
	if err != nil {
		return fmt.Errorf("create session %s: %w", sess.SessionID, err)
	}
	slog.Info("session created", "session_id", sess.SessionID)
	return nil
}

func (s *Store) GetSession(ctx context.Context, sessionID string) (*Session, error) {
	doc, err := s.client.Collection(collSessions).Doc(sessionID).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get session %s: %w", sessionID, err)
	}
	var sess Session
	if err := doc.DataTo(&sess); err != nil {
		return nil, fmt.Errorf("decode session %s: %w", sessionID, err)
	}
	return &sess, nil
}

func (s *Store) AppendChatHistory(ctx context.Context, sessionID string, msgs ...ChatMessage) error {
	ref := s.client.Collection(collSessions).Doc(sessionID)
	_, err := ref.Update(ctx, []firestore.Update{
		{Path: "chat_history", Value: firestore.ArrayUnion(toInterfaceSlice(msgs)...)},
		{Path: "updated_at", Value: time.Now()},
	})
	if err != nil {
		return fmt.Errorf("append chat history %s: %w", sessionID, err)
	}
	return nil
}

func (s *Store) CreateJob(ctx context.Context, job *Job) error {
	job.CreatedAt = time.Now()
	job.UpdatedAt = job.CreatedAt
	_, err := s.client.Collection(collJobs).Doc(job.JobID).Set(ctx, job)
	if err != nil {
		return fmt.Errorf("create job %s: %w", job.JobID, err)
	}
	slog.Info("job created", "job_id", job.JobID, "session_id", job.SessionID)
	return nil
}

func (s *Store) GetJob(ctx context.Context, jobID, sessionID string) (*Job, error) {
	doc, err := s.client.Collection(collJobs).Doc(jobID).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get job %s: %w", jobID, err)
	}
	var job Job
	if err := doc.DataTo(&job); err != nil {
		return nil, fmt.Errorf("decode job %s: %w", jobID, err)
	}
	if job.SessionID != sessionID {
		slog.Warn("session isolation violation", "job_id", jobID, "expected", sessionID, "got", job.SessionID)
		return nil, nil
	}
	return &job, nil
}

func (s *Store) UpdateJob(ctx context.Context, jobID, sessionID string, updates []firestore.Update) error {
	job, err := s.GetJob(ctx, jobID, sessionID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %s not found for session %s", jobID, sessionID)
	}
	updates = append(updates, firestore.Update{Path: "updated_at", Value: time.Now()})
	_, err = s.client.Collection(collJobs).Doc(jobID).Update(ctx, updates)
	if err != nil {
		return fmt.Errorf("update job %s: %w", jobID, err)
	}
	return nil
}

func toInterfaceSlice(msgs []ChatMessage) []interface{} {
	out := make([]interface{}, len(msgs))
	for i, m := range msgs {
		out[i] = m
	}
	return out
}
