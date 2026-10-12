package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/quietbranch88/gogolook-task-api/internal/task"
)

type Authenticator interface {
	Authenticate(context.Context, string) (int64, error)
}
type Handler struct {
	service *task.Service
	auth    Authenticator
	demo    bool
	ready   func(context.Context) error
}

func New(service *task.Service, auth Authenticator, demo bool, ready func(context.Context) error) http.Handler {
	h := &Handler{service: service, auth: auth, demo: demo, ready: ready}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := h.ready(ctx); err != nil {
			publicError(w, 503, "service unavailable")
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("GET /tasks", h.handle)
	mux.HandleFunc("POST /tasks", h.handle)
	mux.HandleFunc("PUT /tasks/{id}", h.handle)
	mux.HandleFunc("DELETE /tasks/{id}", h.handle)
	return mux
}

// scope resolves who is calling and which tenant they selected. Identity comes
// only from the bearer token; no request header is ever read as a user id.
func (h *Handler) scope(w http.ResponseWriter, r *http.Request) (task.Scope, bool) {
	if h.demo {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Tenant-ID") != "" {
			publicError(w, 400, "demo mode uses a fixed identity")
			return task.Scope{}, false
		}
		return task.Scope{UserID: 1, TenantID: 1}, true
	}
	// Tokens are opaque; 32..256 characters rejects obvious junk and oversized
	// values before touching the database (issued tokens are 64 hex characters).
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) < 32 || len(parts[1]) > 256 {
		publicError(w, 401, "valid bearer token required")
		return task.Scope{}, false
	}
	user, err := h.auth.Authenticate(r.Context(), parts[1])
	if err != nil {
		handleError(w, err)
		return task.Scope{}, false
	}
	tenant, err := strconv.ParseInt(r.Header.Get("X-Tenant-ID"), 10, 64)
	if err != nil || tenant < 1 {
		publicError(w, 400, "positive X-Tenant-ID required")
		return task.Scope{}, false
	}
	return task.Scope{UserID: user, TenantID: tenant}, true
}

func (h *Handler) handle(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	scope, ok := h.scope(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		page, ok := decodePage(w, r)
		if !ok {
			return
		}
		tasks, next, err := h.service.List(ctx, scope, page)
		if err != nil {
			handleError(w, err)
			return
		}
		if next > 0 {
			w.Header().Set("X-Next-After", strconv.FormatInt(next, 10))
		}
		// HEAD reaches here too (the mux routes it to the GET pattern); net/http drops the
		// body and keeps the headers, including Content-Length, so no special case is needed.
		writeJSON(w, 200, tasks)
		return
	}
	var id int64
	if r.Method == http.MethodPut || r.Method == http.MethodDelete {
		var err error
		id, err = strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id < 1 {
			publicError(w, 400, "positive task id required")
			return
		}
	}
	if r.Method == http.MethodDelete {
		if err := h.service.Delete(ctx, scope, id); err != nil {
			handleError(w, err)
			return
		}
		w.WriteHeader(204)
		return
	}
	in, ok := decodeInput(w, r)
	if !ok {
		return
	}
	var result task.Task
	var err error
	if r.Method == http.MethodPost {
		if keys, present := r.Header["Idempotency-Key"]; present {
			if len(keys) != 1 || !task.ValidIdempotencyKey(keys[0]) {
				publicError(w, 400, "Idempotency-Key must be 1-128 ASCII letters, digits or ._:-")
				return
			}
			in.IdempotencyKey = keys[0]
		}
		result, err = h.service.Create(ctx, scope, in)
	} else {
		result, err = h.service.Update(ctx, scope, id, in)
	}
	if err != nil {
		handleError(w, err)
		return
	}
	status := 200
	if r.Method == http.MethodPost {
		status = 201
	}
	writeJSON(w, status, result)
}

// decodePage reads the optional ?limit= and ?after= query parameters. Absent
// parameters use the bounded default page; present ones must be valid.
func decodePage(w http.ResponseWriter, r *http.Request) (task.Page, bool) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		publicError(w, 400, "query must use valid URL encoding")
		return task.Page{}, false
	}
	var page task.Page
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > task.MaxPageSize {
			publicError(w, 400, fmt.Sprintf("limit must be an integer from 1 to %d", task.MaxPageSize))
			return task.Page{}, false
		}
		page.Limit = n
	}
	if q.Has("after") {
		n, err := strconv.ParseInt(q.Get("after"), 10, 64)
		if err != nil || n < 0 {
			publicError(w, 400, "after must be a non-negative integer task id")
			return task.Page{}, false
		}
		page.After = n
	}
	return page, true
}

// decodeInput reads the JSON body of POST and PUT. The contract is strict on
// purpose: Content-Type must be application/json (415 otherwise, including
// when it is missing, because curl -d sends form-urlencoded and a silent guess
// would hide that), and the object may contain only name, status, detail and (on PUT) version
// (400 otherwise, so a misspelled field is caught instead of being dropped).
// Exactly one JSON object, no trailing content, 16 KiB cap, name and status
// required. README shows the exact curl invocation.
func decodeInput(w http.ResponseWriter, r *http.Request) (task.Input, bool) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		publicError(w, 415, "Content-Type: application/json is required")
		return task.Input{}, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			publicError(w, 413, "request body too large")
		} else {
			publicError(w, 400, "could not read request body")
		}
		return task.Input{}, false
	}
	// encoding/json replaces malformed UTF-8, so validate before that lossy step.
	if !utf8.Valid(raw) {
		publicError(w, 400, "body must be valid UTF-8")
		return task.Input{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var body struct {
		Version json.RawMessage `json:"version"`
		Name    *string         `json:"name"`
		Status  *int            `json:"status"`
		Detail  *string         `json:"detail"` // optional; absent or null means ""
	}
	if err = decoder.Decode(&body); err == nil {
		var extra any
		if next := decoder.Decode(&extra); next != io.EOF {
			if next == nil {
				err = task.ErrInvalid
			} else {
				err = next
			}
		}
	}
	if err == nil {
		// Struct decoding accepts case variants even with DisallowUnknownFields.
		// Check decoded member names exactly against the endpoint schema too.
		var members map[string]json.RawMessage
		if err = json.Unmarshal(raw, &members); err == nil {
			for name := range members {
				switch name {
				case "name", "status", "detail", "version":
				default:
					err = task.ErrInvalid
				}
			}
		}
	}
	if err != nil {
		publicError(w, 400, "body must be one JSON object with only name, status, detail and (on PUT) version")
		return task.Input{}, false
	}
	if body.Name == nil || body.Status == nil {
		publicError(w, 400, "name and status are required")
		return task.Input{}, false
	}
	in := task.Input{Name: *body.Name, Status: *body.Status}
	if r.Method == http.MethodPut {
		if len(body.Version) == 0 || json.Unmarshal(body.Version, &in.Version) != nil || in.Version < 1 {
			publicError(w, 400, "positive integer version is required for PUT; send the version from the POST or GET response")
			return task.Input{}, false
		}
	} else if len(body.Version) != 0 {
		publicError(w, 400, "version is server-generated on POST")
		return task.Input{}, false
	}
	if body.Detail != nil {
		in.Detail = *body.Detail
	}
	return in, true
}

func handleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, task.ErrInvalid):
		publicError(w, 400, fmt.Sprintf("name must contain 1-%d nonblank characters, status must be 0 or 1, detail at most %d characters", task.MaxNameLength, task.MaxDetailLength))
	case errors.Is(err, task.ErrUnauthorized):
		publicError(w, 401, "invalid credentials")
	case errors.Is(err, task.ErrForbidden):
		publicError(w, 403, "membership required")
	case errors.Is(err, task.ErrNotFound):
		publicError(w, 404, "task not found")
	case errors.Is(err, task.ErrVersionConflict):
		publicError(w, 409, "task version conflict; read current task before retrying")
	case errors.Is(err, task.ErrConflict):
		publicError(w, 409, "idempotency key was used for different input or a deleted task")
	case errors.Is(err, task.ErrUnavailable):
		// Storage unreachable: a retry later can succeed, so say so.
		log.Print("storage unavailable: ", err)
		publicError(w, 503, "service unavailable")
	case errors.Is(err, task.ErrTimeout):
		if !errors.Is(err, context.Canceled) {
			// A cancelled context means the client went away; nobody reads this
			// response, so it is not worth a log line.
			log.Print("request timed out: ", err)
		}
		publicError(w, 504, "request timed out")
	default:
		// A deterministic failure (bug, bad statement). Never 503: a client that
		// follows the retry hint would loop. Cause goes to the process log only.
		log.Print("internal error: ", err)
		publicError(w, 500, "internal error")
	}
}
func publicError(w http.ResponseWriter, status int, message string) {
	switch status {
	case 401:
		w.Header().Set("WWW-Authenticate", `Bearer realm="tasks"`)
	case 503:
		w.Header().Set("Retry-After", "1")
	}
	writeJSON(w, status, map[string]string{"error": message})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
