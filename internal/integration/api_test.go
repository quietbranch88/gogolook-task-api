package integration_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/quietbranch88/gogolook-task-api/internal/httpapi"
	"github.com/quietbranch88/gogolook-task-api/internal/postgres"
	"github.com/quietbranch88/gogolook-task-api/internal/task"
)

type fixture struct {
	pool                         *pgxpool.Pool
	server                       *httptest.Server
	a, b, both, expired, revoked string
}

func randomHex() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails since Go 1.24
	return hex.EncodeToString(b)
}
func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("real PostgreSQL not configured; run docker compose --profile test run --rm tests")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid TEST_DATABASE_URL")
	}
	if config.ConnConfig.Database != "tasks_test" || os.Getenv("TEST_ALLOW_DATABASE") != "tasks_test" {
		t.Fatal("tests require explicit disposable tasks_test database")
	}
	if host := config.ConnConfig.Host; host != "test-db" && host != "localhost" && host != "127.0.0.1" {
		t.Fatal("test database must be local or Compose test-db")
	}
	ctx := context.Background()
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var actual string
	if err = admin.QueryRow(ctx, `SELECT current_database()`).Scan(&actual); err != nil || actual != "tasks_test" {
		t.Fatalf("wrong DB: %s %v", actual, err)
	}
	schema := "test_" + randomHex()[:16]
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	scoped := config.Copy()
	scoped.ConnConfig.RuntimeParams["search_path"] = schema
	scoped.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(ctx, scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err = postgres.Migrate(ctx, pool); err != nil {
		t.Fatal("migration rerun", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO tenants(id,name) VALUES(101,'A'),(102,'B');
 INSERT INTO users(id) VALUES(1001),(1002),(1003);
 INSERT INTO memberships(tenant_id,user_id) VALUES(101,1001),(102,1002),(101,1003),(102,1003);`)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{pool: pool, a: randomHex(), b: randomHex(), both: randomHex(), expired: randomHex(), revoked: randomHex()}
	for _, key := range []struct {
		token            string
		user             int
		expired, revoked bool
	}{{f.a, 1001, false, false}, {f.b, 1002, false, false}, {f.both, 1003, false, false}, {f.expired, 1001, true, false}, {f.revoked, 1001, false, true}} {
		digest := sha256.Sum256([]byte(key.token))
		expiry := time.Now().Add(time.Hour)
		if key.expired {
			expiry = time.Now().Add(-time.Hour)
		}
		var revoked *time.Time
		if key.revoked {
			now := time.Now()
			revoked = &now
		}
		if _, err = pool.Exec(ctx, `INSERT INTO api_keys(token_hash,user_id,expires_at,revoked_at) VALUES($1,$2,$3,$4)`, digest[:], key.user, expiry, revoked); err != nil {
			t.Fatal(err)
		}
	}
	repo := postgres.New(pool)
	f.server = httptest.NewServer(httpapi.New(task.NewService(repo), repo, false, pool.Ping))
	t.Cleanup(f.server.Close)
	return f
}
func (f *fixture) call(t *testing.T, method, path, token, tenant, body string, want int) []byte {
	t.Helper()
	r, err := http.NewRequest(method, f.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if tenant != "" {
		r.Header.Set("X-Tenant-ID", tenant)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	response, err := f.server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("%s %s: want %d got %d: %s", method, path, want, response.StatusCode, data)
	}
	return data
}

// decodeTask decodes a task response and checks its name and status.
func decodeTask(t *testing.T, data []byte, name string, status int) task.Task {
	t.Helper()
	var result task.Task
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.ID < 1 || result.Name != name || result.Status != status {
		t.Fatalf("wrong task: %+v", result)
	}
	return result
}

// Equal instants may carry different time locations; compare them as instants.
func tasksEqual(a, b task.Task) bool {
	return a.ID == b.ID && a.Name == b.Name && a.Status == b.Status &&
		a.Detail == b.Detail && a.Version == b.Version &&
		a.CreatedAt.Equal(b.CreatedAt) && a.UpdatedAt.Equal(b.UpdatedAt)
}

func TestHTTPPostgresCRUDAndPersistence(t *testing.T) {
	f := setup(t)
	if got := string(f.call(t, "GET", "/tasks", f.a, "101", "", 200)); got != "[]\n" {
		t.Fatalf("empty list: %s", got)
	}
	first := decodeTask(t, f.call(t, "POST", "/tasks", f.a, "101", `{"name":"buy tea","status":0}`, 201), "buy tea", 0)
	second := decodeTask(t, f.call(t, "POST", "/tasks", f.a, "101", `{"name":"read","status":1}`, 201), "read", 1)
	data := f.call(t, "GET", "/tasks", f.a, "101", "", 200)
	var listed []task.Task
	if err := json.Unmarshal(data, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || !tasksEqual(listed[0], first) || !tasksEqual(listed[1], second) {
		t.Fatalf("ordered list: %s", data)
	}
	path := fmt.Sprintf("/tasks/%d", first.ID)
	for i := 0; i < 2; i++ {
		decodeTask(t, f.call(t, "PUT", path, f.a, "101", fmt.Sprintf(`{"name":"brew tea","status":1,"version":%d}`, i+1), 200), "brew tea", 1)
	}
	var name string
	var status int
	if err := f.pool.QueryRow(context.Background(), `SELECT name,status FROM tasks WHERE id=$1`, first.ID).Scan(&name, &status); err != nil || name != "brew tea" || status != 1 {
		t.Fatalf("durable update: %s %d %v", name, status, err)
	}
	f.server.Close()
	repo := postgres.New(f.pool)
	f.server = httptest.NewServer(httpapi.New(task.NewService(repo), repo, false, f.pool.Ping))
	t.Cleanup(f.server.Close)
	data = f.call(t, "GET", "/tasks", f.a, "101", "", 200)
	if !strings.Contains(string(data), "brew tea") {
		t.Fatal("state lost after HTTP server recreation")
	}
	if len(f.call(t, "DELETE", path, f.a, "101", "", 204)) != 0 {
		t.Fatal("delete response body")
	}
	f.call(t, "DELETE", path, f.a, "101", "", 404)
	f.call(t, "PUT", path, f.a, "101", `{"name":"ghost","status":0,"version":1}`, 404)
	var count int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM tasks WHERE id=$1`, first.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("physical deletion: %d %v", count, err)
	}
}

func TestTenantIsolationAndRevocation(t *testing.T) {
	f := setup(t)
	a := decodeTask(t, f.call(t, "POST", "/tasks", f.a, "101", `{"name":"A-private","status":0}`, 201), "A-private", 0)
	b := decodeTask(t, f.call(t, "POST", "/tasks", f.b, "102", `{"name":"B-private","status":1}`, 201), "B-private", 1)
	data := f.call(t, "GET", "/tasks", f.b, "102", "", 200)
	var list []task.Task
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !tasksEqual(list[0], b) {
		t.Fatalf("tenant list leaked: %s", data)
	}
	foreign := fmt.Sprintf("/tasks/%d", a.ID)
	for _, token := range []string{f.b, f.both} {
		f.call(t, "PUT", foreign, token, "102", `{"name":"attack","status":1,"version":1}`, 404)
		f.call(t, "DELETE", foreign, token, "102", "", 404)
	}
	for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
		path := "/tasks"
		body := ""
		if method == "POST" || method == "PUT" {
			body = `{"name":"attack","status":0}`
		}
		if method == "PUT" {
			body = `{"name":"attack","status":0,"version":1}`
		}
		if method == "PUT" || method == "DELETE" {
			path = foreign
		}
		f.call(t, method, path, f.b, "101", body, 403)
	}
	var name string
	if err := f.pool.QueryRow(context.Background(), `SELECT name FROM tasks WHERE id=$1`, a.ID).Scan(&name); err != nil || name != "A-private" {
		t.Fatalf("unauthorized mutation: %s %v", name, err)
	}
	if _, err := f.pool.Exec(context.Background(), `DELETE FROM memberships WHERE tenant_id=101 AND user_id=1001`); err != nil {
		t.Fatal(err)
	}
	f.call(t, "GET", "/tasks", f.a, "101", "", 403)
	f.call(t, "PUT", foreign, f.a, "101", `{"name":"after revoke","status":0,"version":1}`, 403)
	f.call(t, "GET", "/tasks", f.both, "101", "", 200)
}

func TestCredentialLifecycleAndFailure(t *testing.T) {
	f := setup(t)
	for _, token := range []string{"", randomHex(), f.expired, f.revoked} {
		f.call(t, "GET", "/tasks", token, "101", "", 401)
	}
	digest := sha256.Sum256([]byte(f.a))
	if _, err := f.pool.Exec(context.Background(), `UPDATE api_keys SET revoked_at=now() WHERE token_hash=$1`, digest[:]); err != nil {
		t.Fatal(err)
	}
	f.call(t, "GET", "/tasks", f.a, "101", "", 401)
	f.pool.Close()
	data := f.call(t, "GET", "/tasks", f.b, "102", "", 503)
	if string(data) != "{\"error\":\"service unavailable\"}\n" {
		t.Fatalf("unsafe failure: %s", data)
	}
	f.call(t, "GET", "/readyz", "", "", "", 503)
	f.call(t, "GET", "/healthz", "", "", "", 200)
}

func TestSchemaConstraintsAndDemoSeed(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, statement := range []string{`INSERT INTO tasks(tenant_id,name,status) VALUES(101,'bad',2)`, `INSERT INTO tasks(tenant_id,name,status) VALUES(101,'',0)`, `INSERT INTO tasks(tenant_id,name,status) VALUES(999,'bad tenant',0)`} {
		if _, err := f.pool.Exec(ctx, statement); err == nil {
			t.Fatalf("constraint allowed: %s", statement)
		}
	}
	for i := 0; i < 2; i++ {
		if err := postgres.SeedDemo(ctx, f.pool); err != nil {
			t.Fatal(err)
		}
	}
	repo := postgres.New(f.pool)
	f.server.Close()
	f.server = httptest.NewServer(httpapi.New(task.NewService(repo), repo, true, f.pool.Ping))
	t.Cleanup(f.server.Close)
	decodeTask(t, f.call(t, "POST", "/tasks", "", "", `{"name":"demo","status":0}`, 201), "demo", 0)
	f.call(t, "GET", "/tasks", "", "101", "", 400)
}

func TestConcurrentUpdatesRemainAtomic(t *testing.T) {
	f := setup(t)
	item := decodeTask(t, f.call(t, "POST", "/tasks", f.a, "101", `{"name":"initial","status":0}`, 201), "initial", 0)
	var wg sync.WaitGroup
	fail := make(chan string, 8)
	codes := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"name":"writer-%d","status":%d,"version":1}`, i, i%2)
			r, err := http.NewRequest("PUT", fmt.Sprintf("%s/tasks/%d", f.server.URL, item.ID), strings.NewReader(body))
			if err != nil {
				fail <- err.Error()
				return
			}
			r.Header.Set("Authorization", "Bearer "+f.a)
			r.Header.Set("X-Tenant-ID", "101")
			r.Header.Set("Content-Type", "application/json")
			res, err := f.server.Client().Do(r)
			if err != nil {
				fail <- err.Error()
				return
			}
			defer res.Body.Close()
			codes <- res.StatusCode
			if res.StatusCode != 200 && res.StatusCode != 409 {
				fail <- fmt.Sprintf("status %d", res.StatusCode)
			}
		}(i)
	}
	wg.Wait()
	close(codes)
	wins := 0
	for code := range codes {
		if code == 200 {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("expected one winner, got %d", wins)
	}
	close(fail)
	for err := range fail {
		t.Error(err)
	}
	var name string
	var status, writer int
	if err := f.pool.QueryRow(context.Background(), `SELECT name,status FROM tasks WHERE id=$1`, item.ID).Scan(&name, &status); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Sscanf(name, "writer-%d", &writer); err != nil || writer < 0 || writer >= 8 || status != writer%2 {
		t.Fatalf("torn update: %s %d", name, status)
	}
}

func TestPaginationKeysetAndTenantScope(t *testing.T) {
	f := setup(t)
	var a []task.Task
	for i := 0; i < 5; i++ {
		a = append(a, decodeTask(t, f.call(t, "POST", "/tasks", f.a, "101", fmt.Sprintf(`{"name":"A-%d","status":%d}`, i, i%2), 201), fmt.Sprintf("A-%d", i), i%2))
	}
	b1 := decodeTask(t, f.call(t, "POST", "/tasks", f.b, "102", `{"name":"B-0","status":0}`, 201), "B-0", 0)
	b2 := decodeTask(t, f.call(t, "POST", "/tasks", f.b, "102", `{"name":"B-1","status":1}`, 201), "B-1", 1)

	// page is call() plus the cursor header
	page := func(path, token, tenant string) ([]task.Task, string) {
		t.Helper()
		r, err := http.NewRequest("GET", f.server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Tenant-ID", tenant)
		res, err := f.server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d", path, res.StatusCode)
		}
		var list []task.Task
		if err := json.NewDecoder(res.Body).Decode(&list); err != nil {
			t.Fatal(err)
		}
		return list, res.Header.Get("X-Next-After")
	}
	eq := func(got []task.Task, want ...task.Task) bool { return slices.EqualFunc(got, want, tasksEqual) }

	// Walk tenant A in pages of two: 2, 2, 1, with the cursor pointing at the last returned id.
	p1, next1 := page("/tasks?limit=2", f.a, "101")
	if !eq(p1, a[0], a[1]) || next1 != fmt.Sprint(a[1].ID) {
		t.Fatalf("page 1: %v next=%q", p1, next1)
	}
	p2, next2 := page("/tasks?limit=2&after="+next1, f.a, "101")
	if !eq(p2, a[2], a[3]) || next2 != fmt.Sprint(a[3].ID) {
		t.Fatalf("page 2: %v next=%q", p2, next2)
	}
	p3, next3 := page("/tasks?limit=2&after="+next2, f.a, "101")
	if !eq(p3, a[4]) || next3 != "" {
		t.Fatalf("page 3: %v next=%q", p3, next3)
	}
	// Exactly-full last page: limit 5 returns all five and no cursor.
	p5, next5 := page("/tasks?limit=5", f.a, "101")
	if !eq(p5, a...) || next5 != "" {
		t.Fatalf("exact page: %v next=%q", p5, next5)
	}
	// after without limit uses the default page; this short tail fits.
	tail, nextTail := page("/tasks?after="+fmt.Sprint(a[1].ID), f.a, "101")
	if !eq(tail, a[2], a[3], a[4]) || nextTail != "" {
		t.Fatalf("tail: %v next=%q", tail, nextTail)
	}
	// No parameters: this small fixture fits within the default page.
	all, nextAll := page("/tasks", f.a, "101")
	if !eq(all, a...) || nextAll != "" {
		t.Fatalf("all: %v next=%q", all, nextAll)
	}

	// Tenant scope under paging: a user of both tenants selecting B, with a cursor
	// below A's ids and a generous limit, must see only B's rows.
	cross, nextCross := page(fmt.Sprintf("/tasks?limit=100&after=%d", a[0].ID-1), f.both, "102")
	if !eq(cross, b1, b2) || nextCross != "" {
		t.Fatalf("tenant leak under paging: %v next=%q", cross, nextCross)
	}
	// Cursor arithmetic cannot reach across tenants either: B's first page must start at b1.
	bp, _ := page("/tasks?limit=1", f.b, "102")
	if !eq(bp, b1) {
		t.Fatalf("B first page: %v", bp)
	}
}

func TestSeedLoadIsIdempotentAndUsable(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	tokens := []string{randomHex(), randomHex()}
	var hashes [][]byte
	for _, tok := range tokens {
		h := sha256.Sum256([]byte(tok))
		hashes = append(hashes, h[:])
	}
	for i := 0; i < 2; i++ {
		if err := postgres.SeedLoad(ctx, f.pool, 5, hashes); err != nil {
			t.Fatalf("seed-load run %d: %v", i+1, err)
		}
	}
	var tenants, keys, tasks int
	if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tenants WHERE id>=$1),(SELECT count(*) FROM api_keys WHERE token_hash=$2 OR token_hash=$3),(SELECT count(*) FROM tasks WHERE tenant_id>=$1)`, postgres.LoadTenantBase, hashes[0], hashes[1]).Scan(&tenants, &keys, &tasks); err != nil {
		t.Fatal(err)
	}
	if tenants != 2 || keys != 2 || tasks != 10 {
		t.Fatalf("second run changed the fixture: tenants=%d keys=%d tasks=%d", tenants, keys, tasks)
	}
	// Seeded tokens authenticate only for their own tenant, and paging works on the fixture.
	first := fmt.Sprint(postgres.LoadTenantBase)
	second := fmt.Sprint(postgres.LoadTenantBase + 1)
	var list []task.Task
	if err := json.Unmarshal(f.call(t, "GET", "/tasks?limit=3", tokens[0], first, "", 200), &list); err != nil || len(list) != 3 {
		t.Fatalf("seeded tenant page: %v %d", err, len(list))
	}
	f.call(t, "GET", "/tasks", tokens[0], second, "", 403)
}

func TestDetailRoundTripAndBaselineLedger(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var versions []int
	rows, err := f.pool.Query(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v)
	}
	rows.Close()
	if !slices.Equal(versions, []int{1}) {
		t.Fatalf("applied versions %v, want [1]", versions)
	}
	if err := postgres.SchemaReady(ctx, f.pool); err != nil {
		t.Fatalf("schema not ready: %v", err)
	}

	// POST without detail stores "", POST with detail stores it verbatim.
	plain := decodeTask(t, f.call(t, "POST", "/tasks", f.a, "101", `{"name":"plain","status":0}`, 201), "plain", 0)
	data := f.call(t, "POST", "/tasks", f.a, "101", `{"name":"rich","status":1,"detail":"  細節 with spaces  "}`, 201)
	var rich task.Task
	if err := json.Unmarshal(data, &rich); err != nil || rich.Detail != "  細節 with spaces  " {
		t.Fatalf("detail not preserved: %s", data)
	}
	var stored string
	if err := f.pool.QueryRow(ctx, `SELECT detail FROM tasks WHERE id=$1`, plain.ID).Scan(&stored); err != nil || stored != "" {
		t.Fatalf("plain detail: %q %v", stored, err)
	}
	// PUT is a full replacement: omitting detail clears it.
	f.call(t, "PUT", fmt.Sprintf("/tasks/%d", rich.ID), f.a, "101", `{"name":"rich","status":1,"version":1}`, 200)
	if err := f.pool.QueryRow(ctx, `SELECT detail FROM tasks WHERE id=$1`, rich.ID).Scan(&stored); err != nil || stored != "" {
		t.Fatalf("PUT did not clear detail: %q %v", stored, err)
	}
	// The database enforces the length independently of the service.
	if _, err := f.pool.Exec(ctx, `INSERT INTO tasks(tenant_id,name,status,detail) VALUES(101,'x',0,repeat('d',10001))`); err == nil {
		t.Fatal("detail length constraint missing")
	}
	// Listing returns detail for every row, paged or not.
	var list []task.Task
	if err := json.Unmarshal(f.call(t, "GET", "/tasks?limit=10", f.a, "101", "", 200), &list); err != nil || len(list) != 2 || list[0].Detail != "" || list[1].Detail != "" {
		t.Fatalf("list detail: %v %+v", err, list)
	}
}

func TestRejectedInputDoesNotChangeStoredTasks(t *testing.T) {
	f := setup(t)
	item := decodeTask(t, f.call(t, "POST", "/tasks", f.a, "101", `{"name":"original","status":0,"detail":"preserved"}`, 201), "original", 0)
	for _, method := range []string{"POST", "PUT"} {
		path := "/tasks"
		if method == "PUT" {
			path += fmt.Sprintf("/%d", item.ID)
		}
		for _, field := range []string{"name", "detail"} {
			t.Run(method+"/"+field, func(t *testing.T) {
				body := "{\"name\":\"bad\xff\",\"status\":1,\"detail\":\"changed\"}"
				if field == "detail" {
					body = "{\"name\":\"changed\",\"status\":1,\"detail\":\"bad\xff\"}"
				}
				if method == "PUT" {
					body = strings.TrimSuffix(body, "}") + `,"version":1}`
				}
				f.call(t, method, path, f.a, "101", body, 400)
			})
		}
	}
	var count, status int
	var name, detail string
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM tasks WHERE tenant_id=101`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rejected input inserted rows: count=%d err=%v", count, err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT name,status,detail FROM tasks WHERE tenant_id=101 AND id=$1`, item.ID).Scan(&name, &status, &detail); err != nil || name != "original" || status != 0 || detail != "preserved" {
		t.Fatalf("rejected input changed stored task: %q %d %q err=%v", name, status, detail, err)
	}
	for _, query := range []string{"limit=", "after=", "limit=1&after="} {
		f.call(t, "GET", "/tasks?"+query, f.a, "101", "", 400)
	}
	// A real replacement character and multibyte Unicode are valid input, not malformed bytes.
	want := "literal \ufffd \u4e2d\u6587"
	valid := decodeTask(t, f.call(t, "POST", "/tasks", f.a, "101", `{"name":"`+want+`","status":0,"detail":"`+want+`"}`, 201), want, 0)
	if err := f.pool.QueryRow(context.Background(), `SELECT name,detail FROM tasks WHERE tenant_id=101 AND id=$1`, valid.ID).Scan(&name, &detail); err != nil || name != want || detail != want {
		t.Fatalf("valid Unicode changed: %q %q err=%v", name, detail, err)
	}
}
