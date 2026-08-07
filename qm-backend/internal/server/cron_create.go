package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const createCronInputMessage = "expected a CreateCronInput"

// createSourceCron moves the durable half of Node's source-authenticated
// CronStore.create into Go. Calendar schedules deliberately remain proxied:
// Node's Croner implementation is still the compatibility authority for
// five-field expressions, IANA zones, and DST. Interval and one-shot entries
// are fully represented by the shared PostgreSQL cron record.
func (h *HTTPServer) createSourceCron(w http.ResponseWriter, r *http.Request, raw []byte) {
	payload, id, calendar, err := sourceCronCreatePayload(raw, time.Now().UnixMilli())
	if calendar {
		h.proxy.ServeHTTP(w, r)
		return
	}
	if err != nil {
		var scheduleErr *cronScheduleUpdateError
		var createErr *sourceCronCreateError
		if errors.As(err, &scheduleErr) || errors.As(err, &createErr) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cron_create_failed", "message": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": err.Error()})
		return
	}
	record, err := h.crons.PutIfAbsent(r.Context(), id, payload)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cron_create_failed", "message": err.Error()})
		return
	}
	cron, err := cronWithoutFireLog(*record)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cron": cron})
}

// sourceCronCreatePayload uses the same 16-hex content identifier as Node's
// CronStore: SHA-256 of JSON content parts joined with NUL. PutIfAbsent then
// preserves Node's create deduplication even while one rollout has Node and
// Go callers active at the same time.
func sourceCronCreatePayload(raw []byte, now int64) (payload json.RawMessage, id string, calendar bool, err error) {
	var input map[string]json.RawMessage
	if json.Unmarshal(raw, &input) != nil || input == nil {
		return nil, "", false, errors.New(createCronInputMessage)
	}
	ownerScope, ok := sourceCronString(input, "ownerScopeId")
	if !ok {
		return nil, "", false, errors.New(createCronInputMessage)
	}
	owner, ok := sourceCronString(input, "owner")
	if !ok {
		return nil, "", false, errors.New(createCronInputMessage)
	}
	createdBy, ok := sourceCronString(input, "createdBy")
	if !ok {
		return nil, "", false, errors.New(createCronInputMessage)
	}
	if !sourceCronHasActionOrMessage(input) || !sourceCronDestinationValid(input) {
		return nil, "", false, errors.New(createCronInputMessage)
	}
	if !sourceCronSamePerson(owner, createdBy) && !sourceCronJSONTruthy(input["ownerConsentedAt"]) {
		return nil, "", false, &sourceCronCreateError{message: "assigning a different owner requires that owner's consent"}
	}
	title := ""
	if rawTitle, present := input["title"]; present {
		var value string
		if json.Unmarshal(rawTitle, &value) != nil {
			return nil, "", false, errors.New(createCronInputMessage)
		}
		title = normalizeCronTitle(value)
	}
	scheduleRaw, present := input["schedule"]
	if !present {
		return nil, "", false, errors.New(createCronInputMessage)
	}
	normalizedSchedule, nextFireAt, isCalendar, scheduleErr := normalizeSourceCreateSchedule(scheduleRaw, now)
	if isCalendar {
		return nil, "", true, nil
	}
	if scheduleErr != nil {
		return nil, "", false, &sourceCronCreateError{message: scheduleErr.Error()}
	}
	contentID, err := sourceCronContentID(input, title)
	if err != nil {
		return nil, "", false, errors.New(createCronInputMessage)
	}
	document := map[string]json.RawMessage{
		"id":           mustMarshalJSON(contentID),
		"ownerScopeId": mustMarshalJSON(ownerScope),
		"owner":        mustMarshalJSON(owner),
		"createdBy":    mustMarshalJSON(createdBy),
		"enabled":      json.RawMessage("true"),
		"createdAt":    mustMarshalJSON(now),
		"schedule":     normalizedSchedule,
		"nextFireAt":   mustMarshalJSON(nextFireAt),
	}
	if title != "" {
		document["title"] = mustMarshalJSON(title)
	}
	for _, key := range []string{"action", "message"} {
		if value, present := input[key]; present {
			document[key] = value
		}
	}
	for _, key := range []string{"destination", "runAs", "members", "ownerConsentedAt", "recipientConsent"} {
		if value, present := input[key]; present && sourceCronJSONTruthy(value) {
			document[key] = value
		}
	}
	payload, err = json.Marshal(document)
	if err != nil {
		return nil, "", false, err
	}
	return payload, contentID, false, nil
}

type sourceCronCreateError struct{ message string }

func (e *sourceCronCreateError) Error() string { return e.message }

func normalizeSourceCreateSchedule(raw json.RawMessage, now int64) (json.RawMessage, int64, bool, error) {
	var input map[string]json.RawMessage
	if json.Unmarshal(raw, &input) != nil || input == nil {
		return nil, 0, false, errors.New(createCronInputMessage)
	}
	if cron, present := input["cron"]; present {
		var expression string
		if json.Unmarshal(cron, &expression) != nil || expression == "" || input["everyMs"] != nil || input["firstFireAt"] != nil {
			return nil, 0, false, errors.New(createCronInputMessage)
		}
		if timezone, present := input["timezone"]; present {
			var value string
			if json.Unmarshal(timezone, &value) != nil {
				return nil, 0, false, errors.New(createCronInputMessage)
			}
		}
		return nil, 0, true, nil
	}
	if input["timezone"] != nil || input["everyMs"] == nil && input["firstFireAt"] == nil {
		return nil, 0, false, errors.New(createCronInputMessage)
	}
	schedule, nextFireAt, err := normalizeSourceIntervalSchedule(raw, now)
	return schedule, nextFireAt, false, err
}

func sourceCronString(input map[string]json.RawMessage, key string) (string, bool) {
	raw, present := input[key]
	if !present {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func sourceCronHasActionOrMessage(input map[string]json.RawMessage) bool {
	for _, key := range []string{"action", "message"} {
		var value string
		if json.Unmarshal(input[key], &value) == nil {
			return true
		}
	}
	return false
}

func sourceCronDestinationValid(input map[string]json.RawMessage) bool {
	raw, present := input["destination"]
	if !present {
		return true
	}
	var destination map[string]json.RawMessage
	return json.Unmarshal(raw, &destination) == nil && destination != nil
}

func sourceCronContentID(input map[string]json.RawMessage, title string) (string, error) {
	parts := make([]string, 0, 9)
	for _, key := range []string{"owner", "ownerScopeId", "schedule", "action", "message", "destination", "runAs", "members"} {
		part, err := sourceCronContentPart(input[key])
		if err != nil {
			return "", err
		}
		parts = append(parts, part)
	}
	titlePart, err := sourceCronJSONString(title)
	if err != nil {
		return "", err
	}
	parts = append(parts, titlePart)
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(digest[:])[:16], nil
}

func sourceCronContentPart(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return sourceCronJSONString(value)
}

func sourceCronJSONString(value any) (string, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSuffix(out.String(), "\n"), nil
}

func sourceCronJSONTruthy(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return false
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return typed != ""
	case float64:
		return typed != 0
	default:
		return true // JavaScript treats objects and arrays as truthy.
	}
}

func sourceCronSamePerson(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if strings.Contains(left, "@") {
		left = strings.ToLower(left)
	}
	if strings.Contains(right, "@") {
		right = strings.ToLower(right)
	}
	return left != "" && left == right
}

func mustMarshalJSON(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}
