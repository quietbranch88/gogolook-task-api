package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/quietbranch88/gogolook-task-api/internal/httpapi"
	"github.com/quietbranch88/gogolook-task-api/internal/postgres"
	"github.com/quietbranch88/gogolook-task-api/internal/task"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mode := os.Getenv("AUTH_MODE")
	if mode == "" {
		mode = "token"
	}
	if mode != "demo" && mode != "token" {
		return errors.New("AUTH_MODE must be token or demo")
	}
	// Reject a bad command line before touching the database.
	if len(os.Args) > 1 && (len(os.Args) != 2 || (os.Args[1] != "migrate" && os.Args[1] != "seed-demo" && os.Args[1] != "seed-load")) {
		return errors.New("usage: api [migrate|seed-demo|seed-load]")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required, e.g. postgres://user:pass@host:5432/db?sslmode=disable; for a zero-setup demo use: docker compose up --build -d --wait")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// Malformed connection strings are not reliably redacted by the driver.
		return errors.New("invalid database configuration")
	}
	if config.HealthCheckPeriod <= 0 {
		return errors.New("pool_health_check_period must be greater than zero")
	}
	// Ten connections is an initial per-process cap, not a capacity guarantee.
	// Budget all replicas, rolling-deployment overlap and other database clients.
	// MinConns targets total connections, not guaranteed idle connections. The
	// connect timeout bounds establishment attempts; pool waiting uses the caller's
	// context. Lifetime jitter reduces synchronized connection retirement.
	config.MaxConns = 10
	if v := os.Getenv("DB_MAX_CONNS"); v != "" {
		n, convErr := strconv.ParseInt(v, 10, 32)
		if convErr != nil || n < 1 || n > 100 {
			return errors.New("DB_MAX_CONNS must be an integer from 1 to 100")
		}
		config.MaxConns = int32(n)
	}
	config.MinConns = min(2, config.MaxConns)
	config.MaxConnLifetimeJitter = 5 * time.Minute
	config.ConnConfig.ConnectTimeout = 2 * time.Second
	if len(os.Args) == 1 {
		// Bound abandoned API transactions even if the process cannot cancel them.
		// Maintenance commands retain their configured database time budgets.
		config.ConnConfig.RuntimeParams["transaction_timeout"] = "10s"
		config.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "10s"
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("database pool unavailable: %w", postgres.SafeError(err))
	}
	defer pool.Close()
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = pingStartup(startup, pool.Ping); err != nil {
		return fmt.Errorf("database unavailable: %w", postgres.SafeError(err))
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			if err = postgres.Migrate(startup, pool); err != nil {
				return fmt.Errorf("migrate: %w", postgres.SafeError(err))
			}
			log.Printf("schema at version %d", postgres.SchemaVersion)
			return nil
		case "seed-demo":
			if mode != "demo" {
				return errors.New("seed-demo requires AUTH_MODE=demo")
			}
			if err = postgres.SeedDemo(startup, pool); err != nil {
				return fmt.Errorf("seed-demo: %w", postgres.SafeError(err))
			}
			return nil
		case "seed-load":
			return seedLoad(startup, pool)
		}
	}
	if err = postgres.SchemaReady(startup, pool); err != nil {
		return fmt.Errorf("schema not ready, run api migrate: %w", postgres.SafeError(err))
	}
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	if diagAddr := os.Getenv("PPROF_ADDR"); diagAddr != "" {
		// Diagnostics (pprof + expvar pool stats) on a separate listener that must be loopback; never on the API port.
		if host, _, splitErr := net.SplitHostPort(diagAddr); splitErr != nil || !isLoopbackHost(host) {
			return errors.New("PPROF_ADDR must be a loopback host:port such as 127.0.0.1:6060")
		}
		diag := &http.Server{Addr: diagAddr, Handler: diagnosticsHandler(pool.Stat), ReadHeaderTimeout: 3 * time.Second}
		go func() {
			if err := diag.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				log.Print("diagnostics listener failed")
			}
		}()
		defer diag.Close()
	}
	repo := postgres.New(pool)
	// repo serves as both the task Repository and the token Authenticator; the bool selects demo mode.
	server := &http.Server{Addr: addr, Handler: httpapi.New(task.NewService(repo), repo, mode == "demo", pool.Ping), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	log.Print("API starting")
	select {
	case err = <-result:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if err = server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return errors.New("HTTP shutdown timed out")
		}
		return nil
	}
}
