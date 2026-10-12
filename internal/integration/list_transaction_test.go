package integration_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestListProtectsMembershipUntilRowsRead checks the authorization ordering through
// HTTP and real PostgreSQL locks, independently of how List ends its transaction.
func TestListProtectsMembershipUntilRowsRead(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	blocker, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err = blocker.Exec(ctx, "LOCK TABLE tasks IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, f.server.URL+"/tasks?limit=100", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.a)
	request.Header.Set("X-Tenant-ID", "101")
	listed := make(chan error, 1)
	go func() {
		response, callErr := f.server.Client().Do(request)
		if callErr != nil {
			listed <- callErr
			return
		}
		defer response.Body.Close()
		body, readErr := io.ReadAll(response.Body)
		if readErr != nil {
			listed <- readErr
		} else if response.StatusCode != 200 || strings.TrimSpace(string(body)) != "[]" {
			listed <- fmt.Errorf("authorized list: status=%d body=%s", response.StatusCode, body)
		} else {
			listed <- nil
		}
	}()
	var listPID int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err = f.pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity
 WHERE datname=current_database() AND wait_event_type='Lock'
 AND query LIKE 'SELECT id,name,status,detail,version,created_at,updated_at FROM tasks WHERE%'`).Scan(&listPID)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if listPID == 0 {
		t.Fatalf("list never reached the blocked task read: %v", err)
	}
	revoker, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var revokePID int
	if err = revoker.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&revokePID); err != nil {
		revoker.Release()
		t.Fatal(err)
	}
	revoked := make(chan error, 1)
	revocationFinished := make(chan struct{})
	go func() {
		defer close(revocationFinished)
		_, revokeErr := revoker.Exec(ctx, "DELETE FROM memberships WHERE tenant_id=101 AND user_id=1001")
		revoked <- revokeErr
	}()
	defer func() {
		cancel()
		_ = blocker.Rollback(context.Background())
		<-revocationFinished
		revoker.Release()
	}()
	// Observe the actual wait edge, rather than accepting a slow operation as proof.
	blocked := false
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case revokeErr := <-revoked:
			t.Fatalf("revocation completed while the authorized list was blocked: %v", revokeErr)
		default:
		}
		if err = f.pool.QueryRow(ctx, "SELECT $1::int = ANY(pg_blocking_pids($2))", listPID, revokePID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("revocation did not wait for the authorized list's membership lock")
	}
	if err = blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-listed; err != nil {
		t.Fatal(err)
	}
	if err = <-revoked; err != nil {
		t.Fatal(err)
	}
	f.call(t, "GET", "/tasks?limit=100", f.a, "101", "", 403)
	f.call(t, "GET", "/tasks?limit=100", f.b, "102", "", 200)
}
