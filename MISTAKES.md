# MISTAKES.md

Repo-specific, code-level traps. Promotion ratchet: Observations (1 hit) →
Patterns (2) → Enforced Rules (3, stable ID). Increment the hit count when an
entry saves you; promotions never demote.

## Enforced Rules (check every task)

_(none yet)_

## Patterns (promote at 3 hits)

- `gh pr create` from this clone targets the parent `comma-compliance/arc-relay` (this repo is a fork) and fails with "Head sha can't be blank … No commits between" → open PRs with `gh api repos/AndersonTechSolutions/arc-relay/pulls --input -` (or `--repo` + `--head AndersonTechSolutions:<branch>`), and pin `--repo` on `gh pr checks/merge`. (hits: 2)

## Observations (first sightings)

- 2026-09-17: A `store.Open(":memory:", …)` test DB looked empty ("no such table: memory_sessions") from a goroutine → `database/sql` opens a new pool connection per concurrent caller and every new connection to `:memory:` is a *separate* empty SQLite database → any test that exercises goroutines (queue workers, cron, handlers under load) must open a file DB under `t.TempDir()`; `:memory:` is only safe for single-goroutine tests. (hits: 1)
- 2026-09-17: Wrote a migration with `ON CONFLICT(session_id, uuid)` against a *partial* unique index (`WHERE uuid IS NOT NULL`) → SQLite rejects the clause unless the index predicate is repeated or the index is non-partial → keep `idx_memory_messages_session_uuid` non-partial (multiple NULLs are allowed under a unique index) and add a regression test that exercises the conflict clause. (hits: 1)
- 2026-10-24: Ran plain `go test ./...` and got 14 `internal/web` failures "no such module: fts5" → the memory migrations need SQLite FTS5, which is only compiled in with the build tag → run `make test` (or `CGO_ENABLED=1 go test -tags sqlite_fts5 ./...`); same tag for vet/govulncheck/gosec. (hits: 1)
- 2026-10-24: Bumping `golang.org/x/crypto` to v0.56.0 silently raised the `go` directive in go.mod to 1.26.0 → the official `golang:*-alpine` images pin `GOTOOLCHAIN=local`, so a 1.25 builder can't build it → when a dep bump raises the `go` line, bump `Dockerfile` (`golang:X-alpine`) and every `go-version:` in `.github/workflows/*.yml` in the same commit. (hits: 1)
- 2026-10-24: HTTPS `git push` of a branch touching `.github/workflows/*` was rejected ("refusing to allow an OAuth App to create or update workflow … without `workflow` scope") → the gh OAuth token lacks `workflow` scope → push over SSH with the ATECHPCS key from 1Password (see the `github-ssh-push-key` memory). (hits: 1)
