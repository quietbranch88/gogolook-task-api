package main

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Retry only the read-only startup probe, before any migration or HTTP work.
// A transient failed handshake must not discard the remaining startup budget.
// Each connection still has its 2s limit; three attempts and brief backoff keep
// an unavailable database bounded without waiting out the entire 30s budget.
func pingStartup(ctx context.Context, ping func(context.Context) error) error {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := ping(ctx)
		if err == nil || attempt == 2 || ctx.Err() != nil || !transientStartupError(err) {
			return err
		}
		timer := time.NewTimer((100 * time.Millisecond) << attempt)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

func transientStartupError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "53300" || pgErr.Code == "57P03"
	}
	var connectErr *pgconn.ConnectError
	if !errors.As(err, &connectErr) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	var networkErr net.Error
	return errors.As(err, &networkErr) && networkErr.Timeout()
}
