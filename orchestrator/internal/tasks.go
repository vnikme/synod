package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Dispatcher struct {
	client         *cloudtasks.Client
	projectID      string
	location       string
	queue          string
	serviceAccount string
}

func NewDispatcher(ctx context.Context) (*Dispatcher, error) {
	client, err := cloudtasks.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("cloudtasks.NewClient: %w", err)
	}
	return &Dispatcher{
		client:         client,
		projectID:      os.Getenv("GCP_PROJECT_ID"),
		location:       os.Getenv("CLOUD_TASKS_LOCATION"),
		queue:          os.Getenv("CLOUD_TASKS_QUEUE"),
		serviceAccount: os.Getenv("SERVICE_ACCOUNT_EMAIL"),
	}, nil
}

func (d *Dispatcher) Close() error {
	return d.client.Close()
}

func (d *Dispatcher) Enqueue(ctx context.Context, targetURL, jobID, sessionID string) error {
	queuePath := fmt.Sprintf("projects/%s/locations/%s/queues/%s",
		d.projectID, d.location, d.queue)

	payload := TaskPayload{JobID: jobID, SessionID: sessionID}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal task payload: %w", err)
	}

	req := &taskspb.CreateTaskRequest{
		Parent: queuePath,
		Task: &taskspb.Task{
			MessageType: &taskspb.Task_HttpRequest{
				HttpRequest: &taskspb.HttpRequest{
					Url:        targetURL,
					HttpMethod: taskspb.HttpMethod_POST,
					Headers:    map[string]string{"Content-Type": "application/json"},
					Body:       body,
					AuthorizationHeader: &taskspb.HttpRequest_OidcToken{
						OidcToken: &taskspb.OidcToken{
							ServiceAccountEmail: d.serviceAccount,
							Audience:            targetURL,
						},
					},
				},
			},
			ScheduleTime: timestamppb.Now(),
		},
	}

	task, err := d.client.CreateTask(ctx, req)
	if err != nil {
		return fmt.Errorf("create task for %s: %w", targetURL, err)
	}
	slog.Info("task enqueued", "target", targetURL, "job_id", jobID, "task", task.Name)
	return nil
}
