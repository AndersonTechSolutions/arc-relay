# MISTAKES.md

Repo-specific, code-level traps. Promotion ratchet: Observations (1 hit) →
Patterns (2) → Enforced Rules (3, stable ID). Increment the hit count when an
entry saves you; promotions never demote.

## Enforced Rules (check every task)

_(none yet)_

## Patterns (promote at 3 hits)

_(none yet)_

## Observations (first sightings)

- 2026-09-17: A `store.Open(":memory:", …)` test DB looked empty ("no such table: memory_sessions") from a goroutine → `database/sql` opens a new pool connection per concurrent caller and every new connection to `:memory:` is a *separate* empty SQLite database → any test that exercises goroutines (queue workers, cron, handlers under load) must open a file DB under `t.TempDir()`; `:memory:` is only safe for single-goroutine tests. (hits: 1)
- 2026-09-17: `gh pr create --base main --head <branch>` from a clone of this repo failed with "Head sha can't be blank … No commits between main and <branch>" although the branch was pushed → this repo is a fork of `comma-compliance/arc-relay`, so `gh` resolves the PR target to the parent unless told otherwise → open PRs with `gh api repos/AndersonTechSolutions/arc-relay/pulls --input -` (or pass `--repo` and `--head AndersonTechSolutions:<branch>`), and pin `--repo` on `gh pr checks/merge`. (hits: 1)
- 2026-09-17: Wrote a migration with `ON CONFLICT(session_id, uuid)` against a *partial* unique index (`WHERE uuid IS NOT NULL`) → SQLite rejects the clause unless the index predicate is repeated or the index is non-partial → keep `idx_memory_messages_session_uuid` non-partial (multiple NULLs are allowed under a unique index) and add a regression test that exercises the conflict clause. (hits: 1)
