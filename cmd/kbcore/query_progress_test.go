package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/service"
)

func TestFormatQueryProgressLineIsCompactAndDescriptive(t *testing.T) {
	line := formatQueryProgressLine(12*time.Second, service.QueryProgressEvent{
		Type: "action_started",
		Step: 3,
		Action: &core.QueryAction{
			Action: "search",
			Query:  "唐太宗\n孙悟空",
		},
		Message:     "开始执行\nsearch",
		Observation: strings.Repeat("证据", 400),
	})
	for _, want := range []string{
		"query elapsed=12s",
		"type=action_started",
		"step=3",
		"action=search",
		`target="唐太宗 孙悟空"`,
		`message="开始执行 search"`,
		"…",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("progress line missing %q: %s", want, line)
		}
	}
	if strings.Contains(line, "\n") {
		t.Fatalf("progress line must stay on one line: %q", line)
	}
}

func TestQueryProgressReporterEmitsHeartbeatAfterSilence(t *testing.T) {
	var output bytes.Buffer
	reporter := newQueryProgressReporter(&output, 10*time.Millisecond)
	reporter.Progress(service.QueryProgressEvent{
		Type:    "action_planning_started",
		Step:    2,
		Message: "正在决定下一步工具动作",
	})
	time.Sleep(35 * time.Millisecond)
	reporter.Stop()

	text := output.String()
	if !strings.Contains(text, "type=action_planning_started") {
		t.Fatalf("initial event missing: %s", text)
	}
	if !strings.Contains(text, "type=heartbeat") || !strings.Contains(text, "last_type=action_planning_started") {
		t.Fatalf("heartbeat missing last stage: %s", text)
	}
}

func TestQueryProgressReporterCanRunWithoutHeartbeat(t *testing.T) {
	var output bytes.Buffer
	reporter := newQueryProgressReporter(&output, 0)
	reporter.Progress(service.QueryProgressEvent{Type: "started", Message: "查询命令已启动"})
	reporter.Stop()
	if strings.Count(strings.TrimSpace(output.String()), "\n") != 0 || !strings.Contains(output.String(), "type=started") {
		t.Fatalf("unexpected output: %q", output.String())
	}
}
