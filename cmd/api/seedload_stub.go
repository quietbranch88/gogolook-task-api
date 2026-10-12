//go:build !loadtest

package main

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// main.go refers to seedLoad unconditionally; this stub keeps the default build
// compiling. The real command is in seedload.go behind the loadtest build tag.
func seedLoad(context.Context, *pgxpool.Pool) error {
	return errors.New("seed-load is not compiled into this binary; build with -tags loadtest")
}
