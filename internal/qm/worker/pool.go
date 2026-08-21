package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/loon-hejw/knowlega/internal/qm/data"
)

type TaskStore interface {
	Claim(context.Context, string, []string, time.Duration) (*data.ClaimedRuntimeTask, error)
	Heartbeat(context.Context, string, string, time.Duration) (bool, bool, error)
	AppendEvent(context.Context, string, string, data.RuntimeTaskEvent) (bool, error)
	Complete(context.Context, string, string, json.RawMessage) (bool, error)
	Release(context.Context, string, string) (bool, error)
	AcknowledgeCancellation(context.Context, string, string) (bool, error)
	Fail(context.Context, string, string, string, bool, time.Duration) (bool, bool, error)
	ReapExpired(context.Context) (int64, int64, error)
}

type EventWriter interface {
	Append(context.Context, string, json.RawMessage) error
}

type Handler func(context.Context, data.RuntimeTask, EventWriter) (json.RawMessage, error)

type PoolConfig struct {
	Name              string
	Kinds             []string
	Concurrency       int
	LeaseTTL          time.Duration
	HeartbeatInterval time.Duration
	PollInterval      time.Duration
	MaxClaimBackoff   time.Duration
}

type TaskError struct {
	Err   error
	Retry bool
	Delay time.Duration
}

func (e *TaskError) Error() string {
	if e == nil || e.Err == nil {
		return "runtime task failed"
	}
	return e.Err.Error()
}

func (e *TaskError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func Retry(err error, delay time.Duration) error {
	return &TaskError{Err: err, Retry: true, Delay: delay}
}

func Permanent(err error) error {
	return &TaskError{Err: err, Retry: false}
}

type Pool struct {
	store   TaskStore
	config  PoolConfig
	handler Handler
	logger  *slog.Logger
}

func NewPool(store TaskStore, config PoolConfig, handler Handler, logger *slog.Logger) (*Pool, error) {
	if store == nil || handler == nil {
		return nil, errors.New("runtime task store and handler are required")
	}
	if config.Name == "" || len(config.Kinds) == 0 {
		return nil, errors.New("runtime task pool name and kinds are required")
	}
	if config.Concurrency <= 0 {
		return nil, errors.New("runtime task pool concurrency must be positive")
	}
	if config.LeaseTTL <= 0 {
		return nil, errors.New("runtime task pool lease ttl must be positive")
	}
	if config.HeartbeatInterval <= 0 || config.HeartbeatInterval >= config.LeaseTTL {
		return nil, errors.New("runtime task heartbeat interval must be positive and shorter than the lease ttl")
	}
	if config.PollInterval <= 0 {
		return nil, errors.New("runtime task poll interval must be positive")
	}
	if config.MaxClaimBackoff < config.PollInterval {
		config.MaxClaimBackoff = 5 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Pool{store: store, config: config, handler: handler, logger: logger}, nil
}

func (p *Pool) Run(ctx context.Context) {
	var wait sync.WaitGroup
	for index := 0; index < p.config.Concurrency; index++ {
		wait.Add(1)
		go func(workerIndex int) {
			defer wait.Done()
			p.runWorker(ctx, workerIndex)
		}(index)
	}
	wait.Wait()
}

func (p *Pool) runWorker(ctx context.Context, index int) {
	host, _ := os.Hostname()
	workerID := fmt.Sprintf("%s-%s-%d-%d-%s", host, p.config.Name, os.Getpid(), index, uuid.NewString())
	claimFailures := 0
	for ctx.Err() == nil {
		task, err := p.store.Claim(ctx, workerID, p.config.Kinds, p.config.LeaseTTL)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			claimFailures++
			p.logger.Error("runtime task claim", "pool", p.config.Name, "worker", workerID, "error", err)
			if !waitContext(ctx, claimBackoff(p.config.PollInterval, p.config.MaxClaimBackoff, claimFailures)) {
				return
			}
			continue
		}
		claimFailures = 0
		if task == nil {
			if !waitContext(ctx, p.config.PollInterval) {
				return
			}
			continue
		}
		p.process(ctx, task)
	}
}

func (p *Pool) process(parent context.Context, claimed *data.ClaimedRuntimeTask) {
	taskCtx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	leaseToken := claimed.ClaimedLeaseToken
	var leaseLost atomic.Bool
	var cancelRequested atomic.Bool
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(p.config.HeartbeatInterval)
		defer ticker.Stop()
		failures := 0
		for {
			select {
			case <-taskCtx.Done():
				return
			case <-ticker.C:
				accepted, requested, err := p.store.Heartbeat(taskCtx, claimed.ID, leaseToken, p.config.LeaseTTL)
				if err != nil {
					failures++
					p.logger.Error("runtime task heartbeat", "pool", p.config.Name, "task", claimed.ID, "error", err)
					if failures < 3 {
						continue
					}
				} else {
					failures = 0
				}
				if !accepted {
					leaseLost.Store(true)
					cancel(errors.New("runtime task lease lost"))
					return
				}
				if requested {
					cancelRequested.Store(true)
					cancel(context.Canceled)
					return
				}
			}
		}
	}()

	writer := &eventWriter{ctx: taskCtx, store: p.store, taskID: claimed.ID, leaseToken: leaseToken}
	writer.sequence.Store(runtimeTaskEventAttemptBase(claimed.Attempts))
	result, handlerErr := p.handler(taskCtx, claimed.RuntimeTask, writer)
	cancel(nil)
	<-heartbeatDone
	if leaseLost.Load() {
		return
	}
	if cancelRequested.Load() {
		if _, err := p.store.AcknowledgeCancellation(parent, claimed.ID, leaseToken); err != nil {
			p.logger.Error("runtime task acknowledge cancellation", "pool", p.config.Name, "task", claimed.ID, "error", err)
		}
		return
	}
	if parent.Err() != nil {
		if _, err := p.store.Release(context.WithoutCancel(parent), claimed.ID, leaseToken); err != nil {
			p.logger.Error("runtime task release", "pool", p.config.Name, "task", claimed.ID, "error", err)
		}
		return
	}
	if handlerErr != nil {
		retry, delay := true, time.Duration(0)
		var taskErr *TaskError
		if errors.As(handlerErr, &taskErr) {
			retry, delay = taskErr.Retry, taskErr.Delay
		}
		accepted, requeued, err := p.store.Fail(parent, claimed.ID, leaseToken, handlerErr.Error(), retry, delay)
		if err != nil {
			p.logger.Error("runtime task fail", "pool", p.config.Name, "task", claimed.ID, "error", err)
		} else if !accepted {
			p.logger.Warn("runtime task failure lost lease", "pool", p.config.Name, "task", claimed.ID)
		} else if requeued {
			p.logger.Warn("runtime task requeued", "pool", p.config.Name, "task", claimed.ID, "error", handlerErr)
		}
		return
	}
	if len(result) == 0 {
		result = json.RawMessage(`{}`)
	}
	accepted, err := p.store.Complete(parent, claimed.ID, leaseToken, result)
	if err != nil {
		p.logger.Error("runtime task complete", "pool", p.config.Name, "task", claimed.ID, "error", err)
	} else if !accepted {
		p.logger.Warn("runtime task completion lost lease", "pool", p.config.Name, "task", claimed.ID)
	}
}

type eventWriter struct {
	ctx        context.Context
	store      TaskStore
	taskID     string
	leaseToken string
	mu         sync.Mutex
	sequence   atomic.Int64
}

func runtimeTaskEventAttemptBase(attempt int) int64 {
	if attempt <= 1 {
		return 0
	}
	return int64(attempt-1) << 32
}

func (w *eventWriter) Append(ctx context.Context, eventType string, payload json.RawMessage) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ctx == nil {
		ctx = w.ctx
	}
	accepted, err := w.store.AppendEvent(ctx, w.taskID, w.leaseToken, data.RuntimeTaskEvent{
		Sequence: w.sequence.Add(1),
		Type:     eventType,
		Payload:  payload,
	})
	if err != nil {
		return err
	}
	if !accepted {
		return errors.New("runtime task event lease lost")
	}
	return nil
}

func claimBackoff(base, maximum time.Duration, failures int) time.Duration {
	if failures <= 0 {
		return base
	}
	multiplier := math.Pow(2, float64(min(failures-1, 8)))
	delay := time.Duration(float64(base) * multiplier)
	if delay > maximum {
		return maximum
	}
	return delay
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type Manager struct {
	store        TaskStore
	pools        []*Pool
	reapInterval time.Duration
	logger       *slog.Logger
}

func NewManager(store TaskStore, reapInterval time.Duration, logger *slog.Logger) *Manager {
	if reapInterval <= 0 {
		reapInterval = 10 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{store: store, reapInterval: reapInterval, logger: logger}
}

func (m *Manager) Add(pool *Pool) {
	if pool != nil {
		m.pools = append(m.pools, pool)
	}
}

func (m *Manager) Run(ctx context.Context) {
	var wait sync.WaitGroup
	for _, pool := range m.pools {
		wait.Add(1)
		go func(pool *Pool) {
			defer wait.Done()
			pool.Run(ctx)
		}(pool)
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		m.runReaper(ctx)
	}()
	wait.Wait()
}

func (m *Manager) runReaper(ctx context.Context) {
	ticker := time.NewTicker(m.reapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			requeued, dead, err := m.store.ReapExpired(ctx)
			if err != nil {
				m.logger.Error("runtime task reap", "error", err)
			} else if requeued != 0 || dead != 0 {
				m.logger.Info("runtime tasks reaped", "requeued", requeued, "dead", dead)
			}
		}
	}
}
