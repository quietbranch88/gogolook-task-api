package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type versionResponse struct {
	code int
	body []byte
	err  error
}

func TestVersionRetryAfterLostResponse(t *testing.T) {
	f := setup(t)
	v := decodeTask(t, f.call(t, "POST", "/tasks", f.a, "101", `{"name":"before","status":0}`, 201), "before", 0)
	path := fmt.Sprintf("/tasks/%d", v.ID)
	// The real backend commits; only the return path is deliberately disconnected.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream, err := http.NewRequestWithContext(r.Context(), r.Method, f.server.URL+r.URL.Path, r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		upstream.Header = r.Header.Clone()
		response, err := f.server.Client().Do(upstream)
		if err != nil {
			t.Error(err)
			return
		}
		data, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil || response.StatusCode != 200 {
			t.Errorf("backend update: %d %s %v", response.StatusCode, data, readErr)
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer proxy.Close()
	body := `{"name":"committed","status":1,"version":1}`
	r, err := http.NewRequest("PUT", proxy.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+f.a)
	r.Header.Set("X-Tenant-ID", "101")
	r.Header.Set("Content-Type", "application/json")
	if response, err := proxy.Client().Do(r); err == nil {
		response.Body.Close()
		t.Fatal("proxy did not lose response")
	}
	f.call(t, "PUT", path, f.a, "101", body, 409)
	var name string
	var status int
	var version int64
	if err = f.pool.QueryRow(context.Background(), `SELECT name,status,version FROM tasks WHERE tenant_id=101 AND id=$1`, v.ID).Scan(&name, &status, &version); err != nil || name != "committed" || status != 1 || version != 2 {
		t.Fatalf("lost-response retry changed state: %s %d v%d %v", name, status, version, err)
	}
}

func versionPut(f *fixture, path, body string) versionResponse {
	r, e := http.NewRequest("PUT", f.server.URL+path, strings.NewReader(body))
	if e != nil {
		return versionResponse{err: e}
	}
	r.Header.Set("Authorization", "Bearer "+f.a)
	r.Header.Set("X-Tenant-ID", "101")
	r.Header.Set("Content-Type", "application/json")
	response, e := f.server.Client().Do(r)
	if e != nil {
		return versionResponse{err: e}
	}
	defer response.Body.Close()
	data, e := io.ReadAll(response.Body)
	return versionResponse{response.StatusCode, data, e}
}
func TestVersionConcurrentUpdate(t *testing.T) {
	f := setup(t)
	data := f.call(t, "POST", "/tasks", f.a, "101", `{"name":"initial","status":0}`, 201)
	var original struct{ ID, Version int64 }
	if e := json.Unmarshal(data, &original); e != nil {
		t.Fatal(e)
	}
	if original.Version != 1 {
		t.Fatalf("new task must expose version 1, got %s", data)
	}
	path := fmt.Sprintf("/tasks/%d", original.ID)
	start := make(chan struct{})
	done := make(chan versionResponse, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			<-start
			done <- versionPut(f, path, fmt.Sprintf(`{"name":"writer-%d","status":%d,"detail":"writer-%d","version":1}`, i, i, i))
		}(i)
	}
	close(start)
	var winner struct {
		Name, Detail string
		Status       int
		Version      int64
	}
	codes := map[int]int{}
	for i := 0; i < 2; i++ {
		r := <-done
		if r.err != nil {
			t.Fatal(r.err)
		}
		codes[r.code]++
		if r.code == 200 {
			if e := json.Unmarshal(r.body, &winner); e != nil {
				t.Fatal(e)
			}
		}
	}
	if codes[200] != 1 || codes[409] != 1 {
		t.Fatalf("same version must yield one 200 and one 409; got %v", codes)
	}
	if winner.Version != 2 || winner.Name != fmt.Sprintf("writer-%d", winner.Status) || winner.Detail != winner.Name {
		t.Fatalf("invalid winner %+v", winner)
	}
	var name, detail string
	var status int
	var version int64
	if e := f.pool.QueryRow(context.Background(), `SELECT name,status,detail,version FROM tasks WHERE tenant_id=101 AND id=$1`, original.ID).Scan(&name, &status, &detail, &version); e != nil {
		t.Fatal(e)
	}
	if name != winner.Name || detail != winner.Detail || status != winner.Status || version != 2 {
		t.Fatalf("durable state differs from sole winner: %s %d %s v%d", name, status, detail, version)
	}
	// A stale retry must not apply the update again, even if its payload is identical.
	f.call(t, "PUT", path, f.a, "101", fmt.Sprintf(`{"name":%q,"status":%d,"detail":%q,"version":1}`, name, status, detail), 409)
	f.call(t, "PUT", path, f.b, "102", `{"name":"outsider","status":0,"version":2}`, 404)
	f.call(t, "PUT", path, f.a, "102", `{"name":"outsider","status":0,"version":2}`, 403)
	var listed []struct{ ID, Version int64 }
	if e := json.Unmarshal(f.call(t, "GET", "/tasks", f.a, "101", "", 200), &listed); e != nil || len(listed) != 1 || listed[0].Version != 2 {
		t.Fatalf("fresh read %+v err=%v", listed, e)
	}
	data = f.call(t, "PUT", path, f.a, "101", `{"name":"reviewed merge","status":1,"version":2}`, 200)
	if e := json.Unmarshal(data, &winner); e != nil || winner.Version != 3 || winner.Name != "reviewed merge" || winner.Detail != "" {
		t.Fatalf("fresh resubmission: %s err=%v", data, e)
	}
	f.call(t, "DELETE", path, f.a, "101", "", 204)
	f.call(t, "PUT", path, f.a, "101", `{"name":"deleted","status":0,"version":3}`, 404)
}

func TestVersionInputAndIdempotentReplay(t *testing.T) {
	f := setup(t)
	body := `{"name":"versioned","status":0}`
	c, v, e := keyedPost(f.server.URL, f.a, "101", "version-create", body)
	if e != nil || c != 201 {
		t.Fatalf("create %d %v", c, e)
	}
	path := fmt.Sprintf("/tasks/%d", v.ID)
	for _, field := range []string{"", `,"version":null`, `,"version":0`, `,"version":-1`, `,"version":1.0`, `,"version":"1"`, `,"version":9223372036854775808`} {
		f.call(t, "PUT", path, f.a, "101", `{"name":"bad","status":0`+field+`}`, 400)
	}
	for _, field := range []string{`1`, `null`, `0`} {
		f.call(t, "POST", "/tasks", f.a, "101", `{"name":"bad","status":0,"version":`+field+`}`, 400)
	}
	f.call(t, "PUT", path, f.a, "101", `{"name":"changed","status":1,"version":1}`, 200)
	c, v, e = keyedPost(f.server.URL, f.a, "101", "version-create", body)
	if e != nil || c != 201 || v.Name != "changed" {
		t.Fatalf("replay %d %+v %v", c, v, e)
	}
	// Decode without depending on production struct fields so the test builds on baseline.
	data, _ := json.Marshal(v)
	var current struct{ Version int64 }
	if e = json.Unmarshal(data, &current); e != nil || current.Version != 2 {
		t.Fatalf("replay version %s %v", data, e)
	}
}
