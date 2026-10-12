package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
	"github.com/quietbranch88/gogolook-task-api/internal/task"
)

// Retain the original cause for errors.Is/As without rendering driver messages,
// which may contain connection strings or row data, into normal process logs.
type classifiedError struct {
	kind, cause error
	reason      string
}

var errStorageFailure = errors.New("storage failure")

// SafeError preserves a storage failure's cause while exposing only a bounded
// diagnostic. Use it at CLI logging boundaries without assigning retry semantics.
func SafeError(err error) error {
	if err == nil {
		return nil
	}
	return &classifiedError{kind: errStorageFailure, cause: err, reason: diagnostic(err)}
}

func (e *classifiedError) Error() string {
	return fmt.Sprintf("%s (%s; cause type %T)", e.kind, e.reason, e.cause)
}
func (e *classifiedError) Unwrap() error        { return e.cause }
func (e *classifiedError) Is(target error) bool { return target == e.kind }

func diagnostic(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	case errors.Is(err, context.Canceled):
		return "context canceled"
	case scanFailure(err):
		return "row decoding failed"
	case errors.Is(err, puddle.ErrClosedPool):
		return "pool closed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed),
		errors.Is(err, pgconn.ErrConnClosed), errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "connection closed"
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && len(pgErr.Code) == 5 {
		for _, c := range pgErr.Code {
			if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') {
				return "database error"
			}
		}
		return "SQLSTATE " + pgErr.Code
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		if networkError.Timeout() {
			return "network timeout"
		}
		return "network error"
	}
	return "storage failure"
}

// classify maps a pgx/pgconn failure onto the domain error kinds.
//
//   - deadline exceeded → task.ErrTimeout (the request budget ran out). A
//     cancelled context (client went away) lands here too; the response is
//     not delivered to anyone in that case
//   - a PostgreSQL error of class 22 (data) or 23 (integrity) → task.ErrInvalid:
//     the database rejected the values, which should already have been caught by
//     Input.Validate and is therefore reported as a client error, not an outage
//   - PostgreSQL 53300 (too_many_connections) or 57P03 (cannot_connect_now)
//     → task.ErrUnavailable: the server temporarily cannot accept a connection
//   - PostgreSQL 25P03 (idle_in_transaction_session_timeout) or 25P04
//     (transaction_timeout) → task.ErrTimeout: a server transaction budget expired
//   - any other PostgreSQL error → safe diagnostic: permanent or unclassified
//     failures must not be presented as safely retryable (500)
//   - connection-level failures (connect error, refused/reset/closed connection,
//     network timeout, closed pool) → task.ErrUnavailable: a later retry can succeed
//   - everything else (scan/encode errors, closed transactions, unknown causes)
//     → safe diagnostic (500): a bug must never carry a retry hint
//
// Callers map pgx.ErrNoRows themselves before calling classify.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &classifiedError{kind: task.ErrTimeout, cause: err, reason: diagnostic(err)}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if len(pgErr.Code) >= 2 && (pgErr.Code[:2] == "22" || pgErr.Code[:2] == "23") {
			return &classifiedError{kind: task.ErrInvalid, cause: err, reason: diagnostic(err)}
		}
		if pgErr.Code == "53300" || pgErr.Code == "57P03" {
			return &classifiedError{kind: task.ErrUnavailable, cause: err, reason: diagnostic(err)}
		}
		if pgErr.Code == "25P03" || pgErr.Code == "25P04" {
			return &classifiedError{kind: task.ErrTimeout, cause: err, reason: diagnostic(err)}
		}
		return SafeError(err)
	}
	if !scanFailure(err) && unavailable(err) {
		return &classifiedError{kind: task.ErrUnavailable, cause: err, reason: diagnostic(err)}
	}
	return SafeError(err)
}

// unavailable reports connection-level failures, the only non-PostgreSQL errors
// that justify asking the client to retry.
func unavailable(err error) bool {
	if errors.Is(err, puddle.ErrClosedPool) || errors.Is(err, pgconn.ErrConnClosed) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return true
	}
	var operationErr *net.OpError
	if errors.As(err, &operationErr) {
		return true
	}
	// Every syscall.Errno implements net.Error, including programmer errors such
	// as EINVAL. Only a known connection errno or an actual network operation
	// above establishes that the failure came from the transport.
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return false
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

// A codec can wrap EOF while decoding a field. That does not mean the database
// connection failed, and retrying the same query will not repair the row type.
func scanFailure(err error) bool {
	var value pgx.ScanArgError
	var pointer *pgx.ScanArgError
	return errors.As(err, &value) || errors.As(err, &pointer)
}
