package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/quietbranch88/gogolook-task-api/internal/postgres"
	"github.com/quietbranch88/gogolook-task-api/internal/task"
)

// Entrypoint tests change process environment/arguments and must not run parallel.
func entrypointEnvironment(t *testing.T) {
	t.Helper()
	previous := os.Args
	os.Args = []string{"api"}
	t.Cleanup(func() { os.Args = previous })
	for _, name := range []string{"AUTH_MODE", "DATABASE_URL", "DB_MAX_CONNS", "HTTP_ADDR", "PPROF_ADDR"} {
		t.Setenv(name, "")
	}
}

func requireRunError(t *testing.T, message string) error {
	t.Helper()
	err := run()
	if err == nil || !strings.Contains(err.Error(), message) {
		t.Fatalf("entrypoint error = %v; want %q", err, message)
	}
	if strings.Contains(err.Error(), "test-secret-must-not-leak") {
		t.Fatal("configuration diagnostic leaked a credential")
	}
	if message == "invalid database configuration" && err.Error() != message {
		t.Fatal("configuration diagnostic must omit driver-provided text")
	}
	return err
}

func TestEntrypointRejectsInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, variable, value, want string }{
		{"auth mode", "AUTH_MODE", "unknown", "AUTH_MODE must be token or demo"},
		{"missing database", "DATABASE_URL", "", "DATABASE_URL is required"},
		{"invalid database URL", "DATABASE_URL", "postgres://u:test-secret-must-not-leak@127.0.0.1:invalid/tasks_test", "invalid database configuration"},
		{"keyword password whitespace", "DATABASE_URL", "host=127.0.0.1 user=u password = test-secret-must-not-leak sslmode=invalid", "invalid database configuration"},
		{"keyword password tab", "DATABASE_URL", "host=127.0.0.1 user=u password\t=test-secret-must-not-leak sslmode=invalid", "invalid database configuration"},
		{"quoted keyword password", "DATABASE_URL", "host=127.0.0.1 user=u password = 'test-secret-must-not-leak' sslmode=invalid", "invalid database configuration"},
		{"keyword TLS password", "DATABASE_URL", "host=127.0.0.1 user=u sslpassword = test-secret-must-not-leak sslmode=invalid", "invalid database configuration"},
		{"URL query password", "DATABASE_URL", "postgres://u@127.0.0.1/tasks_test?password=test-secret-must-not-leak&sslmode=invalid", "invalid database configuration"},
		{"invalid pool parameter", "DATABASE_URL", "postgres://u:test-secret-must-not-leak@127.0.0.1/tasks_test?sslmode=disable&pool_max_conns=invalid", "invalid database configuration"},
		{"health period zero", "DATABASE_URL", "postgres://u:test-secret-must-not-leak@127.0.0.1:1/tasks_test?sslmode=disable&pool_health_check_period=0s", "pool_health_check_period must be greater than zero"},
		{"health period negative", "DATABASE_URL", "postgres://u:test-secret-must-not-leak@127.0.0.1:1/tasks_test?sslmode=disable&pool_health_check_period=-1s", "pool_health_check_period must be greater than zero"},
		{"pool zero", "DB_MAX_CONNS", "0", "DB_MAX_CONNS must be an integer from 1 to 100"},
		{"pool negative", "DB_MAX_CONNS", "-1", "DB_MAX_CONNS must be an integer from 1 to 100"},
		{"pool too large", "DB_MAX_CONNS", "101", "DB_MAX_CONNS must be an integer from 1 to 100"},
		{"pool invalid", "DB_MAX_CONNS", "many", "DB_MAX_CONNS must be an integer from 1 to 100"},
		{"pool integer overflow", "DB_MAX_CONNS", "2147483648", "DB_MAX_CONNS must be an integer from 1 to 100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entrypointEnvironment(t)
			if tc.variable == "DB_MAX_CONNS" {
				t.Setenv("DATABASE_URL", "postgres://u:p@127.0.0.1:1/tasks_test?sslmode=disable")
			}
			t.Setenv(tc.variable, tc.value)
			requireRunError(t, tc.want)
		})
	}
}

func TestEntrypointRejectsUnavailableDatabase(t *testing.T) {
	for _, period := range []string{"", "1s"} {
		t.Run("health_period="+period, func(t *testing.T) {
			entrypointEnvironment(t)
			// Own the bound port but never speak PostgreSQL: exercise an actual connection
			// timeout without depending on a potentially deployed database or service.
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			dsn := "postgres://u:test-secret-must-not-leak@" + listener.Addr().String() + "/tasks_test?sslmode=disable"
			if period != "" {
				dsn += "&pool_health_check_period=" + period
			} else {
				// A valid keyword DSN must also reach the connection attempt.
				_, port, _ := net.SplitHostPort(listener.Addr().String())
				dsn = "host=127.0.0.1 port=" + port + " user=u password = test-secret-must-not-leak dbname=tasks_test sslmode=disable"
			}
			t.Setenv("DATABASE_URL", dsn)
			err = requireRunError(t, "database unavailable")
			var connectionError *pgconn.ConnectError
			if !errors.As(err, &connectionError) {
				t.Fatal("startup failure lost its original connection cause")
			}
			for _, sensitive := range []string{"user=u", "tasks_test", "127.0.0.1", listener.Addr().String(), "test-secret-must-not-leak"} {
				if strings.Contains(err.Error(), sensitive) {
					t.Fatalf("startup diagnostic exposed synthetic sensitive field %q", sensitive)
				}
			}
		})
	}
}

type entrypointDB struct {
	pool, admin         *pgxpool.Pool
	schema, application string
}

func isolatedEntrypointDB(t *testing.T) entrypointDB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("real PostgreSQL not configured; use the Compose test profile")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid TEST_DATABASE_URL")
	}
	if config.ConnConfig.Database != "tasks_test" || os.Getenv("TEST_ALLOW_DATABASE") != "tasks_test" {
		t.Fatal("requires explicitly opted-in disposable tasks_test database")
	}
	host := config.ConnConfig.Host
	if host != "127.0.0.1" && host != "localhost" && host != "test-db" {
		t.Fatal("test database must be local or Compose test-db")
	}
	admin, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var actual string
	if err = admin.QueryRow(t.Context(), "SELECT current_database()").Scan(&actual); err != nil || actual != "tasks_test" {
		t.Fatal("wrong test database")
	}
	random := make([]byte, 8)
	_, _ = rand.Read(random)
	schema := "entrypoint_" + hex.EncodeToString(random)
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	scoped := config.Copy()
	scoped.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("entrypoint tests require a PostgreSQL URL")
	}
	q := u.Query()
	q.Set("search_path", schema)
	q.Set("application_name", schema)
	u.RawQuery = q.Encode()
	t.Setenv("DATABASE_URL", u.String())
	return entrypointDB{pool: pool, admin: admin, schema: schema, application: schema}
}

func TestEntrypointSchemaCommandsAndListenerGuards(t *testing.T) {
	entrypointEnvironment(t)
	f := isolatedEntrypointDB(t)
	requireRunError(t, "schema not ready, run api migrate")
	for _, size := range []string{"1", "100"} {
		t.Setenv("DB_MAX_CONNS", size)
		os.Args = []string{"api", "migrate"}
		if err := run(); err != nil {
			t.Fatal(err)
		}
	}
	var versions int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM schema_migrations").Scan(&versions); err != nil || versions != 1 {
		t.Fatalf("migration versions = %d, err = %v", versions, err)
	}
	os.Args = []string{"api", "seed-demo"}
	requireRunError(t, "seed-demo requires AUTH_MODE=demo") // Unset mode defaults to token.
	t.Setenv("AUTH_MODE", "demo")
	for range 2 {
		if err := run(); err != nil {
			t.Fatal(err)
		}
	}
	var members int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM memberships WHERE tenant_id=1 AND user_id=1").Scan(&members); err != nil || members != 1 {
		t.Fatalf("demo membership = %d, err = %v", members, err)
	}
	for _, args := range [][]string{{"api", "unknown"}, {"api", "migrate", "extra"}} {
		os.Args = args
		requireRunError(t, "usage: api")
	}
	os.Args = []string{"api"}
	for _, address := range []string{"bad-address", "0.0.0.0:6060"} {
		t.Setenv("PPROF_ADDR", address)
		requireRunError(t, "PPROF_ADDR must be a loopback host:port")
	}
	t.Setenv("PPROF_ADDR", "")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	t.Setenv("HTTP_ADDR", listener.Addr().String())
	requireRunError(t, "http server:")
	// An application startup failure must not close the pre-existing listener.
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal("startup failure disturbed the owned competing listener", err)
	}
	_ = conn.Close()
	var active int
	if err := f.admin.QueryRow(t.Context(), "SELECT count(*) FROM pg_stat_activity WHERE application_name=$1", f.application).Scan(&active); err != nil || active != 0 {
		t.Fatalf("startup leaked %d pool connections: %v", active, err)
	}
}

func TestEntrypointMigrationFailureRollsBackAndCanRetry(t *testing.T) {
	entrypointEnvironment(t)
	f := isolatedEntrypointDB(t)
	// A conflicting, pre-existing object makes the actual initial migration fail
	// after earlier DDL. It must preserve that object and roll back its own DDL.
	if _, err := f.pool.Exec(t.Context(), "CREATE TABLE tasks(marker text); INSERT INTO tasks VALUES('preserve me')"); err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"api", "migrate"}
	err := requireRunError(t, "migrate:")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42P07" || !strings.Contains(err.Error(), "SQLSTATE 42P07") {
		t.Fatal("migration failure must retain its cause and safe SQLSTATE diagnostic")
	}
	for _, name := range []string{"tenants", "users", "memberships", "api_keys", "schema_migrations", "task_create_keys"} {
		var present bool
		if err := f.pool.QueryRow(t.Context(), "SELECT to_regclass($1) IS NOT NULL", f.schema+"."+name).Scan(&present); err != nil || present {
			t.Fatalf("failed migration left object %s: present=%v err=%v", name, present, err)
		}
	}
	var marker string
	if err := f.pool.QueryRow(t.Context(), "SELECT marker FROM tasks").Scan(&marker); err != nil || marker != "preserve me" {
		t.Fatal("migration changed the pre-existing object", err)
	}
	if _, err := f.pool.Exec(t.Context(), "DROP TABLE tasks"); err != nil {
		t.Fatal(err)
	}
	if err := run(); err != nil {
		t.Fatal("migration retry failed", err)
	}
	if err := postgres.SchemaReady(t.Context(), f.pool); err != nil {
		t.Fatal(err)
	}
}

func TestEntrypointMaintenancePreservesDatabaseTimeouts(t *testing.T) {
	entrypointEnvironment(t)
	f := isolatedEntrypointDB(t)
	u, err := url.Parse(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("transaction_timeout", "27s")
	q.Set("idle_in_transaction_session_timeout", "28s")
	u.RawQuery = q.Encode()
	t.Setenv("DATABASE_URL", u.String())
	// Observe settings inside actual migration and seed statements, rather than
	// merely checking the Go configuration that should reach PostgreSQL.
	if _, err = f.pool.Exec(t.Context(), `
 CREATE TABLE observed_timeouts(operation text, transaction_timeout text, idle_timeout text);
 CREATE TABLE schema_migrations(version integer PRIMARY KEY, created_at timestamptz DEFAULT statement_timestamp());
 CREATE FUNCTION observe_timeouts() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN
  INSERT INTO observed_timeouts VALUES(TG_TABLE_NAME,
   current_setting('transaction_timeout'), current_setting('idle_in_transaction_session_timeout'));
  RETURN NEW;
 END;
 $$;
 CREATE TRIGGER observe_timeouts AFTER INSERT ON schema_migrations
 FOR EACH ROW EXECUTE FUNCTION observe_timeouts();`); err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"api", "migrate"}
	if err = run(); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(t.Context(), `CREATE TRIGGER observe_timeouts AFTER INSERT ON tenants
 FOR EACH ROW EXECUTE FUNCTION observe_timeouts()`); err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"api", "seed-demo"}
	t.Setenv("AUTH_MODE", "demo")
	if err = run(); err != nil {
		t.Fatal(err)
	}
	for operation, want := range map[string]int{"schema_migrations": 1, "tenants": 1} {
		var observed, matching int
		if err = f.pool.QueryRow(t.Context(), `SELECT count(*), count(*) FILTER
 (WHERE transaction_timeout='27s' AND idle_timeout='28s') FROM observed_timeouts WHERE operation=$1`, operation).Scan(&observed, &matching); err != nil || observed != want || matching != want {
			t.Fatalf("%s settings observed=%d matching=%d want=%d error=%v", operation, observed, matching, want, err)
		}
	}
}

func eventually(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal(description)
		case <-tick.C:
		}
	}
}

func TestEntrypointSIGTERMDrainsWriteAndClosesPool(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SIGTERM is verified in the Linux Compose test profile")
	}
	entrypointEnvironment(t)
	f := isolatedEntrypointDB(t)
	if err := postgres.Migrate(t.Context(), f.pool); err != nil {
		t.Fatal(err)
	}
	if err := postgres.SeedDemo(t.Context(), f.pool); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `
 CREATE TABLE observed_timeouts(transaction_timeout text, idle_timeout text);
 CREATE FUNCTION observe_timeouts() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN
  INSERT INTO observed_timeouts VALUES(current_setting('transaction_timeout'),
   current_setting('idle_in_transaction_session_timeout'));
  RETURN NEW;
 END;
 $$;
 CREATE TRIGGER observe_timeouts AFTER INSERT ON tasks
 FOR EACH ROW EXECUTE FUNCTION observe_timeouts();`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTH_MODE", "demo")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	t.Setenv("HTTP_ADDR", address)
	// Keep a test-owned signal subscription until cleanup, avoiding an accidental
	// default process termination if run() has already stopped its own subscription.
	guard := make(chan os.Signal, 2)
	signal.Notify(guard, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(guard) })
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	var runErr error
	go func() { runErr = run(); close(finished) }()
	t.Cleanup(func() {
		select {
		case <-finished:
			return
		default:
		}
		_ = process.Signal(syscall.SIGTERM)
		select {
		case <-finished:
		case <-time.After(12 * time.Second):
			t.Error("API did not exit during cleanup")
		}
	})
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	t.Cleanup(client.CloseIdleConnections)
	base := "http://" + address
	eventually(t, "API never became ready", func() bool {
		response, err := client.Get(base + "/readyz")
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == 200
	})
	blocker, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Rollback(context.Background()) })
	if _, err = blocker.Exec(t.Context(), "LOCK TABLE tasks IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		status int
		body   []byte
		err    error
	}
	completed := make(chan outcome, 1)
	go func() {
		response, err := client.Post(base+"/tasks", "application/json", strings.NewReader(`{"name":"finish before shutdown","status":0}`))
		if err != nil {
			completed <- outcome{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		completed <- outcome{status: response.StatusCode, body: body, err: err}
	}()
	eventually(t, "write never reached the real PostgreSQL lock", func() bool {
		var blocked bool
		err := f.admin.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE 'INSERT INTO tasks%')", f.application).Scan(&blocked)
		return err == nil && blocked
	})
	if err = process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	eventually(t, "shutdown did not stop accepting new connections", func() bool {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return false
		}
		return true
	})
	select {
	case <-finished:
		t.Fatal("API exited before the in-flight write drained")
	default:
	}
	if err = blocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	var result outcome
	select {
	case result = <-completed:
	case <-time.After(8 * time.Second):
		t.Fatal("in-flight write did not finish")
	}
	if result.err != nil || result.status != 201 {
		t.Fatalf("drained write status=%d error=%v", result.status, result.err)
	}
	var created task.Task
	if err = json.Unmarshal(result.body, &created); err != nil || created.ID < 1 {
		t.Fatal("invalid write result", err)
	}
	select {
	case <-finished:
	case <-time.After(12 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	var name string
	if err = f.pool.QueryRow(t.Context(), "SELECT name FROM tasks WHERE tenant_id=1 AND id=$1", created.ID).Scan(&name); err != nil || name != "finish before shutdown" {
		t.Fatalf("durable result = %q, error=%v", name, err)
	}
	var transactionTimeout, idleTimeout string
	if err = f.pool.QueryRow(t.Context(), "SELECT transaction_timeout,idle_timeout FROM observed_timeouts").Scan(&transactionTimeout, &idleTimeout); err != nil || transactionTimeout != "10s" || idleTimeout != "10s" {
		t.Fatalf("actual API session timeouts transaction=%q idle=%q want 10s each, error=%v", transactionTimeout, idleTimeout, err)
	}
	var connections int
	if err = f.admin.QueryRow(t.Context(), "SELECT count(*) FROM pg_stat_activity WHERE application_name=$1", f.application).Scan(&connections); err != nil || connections != 0 {
		t.Fatalf("shutdown leaked %d pool connections: %v", connections, err)
	}
}
