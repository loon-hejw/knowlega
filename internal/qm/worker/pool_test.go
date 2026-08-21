package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/loon-hejw/knowlega/internal/qm/data"
)

type fakeTaskStore struct {
	mu              sync.Mutex
	task            *data.ClaimedRuntimeTask
	claimed         bool
	heartbeatCancel bool
	events          []data.RuntimeTaskEvent
	completed       chan json.RawMessage
	failed          chan struct {
		retry bool
		delay time.Duration
	}
	cancelled chan struct{}
}

func newFakeTaskStore(task *data.ClaimedRuntimeTask) *fakeTaskStore {
	return &fakeTaskStore{
		task:      task,
		completed: make(chan json.RawMessage, 1),
		failed: make(chan struct {
			retry bool
			delay time.Duration
		}, 1),
		cancelled: make(chan struct{}, 1),
	}
}

func (s *fakeTaskStore) Claim(context.Context, string, []string, time.Duration) (*data.ClaimedRuntimeTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed {
		return nil, nil
	}
	s.claimed = true
	return s.task, nil
}

func (s *fakeTaskStore) Heartbeat(context.Context, string, string, time.Duration) (bool, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return true, s.heartbeatCancel, nil
}

func (s *fakeTaskStore) AppendEvent(_ context.Context, _ string, _ string, event data.RuntimeTaskEvent) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return true, nil
}

func (s *fakeTaskStore) Complete(_ context.Context, _ string, _ string, result json.RawMessage) (bool, error) {
	s.completed <- result
	return true, nil
}

func (s *fakeTaskStore) Release(context.Context, string, string) (bool, error) {
	return true, nil
}

func (s *fakeTaskStore) AcknowledgeCancellation(context.Context, string, string) (bool, error) {
	s.cancelled <- struct{}{}
	return true, nil
}

func (s *fakeTaskStore) Fail(_ context.Context, _ string, _ string, _ string, retry bool, delay time.Duration) (bool, bool, error) {
	s.failed <- struct {
		retry bool
		delay time.Duration
	}{retry: retry, delay: delay}
	return true, retry, nil
}

func (s *fakeTaskStore) ReapExpired(context.Context) (int64, int64, error) {
	return 0, 0, nil
}

func testPool(t *testing.T, store TaskStore, handler Handler, heartbeat time.Duration) *Pool {
	t.Helper()
	pool, err := NewPool(store, PoolConfig{
		Name:              "deploy",
		Kinds:             []string{"deploy.apply"},
		Concurrency:       2,
		LeaseTTL:          100 * time.Millisecond,
		HeartbeatInterval: heartbeat,
		PollInterval:      time.Millisecond,
		MaxClaimBackoff:   10 * time.Millisecond,
	}, handler, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func claimedTask() *data.ClaimedRuntimeTask {
	return &data.ClaimedRuntimeTask{
		RuntimeTask:       data.RuntimeTask{ID: "task-1", Kind: "deploy.apply", Payload: []byte(`{}`)},
		ClaimedLeaseToken: "lease-1",
		ClaimedLeaseOwner: "worker-1",
	}
}

func TestPoolCompletesAndAppendsEvents(t *testing.T) {
	store := newFakeTaskStore(claimedTask())
	pool := testPool(t, store, func(ctx context.Context, task data.RuntimeTask, events EventWriter) (json.RawMessage, error) {
		if task.ID != "task-1" {
			t.Fatalf("task=%#v", task)
		}
		if err := events.Append(ctx, "progress", []byte(`{"step":1}`)); err != nil {
			return nil, err
		}
		return []byte(`{"ok":true}`), nil
	}, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pool.Run(ctx)
		close(done)
	}()
	select {
	case result := <-store.completed:
		if string(result) != `{"ok":true}` {
			t.Fatalf("result=%s", result)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not complete task")
	}
	cancel()
	<-done
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.events) != 1 || store.events[0].Sequence != 1 || store.events[0].Type != "progress" {
		t.Fatalf("events=%#v", store.events)
	}
}

func TestPoolEventSequenceSeparatesRetryAttempts(t *testing.T) {
	task := claimedTask()
	task.Attempts = 3
	store := newFakeTaskStore(task)
	pool := testPool(t, store, func(ctx context.Context, _ data.RuntimeTask, events EventWriter) (json.RawMessage, error) {
		if err := events.Append(ctx, "progress", []byte(`{"step":1}`)); err != nil {
			return nil, err
		}
		return []byte(`{"ok":true}`), nil
	}, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pool.Run(ctx)
		close(done)
	}()
	select {
	case <-store.completed:
	case <-time.After(time.Second):
		t.Fatal("worker did not complete task")
	}
	cancel()
	<-done
	store.mu.Lock()
	defer store.mu.Unlock()
	want := int64(2)<<32 | 1
	if len(store.events) != 1 || store.events[0].Sequence != want {
		t.Fatalf("events=%#v want sequence=%d", store.events, want)
	}
}

type delayedEventStore struct {
	*fakeTaskStore
	firstStarted chan struct{}
	releaseFirst chan struct{}
	mu           sync.Mutex
	committed    []int64
}

func (s *delayedEventStore) AppendEvent(_ context.Context, _ string, _ string, event data.RuntimeTaskEvent) (bool, error) {
	if event.Sequence == 1 {
		close(s.firstStarted)
		<-s.releaseFirst
	}
	s.mu.Lock()
	s.committed = append(s.committed, event.Sequence)
	s.mu.Unlock()
	return true, nil
}

func TestEventWriterCommitsSequencesInOrder(t *testing.T) {
	store := &delayedEventStore{
		fakeTaskStore: newFakeTaskStore(claimedTask()),
		firstStarted:  make(chan struct{}),
		releaseFirst:  make(chan struct{}),
	}
	writer := &eventWriter{ctx: t.Context(), store: store, taskID: "task-1", leaseToken: "lease-1"}
	errs := make(chan error, 2)
	go func() { errs <- writer.Append(t.Context(), "first", json.RawMessage(`{}`)) }()
	<-store.firstStarted
	go func() { errs <- writer.Append(t.Context(), "second", json.RawMessage(`{}`)) }()
	time.Sleep(10 * time.Millisecond)
	close(store.releaseFirst)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.committed) != 2 || store.committed[0] != 1 || store.committed[1] != 2 {
		t.Fatalf("committed=%v", store.committed)
	}
}

func TestPoolHonorsRetryClassification(t *testing.T) {
	store := newFakeTaskStore(claimedTask())
	pool := testPool(t, store, func(context.Context, data.RuntimeTask, EventWriter) (json.RawMessage, error) {
		return nil, Retry(errors.New("temporary"), 50*time.Millisecond)
	}, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pool.Run(ctx)
		close(done)
	}()
	select {
	case failure := <-store.failed:
		if !failure.retry || failure.delay != 50*time.Millisecond {
			t.Fatalf("failure=%#v", failure)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not fail task")
	}
	cancel()
	<-done
}

func TestPoolCancelsClaimedTaskAfterHeartbeat(t *testing.T) {
	store := newFakeTaskStore(claimedTask())
	store.heartbeatCancel = true
	pool := testPool(t, store, func(ctx context.Context, _ data.RuntimeTask, _ EventWriter) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pool.Run(ctx)
		close(done)
	}()
	select {
	case <-store.cancelled:
	case <-time.After(time.Second):
		t.Fatal("worker did not acknowledge cancellation")
	}
	cancel()
	<-done
}
