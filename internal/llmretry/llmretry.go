package llmretry

import (
	"context"
	"time"
)

type Options struct {
	Retries   int
	BaseDelay time.Duration
	MaxDelay  time.Duration
}

type RetryInfo struct {
	Attempt int
	Retries int
	Delay   time.Duration
	Reason  string
}

type OnRetry func(RetryInfo)

func Defaults() Options {
	return Options{Retries: 2, BaseDelay: 800 * time.Millisecond, MaxDelay: 5 * time.Second}
}

func Normalize(opts Options) Options {
	if opts.Retries < 0 {
		opts.Retries = 0
	}
	if opts.BaseDelay <= 0 {
		opts.BaseDelay = 800 * time.Millisecond
	}
	if opts.MaxDelay <= 0 {
		opts.MaxDelay = 5 * time.Second
	}
	if opts.MaxDelay < opts.BaseDelay {
		opts.MaxDelay = opts.BaseDelay
	}
	return opts
}

func Do(ctx context.Context, opts Options, onRetry OnRetry, fn func(attempt int) (string, bool, error)) (string, error) {
	return DoValue[string](ctx, opts, onRetry, fn)
}

func DoValue[T any](ctx context.Context, opts Options, onRetry OnRetry, fn func(attempt int) (T, bool, error)) (T, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var lastErr error
	var zero T
	for attempt := 1; attempt <= opts.Retries+1; attempt++ {
		value, retryable, err := fn(attempt)
		if err == nil {
			return value, nil
		}
		lastErr = err
		if !retryable || attempt > opts.Retries || ctx.Err() != nil {
			return zero, err
		}
		delay := retryDelay(opts, attempt)
		if onRetry != nil {
			onRetry(RetryInfo{
				Attempt: attempt,
				Retries: opts.Retries,
				Delay:   delay,
				Reason:  err.Error(),
			})
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err()
		case <-timer.C:
		}
	}
	return zero, lastErr
}

func retryDelay(opts Options, attempt int) time.Duration {
	delay := opts.BaseDelay
	if delay <= 0 {
		delay = 800 * time.Millisecond
	}
	for i := 1; i < attempt; i++ {
		delay *= 2
	}
	maxDelay := opts.MaxDelay
	if maxDelay <= 0 {
		maxDelay = 5 * time.Second
	}
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

func RetryableStatus(status int) bool {
	return status == 429 || status == 500 || status == 502 || status == 503 || status == 504
}
