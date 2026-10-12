package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/quietbranch88/gogolook-task-api/internal/postgres"
)

// These expectations are the complete persisted schema contract, including the
// original columns. They intentionally do not derive from migration source.
func TestTimestampSchemaInventory(t *testing.T) {
	f := setup(t)
	type column struct{ Type, Nullable, Default, Identity string }
	want := map[string]column{}
	add := func(table, name, typ, nullable, def, identity string) {
		want[table+"."+name] = column{typ, nullable, def, identity}
	}
	for table, names := range map[string][]string{"tenants": {"id"}, "users": {"id"}, "memberships": {"tenant_id", "user_id"}, "api_keys": {"user_id"}, "tasks": {"tenant_id"}, "task_create_keys": {"tenant_id", "user_id"}} {
		for _, name := range names {
			add(table, name, "bigint", "NO", "", "NO")
		}
	}
	add("tasks", "id", "bigint", "NO", "", "YES")
	add("tasks", "version", "bigint", "NO", "1", "NO")
	add("tasks", "status", "smallint", "NO", "", "NO")
	add("tasks", "name", "text", "NO", "", "NO")
	add("tasks", "detail", "text", "NO", "''::text", "NO")
	add("tenants", "name", "text", "NO", "", "NO")
	add("api_keys", "token_hash", "bytea", "NO", "", "NO")
	add("api_keys", "expires_at", "timestamp with time zone", "NO", "", "NO")
	add("api_keys", "revoked_at", "timestamp with time zone", "YES", "", "NO")
	add("task_create_keys", "key_hash", "bytea", "NO", "", "NO")
	add("task_create_keys", "input_hash", "bytea", "NO", "", "NO")
	add("task_create_keys", "task_id", "bigint", "YES", "", "NO")
	add("schema_migrations", "version", "integer", "NO", "", "NO")
	for _, table := range []string{"tenants", "users", "memberships", "api_keys", "tasks", "task_create_keys", "schema_migrations"} {
		add(table, "created_at", "timestamp with time zone", "NO", "statement_timestamp()", "NO")
	}
	for _, table := range []string{"tenants", "api_keys", "tasks"} {
		add(table, "updated_at", "timestamp with time zone", "NO", "statement_timestamp()", "NO")
	}
	rows, err := f.pool.Query(context.Background(), `SELECT table_name,column_name,data_type,is_nullable,COALESCE(column_default,''),is_identity FROM information_schema.columns WHERE table_schema=current_schema()`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]column{}
	for rows.Next() {
		var table, name string
		var c column
		if err := rows.Scan(&table, &name, &c.Type, &c.Nullable, &c.Default, &c.Identity); err != nil {
			t.Fatal(err)
		}
		got[table+"."+name] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(want) != 31 || !reflect.DeepEqual(got, want) {
		t.Fatalf("31-column inventory differs:\ngot %#v\nwant %#v", got, want)
	}
	rows, err = f.pool.Query(context.Background(), `SELECT c.relname,pg_get_constraintdef(k.oid) FROM pg_constraint k JOIN pg_class c ON c.oid=k.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND k.contype='p'`)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{}
	for rows.Next() {
		var table, key string
		if err := rows.Scan(&table, &key); err != nil {
			t.Fatal(err)
		}
		keys[table] = key
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	wantKeys := map[string]string{"tenants": "PRIMARY KEY (id)", "users": "PRIMARY KEY (id)", "memberships": "PRIMARY KEY (tenant_id, user_id)", "api_keys": "PRIMARY KEY (token_hash)", "tasks": "PRIMARY KEY (id)", "task_create_keys": "PRIMARY KEY (tenant_id, user_id, key_hash)", "schema_migrations": "PRIMARY KEY (version)"}
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Fatalf("primary keys changed: %v", keys)
	}
	var count int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM schema_migrations WHERE created_at IS NOT NULL AND version=1`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("fresh migration times count=%d err=%v", count, err)
	}
}

func timestampObject(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"created_at", "updated_at"} {
		raw, ok := value[field].(string)
		if !ok {
			t.Fatalf("required new-row %s timestamp missing: %s", field, data)
		}
		if _, err := time.Parse(time.RFC3339Nano, raw); err != nil || !strings.HasSuffix(raw, "Z") {
			t.Fatalf("%s must be RFC3339 UTC: %s", field, data)
		}
	}
	return value
}

func TestTaskTimestampHTTPDurabilityAndGuards(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	body := `{"name":"timestamp task","status":0}`
	code, v, err := keyedPost(f.server.URL, f.a, "101", "timestamp-key", body)
	if err != nil || code != 201 {
		t.Fatalf("create: %d %v", code, err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	created := timestampObject(t, data)
	if created["created_at"] != created["updated_at"] {
		t.Fatalf("initial times differ: %s", data)
	}
	path := fmt.Sprintf("/tasks/%d", v.ID)
	stored := func() map[string]any {
		t.Helper()
		var raw []byte
		if err := f.pool.QueryRow(ctx, `SELECT row_to_json(x) FROM (SELECT id,name,status,detail,version,created_at AT TIME ZONE 'UTC' AS created_at,updated_at AT TIME ZONE 'UTC' AS updated_at FROM tasks WHERE id=$1) x`, v.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatal(err)
		}
		return obj
	}
	checkDurable := func(want map[string]any) {
		t.Helper()
		var ct, ut time.Time
		var version int64
		if err := f.pool.QueryRow(ctx, `SELECT created_at,updated_at,version FROM tasks WHERE id=$1`, v.ID).Scan(&ct, &ut, &version); err != nil {
			t.Fatal(err)
		}
		if ct.UTC().Format(time.RFC3339Nano) != want["created_at"] || ut.UTC().Format(time.RFC3339Nano) != want["updated_at"] || float64(version) != want["version"] {
			t.Fatalf("response differs from durable times/version: %v %v v%d / %v", ct, ut, version, want)
		}
	}
	checkDurable(created)
	var keyCreated time.Time
	if err := f.pool.QueryRow(ctx, `SELECT created_at FROM task_create_keys WHERE task_id=$1`, v.ID).Scan(&keyCreated); err != nil {
		t.Fatal(err)
	}
	updated := timestampObject(t, f.call(t, "PUT", path, f.a, "101", `{"name":"timestamp task","status":0,"version":1}`, 200))
	ct, _ := time.Parse(time.RFC3339Nano, created["updated_at"].(string))
	ut, _ := time.Parse(time.RFC3339Nano, updated["updated_at"].(string))
	if updated["created_at"] != created["created_at"] || !ut.After(ct) || updated["version"] != float64(2) {
		t.Fatalf("accepted same-content PUT must advance version/time: %v -> %v", created, updated)
	}
	checkDurable(updated)
	before := stored()
	f.call(t, "PUT", path, f.a, "101", `{"name":"stale","status":1,"version":1}`, 409)
	for _, field := range []string{"created_at", "updated_at"} {
		for _, value := range []string{`null`, `"2000-01-01T00:00:00Z"`} {
			f.call(t, "POST", "/tasks", f.a, "101", `{"name":"injected","status":0,"`+field+`":`+value+`}`, 400)
			f.call(t, "PUT", path, f.a, "101", `{"name":"injected","status":0,"version":2,"`+field+`":`+value+`}`, 400)
		}
	}
	f.call(t, "PUT", path, f.b, "102", `{"name":"foreign","status":1,"version":2}`, 404)
	f.call(t, "DELETE", path, f.b, "102", "", 404)
	f.call(t, "PUT", path, f.b, "101", `{"name":"forbidden","status":1,"version":2}`, 403)
	code, v, err = keyedPost(f.server.URL, f.a, "101", "timestamp-key", body)
	if err != nil || code != 201 {
		t.Fatalf("replay: %d %v", code, err)
	}
	data, _ = json.Marshal(v)
	replay := timestampObject(t, data)
	if !reflect.DeepEqual(replay, updated) || !reflect.DeepEqual(stored(), before) {
		t.Fatalf("guards/replay changed row: replay=%v expected=%v", replay, updated)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE tasks SET name=name WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored(), before) {
		t.Fatal("no-op SQL changed timestamps")
	}
	var list []map[string]any
	if err := json.Unmarshal(f.call(t, "GET", "/tasks", f.a, "101", "", 200), &list); err != nil || len(list) != 1 || !reflect.DeepEqual(list[0], updated) {
		t.Fatalf("list timestamps: %v %v", list, err)
	}
	f.call(t, "DELETE", path, f.a, "101", "", 204)
	code, _, err = keyedPost(f.server.URL, f.a, "101", "timestamp-key", body)
	if err != nil || code != 409 {
		t.Fatalf("tombstone replay: %d %v", code, err)
	}
	var retained time.Time
	var taskID *int64
	if err := f.pool.QueryRow(ctx, `SELECT created_at,task_id FROM task_create_keys WHERE tenant_id=101 AND key_hash=$1`, hashBytes("timestamp-key")).Scan(&retained, &taskID); err != nil || taskID != nil || !retained.Equal(keyCreated) {
		t.Fatalf("tombstone lost original age: %v %v %v", retained, taskID, err)
	}
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE id=$1`, v.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("physical delete count=%d %v", count, err)
	}
}

func hashBytes(value string) []byte { digest := sha256.Sum256([]byte(value)); return digest[:] }

func TestTimestampNotNullAndOperationalWrites(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// Every row has known creation and update times: the schema rejects NULL and
	// the update trigger keeps creation time fixed.
	for _, tc := range []struct{ statement, table, column string }{
		{`INSERT INTO tenants(id,name,created_at) VALUES(900,'no time',NULL)`, "tenants", "created_at"},
		{`INSERT INTO tasks(tenant_id,name,status,created_at) VALUES(101,'no time',0,NULL)`, "tasks", "created_at"},
		{`INSERT INTO tasks(tenant_id,name,status,updated_at) VALUES(101,'no time',0,NULL)`, "tasks", "updated_at"},
	} {
		_, err := f.pool.Exec(ctx, tc.statement)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23502" || pgErr.TableName != tc.table || pgErr.ColumnName != tc.column {
			t.Fatalf("want NOT NULL rejection for %s.%s, got %v", tc.table, tc.column, err)
		}
	}
	// Verify mutation triggers and revoked authentication through the real API.
	var keyCreated, keyUpdated time.Time
	if err := f.pool.QueryRow(ctx, `SELECT created_at,updated_at FROM api_keys WHERE token_hash=$1`, hashBytes(f.a)).Scan(&keyCreated, &keyUpdated); err != nil || !keyCreated.Equal(keyUpdated) {
		t.Fatalf("new key initial times: %v %v %v", keyCreated, keyUpdated, err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE api_keys SET revoked_at=statement_timestamp() WHERE token_hash=$1`, hashBytes(f.a)); err != nil {
		t.Fatal(err)
	}
	var afterCreated, afterUpdated time.Time
	if err := f.pool.QueryRow(ctx, `SELECT created_at,updated_at FROM api_keys WHERE token_hash=$1`, hashBytes(f.a)).Scan(&afterCreated, &afterUpdated); err != nil || !afterCreated.Equal(keyCreated) || !afterUpdated.After(keyUpdated) {
		t.Fatalf("revocation times: %v %v %v", afterCreated, afterUpdated, err)
	}
	f.call(t, "GET", "/tasks", f.a, "101", "", 401)
	if err := f.pool.QueryRow(ctx, `SELECT created_at,updated_at FROM tenants WHERE id=101`).Scan(&keyCreated, &keyUpdated); err != nil || !keyCreated.Equal(keyUpdated) {
		t.Fatal("initial tenant times", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE tenants SET name='renamed' WHERE id=101`); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT created_at,updated_at FROM tenants WHERE id=101`).Scan(&afterCreated, &afterUpdated); err != nil || !afterCreated.Equal(keyCreated) || !afterUpdated.After(keyUpdated) {
		t.Fatalf("tenant update times: %v %v %v", afterCreated, afterUpdated, err)
	}
	seedHash := hashBytes("seed-timestamp-token")
	for _, seed := range []func() error{func() error { return postgres.SeedDemo(ctx, f.pool) }, func() error { return postgres.SeedLoad(ctx, f.pool, 2, [][]byte{seedHash}) }} {
		if err := seed(); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func() string {
		t.Helper()
		var value string
		if err := f.pool.QueryRow(ctx, `SELECT jsonb_build_object('tenants',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM tenants t),'users',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM users t),'memberships',(SELECT jsonb_agg(to_jsonb(t) ORDER BY tenant_id,user_id) FROM memberships t),'api_keys',(SELECT jsonb_agg(to_jsonb(t) ORDER BY token_hash) FROM api_keys t),'tasks',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM tasks t),'migrations',(SELECT jsonb_agg(to_jsonb(t) ORDER BY version) FROM schema_migrations t))::text`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := snapshot()
	if err := postgres.SeedDemo(ctx, f.pool); err != nil {
		t.Fatal(err)
	}
	if err := postgres.SeedLoad(ctx, f.pool, 2, [][]byte{seedHash}); err != nil {
		t.Fatal(err)
	}
	if err := postgres.Migrate(ctx, f.pool); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); before != after {
		t.Fatal("seed/migration rerun changed persisted values/times")
	}
}
