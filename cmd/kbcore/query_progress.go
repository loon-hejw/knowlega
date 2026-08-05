package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hejw/knowledge-core/internal/service"
)

const (
	queryProgressHeartbeatInterval = 15 * time.Second
	queryProgressTextLimit         = 500
)

type queryProgressReporter struct {
	mu        sync.Mutex
	out       io.Writer
	startedAt time.Time
	heartbeat time.Duration
	lastEvent service.QueryProgressEvent
	activity  chan struct{}
	stop      chan struct{}
	done      chan struct{}
	stopOnce  sync.Once
}

func newQueryProgressReporter(out io.Writer, heartbeat time.Duration) *queryProgressReporter {
	now := time.Now()
	reporter := &queryProgressReporter{
		out: out, startedAt: now, heartbeat: heartbeat,
		activity: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
	if heartbeat > 0 {
		go reporter.runHeartbeat()
	} else {
		close(reporter.done)
	}
	return reporter
}

func (r *queryProgressReporter) Progress(event service.QueryProgressEvent) {
	now := time.Now()
	r.mu.Lock()
	r.lastEvent = event
	fmt.Fprintln(r.out, formatQueryProgressLine(now.Sub(r.startedAt), event))
	r.mu.Unlock()
	select {
	case r.activity <- struct{}{}:
	default:
	}
}

func (r *queryProgressReporter) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

func (r *queryProgressReporter) runHeartbeat() {
	defer close(r.done)
	timer := time.NewTimer(r.heartbeat)
	defer timer.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-r.activity:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(r.heartbeat)
		case now := <-timer.C:
			r.mu.Lock()
			event := service.QueryProgressEvent{
				Type:    "heartbeat",
				Step:    r.lastEvent.Step,
				Message: "查询仍在运行",
			}
			if r.lastEvent.Type != "" {
				event.Observation = "last_type=" + r.lastEvent.Type
				if message := compactQueryProgressText(r.lastEvent.Message, 160); message != "" {
					event.Observation += " last_message=" + message
				}
			}
			fmt.Fprintln(r.out, formatQueryProgressLine(now.Sub(r.startedAt), event))
			r.mu.Unlock()
			timer.Reset(r.heartbeat)
		}
	}
}

func formatQueryProgressLine(elapsed time.Duration, event service.QueryProgressEvent) string {
	eventType := strings.TrimSpace(event.Type)
	if eventType == "" {
		eventType = "progress"
	}
	parts := []string{
		"query",
		"elapsed=" + elapsed.Round(time.Second).String(),
		"type=" + eventType,
	}
	if event.Step > 0 {
		parts = append(parts, fmt.Sprintf("step=%d", event.Step))
	}
	if event.Action != nil {
		if action := compactQueryProgressText(event.Action.Action, 80); action != "" {
			parts = append(parts, "action="+action)
		}
		if target := queryProgressActionTarget(event); target != "" {
			parts = append(parts, "target="+strconv.Quote(target))
		}
	}
	if message := compactQueryProgressText(event.Message, 240); message != "" {
		parts = append(parts, "message="+strconv.Quote(message))
	}
	if observation := compactQueryProgressText(event.Observation, queryProgressTextLimit); observation != "" {
		parts = append(parts, "observation="+strconv.Quote(observation))
	}
	return strings.Join(parts, " ")
}

func queryProgressActionTarget(event service.QueryProgressEvent) string {
	if event.Action == nil {
		return ""
	}
	for _, value := range []string{event.Action.Path, event.Action.Query, event.Action.Candidate, event.Action.Title} {
		if value = compactQueryProgressText(value, 240); value != "" {
			return value
		}
	}
	return ""
}

func compactQueryProgressText(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "…"
}
