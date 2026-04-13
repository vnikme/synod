package internal

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestRunSweep_FindsAndRecoversStaleJobs(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}

	// Create a stale IN_PROGRESS job (updated_at > 10 min ago)
	seedJob(store, &Job{
		JobID:       "stale-1",
		SessionID:   "sess-1",
		Status:      StatusInProgress,
		ActiveAgent: AgentData,
	})
	// Manually backdate updated_at
	store.mu.Lock()
	store.jobs["stale-1"].UpdatedAt = time.Now().Add(-15 * time.Minute)
	store.mu.Unlock()

	runSweep(context.Background(), store, disp, "http://localhost:8080")

	// Verify job was recovered to QUEUED+orchestrator
	job, _ := store.GetJob(context.Background(), "stale-1", "sess-1")
	if job.Status != StatusQueued {
		t.Errorf("job.Status = %s, want QUEUED (recovered)", job.Status)
	}
	if job.ActiveAgent != AgentOrchestrator {
		t.Errorf("job.ActiveAgent = %s, want orchestrator", job.ActiveAgent)
	}

	// Verify orchestrator was enqueued
	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.enqueued) != 1 {
		t.Fatalf("enqueued = %d, want 1", len(disp.enqueued))
	}
	if disp.enqueued[0].URL != "http://localhost:8080/internal/route" {
		t.Errorf("enqueued URL = %s", disp.enqueued[0].URL)
	}
}

func TestRunSweep_NoStaleJobs(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}

	// Create a recent IN_PROGRESS job (should NOT be recovered)
	seedJob(store, &Job{
		JobID:       "fresh-1",
		SessionID:   "sess-1",
		Status:      StatusInProgress,
		ActiveAgent: AgentAnalyst,
	})
	// Updated just now — not stale

	runSweep(context.Background(), store, disp, "http://localhost:8080")

	// Job should still be IN_PROGRESS
	job, _ := store.GetJob(context.Background(), "fresh-1", "sess-1")
	if job.Status != StatusInProgress {
		t.Errorf("job.Status = %s, want IN_PROGRESS (not stale)", job.Status)
	}

	// No enqueue should have happened
	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.enqueued) != 0 {
		t.Errorf("enqueued = %d, want 0", len(disp.enqueued))
	}
}

func TestRunSweep_SkipsNonInProgressJobs(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}

	// Old but COMPLETED job — should not be recovered
	seedJob(store, &Job{
		JobID:       "old-complete",
		SessionID:   "sess-1",
		Status:      StatusCompleted,
		ActiveAgent: AgentOrchestrator,
	})
	store.mu.Lock()
	store.jobs["old-complete"].UpdatedAt = time.Now().Add(-30 * time.Minute)
	store.mu.Unlock()

	runSweep(context.Background(), store, disp, "http://localhost:8080")

	// Job should remain COMPLETED
	job, _ := store.GetJob(context.Background(), "old-complete", "sess-1")
	if job.Status != StatusCompleted {
		t.Errorf("job.Status = %s, want COMPLETED", job.Status)
	}

	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.enqueued) != 0 {
		t.Errorf("enqueued = %d, want 0", len(disp.enqueued))
	}
}

func TestRecoverJob_AlreadyRecovered(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}

	// Job that was IN_PROGRESS but has since moved to QUEUED
	seedJob(store, &Job{
		JobID:       "j1",
		SessionID:   "sess-1",
		Status:      StatusQueued,
		ActiveAgent: AgentOrchestrator,
	})

	// Simulate a sweep finding the job (it was IN_PROGRESS when queried,
	// but has since transitioned)
	staleSnapshot := &Job{
		JobID:     "j1",
		SessionID: "sess-1",
		Status:    StatusInProgress,
	}
	recoverJob(context.Background(), store, disp, "http://localhost:8080", staleSnapshot)

	// Job should remain QUEUED (CAS failed because not IN_PROGRESS)
	job, _ := store.GetJob(context.Background(), "j1", "sess-1")
	if job.Status != StatusQueued {
		t.Errorf("job.Status = %s, want QUEUED (already recovered)", job.Status)
	}

	// No enqueue
	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.enqueued) != 0 {
		t.Errorf("enqueued = %d, want 0 (job already recovered)", len(disp.enqueued))
	}
}

func TestRunSweep_MultipleStaleJobs(t *testing.T) {
	store := newMockStore()
	disp := &mockDispatcher{}

	for i := 0; i < 3; i++ {
		seedJob(store, &Job{
			JobID:       fmt.Sprintf("stale-%d", i),
			SessionID:   "sess-1",
			Status:      StatusInProgress,
			ActiveAgent: AgentData,
		})
	}
	store.mu.Lock()
	for _, j := range store.jobs {
		j.UpdatedAt = time.Now().Add(-20 * time.Minute)
	}
	store.mu.Unlock()

	runSweep(context.Background(), store, disp, "http://localhost:8080")

	// All 3 should be recovered
	for i := 0; i < 3; i++ {
		job, _ := store.GetJob(context.Background(), fmt.Sprintf("stale-%d", i), "sess-1")
		if job.Status != StatusQueued {
			t.Errorf("stale-%d: status = %s, want QUEUED", i, job.Status)
		}
	}

	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.enqueued) != 3 {
		t.Errorf("enqueued = %d, want 3", len(disp.enqueued))
	}
}
