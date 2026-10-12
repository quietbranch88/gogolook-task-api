# AI usage

I chose investigation-check tracking as the setting; the assignment requires only a task API.
Task status records check completion. The required PUT version is my explicit contract extension.

## What I decided

- Kept tasks focused on investigation checks. Deferred case records, evidence storage and fraud decisions.
- Modeled enterprises as tenants and used membership for access. Kept individual task assignment separate.
- Chose PostgreSQL for transactions and constraints, including future atomic task/outbox writes. Left notification delivery unimplemented.
- Required PUT versions to prevent overwrites; added POST keys for safe retries. Kept DELETE unconditional.
- Bounded pages to 100. Used measured fixture sizes and deferred an in-memory adapter rather than expanding scope.

## What review caught and fixed

- Replaced broad `503` mapping with connection, timeout and unexpected-error categories; unknown and row-decoding failures now return `500`.
- Required creation/update timestamps in the fresh baseline; removed unsupported legacy-null handling.
- Preserved diagnostic causes while hiding DSNs, tokens and database details from public responses and normal logs.
- Checked PostgreSQL readiness over TCP to avoid its temporary initialization server.
- Bound the container API to all interfaces so published ports work.
- Required JSON content type; rejected unknown fields, invalid UTF-8 and malformed paging values.
- Moved credential-generating load fixtures behind the `loadtest` build tag.
- Fixed pre-commit launcher selection and staged-snapshot checks after regressions exposed failures.
- Fixed load gates so failed endpoint checks cause a nonzero exit.

## Tools

Codex drafted implementation, tests and documentation and ran the checks; Claude reviewed.
I selected scope, reviewed results and approved changes.

## Limits

I did not track human effort precisely or build a comparable version without AI, so no time saving is measured.
