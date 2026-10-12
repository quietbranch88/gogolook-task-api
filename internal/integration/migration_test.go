package integration_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/quietbranch88/gogolook-task-api/internal/postgres"
)

func emptyMigrationSchema(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	schema := "migration_" + randomHex()[:16]
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	config := admin.Config()
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestSingleBaselineMigration(t *testing.T) {
	f := setup(t)
	pool := emptyMigrationSchema(t, f.pool)
	if err := postgres.SchemaReady(t.Context(), pool); err == nil {
		t.Fatal("empty schema accepted before migration")
	}
	// Competing first runs must serialize and leave one committed baseline.
	done := make(chan error, 2)
	for range 2 {
		go func() { done <- postgres.Migrate(t.Context(), pool) }()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	var versions []int
	if err := pool.QueryRow(t.Context(), `SELECT array_agg(version ORDER BY version) FROM schema_migrations`).Scan(&versions); err != nil || !reflect.DeepEqual(versions, []int{1}) {
		t.Fatalf("baseline ledger = %v, want [1]: %v", versions, err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO tenants(id,name) VALUES(101,'preserved'); INSERT INTO tasks(tenant_id,name,status,detail) VALUES(101,'existing',1,'keep')`); err != nil {
		t.Fatal(err)
	}
	var version int64
	if err := pool.QueryRow(t.Context(), `SELECT version FROM tasks WHERE tenant_id=101`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("initial task version = %d, want 1: %v", version, err)
	}
	var before, after string
	snapshot := `SELECT jsonb_build_object('tasks',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM tasks t),'migrations',(SELECT jsonb_agg(to_jsonb(t) ORDER BY version) FROM schema_migrations t))::text`
	if err := pool.QueryRow(t.Context(), snapshot).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal("migration rerun", err)
	}
	if err := pool.QueryRow(t.Context(), snapshot).Scan(&after); err != nil || after != before {
		t.Fatalf("rerun changed durable rows: %v", err)
	}
	if err := postgres.SchemaReady(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE tasks SET version=0 WHERE tenant_id=101`); err == nil {
		t.Fatal("nonpositive database version accepted")
	}
}

func TestMigrationRejectsIncompatibleHistoryWithoutChanges(t *testing.T) {
	f := setup(t)
	// Only the single initial version is supported; refuse other or mixed versions.
	for name, versions := range map[string][]int{
		"zero": {0}, "negative": {-1}, "other": {2},
		"several": {2, 3, 4}, "newer": {5}, "mixed": {1, 2}, "extra": {1, 5},
	} {
		t.Run(name, func(t *testing.T) {
			pool := emptyMigrationSchema(t, f.pool)
			if _, err := pool.Exec(t.Context(), `CREATE TABLE schema_migrations(version integer PRIMARY KEY); CREATE TABLE tasks(marker text); INSERT INTO tasks VALUES('preserve me')`); err != nil {
				t.Fatal(err)
			}
			for _, version := range versions {
				if _, err := pool.Exec(t.Context(), `INSERT INTO schema_migrations VALUES($1)`, version); err != nil {
					t.Fatal(err)
				}
			}
			if err := postgres.SchemaReady(t.Context(), pool); err == nil {
				t.Fatal("incompatible history accepted at startup")
			}
			if err := postgres.Migrate(t.Context(), pool); err == nil || !strings.Contains(err.Error(), "incompatible migration history") {
				t.Fatalf("want explicit history rejection, got %v", err)
			}
			var marker string
			if err := pool.QueryRow(t.Context(), `SELECT marker FROM tasks`).Scan(&marker); err != nil || marker != "preserve me" {
				t.Fatal("pre-existing data changed", err)
			}
			var got []int
			if err := pool.QueryRow(t.Context(), `SELECT array_agg(version ORDER BY version) FROM schema_migrations`).Scan(&got); err != nil || !reflect.DeepEqual(got, versions) {
				t.Fatalf("ledger changed: %v, want %v: %v", got, versions, err)
			}
			var count int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema()`).Scan(&count); err != nil || count != 2 {
				t.Fatalf("rejection left schema changes: %d tables, %v", count, err)
			}
		})
	}
}
