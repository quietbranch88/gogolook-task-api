package task

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// Domain error kinds. Adapters translate their own failures into these so that
// the delivery layer can choose a status code without importing a driver.
var (
	ErrInvalid         = errors.New("invalid task")
	ErrNotFound        = errors.New("task not found")
	ErrForbidden       = errors.New("membership required")
	ErrUnauthorized    = errors.New("invalid credentials")
	ErrVersionConflict = errors.New("task version conflict")
	ErrConflict        = errors.New("idempotency key conflicts with prior operation")
	// ErrUnavailable: the storage could not be reached (connection, pool closed).
	// Retrying later is reasonable.
	ErrUnavailable = errors.New("storage unavailable")
	// ErrTimeout: the operation exceeded the request's deadline.
	ErrTimeout = errors.New("storage timeout")
)

// Field limits in Unicode characters (code points). The database enforces the
// same bounds with CHECK constraints.
const (
	MaxNameLength   = 1000
	MaxDetailLength = 10000
)

type Task struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Status    int       `json:"status"` // 0 = not done, 1 = done; nothing else is valid
	Detail    string    `json:"detail"`
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"created_at"` // database-owned, UTC, never null
	UpdatedAt time.Time `json:"updated_at"`
}

// Scope is the authenticated caller and the tenant they selected. Every
// repository operation is restricted to this tenant and requires that the user
// is a member of it.
type Scope struct{ UserID, TenantID int64 }

// Input carries the mutable fields. Detail is optional at the API boundary and
// defaults to the empty string; PUT replaces all three fields.
type Input struct {
	Name           string
	Status         int
	Detail         string
	IdempotencyKey string
	Version        int64 // Required expected version for Update; unused on Create.
}

// ValidIdempotencyKey bounds optional client operation identifiers.
func ValidIdempotencyKey(key string) bool {
	if len(key) < 1 || len(key) > 128 {
		return false
	}
	for _, c := range key {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-') {
			return false
		}
	}
	return true
}

func (i Input) Validate() error {
	if i.IdempotencyKey != "" && !ValidIdempotencyKey(i.IdempotencyKey) {
		return ErrInvalid
	}
	if !utf8.ValidString(i.Name) || strings.TrimSpace(i.Name) == "" || utf8.RuneCountInString(i.Name) > MaxNameLength || strings.ContainsRune(i.Name, '\x00') || (i.Status != 0 && i.Status != 1) {
		return ErrInvalid
	}
	if !utf8.ValidString(i.Detail) || utf8.RuneCountInString(i.Detail) > MaxDetailLength || strings.ContainsRune(i.Detail, '\x00') {
		return ErrInvalid
	}
	return nil
}

// DefaultPageSize and MaxPageSize bound list work even when limit is omitted.
const DefaultPageSize = 100
const MaxPageSize = 100

// Page is an optional keyset window over a tenant's tasks ordered by id.
// Limit 0 selects DefaultPageSize; After is the id strictly before the first row.
type Page struct {
	After int64
	Limit int
}

func (p Page) Validate() error {
	if p.After < 0 || p.Limit < 0 || p.Limit > MaxPageSize {
		return ErrInvalid
	}
	return nil
}

// Repository.List returns rows with id > page.After in ascending id order. When
// page.Limit > 0 it returns at most Limit+1 rows so the service can tell
// whether another page exists without a second query.
type Repository interface {
	List(context.Context, Scope, Page) ([]Task, error)
	Create(context.Context, Scope, Input) (Task, error)
	Update(context.Context, Scope, int64, Input) (Task, error)
	Delete(context.Context, Scope, int64) error
}

type Service struct{ repo Repository }

func NewService(r Repository) *Service { return &Service{repo: r} }

// List returns the page and, when more rows follow, the id to pass as the next
// After. next is 0 when the page is the last one.
func (s *Service) List(ctx context.Context, scope Scope, page Page) ([]Task, int64, error) {
	if err := page.Validate(); err != nil {
		return nil, 0, err
	}
	if page.Limit == 0 {
		page.Limit = DefaultPageSize
	}
	tasks, err := s.repo.List(ctx, scope, page)
	if err != nil {
		return nil, 0, err
	}
	if len(tasks) > page.Limit {
		tasks = tasks[:page.Limit]
		return tasks, tasks[len(tasks)-1].ID, nil
	}
	return tasks, 0, nil
}
func (s *Service) Create(ctx context.Context, scope Scope, in Input) (Task, error) {
	if err := in.Validate(); err != nil {
		return Task{}, err
	}
	return s.repo.Create(ctx, scope, in)
}
func (s *Service) Update(ctx context.Context, scope Scope, id int64, in Input) (Task, error) {
	if id < 1 || in.Version < 1 {
		return Task{}, ErrInvalid
	}
	if err := in.Validate(); err != nil {
		return Task{}, err
	}
	return s.repo.Update(ctx, scope, id, in)
}
func (s *Service) Delete(ctx context.Context, scope Scope, id int64) error {
	if id < 1 {
		return ErrInvalid
	}
	return s.repo.Delete(ctx, scope, id)
}
