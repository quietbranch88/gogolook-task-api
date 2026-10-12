package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/quietbranch88/gogolook-task-api/internal/task"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func keyedPost(base, token, tenant, key, body string) (int, task.Task, error) {
	r, err := http.NewRequest("POST", base+"/tasks", strings.NewReader(body))
	if err != nil {
		return 0, task.Task{}, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Tenant-ID", tenant)
	r.Header.Set("Idempotency-Key", key)
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		return 0, task.Task{}, err
	}
	defer response.Body.Close()
	var value task.Task
	if response.StatusCode == 201 {
		err = json.NewDecoder(response.Body).Decode(&value)
	} else {
		_, err = io.Copy(io.Discard, response.Body)
	}
	return response.StatusCode, value, err
}

func TestIdempotentCreate(t *testing.T) {
	f := setup(t)
	body := `{"name":"one operation","status":0}`
	type outcome struct {
		code  int
		value task.Task
		err   error
	}
	done := make(chan outcome, 12)
	for i := 0; i < 12; i++ {
		go func() { c, v, e := keyedPost(f.server.URL, f.a, "101", "same-key", body); done <- outcome{c, v, e} }()
	}
	var id int64
	for i := 0; i < 12; i++ {
		o := <-done
		if o.err != nil || o.code != 201 {
			t.Fatalf("create: %+v", o)
		}
		if id == 0 {
			id = o.value.ID
		}
		if o.value.ID != id {
			t.Fatalf("duplicate task ids: %d and %d", id, o.value.ID)
		}
	}
	var count int
	if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM tasks WHERE tenant_id=101").Scan(&count); err != nil || count != 1 {
		t.Fatalf("durable count=%d err=%v", count, err)
	}
	c, v, e := keyedPost(f.server.URL, f.a, "101", "same-key", `{"status":0,"detail":"","name":"one operation"}`)
	if e != nil || c != 201 || v.ID != id {
		t.Fatalf("equivalent input: %d %+v %v", c, v, e)
	}
	c, _, e = keyedPost(f.server.URL, f.a, "101", "same-key", `{"name":"different","status":0}`)
	if e != nil || c != 409 {
		t.Fatalf("changed input: %d %v", c, e)
	}
	// An independent key, user or tenant must not replay somebody else's operation.
	for _, args := range [][3]string{{f.a, "101", "other-key"}, {f.both, "101", "same-key"}, {f.both, "102", "same-key"}} {
		c, v, e = keyedPost(f.server.URL, args[0], args[1], args[2], body)
		if e != nil || c != 201 || v.ID == id {
			t.Fatalf("independent scope: %d %+v %v", c, v, e)
		}
	}
	c, _, e = keyedPost(f.server.URL, f.a, "102", "same-key", body)
	if e != nil || c != 403 {
		t.Fatalf("cross tenant: %d %v", c, e)
	}
	// Replay reads current task state, and never resurrects a deleted task.
	f.call(t, "PUT", fmt.Sprintf("/tasks/%d", id), f.a, "101", `{"name":"updated","status":1,"version":1}`, 200)
	c, v, e = keyedPost(f.server.URL, f.a, "101", "same-key", body)
	if e != nil || c != 201 || v.ID != id || v.Name != "updated" {
		t.Fatalf("current replay: %d %+v %v", c, v, e)
	}
	f.call(t, "DELETE", fmt.Sprintf("/tasks/%d", id), f.a, "101", "", 204)
	c, _, e = keyedPost(f.server.URL, f.a, "101", "same-key", body)
	if e != nil || c != 409 {
		t.Fatalf("deleted replay: %d %v", c, e)
	}
	if _, err := f.pool.Exec(context.Background(), "DELETE FROM memberships WHERE tenant_id=101 AND user_id=1001"); err != nil {
		t.Fatal(err)
	}
	c, _, e = keyedPost(f.server.URL, f.a, "101", "other-key", body)
	if e != nil || c != 403 {
		t.Fatalf("revoked replay: %d %v", c, e)
	}
}

func TestIdempotentRetryAfterLostResponse(t *testing.T) {
	f := setup(t)
	// Real backend commits; this test proxy deliberately loses its response.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream, err := http.NewRequestWithContext(r.Context(), r.Method, f.server.URL+r.URL.Path, r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		upstream.Header = r.Header.Clone()
		response, err := http.DefaultClient.Do(upstream)
		if err != nil {
			t.Error(err)
			return
		}
		_, err = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 201 {
			t.Errorf("backend commit response: %v %d", err, response.StatusCode)
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer proxy.Close()
	body := `{"name":"lost response","status":0}`
	if _, _, err := keyedPost(proxy.URL, f.a, "101", "retry-after-loss", body); err == nil {
		t.Fatal("proxy did not lose the response")
	}
	c, v, e := keyedPost(f.server.URL, f.a, "101", "retry-after-loss", body)
	if e != nil || c != 201 {
		t.Fatalf("retry: %d %+v %v", c, v, e)
	}
	var count int
	var stored int64
	if err := f.pool.QueryRow(context.Background(), "SELECT count(*),min(id) FROM tasks WHERE tenant_id=101").Scan(&count, &stored); err != nil || count != 1 || stored != v.ID {
		t.Fatalf("durable retry result count=%d stored=%d response=%d err=%v", count, stored, v.ID, err)
	}
}

func TestIdempotentRetryAfterWriteTimeout(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	blocker, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err = blocker.Exec(ctx, "LOCK TABLE tasks IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	body := `{"name":"retry timeout","status":0}`
	c, _, err := keyedPost(f.server.URL, f.a, "101", "retry-timeout", body)
	if err != nil || c != 504 {
		t.Fatalf("blocked write: %d %v", c, err)
	}
	if err = blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	c, _, err = keyedPost(f.server.URL, f.a, "101", "retry-timeout", body)
	if err != nil || c != 201 {
		t.Fatalf("retry: %d %v", c, err)
	}
	var count int
	if err = f.pool.QueryRow(ctx, "SELECT count(*) FROM tasks WHERE tenant_id=101").Scan(&count); err != nil || count != 1 {
		t.Fatalf("retry count=%d %v", count, err)
	}
}

func TestDefaultPaginationReturnsAllRowsAcrossPages(t *testing.T) {
	f := setup(t)
	if _, err := f.pool.Exec(context.Background(), "INSERT INTO tasks(tenant_id,name,status,detail) SELECT 101,'page',0,repeat('x',1024) FROM generate_series(1,205)"); err != nil {
		t.Fatal(err)
	}
	path := "/tasks"
	seen := map[int64]bool{}
	for _, want := range []int{100, 100, 5} {
		r, _ := http.NewRequest("GET", f.server.URL+path, nil)
		r.Header.Set("Authorization", "Bearer "+f.a)
		r.Header.Set("X-Tenant-ID", "101")
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		var rows []task.Task
		err = json.NewDecoder(response.Body).Decode(&rows)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 || len(rows) != want {
			t.Fatalf("page size=%d want=%d status=%d err=%v", len(rows), want, response.StatusCode, err)
		}
		for _, row := range rows {
			if seen[row.ID] || len(row.Detail) != 1024 {
				t.Fatalf("duplicate/incomplete row: %d", row.ID)
			}
			seen[row.ID] = true
		}
		next := response.Header.Get("X-Next-After")
		if want == 5 {
			if next != "" {
				t.Fatalf("unexpected last cursor %s", next)
			}
		} else {
			if next != fmt.Sprint(rows[len(rows)-1].ID) {
				t.Fatalf("bad cursor %s", next)
			}
			path = "/tasks?after=" + next
		}
	}
	if len(seen) != 205 {
		t.Fatalf("walk lost rows: %d", len(seen))
	}
	f.call(t, "GET", "/tasks?limit=101", f.a, "101", "", 400)
}

func TestIdempotencyHeaderValidation(t *testing.T) {
	f := setup(t)
	for _, key := range []string{"", "contains space", "two,keys", strings.Repeat("x", 129)} {
		code, _, err := keyedPost(f.server.URL, f.a, "101", key, `{"name":"invalid key","status":0}`)
		if err != nil || code != 400 {
			t.Fatalf("invalid key status=%d err=%v", code, err)
		}
	}
	// The extension must not deduplicate independent ordinary creates.
	for i := 0; i < 2; i++ {
		f.call(t, "POST", "/tasks", f.a, "101", `{"name":"ordinary","status":0}`, 201)
	}
	var count int
	if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM tasks WHERE tenant_id=101").Scan(&count); err != nil || count != 2 {
		t.Fatalf("ordinary creates count=%d err=%v", count, err)
	}
}
