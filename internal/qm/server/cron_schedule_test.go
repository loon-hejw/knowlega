package server

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNormalizeSourceIntervalSchedule(t *testing.T) {
	now := int64(1_700_000_000_000)
	raw, next, err := normalizeSourceIntervalSchedule([]byte(`{"everyMs":900000,"ignored":"value"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	var schedule map[string]int64
	if err := json.Unmarshal(raw, &schedule); err != nil {
		t.Fatal(err)
	}
	if next != now+900000 || schedule["everyMs"] != 900000 || schedule["firstFireAt"] != next || len(schedule) != 2 {
		t.Fatalf("normalized schedule=%s next=%d", raw, next)
	}

	raw, next, err = normalizeSourceIntervalSchedule([]byte(`{"firstFireAt":1700000100000}`), now)
	if err != nil || next != 1_700_000_100_000 || string(raw) != `{"firstFireAt":1700000100000}` {
		t.Fatalf("one-shot schedule=%s next=%d err=%v", raw, next, err)
	}
}

func TestNormalizeSourceIntervalScheduleErrorsMatchNodeBoundary(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		updateFail bool
		message    string
	}{
		{name: "missing schedule", raw: `{}`, message: cronPatchMessage},
		{name: "timezone interval", raw: `{"everyMs":60000,"timezone":"UTC"}`, message: cronPatchMessage},
		{name: "non numeric interval", raw: `{"everyMs":"60000"}`, message: cronPatchMessage},
		{name: "fractional first fire", raw: `{"firstFireAt":1.5}`, updateFail: true, message: "schedule.firstFireAt must be a finite integer timestamp in ms"},
		{name: "too frequent", raw: `{"everyMs":1}`, updateFail: true, message: "schedule.everyMs must be at least 60000ms"},
		{name: "daily interval", raw: `{"everyMs":86400000}`, updateFail: true, message: "clock-time schedule in disguise"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := normalizeSourceIntervalSchedule([]byte(tt.raw), 1)
			if err == nil || !strings.Contains(err.Error(), tt.message) {
				t.Fatalf("err=%v", err)
			}
			var updateErr *cronScheduleUpdateError
			if errors.As(err, &updateErr) != tt.updateFail {
				t.Fatalf("update error=%v want=%v", err, tt.updateFail)
			}
		})
	}
}
