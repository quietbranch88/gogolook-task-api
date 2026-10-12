package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The real driver must time out a PostgreSQL handshake and then reach the
// schema guard through a fresh connection. Keeping a healthy connection around
// would let a pool retry pass without ever exercising the failed startup path.
func TestEntrypointRecoversAfterTransientHandshakeTimeout(t *testing.T) {
	entrypointEnvironment(t)
	isolatedEntrypointDB(t)
	u, err := url.Parse(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	upstream := net.JoinHostPort(u.Hostname(), u.Port())
	if u.Port() == "" {
		upstream = net.JoinHostPort(u.Hostname(), "5432")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var stalled, forwarded atomic.Int32
	var workers sync.WaitGroup
	// Every connection accepted during this interval remains silent until the
	// client's unchanged 2s connection deadline closes it. This also covers the
	// pool's background warmup winning the race with the foreground Ping.
	faultUntil := time.Now().Add(3 * time.Second)
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			client, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			stall := time.Now().Before(faultUntil)
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer client.Close()
				_ = client.SetDeadline(time.Now().Add(10 * time.Second))
				if stall {
					stalled.Add(1)
					_, _ = io.Copy(io.Discard, client)
					return
				}
				server, dialErr := net.DialTimeout("tcp", upstream, time.Second)
				if dialErr != nil {
					return
				}
				defer server.Close()
				forwarded.Add(1)
				done := make(chan struct{})
				go func() {
					_, _ = io.Copy(server, client)
					_ = server.Close()
					close(done)
				}()
				_, _ = io.Copy(client, server)
				_ = client.Close()
				<-done
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		workers.Wait()
	})
	u.Host = listener.Addr().String()
	t.Setenv("DATABASE_URL", u.String())
	t.Setenv("DB_MAX_CONNS", "1")
	err = run()
	if err == nil || !strings.Contains(err.Error(), "schema not ready, run api migrate") {
		t.Fatalf("startup did not recover from the temporary handshake stall: %v", err)
	}
	if stalled.Load() < 1 || forwarded.Load() < 1 {
		t.Fatalf("test must exercise both stalled and recovered connections: stalled=%d forwarded=%d", stalled.Load(), forwarded.Load())
	}
	t.Logf("driver timed out %d stalled connection(s), then schema guard observed PostgreSQL through %d forwarded connection(s)", stalled.Load(), forwarded.Load())
}

func TestStartupPingDoesNotRetryPermanentErrors(t *testing.T) {
	for _, code := range []string{"28P01", "28000", "3D000", "53400", "42501"} {
		t.Run(code, func(t *testing.T) {
			failure := &pgconn.PgError{Code: code, Message: "synthetic private database message"}
			calls := 0
			err := pingStartup(t.Context(), func(context.Context) error {
				calls++
				return failure
			})
			if err != failure || calls != 1 {
				t.Fatalf("permanent startup failure must preserve cause and stop immediately: calls=%d err=%v", calls, err)
			}
		})
	}
	failure := errors.New("unclassified connection configuration failure")
	calls := 0
	err := pingStartup(t.Context(), func(context.Context) error {
		calls++
		return failure
	})
	if err != failure || calls != 1 {
		t.Fatalf("unclassified startup failure must not be retried: calls=%d err=%v", calls, err)
	}
}

func TestStartupPingRecoversFromTemporaryDatabaseRejection(t *testing.T) {
	for _, code := range []string{"53300", "57P03"} {
		t.Run(code, func(t *testing.T) {
			calls := 0
			err := pingStartup(t.Context(), func(context.Context) error {
				calls++
				if calls == 1 {
					return &pgconn.PgError{Code: code}
				}
				return nil
			})
			if err != nil || calls != 2 {
				t.Fatalf("temporary rejection must permit a fresh startup probe: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestStartupPingBoundsPersistentFailure(t *testing.T) {
	failure := &pgconn.PgError{Code: "57P03"}
	calls := 0
	err := pingStartup(t.Context(), func(context.Context) error {
		calls++
		return failure
	})
	if err != failure || calls != 3 {
		t.Fatalf("unavailable database must stop after three probes and preserve its final cause: calls=%d err=%v", calls, err)
	}
}

func TestStartupPingHonorsCancellation(t *testing.T) {
	t.Run("before connection", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		calls := 0
		err := pingStartup(ctx, func(context.Context) error {
			calls++
			return nil
		})
		if !errors.Is(err, context.Canceled) || calls != 0 {
			t.Fatalf("canceled startup must not attempt a connection: calls=%d err=%v", calls, err)
		}
	})
	t.Run("during backoff", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		calls := 0
		err := pingStartup(ctx, func(context.Context) error {
			calls++
			return &pgconn.PgError{Code: "57P03"}
		})
		if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
			t.Fatalf("startup deadline must interrupt backoff before another probe: calls=%d err=%v", calls, err)
		}
	})
}
