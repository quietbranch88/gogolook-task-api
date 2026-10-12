package postgres

import (
	"context"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/001_schema.sql
var migrations embed.FS

// SchemaVersion identifies the initial application schema.
const SchemaVersion = 1

// Migrate applies the embedded baseline in one transaction guarded by an advisory
// lock. A completed baseline is a no-op; incompatible histories are rejected.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Arbitrary but fixed key for this application's migrations; it must never change,
	// and nothing else on the same database may use it.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(710112026)`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(version integer PRIMARY KEY, created_at timestamptz NOT NULL DEFAULT statement_timestamp())`); err != nil {
		return err
	}
	var incompatible bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version <> $1)`, SchemaVersion).Scan(&incompatible); err != nil {
		return err
	}
	if incompatible {
		return fmt.Errorf("incompatible migration history: this build requires baseline %d in a fresh application schema", SchemaVersion)
	}
	var applied bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, SchemaVersion).Scan(&applied); err != nil {
		return err
	}
	if !applied {
		sql, readErr := migrations.ReadFile("migrations/001_schema.sql")
		if readErr != nil {
			return readErr
		}
		if _, err = tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("migration %d: %w", SchemaVersion, err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES($1)`, SchemaVersion); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// SchemaReady requires the baseline's sole ledger entry. It does not inspect
// actual tables, columns, constraints or triggers for manual schema drift.
func SchemaReady(ctx context.Context, pool *pgxpool.Pool) error {
	var ok bool
	if err := pool.QueryRow(ctx, `SELECT count(*) = 1 AND COALESCE(bool_and(version=$1),false) FROM schema_migrations`, SchemaVersion).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("schema requires the sole baseline version %d", SchemaVersion)
	}
	return nil
}
