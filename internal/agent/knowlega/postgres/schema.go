package postgres

import (
	"context"
	"database/sql"
	"net/url"
	"strings"

	// Open dials with the pgx stdlib driver, so this package registers it
	// rather than relying on every caller to do so.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Schema is the dedicated PostgreSQL schema that owns every Knowlega table.
//
// Knowlega and the QM control plane both define a `projects` table with
// different shapes. They share one database, so without this schema the second
// bootstrap to run has its `CREATE TABLE IF NOT EXISTS projects` silently
// skipped and every query against the loser's shape fails at runtime.
const Schema = "knowledge_core"

// SearchPath keeps `public` after the Knowlega schema so shared extensions
// installed there (notably `vector`) stay resolvable.
const SearchPath = Schema + ",public"

// SearchPathDSN returns dsn with the Knowlega search_path applied, replacing any
// search_path already present. Every connection backing a Store must go through
// this, otherwise Migrate creates Knowlega's tables in `public` and collides
// with the QM control plane.
func SearchPathDSN(dsn string) string {
	if parsed, err := url.Parse(dsn); err == nil && parsed.Scheme != "" {
		query := parsed.Query()
		query.Set("search_path", SearchPath)
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	return dsn + separator + "search_path=" + SearchPath
}

// Open connects to dsn with the Knowlega search_path applied, ensures the
// schema exists, and returns a ready database handle. Callers own the returned
// *sql.DB and must close it.
func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", SearchPathDSN(dsn))
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	// CREATE SCHEMA cannot run through the search_path connection before the
	// schema exists on some pooled setups, but it is valid here: an unknown
	// search_path entry is ignored until something resolves against it.
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+Schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// OpenStore is Open followed by NewStore and Migrate. This is the only
// supported way to build a Store from a DSN.
func OpenStore(ctx context.Context, dsn string) (*Store, func(), error) {
	db, err := Open(ctx, dsn)
	if err != nil {
		return nil, func() {}, err
	}
	store := NewStore(db)
	if err := store.Migrate(ctx); err != nil {
		db.Close()
		return nil, func() {}, err
	}
	return store, func() { _ = db.Close() }, nil
}