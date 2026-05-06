# Skill Update Tracking

Arc Relay can track opt-in upstream sources for skills you publish, detect
drift via daily cron + on-demand checks, and classify drift severity via
an LLM (with offline fallback).

## Why

Skills published through Arc Relay frequently mirror community repositories
(e.g. a forked Claude Code skill, a vendored helper from another project).
Upstream commits land between releases — bug fixes, security patches, breaking
changes — and there's no built-in way to know your published bundle has fallen
behind. Manually diffing every skill against its source each week doesn't scale.

Upstream tracking solves this with three pieces:
- A per-skill `skill_upstreams` row that records `git url + subpath + ref`.
- A daily cron + on-demand endpoint that fetches the upstream, runs cheap
  two-stage drift detection, and persists a `DriftReport` when content actually
  changed.
- An LLM classifier that labels drift `cosmetic` / `minor` / `major` /
  `security` so operators can triage at a glance.

The feature is opt-in per-skill: skills without a declared upstream pay nothing
and never appear in the cron's working set.

## Publishing with upstream tracking

Declare the upstream in your skill directory.

`<skill-dir>/.arc-sync/upstream.toml`:

```toml
[upstream]
type    = "git"             # only "git" supported today
url     = "https://github.com/example/skill-repo"
subpath = "skills/foo"      # path within the repo to track (empty = whole repo)
ref     = "main"            # branch, tag, or sha
```

Then push as usual:

```bash
arc-sync skill push <skill-dir> --version 0.1.0
```

Or override the sidecar with CLI flags:

```bash
arc-sync skill push <skill-dir> --version 0.1.0 \
  --upstream-git https://github.com/example/skill-repo \
  --upstream-path skills/foo \
  --upstream-ref main
```

To disable tracking on a previously-tracked skill:

```bash
arc-sync skill push <skill-dir> --version 0.2.0 --no-upstream
```

Each successful push clears any open drift state for the skill — the relay
re-records the published archive's `last_seen_hash` so the next cron cycle
compares against the new baseline.

## Changing upstream tracking after the fact

You don't need to bump a skill's version (or re-upload the archive) to fix
a typo'd ref, retarget a moved repo, enable tracking on a skill that was
pushed `--no-upstream`, or stop tracking entirely. Three interchangeable
surfaces all hit the same `skill_upstreams` row:

### CLI

```bash
# Add or replace the row in place (any combination of flags can change)
arc-sync skill set-upstream my-skill \
    --git-url https://github.com/example/repo \
    --path skills/my-skill \
    --ref main

# Stop tracking (also clears the stale `outdated` flag)
arc-sync skill clear-upstream my-skill
```

### HTTP API

```bash
PUT /api/skills/{slug}/upstream
{
  "type": "git",                         # optional, defaults to "git"
  "git_url": "https://github.com/...",   # required, non-empty
  "git_subpath": "skills/my-skill",      # optional, "" = repo root
  "git_ref": "main"                      # optional, defaults to "HEAD"
}

DELETE /api/skills/{slug}/upstream       # no body
```

Both verbs require admin role **or** an API key with the `skills:write`
capability — same gate as `POST /api/skills/{slug}/versions/{version}`.
Validation errors return `400`, missing slug `404`, wrong content type
`415`, missing/invalid auth `401/403`.

### Dashboard

On `/skills/{slug}`, the **Update tracking** card has:

- **No upstream configured** → an inline form to set git URL, subpath, and ref
- **Upstream configured** → a collapsed `Edit upstream tracking` form prefilled
  with the current values, plus a **Clear tracking** button (confirm-gated)

Form posts go to `POST /skills/{slug}/upstream` (CSRF-checked, admin-only).

### Semantics

PUT preserves `last_seen_*` and `drift_*` from any prior row — replacing
the pointer doesn't invalidate the prior check. The next cron run resolves
whether the new ref/path is in sync. Setting upstream on a brand-new row
leaves those columns NULL until the first cron tick.

DELETE is atomic: the `skill_upstreams` row is removed and `skills.outdated`
is cleared in the same transaction. With no upstream to compare against,
the "outdated" flag has no referent — leaving it set would be misleading.

DELETE does **not** delete the skill itself or any of its versions.

## How drift detection works

Two paths trigger a check:

1. **Daily cron** (configurable via `[skills.checker]` interval): iterates
   every skill with declared upstream tracking, fetches the configured ref,
   runs cheap two-stage detection (commit log filter + deterministic content
   hash), and only invokes the LLM when real drift is found.

2. **On-demand**: `arc-sync skill check-updates <slug>` (admin-only) runs the
   same flow synchronously and prints the result.

The two-stage detection skips three categories of "non-drift":

- `NoMovement`: upstream HEAD didn't move since the last check.
- `NoPathTouch`: HEAD moved but no commits touched the tracked subpath.
- `RevertedToSame`: subpath changed and reverted to byte-identical content.

Only the fourth outcome — actual content drift — triggers the LLM
classification step.

## Severity rubric

The LLM (default `gpt-4o-mini`) classifies drift as one of:

- **`cosmetic`** — typo fixes, formatting, comment-only edits
- **`minor`** — documentation tweaks, non-functional refactors, small clarifications
- **`major`** — behavior changes, new features, breaking changes
- **`security`** — vulnerability fixes, auth-related changes, credential handling
- **`unknown`** — ambiguous or insufficient context (also the offline-fallback severity)

The LLM also produces a one-line summary and a recommended action. When
`ARC_RELAY_LLM_API_KEY` is unset, the relay falls back to a deterministic
description (e.g. `"3 commits modified 5 files. See diff summary for details."`)
and `severity=unknown`. The cron loop never fails noisily on transient LLM
outages — drift is recorded with the offline triple, operators see the row,
and the next cycle retries.

## Reading drift output

Via `arc-sync skill list --remote`, outdated skills show
`outdated · <severity>` in the STATUS column.

Via `arc-sync skill check-updates <slug>`:

- `up-to-date` (HTTP 204) — no drift detected
- `outdated · <severity>: <summary>` (HTTP 200) — drift detected; the
  recommended action prints below
- `skill not found` (HTTP 404)
- `no upstream tracking configured` (HTTP 409)
- `upstream fetch failed` (HTTP 502)

Via `GET /api/skills/<slug>` (admin), an outdated skill includes a `drift`
JSON block with the full report:

```json
{
  "drift": {
    "severity": "minor",
    "summary": "Documentation reformatting and a typo fix in the README.",
    "recommended_action": "Pull when convenient; no behavior changes.",
    "commits_ahead": 2,
    "upstream_sha": "abc123…",
    "upstream_hash": "sha256:…",
    "detected_at": "2026-04-30T12:00:00Z",
    "relay_version": "0.0.16",
    "relay_hash": "sha256:…",
    "llm_model": "gpt-4o-mini"
  }
}
```

## Clearing drift

Each successful skill push clears the drift state for that skill. The relay
re-records `last_seen_hash` from the newly published archive, so the next
cron cycle compares against the new baseline.

Yanking a version (without publishing a new one) does not clear drift — the
underlying tracked content hasn't changed, so the report stays accurate.

## Operator configuration

`config.toml`:

```toml
[skills.checker]
enabled = true
interval = "24h"
upstream_cache_dir = "/var/lib/arc-relay/upstream-cache"
git_clone_timeout = "60s"
llm_diff_max_bytes = 32768
llm_per_file_max_bytes = 4096
# llm_model = "gpt-4o-mini"  # uncomment to override the relay's default model
```

### Field reference

| Field | Default | Purpose |
|---|---|---|
| `enabled` | `false` | Turn the cron + on-demand path on. The feature is opt-in to keep the cron's working set empty for deployments that don't use upstream tracking. |
| `interval` | `24h` | Cron interval. The plan's design assumes a daily cycle; shorter values incur more git fetches and LLM calls. |
| `upstream_cache_dir` | `<data_dir>/upstream-cache` | Where the daemon clones upstream repos. Each skill gets a stable subdirectory keyed by upstream URL. |
| `git_clone_timeout` | `60s` | Per-clone timeout. Increase for large repos or slow upstreams. |
| `llm_model` | `""` (falls back to shared `llm.Client` default) | Optional per-checker LLM override. Empty uses whatever model the relay's shared `[llm]` config selects (which itself defaults to `gpt-4o-mini`). |
| `llm_diff_max_bytes` | `32768` | Maximum bytes of `git diff --stat` output sent to the LLM per check. Keeps prompts bounded on skills with hundreds of changed files. |
| `llm_per_file_max_bytes` | `4096` | Reserved for future per-file truncation in the LLM prompt builder. **Currently unused** — the LLM only sees the pre-truncated diff summary. The field is exposed today so operators don't have to rotate config when a future build wires it up. |

### Environment variable overrides

- `ARC_RELAY_SKILLS_CHECKER_ENABLED` — `1`/`true`/`yes`/`on` to enable; `0`/`false`/`no`/`off` to disable
- `ARC_RELAY_SKILLS_CHECKER_INTERVAL` — duration string (e.g. `24h`, `12h30m`)
- `ARC_RELAY_SKILLS_CHECKER_UPSTREAM_CACHE_DIR` — path
- `ARC_RELAY_LLM_API_KEY` — OpenAI API key (shared with the tool optimizer; same key)

Env overrides apply before defaults, so a parseable override of zero/empty
falls through to the default rather than being preserved.

### Prometheus metrics

The checker exposes:

- `arc_relay_skill_checks_total{result}` — counter of checks per outcome
  (`no_movement`, `no_path_touch`, `reverted_to_same`, `drift`, `error`)
- `arc_relay_skill_check_duration_seconds` — histogram of check durations

These register against the default Prometheus registry. Wire `/metrics` via
`promhttp.Handler()` in your server setup if you don't already have a metrics
endpoint.

## Troubleshooting

**Drift never clears after a push.** Check that the published archive's
subtree hash matches the current upstream — if you published an unrelated
version, the relay correctly still flags drift against `ref`. Either pull the
upstream into your skill dir before the next push, or yank+republish from a
clean checkout of the upstream.

**`upstream fetch failed` (HTTP 502) on every check.** Most often a network
or auth issue: the relay's git client uses the host's default credentials
helper. For private repos, configure the relay's container with a
`GIT_ASKPASS` or `GIT_SSH_COMMAND` that resolves credentials at fetch time.

**LLM consistently returns `unknown`.** The classifier downgrades to
`unknown` when the response is off-rubric, when `ARC_RELAY_LLM_API_KEY` is
unset, or when the LLM call errors. Check the relay logs for `classify: LLM`
warnings — the underlying cause prints alongside.
