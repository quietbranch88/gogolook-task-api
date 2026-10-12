package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LoadTenantBase is the first tenant/user id used by SeedLoad. It sits far above
// the demo tenant (1) and any hand-seeded fixture ids so the two never collide.
const LoadTenantBase int64 = 100001

// SeedLoad creates len(tokenHashes) tenants with ids LoadTenantBase+i. Each tenant
// gets one user with the same id (so the "tenant" value printed by seed-load
// identifies both), a membership, a 7-day api key per token hash, and tasksPerTenant tasks
// for every tenant that has none yet. It is idempotent for identities and does
// not add tasks to a tenant that already has rows.
func SeedLoad(ctx context.Context, pool *pgxpool.Pool, tasksPerTenant int, tokenHashes [][]byte) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for i, hash := range tokenHashes {
		id := LoadTenantBase + int64(i)
		// One statement per Exec: the extended protocol rejects multi-statement strings with parameters.
		if _, err = tx.Exec(ctx, `INSERT INTO tenants(id,name) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, fmt.Sprintf("Load tenant %d", id)); err != nil {
			return err
		}
		for _, stmt := range []string{
			`INSERT INTO users(id) VALUES($1) ON CONFLICT DO NOTHING`,
			`INSERT INTO memberships(tenant_id,user_id) VALUES($1,$1) ON CONFLICT DO NOTHING`,
		} {
			if _, err = tx.Exec(ctx, stmt, id); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `INSERT INTO api_keys(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '7 days') ON CONFLICT DO NOTHING`, hash, id); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO tasks(tenant_id,name,status)
 SELECT $1,'load task '||g,g%2 FROM generate_series(1,$2::int) g
 WHERE NOT EXISTS (SELECT 1 FROM tasks WHERE tenant_id=$1)`, id, tasksPerTenant); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// SeedDemo creates the fixed demo tenant and user. The parameter-less Exec runs
// the multi-statement string as one implicit transaction. Local demo data only.
func SeedDemo(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
 INSERT INTO tenants(id,name) VALUES(1,'Local demo') ON CONFLICT DO NOTHING;
 INSERT INTO users(id) VALUES(1) ON CONFLICT DO NOTHING;
 INSERT INTO memberships(tenant_id,user_id) VALUES(1,1) ON CONFLICT DO NOTHING;
 `)
	return err
}
