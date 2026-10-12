package main

import (
	"expvar"
	"net"
	"net/http"
	"net/http/pprof"

	"github.com/jackc/pgx/v5/pgxpool"
)

// isLoopbackHost accepts only addresses that cannot be reached from another machine.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// diagnosticsHandler serves pprof and expvar on a mux that is never mounted on
// the API listener. It is only started when PPROF_ADDR is set, and that address
// is checked to be loopback. Call it once per process: expvar.Publish panics on a
// second registration, which is preferable to silently reporting a stale pool.
// Compare interval deltas with acquired/idle samples and cancellations. Acquire
// duration is cumulative for successful acquisitions, not current queue latency;
// increasing counters alone do not establish pool saturation.
func diagnosticsHandler(stats func() *pgxpool.Stat) http.Handler {
	expvar.Publish("db_pool", expvar.Func(func() any {
		s := stats()
		return map[string]int64{
			"max_conns":                 int64(s.MaxConns()),
			"total_conns":               int64(s.TotalConns()),
			"idle_conns":                int64(s.IdleConns()),
			"acquired_conns":            int64(s.AcquiredConns()),
			"constructing_conns":        int64(s.ConstructingConns()),
			"acquire_count":             s.AcquireCount(),
			"empty_acquire_count":       s.EmptyAcquireCount(),
			"canceled_acquire_count":    s.CanceledAcquireCount(),
			"acquire_duration_total_ms": s.AcquireDuration().Milliseconds(), // sum over all acquires, not a current latency
			"new_conns_count":           s.NewConnsCount(),
			"max_lifetime_destroys":     s.MaxLifetimeDestroyCount(),
			"max_idle_destroys":         s.MaxIdleDestroyCount(),
		}
	}))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	mux.Handle("GET /debug/vars", expvar.Handler())
	return mux
}
