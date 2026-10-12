# Load test

`k6.js` runs mixed task traffic with fixed-rate, tenant-skew and exploratory ramp scenarios. You need [k6](https://k6.io), Python and jq, and a running API in token mode with the load fixture. Measurements and environment details are in [Design](../docs/DESIGN.md#load-test).

```sh
export DATABASE_URL=postgres://user:pass@127.0.0.1:5432/tasks?sslmode=disable   # a local database you own
go run ./cmd/api migrate
go run -tags loadtest ./cmd/api seed-load > loadtest/tokens.jsonl   # 20 tenants x 3000 tasks, one token each
python3 -c "import json,sys; print(json.dumps([json.loads(l) for l in open('loadtest/tokens.jsonl')]))" > loadtest/tokens.json
PPROF_ADDR=127.0.0.1:6060 go run ./cmd/api &                        # token mode; diagnostics for pool stats and profiles
cd loadtest
k6 run -e MODE=slo -e LIMIT=100 k6.js        # 50 iterations/s for 60 s, paged list
k6 run -e MODE=slo k6.js                     # default-page comparison: omitted LIMIT means 100 rows
k6 run -e MODE=slo -e LIMIT=100 -e SKEW=1 k6.js   # half the traffic on one tenant
k6 run -e MODE=ceiling -e LIMIT=100 k6.js    # exploratory ramp from 200 to 2000 iterations/s
```

Pool statistics during a run: `curl -s 127.0.0.1:6060/debug/vars | jq .db_pool`. A CPU profile: `curl -o cpu.pprof '127.0.0.1:6060/debug/pprof/profile?seconds=30'`, then `go tool pprof -top cpu.pprof`.

`tokens.json` and `tokens.jsonl` contain credentials for the local fixture; both match the `.gitignore` pattern `loadtest/tokens.json*`.

In `MODE=slo`, all endpoint checks must pass, with zero HTTP failures and zero dropped iterations.
`MODE=ceiling` is exploratory and has no pass/fail thresholds. Both modes report latency percentiles.

## Acceptance-gate regression

Run `python3 loadtest/test_gate.py --output /path/to/scratch/gate-results` with k6 installed.
Exercise valid CRUD and invalid responses against a loopback stub; inspect its logs and exit codes.
Check endpoint assertions and thresholds; use a real API run for workload measurements.
Give each VU a distinct PUT target and carry its version; setup fails when fixtures are insufficient.

The API uses default/max 100 paging. `LIMIT=""` tests the default page. Follow
`X-Next-After` outside the latency workload to exercise page traversal. This
workload deliberately omits Idempotency-Key to measure distinct writes; retry
semantics are tested against real PostgreSQL in the integration suite.
