package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSourceCronCreatePayloadMirrorsNodeContentIDAndNormalizesInterval(t *testing.T) {
	raw := []byte(`{"ownerScopeId":"personal:alice","owner":"alice","createdBy":"alice","schedule":{"everyMs":60000},"action":"check","destination":{"type":"principal","target":"alice"},"title":"  Check   inbox  "}`)
	payload, id, calendar, err := sourceCronCreatePayload(raw, 1_000_000)
	if err != nil || calendar {
		t.Fatalf("create payload err=%v calendar=%t", err, calendar)
	}
	// Calculated by Node's CronStore hashId/contentPart contract. This proves a
	// concurrent Node and Go create choose the same DurableMap key.
	if id != "b86794f0f116917a" {
		t.Fatalf("content id=%q", id)
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	if document["id"] != id || document["title"] != "Check inbox" || document["createdAt"] != float64(1_000_000) {
		t.Fatalf("document=%#v", document)
	}
	schedule, ok := document["schedule"].(map[string]any)
	if !ok || schedule["everyMs"] != float64(60_000) || schedule["firstFireAt"] != float64(1_060_000) || document["nextFireAt"] != float64(1_060_000) {
		t.Fatalf("normalized schedule=%#v document=%#v", schedule, document)
	}
}

func TestSourceCronCreatePayloadLeavesCalendarSchedulesToNode(t *testing.T) {
	payload, id, calendar, err := sourceCronCreatePayload([]byte(`{"ownerScopeId":"personal:alice","owner":"alice","createdBy":"alice","schedule":{"cron":"30 7 * * 1-5","timezone":"America/Los_Angeles"},"action":"check"}`), 1_000_000)
	if err != nil || !calendar || payload != nil || id != "" {
		t.Fatalf("calendar payload=%s id=%q calendar=%t err=%v", payload, id, calendar, err)
	}
}

func TestSourceCronCreatePayloadRequiresCrossOwnerConsent(t *testing.T) {
	_, _, calendar, err := sourceCronCreatePayload([]byte(`{"ownerScopeId":"personal:bob","owner":"bob","createdBy":"alice","schedule":{"firstFireAt":1000000},"action":"check"}`), 1_000_000)
	var createErr *sourceCronCreateError
	if calendar || !errors.As(err, &createErr) || createErr.Error() != "assigning a different owner requires that owner's consent" {
		t.Fatalf("calendar=%t err=%v", calendar, err)
	}
}

func TestCreateSourceCronProxiesCalendarSchedules(t *testing.T) {
	proxied := false
	h := &HTTPServer{proxy: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied = true
		w.WriteHeader(http.StatusNoContent)
	})}
	request := httptest.NewRequest(http.MethodPost, "/v1/crons", nil)
	response := httptest.NewRecorder()
	h.createSourceCron(response, request, []byte(`{"ownerScopeId":"personal:alice","owner":"alice","createdBy":"alice","schedule":{"cron":"30 7 * * 1-5"},"action":"check"}`))
	if !proxied || response.Code != http.StatusNoContent {
		t.Fatalf("proxied=%t response=%d", proxied, response.Code)
	}
}
