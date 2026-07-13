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
