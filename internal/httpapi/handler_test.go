package httpapi

import (
	"context"
	"errors"
	"github.com/quietbranch88/gogolook-task-api/internal/task"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type stub struct {
	calls int
	err   error
	scope task.Scope
	page  task.Page
	list  []task.Task
	in    task.Input
	id    int64
}

func (s *stub) List(_ context.Context, sc task.Scope, p task.Page) ([]task.Task, error) {
	s.calls++
	s.scope = sc
	s.page = p
	if s.list == nil {
		return []task.Task{}, s.err
	}
	return s.list, s.err
}
func (s *stub) Create(_ context.Context, sc task.Scope, in task.Input) (task.Task, error) {
	s.calls++
	s.scope = sc
	s.in = in
	return task.Task{ID: 5, Name: in.Name, Status: in.Status, Detail: in.Detail, Version: 1}, s.err
}
func (s *stub) Update(ctx context.Context, sc task.Scope, id int64, in task.Input) (task.Task, error) {
	s.id = id
	v, err := s.Create(ctx, sc, in)
	v.Version = in.Version + 1
	return v, err
}
func (s *stub) Delete(_ context.Context, sc task.Scope, id int64) error {
	s.calls++
	s.scope = sc
	return s.err
}
func (s *stub) Authenticate(context.Context, string) (int64, error) { return 20, s.err }
func request(h http.Handler, method, path, body, media string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if media != "" {
		r.Header.Set("Content-Type", media)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestHTTPInputRejection(t *testing.T) {
	cases := []struct {
		name, body, media string
		code              int
	}{
		{"missing name", `{"status":0}`, "application/json", 400}, {"missing status", `{"name":"a"}`, "application/json", 400},
		{"null name", `{"name":null,"status":0}`, "application/json", 400}, {"null status", `{"name":"a","status":null}`, "application/json", 400},
		{"null object", `null`, "application/json", 400}, {"array", `[]`, "application/json", 400},
		{"wrong name type", `{"name":1,"status":0}`, "application/json", 400}, {"float", `{"name":"a","status":1.0}`, "application/json", 400},
		{"string status", `{"name":"a","status":"0"}`, "application/json", 400}, {"status two", `{"name":"a","status":2}`, "application/json", 400},
		{"blank", `{"name":"  ","status":0}`, "application/json", 400}, {"malformed", `{`, "application/json", 400},
		{"trailing JSON", `{"name":"a","status":0}{}`, "application/json", 400}, {"trailing junk", `{"name":"a","status":0}x`, "application/json", 400},
		{"empty", "", "application/json", 400}, {"wrong media", `{}`, "text/plain", 415}, {"form media (what curl -d sends)", `{"name":"a","status":0}`, "application/x-www-form-urlencoded", 415}, {"missing media", `{"name":"a","status":0}`, "", 415},
		{"unknown field", `{"name":"a","status":0,"tenant_id":2}`, "application/json", 400}, {"misspelled field", `{"name":"a","status":0,"detial":"x"}`, "application/json", 400}, {"body id", `{"id":9,"name":"a","status":0}`, "application/json", 400},
		{"oversized", `{"name":"` + strings.Repeat("a", 17000) + `","status":0}`, "application/json", 413},
		{"large trailing whitespace", `{"name":"a","status":0}` + strings.Repeat(" ", 17000), "application/json", 413},
		{"invalid UTF-8 name", "{\"name\":\"bad\xff\",\"status\":0}", "application/json", 400},
		{"invalid UTF-8 detail", "{\"name\":\"ok\",\"status\":0,\"detail\":\"bad\xff\"}", "application/json", 400},
		{"oversized invalid UTF-8", "{\"name\":\"\xff" + strings.Repeat("a", 17000) + "\",\"status\":0}", "application/json", 413},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, method := range []string{"POST", "PUT"} {
				s := &stub{}
				h := New(task.NewService(s), s, true, func(context.Context) error { return nil })
				path := "/tasks"
				if method == "PUT" {
					path += "/1"
				}
				body := tc.body
				if method == "PUT" && strings.HasSuffix(body, "}") {
					body = strings.TrimSuffix(body, "}") + `,"version":1}`
				}
				w := request(h, method, path, body, tc.media, nil)
				if w.Code != tc.code || s.calls != 0 {
					t.Fatalf("%s: got %d (%s), calls %d; want %d", method, w.Code, w.Body, s.calls, tc.code)
				}
			}
		})
	}
}
func TestHTTPResponsesAndRoutes(t *testing.T) {
	s := &stub{}
	h := New(task.NewService(s), s, true, func(context.Context) error { return nil })
	cases := []struct {
		method, path, body string
		code               int
		want               string
	}{
		{"GET", "/tasks", "", 200, "[]\n"}, {"POST", "/tasks", `{"name":"work","status":0}`, 201, `{"id":5,"name":"work","status":0,"detail":"","version":1,"created_at":"0001-01-01T00:00:00Z","updated_at":"0001-01-01T00:00:00Z"}` + "\n"},
		{"PUT", "/tasks/5", `{"name":"work","status":1,"version":1}`, 200, `{"id":5,"name":"work","status":1,"detail":"","version":2,"created_at":"0001-01-01T00:00:00Z","updated_at":"0001-01-01T00:00:00Z"}` + "\n"},
		{"DELETE", "/tasks/5", "", 204, ""}, {"GET", "/tasks/5", "", 405, ""}, {"PATCH", "/tasks/5", "", 405, ""},
		{"PUT", "/tasks/0", `{}`, 400, ""}, {"DELETE", "/tasks/nope", "", 400, ""}, {"DELETE", "/tasks/9223372036854775808", "", 400, ""},
		// diagnostics live on a separate listener; the API mux must never expose them
		{"GET", "/debug/pprof/", "", 404, ""}, {"GET", "/debug/vars", "", 404, ""},
	}
	for _, tc := range cases {
		w := request(h, tc.method, tc.path, tc.body, "application/json", nil)
		if w.Code != tc.code {
			t.Fatalf("%s %s: %d", tc.method, tc.path, w.Code)
		}
		if tc.want != "" && w.Body.String() != tc.want {
			t.Fatalf("got %s want %s", w.Body, tc.want)
		}
		if tc.code == 204 && w.Body.Len() != 0 {
			t.Fatal("204 has body")
		}
	}
}
func TestAuthenticationBoundary(t *testing.T) {
	s := &stub{}
	h := New(task.NewService(s), s, false, func(context.Context) error { return nil })
	token := strings.Repeat("a", 64)
	for _, headers := range []map[string]string{nil, {"Authorization": "Basic " + token}, {"Authorization": "Bearer short"}} {
		w := request(h, "GET", "/tasks", "", "", headers)
		if w.Code != 401 || s.calls != 0 || w.Header().Get("WWW-Authenticate") != `Bearer realm="tasks"` {
			t.Fatalf("authentication did not reject request properly: %d %q", w.Code, w.Header().Get("WWW-Authenticate"))
		}
	}
	w := request(h, "GET", "/tasks", "", "", map[string]string{"Authorization": "Bearer " + token, "X-Tenant-ID": "7"})
	if w.Code != 200 || s.scope != (task.Scope{UserID: 20, TenantID: 7}) {
		t.Fatalf("trusted identity: %d %+v", w.Code, s.scope)
	}
	for _, bad := range []string{"0", "-1", "abc", "1.5"} {
		w = request(h, "GET", "/tasks", "", "", map[string]string{"Authorization": "Bearer " + token, "X-Tenant-ID": bad})
		if w.Code != 400 {
			t.Fatalf("tenant %q accepted: %d", bad, w.Code)
		}
	}
	// a request header can never become the identity: it is simply not read
	w = request(h, "GET", "/tasks", "", "", map[string]string{"Authorization": "Bearer " + token, "X-Tenant-ID": "7", "X-User-ID": "999"})
	if w.Code != 200 || s.scope.UserID != 20 {
		t.Fatalf("identity must come from the token only: %d %+v", w.Code, s.scope)
	}
	w = request(h, "GET", "/tasks", "", "", map[string]string{"Authorization": "Bearer " + token})
	if w.Code != 400 {
		t.Fatal("missing tenant accepted")
	}
	demo := New(task.NewService(s), s, true, func(context.Context) error { return nil })
	w = request(demo, "GET", "/tasks", "", "", map[string]string{"X-Tenant-ID": "7"})
	if w.Code != 400 {
		t.Fatal("demo allowed tenant override")
	}
}
func TestSafeErrorResponses(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{{task.ErrVersionConflict, 409}, {task.ErrForbidden, 403}, {task.ErrNotFound, 404}, {task.ErrUnauthorized, 401}, {task.ErrUnavailable, 503}, {task.ErrTimeout, 504}, {errors.New("password=secret SQL exception"), 500}} {
		s := &stub{err: tc.err}
		h := New(task.NewService(s), s, true, func(context.Context) error { return tc.err })
		w := request(h, "GET", "/tasks", "", "", nil)
		if w.Code != tc.code || strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("unsafe error: %d %s", w.Code, w.Body)
		}
		if (tc.code == 503) != (w.Header().Get("Retry-After") != "") {
			t.Fatalf("Retry-After only on 503: %d %q", w.Code, w.Header().Get("Retry-After"))
		}
		if request(h, "GET", "/readyz", "", "", nil).Code != 503 {
			t.Fatal("readiness ignored dependency")
		}
		if request(h, "GET", "/healthz", "", "", nil).Code != 200 {
			t.Fatal("liveness depends on DB")
		}
	}
}

func TestPaginationQuery(t *testing.T) {
	three := []task.Task{{ID: 10, Name: "a", Status: 0}, {ID: 11, Name: "b", Status: 1}, {ID: 12, Name: "c", Status: 0}}
	// Invalid query parameters are rejected before any repository call.
	for _, q := range []string{"limit=", "after=", "limit=1&after=", "limit", "after", "limit=0", "limit=-1", "limit=101", "limit=abc", "limit=1.5", "after=-1", "after=x", "limit=2&after=-5"} {
		s := &stub{list: three}
		h := New(task.NewService(s), s, true, func(context.Context) error { return nil })
		w := request(h, "GET", "/tasks?"+q, "", "", nil)
		if w.Code != 400 || s.calls != 0 || w.Header().Get("X-Next-After") != "" {
			t.Fatalf("%s: code %d calls %d", q, w.Code, s.calls)
		}
	}
	// No parameters: default page, no cursor when this short fixture fits.
	s := &stub{list: three}
	h := New(task.NewService(s), s, true, func(context.Context) error { return nil })
	w := request(h, "GET", "/tasks", "", "", nil)
	if w.Code != 200 || s.page != (task.Page{Limit: 100}) || w.Header().Get("X-Next-After") != "" || !strings.Contains(w.Body.String(), `"id":12`) {
		t.Fatalf("default page: %d %+v %s", w.Code, s.page, w.Body)
	}
	// limit=2 with three rows available: two rows in the body and a cursor to the second.
	s = &stub{list: three}
	h = New(task.NewService(s), s, true, func(context.Context) error { return nil })
	w = request(h, "GET", "/tasks?limit=2&after=7", "", "", nil)
	if w.Code != 200 || s.page != (task.Page{Limit: 2, After: 7}) || w.Header().Get("X-Next-After") != "11" {
		t.Fatalf("page: %d %+v next=%q", w.Code, s.page, w.Header().Get("X-Next-After"))
	}
	if body := w.Body.String(); !strings.Contains(body, `"id":11`) || strings.Contains(body, `"id":12`) {
		t.Fatalf("page body: %s", body)
	}
	// limit=100 is the inclusive maximum.
	s = &stub{list: three}
	h = New(task.NewService(s), s, true, func(context.Context) error { return nil })
	if w = request(h, "GET", "/tasks?limit=100", "", "", nil); w.Code != 200 || s.page.Limit != 100 {
		t.Fatalf("max limit: %d %+v", w.Code, s.page)
	}
}

func TestPaginationQueryEncoding(t *testing.T) {
	for _, query := range []string{
		"limit=%", "limit=%2", "limit=%GG", "after=%GG", "%GG=2",
		"limit=2&after=%GG", "limit=2;after=7", "limit=2&extra=one;two",
	} {
		t.Run(query, func(t *testing.T) {
			for _, method := range []string{"GET", "HEAD"} {
				s := &stub{}
				h := New(task.NewService(s), s, true, func(context.Context) error { return nil })
				w := request(h, method, "/tasks?"+query, "", "", nil)
				if w.Code != 400 || s.calls != 0 || w.Header().Get("X-Next-After") != "" {
					t.Fatalf("%s: status=%d calls=%d next=%q; want 400 before repository", method, w.Code, s.calls, w.Header().Get("X-Next-After"))
				}
			}
		})
	}
	for _, query := range []string{"limit=2&after=7", "%6cimit=%32&%61fter=%37", "limit=2&after=7&extra=one%3Btwo", "limit=2&after=7&extra=ignored"} {
		t.Run(query, func(t *testing.T) {
			s := &stub{}
			h := New(task.NewService(s), s, true, func(context.Context) error { return nil })
			w := request(h, "GET", "/tasks?"+query, "", "", nil)
			if w.Code != 200 || s.calls != 1 || s.page != (task.Page{Limit: 2, After: 7}) {
				t.Fatalf("valid encoding: status=%d calls=%d page=%+v", w.Code, s.calls, s.page)
			}
		})
	}
}

func TestExactJSONMemberNames(t *testing.T) {
	cases := []struct {
		name, method, body string
		code               int
	}{
		{"uppercase name", "POST", `{"NAME":"work","status":0}`, 400},
		{"mixed case status", "POST", `{"name":"work","Status":0}`, 400},
		{"mixed case detail", "POST", `{"name":"work","status":0,"Detail":"notes"}`, 400},
		{"mixed case version", "PUT", `{"name":"work","status":0,"Version":1}`, 400},
		{"uppercase name on PUT", "PUT", `{"NAME":"work","status":0,"version":1}`, 400},
		{"escaped uppercase name", "POST", `{"\u004eAME":"work","status":0}`, 400},
		{"case variant beside valid name", "POST", `{"name":"work","NAME":"other","status":0}`, 400},
		// Exact duplicate members retain encoding/json's existing last-value behavior.
		{"duplicate name", "POST", `{"name":"other","name":"work","status":0,"detail":"notes"}`, 201},
		{"escaped duplicate name", "POST", `{"name":"other","\u006eame":"work","status":0,"detail":"notes"}`, 201},
		{"duplicate status", "POST", `{"name":"work","status":1,"status":0,"detail":"notes"}`, 201},
		{"duplicate detail", "POST", `{"name":"work","status":0,"detail":"a","detail":"notes"}`, 201},
		{"escaped duplicate version", "PUT", `{"name":"work","status":0,"detail":"notes","version":2,"\u0076ersion":1}`, 200},
		{"lowercase POST", "POST", `{"name":"work","status":0,"detail":"notes"}`, 201},
		{"lowercase PUT", "PUT", `{"name":"work","status":0,"detail":"notes","version":1}`, 200},
		{"escaped lowercase keys", "POST", `{"\u006eame":"work","\u0073tatus":0,"detail":"notes"}`, 201},
		{"BOM", "POST", "\xef\xbb\xbf" + `{"name":"work","status":0}`, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &stub{}
			h := New(task.NewService(s), s, true, func(context.Context) error { return nil })
			path := "/tasks"
			if tc.method == "PUT" {
				path += "/5"
			}
			w := request(h, tc.method, path, tc.body, "application/json", nil)
			if w.Code != tc.code {
				t.Fatalf("status=%d body=%s; want %d", w.Code, w.Body, tc.code)
			}
			if tc.code == 400 && s.calls != 0 {
				t.Fatalf("invalid JSON reached repository: calls=%d", s.calls)
			}
			if tc.code < 300 && (s.calls != 1 || s.in.Name != "work" || s.in.Status != 0 || s.in.Detail != "notes") {
				t.Fatalf("valid JSON changed: calls=%d input=%+v", s.calls, s.in)
			}
			if tc.code == 200 && s.in.Version != 1 {
				t.Fatalf("PUT version=%d; want 1", s.in.Version)
			}
		})
	}
}

func TestDetailIsOptionalAndValidated(t *testing.T) {
	// absent, null and present detail all decode; wrong type and overlong are rejected before the repository.
	cases := []struct {
		name, body string
		code       int
		want       string
	}{
		{"absent", `{"name":"a","status":0}`, 201, ""},
		{"null", `{"name":"a","status":0,"detail":null}`, 201, ""},
		{"present", `{"name":"a","status":0,"detail":"milk, eggs"}`, 201, "milk, eggs"},
		{"wrong type", `{"name":"a","status":0,"detail":7}`, 400, ""},
		{"too long", `{"name":"a","status":0,"detail":"` + strings.Repeat("d", 10001) + `"}`, 400, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &stub{}
			h := New(task.NewService(s), s, true, func(context.Context) error { return nil })
			w := request(h, "POST", "/tasks", tc.body, "application/json", nil)
			if w.Code != tc.code {
				t.Fatalf("code %d body %s", w.Code, w.Body)
			}
			if tc.code == 201 && (s.calls != 1 || s.in.Detail != tc.want || !strings.Contains(w.Body.String(), `"detail":"`+tc.want+`"`)) {
				t.Fatalf("detail round trip: calls=%d in=%q body=%s", s.calls, s.in.Detail, w.Body)
			}
			if tc.code == 400 && s.calls != 0 {
				t.Fatal("invalid detail reached repository")
			}
		})
	}
}

func TestHeadServesLikeGet(t *testing.T) {
	// A real server, because httptest.ResponseRecorder does not apply HEAD semantics.
	s := &stub{list: []task.Task{{ID: 1, Name: "x", Status: 0}}}
	srv := httptest.NewServer(New(task.NewService(s), s, true, func(context.Context) error { return nil }))
	defer srv.Close()
	get, err := http.Get(srv.URL + "/tasks")
	if err != nil {
		t.Fatal(err)
	}
	get.Body.Close()
	head, err := http.Head(srv.URL + "/tasks")
	if err != nil {
		t.Fatal(err)
	}
	head.Body.Close()
	if head.StatusCode != 200 || s.calls != 2 || head.ContentLength != get.ContentLength || head.ContentLength <= 0 {
		t.Fatalf("HEAD: status=%d calls=%d length=%d (GET %d)", head.StatusCode, s.calls, head.ContentLength, get.ContentLength)
	}
	for _, h := range []string{"Content-Type", "Cache-Control", "X-Content-Type-Options"} {
		if head.Header.Get(h) != get.Header.Get(h) {
			t.Fatalf("HEAD %s=%q, GET %s=%q", h, head.Header.Get(h), h, get.Header.Get(h))
		}
	}
}
