package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/hejw/qm-backend/internal/auth"
	"github.com/hejw/qm-backend/internal/config"
	"github.com/hejw/qm-backend/internal/data"
)

func TestSourceCronCreateRouteUsesSharedDurableRecord(t *testing.T) {
	databaseURL := os.Getenv("QM_BACKEND_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("QM_BACKEND_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pg, err := data.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)
	if _, err := pg.Pool.Exec(ctx, "SELECT pg_advisory_lock(hashtext('qm-backend-integration-tests'))"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.Pool.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtext('qm-backend-integration-tests'))")
	})
	if err := pg.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "TRUNCATE crons,durable_map_versions,source_auth_replay RESTART IDENTITY"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.QM.RouteMode = "go"
	cfg.Auth.SourceSigningSecret = "cron-create-source-secret"
	h := &HTTPServer{
		config: cfg,
		crons:  data.NewCronRepository(pg),
		auth:   auth.Verifier{SourceSecret: cfg.Auth.SourceSigningSecret, ReplayWindow: time.Minute, DB: pg.Pool},
		logger: slog.Default(),
	}
	if !h.owns(http.MethodPost, "/v1/crons") {
		t.Fatal("source cron creation is not Go-owned")
	}
	body := []byte(`{"ownerScopeId":"personal:alice","owner":"alice","createdBy":"alice","schedule":{"everyMs":60000},"action":"check","destination":{"type":"principal","target":"alice"},"title":"  Check   inbox  "}`)
	create := func(attempt int) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/crons?attempt="+strconv.Itoa(attempt), bytes.NewReader(body))
		signSourceRequest(t, req, cfg.Auth.SourceSigningSecret, body)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, req)
		return response
	}
	started := time.Now().UnixMilli()
	first := create(1)
	if first.Code != http.StatusOK {
		t.Fatalf("first create=%d %s", first.Code, first.Body.String())
	}
	var reply struct {
		Cron struct {
			ID         string `json:"id"`
			Title      string `json:"title"`
			NextFireAt int64  `json:"nextFireAt"`
			Schedule   struct {
				EveryMS     int64 `json:"everyMs"`
				FirstFireAt int64 `json:"firstFireAt"`
			} `json:"schedule"`
		} `json:"cron"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Cron.ID != "b86794f0f116917a" || reply.Cron.Title != "Check inbox" || reply.Cron.Schedule.EveryMS != 60_000 || reply.Cron.Schedule.FirstFireAt != reply.Cron.NextFireAt || reply.Cron.NextFireAt < started+59_000 {
		t.Fatalf("first reply=%s", first.Body.String())
	}
	second := create(2)
	if second.Code != http.StatusOK || !bytes.Contains(second.Body.Bytes(), []byte(`"id":"b86794f0f116917a"`)) {
		t.Fatalf("deduplicated create=%d %s", second.Code, second.Body.String())
	}
	var records int
	if err := pg.Pool.QueryRow(ctx, "SELECT count(*) FROM crons WHERE id=$1", reply.Cron.ID).Scan(&records); err != nil || records != 1 {
		t.Fatalf("stored records=%d err=%v", records, err)
	}
	fastBody := []byte(`{"ownerScopeId":"personal:alice","owner":"alice","createdBy":"alice","schedule":{"everyMs":1},"action":"check"}`)
	fastReq := httptest.NewRequest(http.MethodPost, "/v1/crons", bytes.NewReader(fastBody))
	signSourceRequest(t, fastReq, cfg.Auth.SourceSigningSecret, fastBody)
	fastResponse := httptest.NewRecorder()
	h.ServeHTTP(fastResponse, fastReq)
	if fastResponse.Code != http.StatusBadRequest || !bytes.Contains(fastResponse.Body.Bytes(), []byte(`"error":"cron_create_failed"`)) || !bytes.Contains(fastResponse.Body.Bytes(), []byte("at least 60000ms")) {
		t.Fatalf("fast interval create=%d %s", fastResponse.Code, fastResponse.Body.String())
	}
}
