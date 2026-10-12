# Gogolook task API

[Browse the repository](https://github.com/quietbranch88/gogolook-task-api): this Go REST API implements the take-home's four task endpoints.
I chose investigation checks for a scam-case review platform as the setting; task status records check completion.

> **Reviewer note:** PUT requires the current `version` returned by POST/GET to prevent concurrent overwrites.
> Sending only `name` and `status` returns `400`; use the PUT example below.

## Run

Install Docker with Compose v2 and BuildKit.
Compose starts four services: PostgreSQL 17.11, migration, demo seed and API; **no Bearer token is needed**.
Omit `Authorization` and `X-Tenant-ID`; running the image separately requires PostgreSQL 17+.
```sh
docker compose up --build -d --wait
curl -i http://localhost:8080/readyz # Continue when this returns 200.
curl -i -X POST http://localhost:8080/tasks -H 'Content-Type: application/json' -d '{"name":"Check suspicious phone number","status":0}'
curl -i http://localhost:8080/tasks
TASK_ID=1 # Replace with the ID returned by POST.
# version comes from the POST/GET response
curl -i -X PUT "http://localhost:8080/tasks/$TASK_ID" -H 'Content-Type: application/json' -d '{"name":"Complete evidence review","status":1,"version":1}'
curl -i -X DELETE "http://localhost:8080/tasks/$TASK_ID"
docker compose down # Keep data; add -v only to delete demo data.
```
Use a fresh database for the initial schema; see [schema](docs/DESIGN.md#schema) before reusing an existing database.
Test token mode with the [synthetic fixture](loadtest/README.md#token-mode); `seed-load` requires `-tags loadtest`.

## Test

Install Go 1.26.9 for local commands. Compose supplies its own Go toolchain and runs race tests, not vet.
The real-recovery test skips without a separate standby fixture; see [verification](docs/DESIGN.md#verified).

```sh
go test ./... # Database tests skip without explicit opt-in.
go vet ./...
docker compose --profile test run --rm tests # Real PostgreSQL and race detector.
```

## Contract

[OpenAPI](docs/openapi.yaml) defines the authoritative schemas and responses.
Keep the encoded JSON body within 48 KiB. Missing/non-JSON Content-Type returns `415`; unknown fields return `400`.

| Field | Rule |
| --- | --- |
| `name` | Required string; 1–1000 code points; include a non-whitespace character. |
| `status` | Required JSON integer: `0` incomplete, `1` complete. |
| `detail` | Optional string; up to 10000 code points; absent/null means empty; PUT omission clears it. |
| `version` | Generated on creation; supply the current positive integer on PUT. |

Stale PUT versions and conflicting creation keys return `409`.

## What I added, and why

| Addition | Reason |
| --- | --- |
| PostgreSQL | Persist checks across restarts and enforce CHECK constraints. |
| Tenant membership and tokens | Separate investigation teams' data; Compose uses one shared demo identity. |
| PUT version | Detect concurrent overwrites and return conflicts. |
| POST Idempotency-Key | Reuse a key and matching input to retry one creation. |
| Paging | Bound GET to 100 rows; follow `X-Next-After`. |
| Timestamps | Record creation and update times on the server. |

I chose optimistic locking because investigators may update the same task concurrently.
I expect that risk to increase as more people join.
The current load-test target is a first-version baseline; future workloads and design trade-offs can be discussed in the interview.

## Not done

This implementation omits an in-memory adapter, GET by ID, roles, rate limiting and a metrics exporter.
Case records, evidence storage, fraud decisions and notifications remain future work.

See [Design](docs/DESIGN.md) for decisions and verification, and [AI usage](docs/AI_USAGE.md) for how the work was produced.
