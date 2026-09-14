package agent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"
)

const piSignalPollInterval = 5 * time.Second

type piSignalController struct {
	input       TurnInput
	cancelModel context.CancelFunc
	steers      chan TurnSignal
	followUps   chan TurnSignal
	cancelPoll  context.CancelFunc
	done        chan struct{}
	aborted     atomic.Bool
}

func startPiSignals(ctx context.Context, input TurnInput, cancelModel context.CancelFunc) *piSignalController {
	if input.RunID == "" || input.TakePendingSignals == nil {
		return nil
	}
	pollCtx, cancelPoll := context.WithCancel(ctx)
	controller := &piSignalController{input: input, cancelModel: cancelModel, steers: make(chan TurnSignal, 64), followUps: make(chan TurnSignal, 64), cancelPoll: cancelPoll, done: make(chan struct{})}
	go controller.run(pollCtx)
	return controller
}

func (c *piSignalController) run(ctx context.Context) {
	defer close(c.done)
	c.poll(ctx)
	ticker := time.NewTicker(piSignalPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.poll(ctx)
		}
	}
}

func (c *piSignalController) poll(ctx context.Context) {
	signals, err := c.input.TakePendingSignals(ctx, c.input.RunID)
	if err != nil {
		return
	}
	for _, signal := range signals {
		switch signal.Kind {
		case "abort":
			c.aborted.Store(true)
			c.cancelModel()
		case "steer", "followUp":
			if signal.Text == "" {
				continue
			}
			queue := c.steers
			if signal.Kind == "followUp" {
				queue = c.followUps
			}
			select {
			case queue <- signal:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (c *piSignalController) stop() {
	if c == nil {
		return
	}
	c.cancelPoll()
	<-c.done
}

func (c *piSignalController) applySteers(ctx context.Context, input TurnInput, messages *[]PiMessage) (int, error) {
	if c == nil {
		return 0, nil
	}
	return c.applyQueued(ctx, input, messages, c.steers)
}

func (c *piSignalController) applyFollowUps(ctx context.Context, input TurnInput, messages *[]PiMessage) (int, error) {
	if c == nil {
		return 0, nil
	}
	return c.applyQueued(ctx, input, messages, c.followUps)
}

func (c *piSignalController) applyQueued(ctx context.Context, input TurnInput, messages *[]PiMessage, queue <-chan TurnSignal) (int, error) {
	count := 0
	for {
		select {
		case signal := <-queue:
			payload := map[string]any{"text": signal.Text, "steered": true}
			var source map[string]json.RawMessage
			if json.Unmarshal(signal.Payload, &source) == nil {
				var ts string
				if json.Unmarshal(source["ts"], &ts) == nil && ts != "" {
					payload["ts"] = ts
				}
			}
			encoded, _ := json.Marshal(payload)
			if _, err := input.Emit(ctx, NewEntry{Type: "user", Payload: encoded, ScopeLabel: input.ScopeLabel}); err != nil {
				return count, err
			}
			*messages = append(*messages, PiMessage{Role: "user", Content: signal.Text})
			count++
		default:
			return count, nil
		}
	}
}
