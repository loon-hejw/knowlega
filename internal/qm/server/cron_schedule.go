package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

const (
	minimumRecurringCronMS int64 = 60_000
	dayMS                  int64 = 24 * 60 * 60 * 1000
	maxJSSafeInteger       int64 = 9_007_199_254_740_991
)

// cronScheduleUpdateError is deliberately separate from malformed API input.
// The Node route accepts a numeric schedule shape first, then returns
// cron_update_failed when the scheduler rejects its values.
type cronScheduleUpdateError struct{ message string }

func (e *cronScheduleUpdateError) Error() string { return e.message }

// normalizeSourceIntervalSchedule mirrors the Node source route plus
// normalizeSchedule/validateUserSchedule for schedules that do not contain a
// calendar cron. Calendar schedules remain on Node so Croner's exact IANA/DST
// behavior is not silently replaced during the staged migration.
func normalizeSourceIntervalSchedule(raw json.RawMessage, now int64) (json.RawMessage, int64, error) {
	var input map[string]json.RawMessage
	if json.Unmarshal(raw, &input) != nil || input == nil {
		return nil, 0, errors.New(cronPatchMessage)
	}
	if _, calendar := input["cron"]; calendar {
		return nil, 0, errors.New("calendar cron schedules must be handled by Node")
	}
	if _, timezone := input["timezone"]; timezone {
		return nil, 0, errors.New(cronPatchMessage)
	}
	everyRaw, hasEvery := input["everyMs"]
	firstRaw, hasFirst := input["firstFireAt"]
	if !hasEvery && !hasFirst {
		return nil, 0, errors.New(cronPatchMessage)
	}
	var every, first int64
	var err error
	if hasEvery {
		if every, err = scheduleJSONInteger(everyRaw); err != nil {
			if errors.Is(err, errScheduleNotNumber) {
				return nil, 0, errors.New(cronPatchMessage)
			}
			return nil, 0, &cronScheduleUpdateError{message: "schedule.everyMs must be a positive integer number of ms"}
		}
		if every < minimumRecurringCronMS {
			return nil, 0, &cronScheduleUpdateError{message: fmt.Sprintf("schedule.everyMs must be at least %dms", minimumRecurringCronMS)}
		}
		if every >= dayMS {
			return nil, 0, &cronScheduleUpdateError{message: "schedule.everyMs >= 24h is almost always a clock-time schedule in disguise — it anchors to an arbitrary epoch, has no timezone, and drifts with DST. Use {cron,timezone} for daily/weekly/monthly runs (e.g. { cron: \"30 7 * * 1-5\", timezone: \"America/Los_Angeles\" }). Reserve everyMs for genuine sub-day polling where wall-clock time does not matter."}
		}
	}
	if hasFirst {
		if first, err = scheduleJSONInteger(firstRaw); err != nil {
			if errors.Is(err, errScheduleNotNumber) {
				return nil, 0, errors.New(cronPatchMessage)
			}
			return nil, 0, &cronScheduleUpdateError{message: "schedule.firstFireAt must be a finite integer timestamp in ms"}
		}
	}
	if !hasFirst {
		first = now + every
	}
	normalized := map[string]int64{"firstFireAt": first}
	if hasEvery {
		normalized["everyMs"] = every
	}
	result, _ := json.Marshal(normalized)
	return result, first, nil
}

var errScheduleNotNumber = errors.New("schedule value is not a number")

func scheduleJSONInteger(raw json.RawMessage) (int64, error) {
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, errScheduleNotNumber
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value || value > float64(maxJSSafeInteger) || value < -float64(maxJSSafeInteger) {
		return 0, errors.New("not a safe integer")
	}
	return int64(value), nil
}
