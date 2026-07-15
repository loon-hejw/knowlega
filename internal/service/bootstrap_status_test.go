package service

import (
	"errors"
	"testing"
	"time"
)

func TestBootstrapTrackerProgressRetryAndSuccess(t *testing.T) {
	tracker := NewBootstrapTracker("/tmp/demo", 3)
	tracker.SetAttempt(1)
	tracker.UpdateProgress("analysis", "a.md", 1, 3, 0, 0, "")
	tracker.UpdateProgress("completed", "a.md", 1, 3, 2, 1, "")
	next := time.Now().UTC().Add(time.Minute)
	tracker.SetRetry(errors.New("temporary"), 1, next)
	status := tracker.Snapshot()
	if status.Status != "retrying" || status.CompletedSources != 1 || status.Files != 2 || status.NextRetryAt == nil {
		t.Fatalf("retry status=%+v", status)
	}
	tracker.Succeed(3, 2)
	status = tracker.Snapshot()
	if status.Status != "succeeded" || !tracker.Ready() || status.CompletedSources != 3 || status.FinishedAt == nil {
		t.Fatalf("success status=%+v", status)
	}
}

func TestBootstrapTrackerShowsConcurrentTasksAndImpactRequeue(t *testing.T) {
	tracker := NewBootstrapTracker("/tmp/demo", 3)
	tracker.UpdateTaskProgress("analysis", "a.md", 1, 3, 0, 0, 1, "")
	tracker.UpdateTaskProgress("generation", "b.md", 2, 3, 0, 0, 2, "")
	status := tracker.Snapshot()
	if len(status.ActiveSources) != 2 || status.PendingSources != 1 {
		t.Fatalf("active status=%+v", status)
	}
	tracker.UpdateTaskProgress("completed", "a.md", 1, 3, 2, 1, 1, "")
	tracker.UpdateTaskProgress("impact_requeued", "a.md", 1, 3, 0, 0, 2, "shared page changed")
	tracker.RecordLLMCall(1500*time.Millisecond, false)
	tracker.RecordLLMCall(500*time.Millisecond, true)
	tracker.RecordSourceDuration("a.md", 3*time.Second)
	tracker.RecordSourceDuration("a.md", 2*time.Second)
	status = tracker.Snapshot()
	if status.CompletedSources != 0 || status.RequeuedSources != 1 || status.ImpactRequeues != 1 || status.Files != 0 || status.Reviews != 0 {
		t.Fatalf("requeued status=%+v", status)
	}
	if status.LLMCalls != 2 || status.LLMFailures != 1 || status.LLMDurationMS != 2000 || status.AverageSourceMS != 5000 || status.SourceDurations["a.md"] != 5000 || status.RequeueRate == 0 {
		t.Fatalf("metrics status=%+v", status)
	}
}

func TestBootstrapTrackerRemovesFailedTaskBeforeRetry(t *testing.T) {
	tracker := NewBootstrapTracker("/tmp/demo", 2)
	tracker.UpdateTaskProgress("generation", "a.md", 1, 2, 0, 0, 1, "")
	tracker.UpdateTaskProgress("generation", "b.md", 2, 2, 0, 0, 1, "")
	tracker.UpdateTaskProgress("failed_requeued", "a.md", 1, 2, 0, 0, 2, "temporary failure")
	status := tracker.Snapshot()
	if len(status.ActiveSources) != 1 || status.ActiveSources[0].Path != "b.md" || status.RequeuedSources != 1 || status.FailureRequeues != 1 {
		t.Fatalf("requeued status=%+v", status)
	}
	tracker.UpdateTaskProgress("analysis", "a.md", 1, 2, 0, 0, 2, "")
	status = tracker.Snapshot()
	if len(status.ActiveSources) != 2 || status.Error != "" {
		t.Fatalf("retry status=%+v", status)
	}
}

func TestBootstrapTrackerRemovesTransientTaskWhileQueued(t *testing.T) {
	tracker := NewBootstrapTracker("/tmp/demo", 2)
	tracker.UpdateTaskProgress("generation", "a.md", 1, 2, 0, 0, 1, "")
	tracker.UpdateTaskProgress("generation", "b.md", 2, 2, 0, 0, 1, "")
	tracker.UpdateTaskProgress("transient_requeued", "a.md", 1, 2, 0, 0, 1, "context deadline exceeded")
	status := tracker.Snapshot()
	if len(status.ActiveSources) != 1 || status.ActiveSources[0].Path != "b.md" || status.RequeuedSources != 1 || status.FailureRequeues != 1 {
		t.Fatalf("transient queue status=%+v", status)
	}
}

func TestBootstrapTrackerKeepsTransientConflictInConflictQueue(t *testing.T) {
	tracker := NewBootstrapTracker("/tmp/demo", 2)
	tracker.UpdateTaskProgress("analysis", "a.md", 1, 2, 0, 0, 1, "")
	tracker.UpdateTaskProgress("transient_conflict_requeued", "a.md", 1, 2, 0, 0, 1, "context deadline exceeded")
	status := tracker.Snapshot()
	if len(status.ActiveSources) != 0 || status.QueuedConflictSources != 1 || status.RequeuedSources != 1 || status.FailureRequeues != 1 || status.ConflictRequeues != 0 {
		t.Fatalf("transient conflict status=%+v", status)
	}
}

func TestBootstrapTrackerSeedsDurableProgressWithoutDoubleCountingSkip(t *testing.T) {
	tracker := NewBootstrapTracker("/tmp/demo", 3)
	tracker.SeedCompleted("a.md", 4, 2)
	tracker.UpdateTaskProgress("skipped", "a.md", 1, 3, 4, 2, 1, "")
	tracker.UpdateTaskProgress("completed", "b.md", 2, 3, 3, 1, 1, "")
	status := tracker.Snapshot()
	if status.CompletedSources != 2 || status.RestoredSources != 1 || status.CompletedThisRun != 1 || status.Files != 7 || status.Reviews != 3 {
		t.Fatalf("status=%+v", status)
	}
	tracker.StartLLMCall("generation-1", time.Now().Add(-2*time.Second))
	status = tracker.Snapshot()
	if status.LLMInFlight != 1 || status.OldestLLMCallMS < 1500 {
		t.Fatalf("inflight status=%+v", status)
	}
	tracker.FinishLLMCall("generation-1", 2*time.Second, false)
	status = tracker.Snapshot()
	if status.LLMInFlight != 0 || status.LLMCalls != 1 || status.LLMDurationMS != 2000 {
		t.Fatalf("finished status=%+v", status)
	}
}
