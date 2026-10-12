package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDiagnosticsHandlerServesPprofAndPoolStats(t *testing.T) {
	const helperEnv = "TASKS_DIAGNOSTICS_TEST_HELPER"
	if os.Getenv(helperEnv) != "1" {
		// expvar registration intentionally happens once per API process. Give
		// every test invocation a fresh process, including repeated (-count) runs.
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable,
			"-test.run=^TestDiagnosticsHandlerServesPprofAndPoolStats$", "-test.count=1", "-test.timeout=20s")
		for _, env := range os.Environ() {
			key, _, _ := strings.Cut(env, "=")
			if !strings.EqualFold(key, helperEnv) {
				cmd.Env = append(cmd.Env, env)
			}
		}
		cmd.Env = append(cmd.Env, helperEnv+"=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("diagnostics subprocess failed: %v (context: %v)\n%s", err, ctx.Err(), output)
		}
		return
	}

	// A pool that never connects still reports statistics, which is all the handler needs.
	// Port 1 is never listening and MinConns is 0, so no connection is attempted.
	config, err := pgxpool.ParseConfig("postgres://u:p@127.0.0.1:1/db?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 7
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	h := diagnosticsHandler(pool.Stat)
	for _, path := range []string{"/debug/pprof/", "/debug/vars"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/debug/vars", nil))
	body := w.Body.String()
	// expvar prints the top-level key with a space and the Func's JSON compact.
	for _, want := range []string{`"db_pool": {`, `"max_conns":7`, `"empty_acquire_count":0`, `"acquire_duration_total_ms":0`} {
		if !strings.Contains(body, want) {
			t.Fatalf("expvar body lacks %s: %s", want, body)
		}
	}
	// The diagnostics mux must not answer application routes.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("diagnostics mux served /tasks: %d", w.Code)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true, "127.5.5.5": true,
		"": false, "0.0.0.0": false, "::": false, "10.0.0.5": false, "example.com": false} {
		if got := isLoopbackHost(host); got != want {
			t.Fatalf("isLoopbackHost(%q)=%v want %v", host, got, want)
		}
	}
}
