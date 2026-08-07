package data

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSoulRepositoryUpdateLatestKeepsNodeHistoryShape(t *testing.T) {
	pg := openIntegrationPostgres(t)
	ctx := context.Background()
	if _, err := pg.Pool.Exec(ctx, "DELETE FROM soul_history"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "DELETE FROM soul_configs"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "DELETE FROM durable_map_versions WHERE tbl='soul_configs'"); err != nil {
		t.Fatal(err)
	}
	scope := "personal:alice"
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO soul_history(id,json) VALUES($1,$2::jsonb)", "legacy-soul-1", `{"scopeId":"personal:alice","content":"legacy","version":1,"updatedAt":11,"updatedBy":"alice"}`); err != nil {
		t.Fatal(err)
	}
	// Pre-embedded-history documents are still present in some deployments.
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO soul_configs(id,json) VALUES($1,$2::jsonb)", scope, `{"scopeId":"personal:alice","content":"current","version":2,"updatedAt":22,"updatedBy":"alice"}`); err != nil {
		t.Fatal(err)
	}
	repository := NewSoulRepository(pg)
	version, err := repository.UpdateLatest(ctx, scope, "new", "alice")
	if err != nil || version != 3 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var raw []byte
	if err := pg.Pool.QueryRow(ctx, "SELECT json FROM soul_configs WHERE id=$1", scope).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Content    string         `json:"content"`
		Version    int            `json:"version"`
		History    []SoulRevision `json:"history"`
		MutationID string         `json:"mutationId"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Content != "new" || saved.Version != 3 || len(saved.History) != 3 || saved.History[0].Version != 3 || saved.History[1].Version != 2 || saved.History[2].Version != 1 || saved.History[2].Content != "legacy" || len(saved.MutationID) != 24 {
		t.Fatalf("saved=%s parsed=%#v", raw, saved)
	}
	var cacheVersion int
	if err := pg.Pool.QueryRow(ctx, "SELECT v FROM durable_map_versions WHERE tbl='soul_configs'").Scan(&cacheVersion); err != nil || cacheVersion != 1 {
		t.Fatalf("cache version=%d err=%v", cacheVersion, err)
	}
	version, err = repository.UpdateLatest(ctx, scope, "newest", "alice")
	if err != nil || version != 4 {
		t.Fatalf("next version=%d err=%v", version, err)
	}
}
