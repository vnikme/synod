package internal

import (
	"context"
	"log/slog"
	"time"
)

const (
	// sweepInterval is how often the recovery sweep runs.
	sweepInterval = 2 * time.Minute

	// staleJobThreshold defines how long a job can be IN_PROGRESS before
	// the sweep considers it stuck. This must be longer than the longest
	// expected agent execution (analyst w/ retries ≈ 3–4 min) to avoid
	// recovering jobs that are still executing normally.
	staleJobThreshold = 10 * time.Minute
)

// StartRecoverySweep launches a background goroutine that periodically scans
// for jobs stuck in IN_PROGRESS state (updated_at older than staleJobThreshold)
// and transitions them back to QUEUED+orchestrator so the orchestrator can
// re-evaluate. This handles the case where a post-claim Firestore write or
// enqueue failed, leaving a job permanently stuck.
//
// The goroutine exits when ctx is cancelled.
func StartRecoverySweep(ctx context.Context, store JobStore, dispatcher TaskDispatcher, selfURL string) {
	go func() {
		// Stagger the first sweep to avoid hitting Firestore immediately at startup.
		select {
		case <-time.After(30 * time.Second):
		case <-ctx.Done():
			return
		}

		ticker := time.NewTicker(sweepInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runSweep(ctx, store, dispatcher, selfURL)
			}
		}
	}()
}

func runSweep(ctx context.Context, store JobStore, dispatcher TaskDispatcher, selfURL string) {
	cutoff := time.Now().Add(-staleJobThreshold)
	jobs, err := store.FindStaleJobs(ctx, StatusInProgress, cutoff)
	if err != nil {
		slog.Error("recovery sweep: failed to query stale jobs", "error", err)
		return
	}
	if len(jobs) == 0 {
		return
	}

	slog.Warn("recovery sweep: found stale jobs", "count", len(jobs))
	for _, job := range jobs {
		recoverJob(ctx, store, dispatcher, selfURL, job)
	}
}

func recoverJob(ctx context.Context, store JobStore, dispatcher TaskDispatcher, selfURL string, job *Job) {
	// CAS: only recover if STILL in IN_PROGRESS. Another sweep or normal
	// execution may have already resolved it.
	recovered, err := store.RecoverStaleJob(ctx, job.JobID, job.SessionID)
	if err != nil {
		slog.Error("recovery sweep: RecoverStaleJob failed",
			"job_id", job.JobID, "session_id", job.SessionID, "error", err)
		return
	}
	if !recovered {
		// Job already moved out of IN_PROGRESS — no action needed.
		return
	}

	slog.Warn("recovery sweep: recovered stuck job → QUEUED+orchestrator",
		"job_id", job.JobID, "session_id", job.SessionID,
		"agent", job.ActiveAgent, "updated_at", job.UpdatedAt)

	// Enqueue orchestrator to re-evaluate. If this fails, the job will be
	// QUEUED+orchestrator with no Cloud Task. The sweep only targets
	// IN_PROGRESS jobs, so this case requires manual re-enqueue (e.g.,
	// POST /internal/route with the job_id and session_id).
	if err := dispatcher.Enqueue(ctx, selfURL+"/internal/route", job.JobID, job.SessionID); err != nil {
		slog.Error("recovery sweep: enqueue failed after recovery",
			"job_id", job.JobID, "session_id", job.SessionID, "error", err)
	}
}
