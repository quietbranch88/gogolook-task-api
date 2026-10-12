package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/quietbranch88/gogolook-task-api/internal/task"
)

func (r *Repository) createIdempotent(ctx context.Context, scope task.Scope, in task.Input) (task.Task, error) {
	tx, err := r.beginAuthorized(ctx, scope)
	if err != nil {
		return task.Task{}, err
	}
	defer tx.Rollback(ctx)
	key := sha256.Sum256([]byte(in.IdempotencyKey))
	// Strings and an integer have a deterministic JSON representation; transport
	// whitespace, field order and omitted/defaulted detail are not operation identity.
	canonical, _ := json.Marshal(struct {
		Name   string
		Status int
		Detail string
	}{in.Name, in.Status, in.Detail})
	input := sha256.Sum256(canonical)
	inserted, err := tx.Exec(ctx, `INSERT INTO task_create_keys(tenant_id,user_id,key_hash,input_hash) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, scope.TenantID, scope.UserID, key[:], input[:])
	if err != nil {
		return task.Task{}, classify(err)
	}
	var result task.Task
	if inserted.RowsAffected() == 0 {
		// A conflicting in-flight insertion must settle before INSERT returns.
		// A fresh READ COMMITTED statement then observes its committed key row.
		var prior []byte
		var id *int64
		if err = tx.QueryRow(ctx, `SELECT input_hash,task_id FROM task_create_keys WHERE tenant_id=$1 AND user_id=$2 AND key_hash=$3`, scope.TenantID, scope.UserID, key[:]).Scan(&prior, &id); err != nil {
			return task.Task{}, classify(err)
		}
		if !bytes.Equal(prior, input[:]) || id == nil {
			return task.Task{}, task.ErrConflict
		}
		err = tx.QueryRow(ctx, `SELECT id,name,status,detail,version,created_at,updated_at FROM tasks WHERE tenant_id=$1 AND id=$2`, scope.TenantID, *id).Scan(&result.ID, &result.Name, &result.Status, &result.Detail, &result.Version, &result.CreatedAt, &result.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return task.Task{}, task.ErrConflict
		}
		if err != nil {
			return task.Task{}, classify(err)
		}
		// A replay changes no application data and needs no durable commit.
		normalizeTaskTimes(&result)
		return result, classify(tx.Rollback(ctx))
	}
	err = tx.QueryRow(ctx, `INSERT INTO tasks(tenant_id,name,status,detail) VALUES($1,$2,$3,$4) RETURNING id,name,status,detail,version,created_at,updated_at`, scope.TenantID, in.Name, in.Status, in.Detail).Scan(&result.ID, &result.Name, &result.Status, &result.Detail, &result.Version, &result.CreatedAt, &result.UpdatedAt)
	if err != nil {
		return task.Task{}, classify(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE task_create_keys SET task_id=$1 WHERE tenant_id=$2 AND user_id=$3 AND key_hash=$4`, result.ID, scope.TenantID, scope.UserID, key[:]); err != nil {
		return task.Task{}, classify(err)
	}
	// Key reservation and task creation have exactly the same commit outcome.
	normalizeTaskTimes(&result)
	return result, classify(tx.Commit(ctx))
}
