package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/quietbranch88/gogolook-task-api/internal/httpapi"
	"github.com/quietbranch88/gogolook-task-api/internal/postgres"
	"github.com/quietbranch88/gogolook-task-api/internal/task"
)

// Exercises an actual PostgreSQL startup rejection through token
// authentication, then the authenticated create/commit path after recovery.
// The oracle is the storage-unavailable and tenant-scope HTTP contract, not a
// fabricated driver error or an alternate copy of the application's schema.
func TestHTTPPostgresConnectionLimitRecovery(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	config := f.pool.Config().Copy()
	schema := config.ConnConfig.RuntimeParams["search_path"]
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	role := "connection_limit_" + randomHex()[:16]
	quotedRole := pgx.Identifier{role}.Sanitize()
	password := randomHex() // Hex-only, generated for this disposable role.
	if _, err := f.pool.Exec(ctx, "CREATE ROLE "+quotedRole+
		" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 1 PASSWORD '"+password+"'"); err != nil {
		t.Fatalf("create isolated connection-limit role (test DB needs a superuser): %v", err)
	}
	// Registered before the role's connections: LIFO cleanup closes those first,
	// revokes only this test's grants, and drops the role before setup's schema.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		for _, statement := range []string{
			"REVOKE ALL PRIVILEGES ON TABLE " + quotedSchema + ".api_keys, " + quotedSchema + ".memberships, " + quotedSchema + ".tasks FROM " + quotedRole,
			"REVOKE ALL PRIVILEGES ON SEQUENCE " + quotedSchema + ".tasks_id_seq FROM " + quotedRole,
			"REVOKE ALL PRIVILEGES ON SCHEMA " + quotedSchema + " FROM " + quotedRole,
			"DROP ROLE " + quotedRole,
		} {
			if _, err := f.pool.Exec(cleanupCtx, statement); err != nil {
				t.Errorf("clean up isolated connection-limit role: %v", err)
			}
		}
	})
	for _, statement := range []string{
		"GRANT USAGE ON SCHEMA " + quotedSchema + " TO " + quotedRole,
		"GRANT SELECT ON TABLE " + quotedSchema + ".api_keys TO " + quotedRole,
		// SELECT FOR SHARE requires UPDATE permission as well as SELECT.
		"GRANT SELECT, UPDATE ON TABLE " + quotedSchema + ".memberships TO " + quotedRole,
		"GRANT SELECT, INSERT ON TABLE " + quotedSchema + ".tasks TO " + quotedRole,
		"GRANT USAGE ON SEQUENCE " + quotedSchema + ".tasks_id_seq TO " + quotedRole,
	} {
		if _, err := f.pool.Exec(ctx, statement); err != nil {
			t.Fatalf("grant fixture-only access: %v", err)
		}
	}

	config.ConnConfig.User = role
	config.ConnConfig.Password = password
	config.MinConns = 0
	config.MinIdleConns = 0
	config.MaxConns = 1
	blocker, err := pgx.ConnectConfig(ctx, config.ConnConfig.Copy())
	if err != nil {
		t.Fatalf("occupy the isolated role's sole connection: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := blocker.Close(closeCtx); err != nil {
			t.Errorf("close isolated blocking connection: %v", err)
		}
	})
	var actualRole, actualSchema string
	var superuser bool
	var connectionLimit int
	if err := blocker.QueryRow(ctx, `SELECT current_user, current_schema(), rolsuper, rolconnlimit FROM pg_roles WHERE rolname=current_user`).Scan(&actualRole, &actualSchema, &superuser, &connectionLimit); err != nil {
		t.Fatal(err)
	}
	if actualRole != role || actualSchema != schema || superuser || connectionLimit != 1 {
		t.Fatal("blocking connection did not use the limited non-superuser role and fixture schema")
	}
	probe, err := pgx.ConnectConfig(ctx, config.ConnConfig.Copy())
	if probe != nil {
		_ = probe.Close(ctx)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "53300" {
		t.Fatalf("second real connection must fail at PostgreSQL startup with SQLSTATE 53300; got %v", err)
	}
	t.Log("confirmed genuine PostgreSQL startup rejection: SQLSTATE 53300")

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repo := postgres.New(pool)
	server := httptest.NewServer(httpapi.New(task.NewService(repo), repo, false, pool.Ping))
	t.Cleanup(server.Close)
	request := func(method, token, tenant, body string) (int, http.Header, []byte) {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, method, server.URL+"/tasks", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Tenant-ID", tenant)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		res, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, res.Header, data
	}
	assertCounts := func(wantA, wantB int) {
		t.Helper()
		var a, b int
		if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tasks WHERE tenant_id=101), (SELECT count(*) FROM tasks WHERE tenant_id=102)`).Scan(&a, &b); err != nil {
			t.Fatal(err)
		}
		if a != wantA || b != wantB {
			t.Fatalf("durable tenant counts: A=%d B=%d; want A=%d B=%d", a, b, wantA, wantB)
		}
	}
	const body = `{"name":"create after connection recovery","status":1,"detail":"preserve this detail"}`
	status, headers, data := request(http.MethodPost, f.a, "101", body)
	if status != http.StatusServiceUnavailable || headers.Get("Retry-After") != "1" || string(data) != "{\"error\":\"service unavailable\"}\n" {
		t.Errorf("startup refusal: want 503, Retry-After 1 and fixed safe body; got status=%d Retry-After=%q body=%q", status, headers.Get("Retry-After"), data)
	}
	assertCounts(0, 0)

	blockedPID := blocker.PgConn().PID()
	if err := blocker.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// Wait only for this connection to leave PostgreSQL's backend registry. This
	// avoids a termination race without retrying the application write.
	recoveryCtx, recoveryCancel := context.WithTimeout(ctx, 5*time.Second)
	defer recoveryCancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var stillConnected bool
		if err := f.pool.QueryRow(recoveryCtx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND usename=$2 AND datname=current_database())`, blockedPID, role).Scan(&stillConnected); err != nil {
			t.Fatal(err)
		}
		if !stillConnected {
			break
		}
		select {
		case <-ticker.C:
		case <-recoveryCtx.Done():
			t.Fatal("isolated blocking connection did not finish closing")
		}
	}

	status, _, data = request(http.MethodPost, f.a, "101", body)
	if status != http.StatusCreated {
		t.Fatalf("same authenticated POST after recovery: want 201; got %d body=%q", status, data)
	}
	created := decodeTask(t, data, "create after connection recovery", 1)
	if created.Detail != "preserve this detail" || created.Version != 1 {
		t.Fatalf("incorrect recovered result: %+v", created)
	}
	assertCounts(1, 0)
	var stored task.Task
	if err := f.pool.QueryRow(ctx, `SELECT id,name,status,detail,version,created_at,updated_at FROM tasks WHERE tenant_id=101 AND id=$1`, created.ID).Scan(&stored.ID, &stored.Name, &stored.Status, &stored.Detail, &stored.Version, &stored.CreatedAt, &stored.UpdatedAt); err != nil || !tasksEqual(stored, created) {
		t.Fatalf("recovered response differs from committed tenant-owned task: stored=%+v response=%+v err=%v", stored, created, err)
	}

	for _, check := range []struct {
		token, tenant string
		want          []task.Task
	}{{f.a, "101", []task.Task{created}}, {f.b, "102", []task.Task{}}} {
		status, _, data = request(http.MethodGet, check.token, check.tenant, "")
		var listed []task.Task
		if err := json.Unmarshal(data, &listed); status != http.StatusOK || err != nil || !slices.EqualFunc(listed, check.want, tasksEqual) {
			t.Fatalf("tenant %s read after recovery: status=%d body=%q err=%v", check.tenant, status, data, err)
		}
	}
	status, _, data = request(http.MethodPost, f.a, "102", body)
	if status != http.StatusForbidden || string(data) != "{\"error\":\"membership required\"}\n" {
		t.Fatalf("cross-tenant create after recovery: want 403; got %d body=%q", status, data)
	}
	assertCounts(1, 0)
}
