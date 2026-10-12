# Design

I chose the scam-case review setting; the assignment requires only a task API.
Each enterprise is a tenant; membership grants users access to its investigation checks.
Tasks record check completion. Fraud decisions belong outside this API.

## Layout

| Package | Responsibility |
| --- | --- |
| `internal/task` | Define Task, validation, repository interface and service. |
| `internal/httpapi` | Translate HTTP requests and responses. |
| `internal/postgres` | Execute SQL, transactions, migrations and error classification. |
| `cmd/api` | Configure the pool, connect adapters and manage shutdown. |

Both adapters depend on `task`; `task` imports neither HTTP nor PostgreSQL.

## Request path

In token mode, `PUT /tasks/1 {"name":"Review evidence","status":1,"version":1}` follows this path:
1. Authenticate the bearer token through its digest; reject expired or revoked credentials.
2. Select the tenant through `X-Tenant-ID`; treat selection separately from membership.
3. Decode JSON and validate editable fields and version, following [OpenAPI](openapi.yaml).
4. Check membership with `FOR SHARE` inside the transaction; return `403` if absent. Concurrent membership deletion waits until this transaction finishes.
5. Match tenant, task ID and version in one UPDATE; increment version and return the updated row.
6. Distinguish stale versions from absent tenant-scoped tasks; commit before returning success.

## Concurrency

| Mechanism | Guarantees | Boundary |
| --- | --- | --- |
| Tenant-qualified SQL | Restricts task access to the selected tenant. | Enforce membership in the same transaction; retain this rule in future queries. |
| Atomic versioned PUT | Allows one winner for the same observed version. | Reconcile conflicts; DELETE remains unconditional. |
| Scoped POST key | Creates one task for matching user, tenant, key and input. | Replay returns current fields; changed input or deleted tasks conflict. |
| Reservation and task transaction | Commits both records together. | A lost acknowledgment can hide a committed write. |
| Membership `FOR SHARE` | Orders membership revocation after an authorized operation. | Token revocation leaves already-authenticated requests running. |

## Schema

| Table | Purpose |
| --- | --- |
| `tenants` | Own task data and tenant names. |
| `users` | Represent authenticated identities. |
| `memberships` | Grant a user access to a tenant. |
| `api_keys` | Store token digests, expiry and revocation. |
| `tasks` | Store editable fields, version and database-owned `NOT NULL` creation/update timestamps. |
| `task_create_keys` | Retain key/input digests and references; task deletion leaves a null-reference tombstone. |
| `schema_migrations` | Record the initial schema, version `1`. |

PostgreSQL defaults set creation times; triggers timestamp changes to tasks, tenants and API keys.
Successful PUT preserves creation time and updates the modification time with each version increment.
The migration command applies `001_schema.sql` atomically under an advisory lock; reruns preserve existing rows.
It rejects other ledger versions without mutation. Use a fresh database; startup checks the connection and ledger.

## Error mapping

See [OpenAPI](openapi.yaml) for response codes.
Connection failures and temporary database rejection return `503` with `Retry-After: 1`; timeouts return `504`.
Unknown failures, including row-decoding errors, return `500` without retry advice.

Public errors contain fixed messages. Normal application logs omit SQL, DSNs, tokens and case contents.
Storage diagnostics retain SQLSTATE and cause type; requests are not body-logged.

## Verified

| Check | Environment | Result |
| --- | --- | --- |
| Full unit and PostgreSQL suite, `-race` | Go 1.26.9/Linux ARM64, PostgreSQL 17.11, OrbStack; runner 2 CPUs/2 GiB | 58 top-level passes, no failures or race reports; one recovery skip. |
| Real recovery refusal | Disposable standby, race detector | Pass; no skip with recovery opt-in. |
| Delayed startup handshake | PostgreSQL behind a TCP proxy, race detector | Fails before the fix; passes afterward, including repeated focused runs. |
| Migration | PostgreSQL, race detector | Pass: fresh/repeated/concurrent runs, rollback/retry, required timestamps and incompatible-version rejection. |
| Connection and response failures | HTTP and real PostgreSQL | Pass: connection-limit recovery, lost responses, write timeouts and cuts before/after COMMIT. |
| Sensitive diagnostics | Unit and real PostgreSQL tests | Pass: public errors and normal storage logs redact credentials and database details. |
| Static and hook checks | Current checkout | Normal/loadtest vet pass; hook regression 5/5 pass. |
| Image build and HTTP smoke | Final Dockerfile image, isolated PostgreSQL | Build pass; 11 HTTP checks pass, including CRUD, replay, stale version and tombstones. |

The full run used `go test -race -count=1 -timeout=240s -v ./...`.
Reproduce it through `docker compose --profile test run --rm tests`; the separate recovery fixture needs explicit opt-in.
Enable recovery with `RECOVERY_57P03_DATABASE_URL` and `RECOVERY_57P03_ALLOW_DATABASE=tasks_test` against a disposable standby.
Run `go test -race -count=1 ./internal/integration -run '^TestHTTPPostgresRealRecoveryRefusal$'` with `hot_standby=off`.

## Load test

See [loadtest/README.md](../loadtest/README.md) for the workload.
These earlier Windows/WSL2 runs shared one host across API, PostgreSQL and k6; task counts do not establish distinct-case counts.

| Fixture | Value |
| --- | --- |
| Enterprises / test identities | 20 / 20; one identity per enterprise |
| Initial tasks per enterprise / total | 3,000 / 60,000 |
| Offered iterations per second / duration | 50 / 60 seconds |
| Trials per pool cap | 3 |

| Pool cap | Median run p95, ms | Total drops (3 trials) | Failed checks / HTTP failures |
| --- | --- | --- | --- |
| 5 | 201.93 | 32 | 0 / 0% |
| 10 | 159.95 | 3 | 0 / 0% |
| 20 | 160.51 | 3 | 0 / 0% |

Five of nine trials missed the zero-drop gate. Pool 10 is provisional; remeasure the current build before setting capacity.

## Trade-offs

- PostgreSQL adds a database process to run and maintain. Compose handles local setup.
- Required versions prevent concurrent overwrites. Clients must reconcile conflicts.
- Creation-key tombstones protect delayed retries. They need a retention policy before automatic cleanup.
- Cursor paging bounds list cost. Each page reads live data rather than a snapshot.
- Startup retries cover transient probes. Permanent errors fail immediately; application writes are not retried.

| Runtime budget | Setting |
| --- | --- |
| Connection attempt / startup probes | 2 seconds / at most 3 probes within the 30-second startup context |
| Task request / API database transaction | 5 seconds / 10 seconds |
| Shutdown / Compose grace | 10 seconds / 20 seconds |
| Pool connections | Default 10; configured range 1-100 |
| DSN precedence | The program overrides pool max/min, connection timeout and lifetime jitter; `DB_MAX_CONNS` selects the maximum. |
| Transaction timeout overrides | API sets transaction and idle-in-transaction limits to 10 seconds; migrate/seed commands retain DSN values. |

## Future notifications

PostgreSQL also supports the planned transactional outbox: a task change and its event can commit atomically.
A future worker would retry enterprise or downstream delivery; consumers would deduplicate by event identity.
The outbox table and worker are not implemented.

## Known limits

- Paging can miss lower IDs committed after the cursor advances; refresh from the beginning to see them.
- Unkeyed POST retries can create duplicates; timeouts and disconnects can leave commit outcomes uncertain.
- Token provisioning is manual; signup and role management are absent.
- Demo mode uses one shared identity and rejects non-empty `Authorization` or `X-Tenant-ID` headers.
- TLS, rate limiting, backups and retention need operational work; failover and restoration remain unverified.
