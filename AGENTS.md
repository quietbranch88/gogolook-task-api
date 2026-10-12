# Working in this repository

- Build and test: `go test ./...`, `go vet ./...`. Database tests need `TEST_DATABASE_URL` pointing at a database named `tasks_test` on localhost plus `TEST_ALLOW_DATABASE=tasks_test` (an unset `TEST_DATABASE_URL` skips them; a URL without that guard or naming another database fails fast), or run `docker compose --profile test run --rm tests`. The connection-limit fixture requires a disposable test superuser to create/drop its isolated role; Compose provides one.
- Dependency direction: `internal/httpapi` and `internal/postgres` both import `internal/task`; `internal/task` imports neither; `cmd/api` imports all three.
- The contract is `docs/openapi.yaml`; change it in the same commit as the behaviour. `docs/DESIGN.md` says what the exercise requires versus what was added.
- Insert tasks with the trusted, membership-authorized tenant ID. Qualify task reads, updates and deletes by that tenant; a globally unique task ID is never sufficient authorization.
