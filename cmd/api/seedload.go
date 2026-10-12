//go:build loadtest

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/quietbranch88/gogolook-task-api/internal/postgres"
)

// seedLoad creates the load-test fixture described in docs/DESIGN.md. It is
// compiled only with `-tags loadtest`, so the shipped binary and image do not
// contain a command that prints credentials. Fixture:
// SEED_LOAD_TENANTS tenants (default 20), one user and one 7-day bearer token
// each, and SEED_LOAD_TASKS tasks per tenant (default 3000).
// Tokens are printed once as JSON lines for the load generator;
// only their SHA-256 digests are stored. Local load fixtures only.
func seedLoad(ctx context.Context, pool *pgxpool.Pool) error {
	tenants, err := envInt("SEED_LOAD_TENANTS", 20, 1, 1000)
	if err != nil {
		return err
	}
	tasks, err := envInt("SEED_LOAD_TASKS", 3000, 0, 1_000_000)
	if err != nil {
		return err
	}
	tokens := make([]string, tenants)
	hashes := make([][]byte, tenants)
	for i := range tokens {
		raw := make([]byte, 32)
		_, _ = rand.Read(raw) // never fails since Go 1.24
		tokens[i] = hex.EncodeToString(raw)
		sum := sha256.Sum256([]byte(tokens[i]))
		hashes[i] = sum[:]
	}
	if err := postgres.SeedLoad(ctx, pool, tasks, hashes); err != nil {
		return fmt.Errorf("seed-load: %w", postgres.SafeError(err))
	}
	enc := json.NewEncoder(os.Stdout)
	for i, token := range tokens {
		// "tenant" is the X-Tenant-ID to send; the user has the same id.
		if err := enc.Encode(map[string]any{"tenant": postgres.LoadTenantBase + int64(i), "token": token}); err != nil {
			return err
		}
	}
	return nil
}

func envInt(name string, def, lo, hi int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, errors.New(name + " out of range")
	}
	return n, nil
}
