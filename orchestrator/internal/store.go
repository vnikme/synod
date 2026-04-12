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
	_, err := s.client.Collection("sessions").Doc(sessionID).Update(ctx, []firestore.Update{
		{Path: "chat_history", Value: firestore.ArrayUnion(msg)},
	})
	return err
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
	job, err := s.GetJob(ctx, jobID, sessionID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %s not found for session %s", jobID, sessionID)
	}
	updates = append(updates, firestore.Update{Path: "updated_at", Value: time.Now()})
	_, err = s.client.Collection("jobs").Doc(jobID).Update(ctx, updates)
	return err
}
