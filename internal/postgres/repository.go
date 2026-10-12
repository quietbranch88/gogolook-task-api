package postgres

import (
	"context"
	"crypto/sha256"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/quietbranch88/gogolook-task-api/internal/task"
)

type Repository struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// Authenticate resolves a bearer token to a user id. Only the SHA-256 digest is
// compared; expired and revoked keys never match.
func (r *Repository) Authenticate(ctx context.Context, token string) (int64, error) {
	hash := sha256.Sum256([]byte(token))
	var id int64
	err := r.pool.QueryRow(ctx, `SELECT user_id FROM api_keys WHERE token_hash=$1 AND expires_at>now() AND revoked_at IS NULL`, hash[:]).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, task.ErrUnauthorized
	}
	return id, classify(err)
}

// beginAuthorized opens the transaction every task operation runs in and checks
// the caller's membership inside it. The caller owns the returned transaction
// and must Commit or Rollback it. FOR SHARE blocks a concurrent deletion of that
// membership row until this transaction ends, so a revocation never commits in
// the middle of an authorized operation.
func (r *Repository) beginAuthorized(ctx context.Context, scope task.Scope) (pgx.Tx, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, classify(err)
	}
	var id int64
	err = tx.QueryRow(ctx, `SELECT user_id FROM memberships WHERE tenant_id=$1 AND user_id=$2 FOR SHARE`, scope.TenantID, scope.UserID).Scan(&id)
	if err != nil {
		_ = tx.Rollback(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, task.ErrForbidden
		}
		return nil, classify(err)
	}
	return tx, nil
}

// List receives a validated, normalized page from the service and fetches one
// extra row so the service can report whether another page exists.
func (r *Repository) List(ctx context.Context, scope task.Scope, page task.Page) ([]task.Task, error) {
	tx, err := r.beginAuthorized(ctx, scope)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	limit := page.Limit + 1
	rows, err := tx.Query(ctx, `SELECT id,name,status,detail,version,created_at,updated_at FROM tasks WHERE tenant_id=$1 AND id>$2 ORDER BY id LIMIT $3`, scope.TenantID, page.After, limit)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	result := make([]task.Task, 0)
	for rows.Next() {
		var t task.Task
		if err = rows.Scan(&t.ID, &t.Name, &t.Status, &t.Detail, &t.Version, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, classify(err)
		}
		normalizeTaskTimes(&t)
		result = append(result, t)
	}
	if err = rows.Err(); err != nil {
		return nil, classify(err)
	}
	// Listing changes no durable application data. Roll back the read transaction
	// to release its membership lock without a synchronous WAL commit. The lock
	// remains held until every row has been read, preserving revocation ordering.
	if err = tx.Rollback(ctx); err != nil {
		return nil, classify(err)
	}
	return result, nil
}

func (r *Repository) Create(ctx context.Context, scope task.Scope, in task.Input) (task.Task, error) {
	if in.IdempotencyKey != "" {
		return r.createIdempotent(ctx, scope, in)
	}
	tx, err := r.beginAuthorized(ctx, scope)
	if err != nil {
		return task.Task{}, err
	}
	defer tx.Rollback(ctx)
	var t task.Task
	err = tx.QueryRow(ctx, `INSERT INTO tasks(tenant_id,name,status,detail) VALUES($1,$2,$3,$4) RETURNING id,name,status,detail,version,created_at,updated_at`, scope.TenantID, in.Name, in.Status, in.Detail).Scan(&t.ID, &t.Name, &t.Status, &t.Detail, &t.Version, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return task.Task{}, classify(err)
	}
	normalizeTaskTimes(&t)
	return t, classify(tx.Commit(ctx))
}

func (r *Repository) Update(ctx context.Context, scope task.Scope, id int64, in task.Input) (task.Task, error) {
	tx, err := r.beginAuthorized(ctx, scope)
	if err != nil {
		return task.Task{}, err
	}
	defer tx.Rollback(ctx)
	var t task.Task
	err = tx.QueryRow(ctx, `UPDATE tasks SET name=$1,status=$2,detail=$3,version=version+1 WHERE tenant_id=$4 AND id=$5 AND version=$6 RETURNING id,name,status,detail,version,created_at,updated_at`, in.Name, in.Status, in.Detail, scope.TenantID, id, in.Version).Scan(&t.ID, &t.Name, &t.Status, &t.Detail, &t.Version, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// A fresh tenant-scoped read distinguishes a stale version from an absent
		// task. It never authorizes an update or reveals another tenant's row.
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE tenant_id=$1 AND id=$2)`, scope.TenantID, id).Scan(&exists); err != nil {
			return task.Task{}, classify(err)
		}
		if exists {
			return task.Task{}, task.ErrVersionConflict
		}
		return task.Task{}, task.ErrNotFound
	}
	if err != nil {
		return task.Task{}, classify(err)
	}
	normalizeTaskTimes(&t)
	return t, classify(tx.Commit(ctx))
}

// PostgreSQL stores instants independently of the connection timezone. Emit
// their UTC representation consistently.
func normalizeTaskTimes(t *task.Task) {
	t.CreatedAt = t.CreatedAt.UTC()
	t.UpdatedAt = t.UpdatedAt.UTC()
}

func (r *Repository) Delete(ctx context.Context, scope task.Scope, id int64) error {
	tx, err := r.beginAuthorized(ctx, scope)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `DELETE FROM tasks WHERE tenant_id=$1 AND id=$2`, scope.TenantID, id)
	if err != nil {
		return classify(err)
	}
	if result.RowsAffected() == 0 {
		return task.ErrNotFound
	}
	return classify(tx.Commit(ctx))
}
