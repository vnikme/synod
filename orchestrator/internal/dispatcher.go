package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
)

type Dispatcher struct {
	client         *cloudtasks.Client
	projectID      string
	location       string
	queue          string
	serviceAccount string
}

func NewDispatcher(ctx context.Context) (*Dispatcher, error) {
	projectID := os.Getenv("GCP_PROJECT_ID")
	if projectID == "" {
		return nil, fmt.Errorf("missing required env var GCP_PROJECT_ID")
	}
	location := os.Getenv("CLOUD_TASKS_LOCATION")
	if location == "" {
		return nil, fmt.Errorf("missing required env var CLOUD_TASKS_LOCATION")
	}
	queue := os.Getenv("CLOUD_TASKS_QUEUE")
	if queue == "" {
		return nil, fmt.Errorf("missing required env var CLOUD_TASKS_QUEUE")
	}
	serviceAccount := os.Getenv("SERVICE_ACCOUNT_EMAIL")
	if serviceAccount == "" {
		return nil, fmt.Errorf("missing required env var SERVICE_ACCOUNT_EMAIL")
	}
	client, err := cloudtasks.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("cloudtasks.NewClient: %w", err)
	}
	return &Dispatcher{
		client:         client,
		projectID:      projectID,
		location:       location,
		queue:          queue,
		serviceAccount: serviceAccount,
	}, nil
}

func (d *Dispatcher) Close() error {
	return d.client.Close()
}

func (d *Dispatcher) Enqueue(ctx context.Context, targetURL, jobID, sessionID string) error {
	payload := TaskPayload{JobID: jobID, SessionID: sessionID}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	queuePath := fmt.Sprintf("projects/%s/locations/%s/queues/%s",
		d.projectID, d.location, d.queue)
	task := &taskspb.Task{
		MessageType: &taskspb.Task_HttpRequest{
			HttpRequest: &taskspb.HttpRequest{
				Url:        targetURL,
				HttpMethod: taskspb.HttpMethod_POST,
				Body:       body,
				Headers:    map[string]string{"Content-Type": "application/json"},
				AuthorizationHeader: &taskspb.HttpRequest_OidcToken{
					OidcToken: &taskspb.OidcToken{
						ServiceAccountEmail: d.serviceAccount,
					},
				},
			},
		},
	}
	created, err := d.client.CreateTask(ctx, &taskspb.CreateTaskRequest{
		Parent: queuePath,
		Task:   task,
	})
	if err != nil {
		return fmt.Errorf("CreateTask: %w", err)
	}
	slog.Info("task enqueued", "task_name", created.Name, "target", targetURL, "job_id", jobID)
	return nil
}
