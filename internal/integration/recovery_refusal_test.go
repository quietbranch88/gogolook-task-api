package integration_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

// This separate opt-in expects an owned, disposable PostgreSQL tasks_test
// database already in recovery with hot_standby=off. It does not create that
// condition, stop a server, or accept a substituted dialer. The ordinary suite
// skips it; an external recovery harness must promote the server and verify
// durable state and retry recovery afterward.
func TestHTTPPostgresRealRecoveryRefusal(t *testing.T) {
	dsn := os.Getenv("RECOVERY_57P03_DATABASE_URL")
	if dsn == "" || os.Getenv("RECOVERY_57P03_ALLOW_DATABASE") != "tasks_test" {
		t.Skip("requires separately owned PostgreSQL recovery fixture and explicit opt-in")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || config.ConnConfig.Database != "tasks_test" {
		t.Fatal("recovery fixture must use a valid tasks_test configuration")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, config.ConnConfig.Copy())
	if conn != nil {
		_ = conn.Close(ctx)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57P03" {
		t.Fatal("real PostgreSQL startup must reject connection with SQLSTATE 57P03")
	}
	t.Log("real pgx connection observed PostgreSQL SQLSTATE 57P03")
	config.MinConns = 0
	config.MinIdleConns = 0
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("could not initialize recovery fixture pool")
	}
	defer pool.Close()
	repo := postgres.New(pool)
	server := httptest.NewServer(httpapi.New(task.NewService(repo), repo, true, pool.Ping))
	defer server.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/tasks", strings.NewReader(`{"name":"real recovery refusal","status":0}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "real-recovery-refusal")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal("request to recovery fixture HTTP handler failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "1" || string(body) != "{\"error\":\"service unavailable\"}\n" {
		t.Fatalf("recovery refusal: want fixed safe 503 with Retry-After 1; status=%d retry=%q body=%q", response.StatusCode, response.Header.Get("Retry-After"), body)
	}
}
