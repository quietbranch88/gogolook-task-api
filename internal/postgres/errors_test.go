package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/puddle/v2"
	"github.com/quietbranch88/gogolook-task-api/internal/httpapi"
	"github.com/quietbranch88/gogolook-task-api/internal/task"
)

func TestClassify(t *testing.T) {
	syntax := &pgconn.PgError{Code: "42601", Message: "syntax error"}
	cases := []struct {
		name string
		in   error
		want error // nil means no retryable or invalid-input classification
	}{
		{"nil stays nil", nil, nil},
		{"deadline is a timeout", context.DeadlineExceeded, task.ErrTimeout},
		{"wrapped deadline is a timeout", fmt.Errorf("query: %w", context.DeadlineExceeded), task.ErrTimeout},
		{"cancellation is a timeout", context.Canceled, task.ErrTimeout},
		{"deadline takes priority over connection capacity", errors.Join(&pgconn.PgError{Code: "53300"}, context.DeadlineExceeded), task.ErrTimeout},
		{"cancellation takes priority over connection capacity", errors.Join(&pgconn.PgError{Code: "53300"}, context.Canceled), task.ErrTimeout},
		{"deadline takes priority over server recovery", errors.Join(&pgconn.PgError{Code: "57P03"}, context.DeadlineExceeded), task.ErrTimeout},
		{"cancellation takes priority over server recovery", errors.Join(&pgconn.PgError{Code: "57P03"}, context.Canceled), task.ErrTimeout},
		{"check violation is invalid input", &pgconn.PgError{Code: "23514"}, task.ErrInvalid},
		{"foreign key violation is invalid input", &pgconn.PgError{Code: "23503"}, task.ErrInvalid},
		{"data exception is invalid input", &pgconn.PgError{Code: "22001"}, task.ErrInvalid},
		{"wrapped integrity error is invalid input", fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23505"}), task.ErrInvalid},
		{"other server errors stay permanent", syntax, nil},
		{"connection failures are unavailable", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, task.ErrUnavailable},
		{"driver connect error is unavailable", driverConnectError(t, syscall.ECONNREFUSED), task.ErrUnavailable},
		{"driver connection deadline is a timeout", driverConnectTimeout(t), task.ErrTimeout},
		{"closed pool is unavailable", puddle.ErrClosedPool, task.ErrUnavailable},
		{"wrapped closed pool is unavailable", fmt.Errorf("acquire: %w", puddle.ErrClosedPool), task.ErrUnavailable},
		{"closed driver connection is unavailable", pgconn.ErrConnClosed, task.ErrUnavailable},
		{"wrapped closed driver connection is unavailable", fmt.Errorf("query: %w", pgconn.ErrConnClosed), task.ErrUnavailable},
		{"closed network is unavailable", net.ErrClosed, task.ErrUnavailable},
		{"EOF is unavailable", io.EOF, task.ErrUnavailable},
		{"unexpected EOF is unavailable", io.ErrUnexpectedEOF, task.ErrUnavailable},
		{"reset connection is unavailable", fmt.Errorf("read: %w", syscall.ECONNRESET), task.ErrUnavailable},
		{"broken pipe is unavailable", fmt.Errorf("write: %w", syscall.EPIPE), task.ErrUnavailable},
		{"unknown failure stays permanent", errors.New("private unexpected Go failure"), nil},
		{"text resembling a connection error stays permanent", errors.New("failed to connect to host: dial error"), nil},
		{"text resembling a closed pool stays permanent", errors.New("closed pool"), nil},
		{"text resembling a closed connection stays permanent", errors.New("query: conn closed"), nil},
		{"closed transaction stays permanent", pgx.ErrTxClosed, nil},
		{"rolled back transaction stays permanent", pgx.ErrTxCommitRollback, nil},
		{"unrelated syscall stays permanent", syscall.EINVAL, nil},
		{"typed network timeout is unavailable", &net.DNSError{Err: "private", IsTimeout: true}, task.ErrUnavailable},
		{"actual driver scan error stays permanent", driverScanError(t), nil},
		{"scan EOF is a decoding failure", pgx.ScanArgError{ColumnIndex: 0, FieldName: "private", Err: io.EOF}, nil},
		{"wrapped scan EOF is a decoding failure", fmt.Errorf("query: %w", &pgx.ScanArgError{ColumnIndex: 0, FieldName: "private", Err: io.ErrUnexpectedEOF}), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.in)
			switch {
			case tc.in == nil:
				if got != nil {
					t.Fatalf("nil in, %v out", got)
				}
			case tc.want == nil:
				if !errors.Is(got, tc.in) || !errors.Is(got, errStorageFailure) || errors.Is(got, task.ErrUnavailable) || errors.Is(got, task.ErrTimeout) || errors.Is(got, task.ErrInvalid) {
					t.Fatalf("permanent error lost its cause or changed classification: %v", got)
				}
			default:
				if !errors.Is(got, tc.want) {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestClassifyPostgresConnectionErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
		want error // nil means no retryable or invalid-input classification
	}{
		{"too many connections", "53300", task.ErrUnavailable},
		{"cannot connect now", "57P03", task.ErrUnavailable},
		{"idle transaction timeout", "25P03", task.ErrTimeout},
		{"transaction timeout", "25P04", task.ErrTimeout},
		{"invalid transaction state", "25000", nil},
		{"no active transaction", "25P01", nil},
		{"failed transaction", "25P02", nil},
		{"invalid password", "28P01", nil},
		{"invalid authorization", "28000", nil},
		{"missing database", "3D000", nil},
		{"configuration limit", "53400", nil},
		{"syntax error", "42601", nil},
		{"invalid SQLSTATE", "secret", nil},
		{"data exception", "22001", task.ErrInvalid},
		{"integrity violation", "23514", task.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pgErr := &pgconn.PgError{Code: tc.code, Message: "password=secret", Detail: "private row"}
			for _, wrapped := range []bool{false, true} {
				t.Run(fmt.Sprintf("driver_wrapped=%t", wrapped), func(t *testing.T) {
					var cause error = pgErr
					if wrapped {
						cause = fmt.Errorf("acquire: %w", driverConnectError(t, pgErr))
					}
					got := classify(cause)
					if tc.want == nil {
						if errors.Is(got, task.ErrUnavailable) || errors.Is(got, task.ErrTimeout) || errors.Is(got, task.ErrInvalid) {
							t.Fatalf("permanent error changed classification: %v", got)
						}
					} else if !errors.Is(got, tc.want) {
						t.Errorf("SQLSTATE %s: got %v, want %v", tc.code, got, tc.want)
					}
					var retained *pgconn.PgError
					if !errors.Is(got, cause) || !errors.As(got, &retained) || retained != pgErr {
						t.Error("original and typed database causes must be retained")
					}
					wantDiagnostic := "SQLSTATE " + tc.code
					if tc.code == "secret" {
						wantDiagnostic = "storage failure"
					}
					if !strings.Contains(got.Error(), wantDiagnostic) || strings.Contains(got.Error(), "secret") || strings.Contains(got.Error(), "private") {
						t.Errorf("unsafe or missing diagnostic: %v", got)
					}
				})
			}
		})
	}
}

type failedStorageAuthenticator struct{ cause error }

func (a failedStorageAuthenticator) Authenticate(context.Context, string) (int64, error) {
	return 0, classify(a.cause)
}

func TestStorageFailureHTTPDiagnostics(t *testing.T) {
	// Substitute only the storage failure. Exercise the real HTTP error mapping
	// and process logger, using the driver's real connection-error wrapper.
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	for _, tc := range []struct {
		name          string
		cause         error
		status        int
		body, reason  string
		driverWrapped bool
	}{
		{"permanent", &pgconn.PgError{Code: "28P01", Message: "secret", Detail: "private"}, 500, "internal error", "SQLSTATE 28P01", true},
		{"retryable", &pgconn.PgError{Code: "53300", Message: "secret", Detail: "private"}, 503, "service unavailable", "SQLSTATE 53300", true},
		{"deadline", fmt.Errorf("secret: %w", context.DeadlineExceeded), 504, "request timed out", "deadline exceeded", true},
		{"driver connection timeout", driverConnectTimeout(t), 504, "request timed out", "deadline exceeded", false},
		{"driver connection refusal", syscall.ECONNREFUSED, 503, "service unavailable", "connection refused", true},
		{"idle transaction timeout", &pgconn.PgError{Code: "25P03", Message: "secret", Detail: "private"}, 504, "request timed out", "SQLSTATE 25P03", true},
		{"transaction timeout", &pgconn.PgError{Code: "25P04", Message: "secret", Detail: "private"}, 504, "request timed out", "SQLSTATE 25P04", true},
		{"failed transaction", &pgconn.PgError{Code: "25P02", Message: "secret", Detail: "private"}, 500, "internal error", "SQLSTATE 25P02", true},
		{"unknown Go failure", errors.New("secret unexpected Go failure with private row"), 500, "internal error", "storage failure", false},
		{"driver scan failure", driverScanError(t), 500, "internal error", "row decoding failed", false},
		{"scan EOF", pgx.ScanArgError{ColumnIndex: 0, FieldName: "private", Err: io.EOF}, 500, "internal error", "row decoding failed", false},
		{"transport disconnect", &net.OpError{Op: "read", Net: "tcp", Err: fmt.Errorf("private: %w", io.ErrUnexpectedEOF)}, 503, "service unavailable", "connection closed", false},
		{"closed pool", fmt.Errorf("secret: %w", puddle.ErrClosedPool), 503, "service unavailable", "pool closed", false},
		{"closed connection", fmt.Errorf("secret: %w", pgconn.ErrConnClosed), 503, "service unavailable", "connection closed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs.Reset()
			cause := tc.cause
			if tc.driverWrapped {
				cause = driverConnectError(t, cause)
			}
			handler := httpapi.New(nil, failedStorageAuthenticator{cause}, false, nil)
			request := httptest.NewRequest(http.MethodGet, "/tasks", nil)
			request.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 64))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.status || response.Body.String() != "{\"error\":\""+tc.body+"\"}\n" {
				t.Fatalf("public response = %d %s", response.Code, response.Body)
			}
			if (response.Header().Get("Retry-After") != "") != (tc.status == 503) {
				t.Fatal("only a retryable failure may include Retry-After")
			}
			if !strings.Contains(logs.String(), tc.reason) {
				t.Fatal("log lost the safe diagnostic")
			}
			for _, sensitive := range []string{"secret", "private", "127.0.0.1", "user=test", "host="} {
				if strings.Contains(logs.String(), sensitive) {
					t.Fatalf("process log exposed synthetic sensitive field %q", sensitive)
				}
			}
		})
	}
}

// Exercise the actual pgx row decoder with an incompatible destination. The
// resulting ScanArgError contains a field name and row value that logs must omit.
func driverScanError(t *testing.T) error {
	t.Helper()
	var value int
	err := pgx.ScanRow(pgtype.NewMap(), []pgconn.FieldDescription{{Name: "private", DataTypeOID: pgtype.TextOID, Format: pgx.TextFormatCode}}, [][]byte{[]byte("secret")}, &value)
	var scanErr pgx.ScanArgError
	if !errors.As(err, &scanErr) {
		t.Fatalf("expected actual pgx ScanArgError, got %v", err)
	}
	return err
}

// Use the driver's actual ConnectError wrapper with a substituted dial boundary.
// This checks driver wrapping without requiring a PostgreSQL server.
func driverConnectError(t *testing.T, cause error) error {
	t.Helper()
	config, err := pgconn.ParseConfig("host=127.0.0.1 user=test password=secret dbname=private sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	config.DialFunc = func(context.Context, string, string) (net.Conn, error) {
		return nil, cause
	}
	_, err = pgconn.ConnectConfig(context.Background(), config)
	var connectErr *pgconn.ConnectError
	if !errors.As(err, &connectErr) || !errors.Is(err, cause) {
		t.Fatalf("expected driver ConnectError preserving the cause, got %v", err)
	}
	return err
}

// Let pgconn create and expire its own connection deadline. The caller's
// context remains live, so the error comes from ConnectTimeout rather than
// an expired request context or a fabricated wrapper.
func driverConnectTimeout(t *testing.T) error {
	t.Helper()
	config, err := pgconn.ParseConfig("host=127.0.0.1 user=test password=secret dbname=private sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	config.ConnectTimeout = 10 * time.Millisecond
	config.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	caller, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = pgconn.ConnectConfig(caller, config)
	var connectErr *pgconn.ConnectError
	if caller.Err() != nil || !errors.As(err, &connectErr) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a driver connection deadline with a live caller, got %v; caller=%v", err, caller.Err())
	}
	return err
}

func TestClassifiedCauseAndSafeDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cause      error
		kind       error
		diagnostic string
	}{
		{"deadline", fmt.Errorf("password=secret: %w", context.DeadlineExceeded), task.ErrTimeout, "deadline exceeded"},
		{"cancelled", fmt.Errorf("token=secret: %w", context.Canceled), task.ErrTimeout, "context canceled"},
		{"refused", &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("password=secret: %w", syscall.ECONNREFUSED)}, task.ErrUnavailable, "connection refused"},
		{"closed", puddle.ErrClosedPool, task.ErrUnavailable, "pool closed"},
		{"unknown", errors.New("postgres://user:secret@host/db task=private"), errStorageFailure, "storage failure"},
		{"scan", driverScanError(t), errStorageFailure, "row decoding failed"},
		{"constraint", &pgconn.PgError{Code: "23514", Message: "secret", Detail: "private"}, task.ErrInvalid, "SQLSTATE 23514"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.cause)
			if !errors.Is(got, tc.kind) || !errors.Is(got, tc.cause) {
				t.Fatalf("lost error kind or original cause: %v", got)
			}
			if !strings.Contains(got.Error(), tc.diagnostic) || strings.Contains(got.Error(), "secret") || strings.Contains(got.Error(), "private") {
				t.Fatalf("unsafe or missing diagnostic: %v", got)
			}
			var pgErr *pgconn.PgError
			if tc.name == "constraint" && (!errors.As(got, &pgErr) || pgErr.Code != "23514") {
				t.Fatal("typed database cause lost")
			}
		})
	}
}

func TestSafeError(t *testing.T) {
	if SafeError(nil) != nil {
		t.Fatal("successful operation became an error")
	}
	for _, tc := range []struct {
		name, code, diagnostic string
	}{
		{"permanent SQLSTATE", "28P01", "SQLSTATE 28P01"},
		{"invalid length", "secret", "storage failure"},
		{"lowercase SQLSTATE", "28p01", "database error"},
		{"control character SQLSTATE", "28\n01", "database error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pgErr := &pgconn.PgError{Code: tc.code, Message: "secret", Detail: "private", Hint: "private"}
			cause := fmt.Errorf("private: %w", driverConnectError(t, pgErr))
			got := SafeError(cause)
			var retained *pgconn.PgError
			if !errors.Is(got, cause) || !errors.As(got, &retained) || retained != pgErr {
				t.Fatal("safe diagnostic lost the original error chain")
			}
			if !strings.Contains(got.Error(), tc.diagnostic) || !strings.Contains(got.Error(), "cause type *fmt.wrapError") {
				t.Fatal("safe diagnostic lost bounded diagnostic metadata")
			}
			for _, sensitive := range []string{"secret", "private", "127.0.0.1", "user=test", "28p01", "28\n01"} {
				if strings.Contains(got.Error(), sensitive) {
					t.Fatalf("diagnostic exposed synthetic sensitive field %q", sensitive)
				}
			}
		})
	}
}
