# Changelog

All notable changes to Arc Relay (formerly MCP Wrangler) are documented here.

## [Unreleased]

### Added
- **Memory dashboard Tier 1 UX upgrade** - `/memory` landing replaced the flat 5-row teaser with a project-clustered card grid; sessions list gained 25/page pagination + project filter; session detail gained a rich header (msg count / total chars / platform / last-seen) plus collapsible `<details>` for messages over 2,000 chars; search gained project + role + since-epoch filters and groups results by session. Epoch timestamps render as relative ("3m ago", "2h ago") with absolute UTC on hover; pre-formats int64 stats in the handler to dodge the int-only `commas` template helper.
- **Memory extraction → mem0 (Phase B)** - distil transcripts into structured memories, surfaced via `/recall` alongside FTS5 hits.
  - New `internal/memory/extractor` package: rule-based filter (drops tool/system messages, sub-20-character acknowledgements, bash/JSON envelopes), 5,000-char message-aligned chunker, `agent_id="transcripts-<sanitized-basename>"` derivation
  - mem0 wired through the existing `code-memory` MCP backend (no direct mem0 client); username resolver maps relay UUID → `User.Username` so extracted memories share a namespace with the user's interactive `mcp__code-memory__*` calls
  - New schema (`migrations-memory/002_memory_extractions.sql`): `last_extracted_at` column on `memory_sessions`, `memory_extractions` provenance table mapping each chunk to its returned mem0 memory IDs
  - Three triggers: watcher 60s mtime quiescence after a successful ingest, server-side cron sweep every 30 minutes (cap 50 sessions/cycle, gates on `last_seen_at > 1h ago AND last_extracted_at < last_seen_at`), and manual `arc-sync memory extract <session-id>`
  - Async extract endpoint — `POST /api/memory/extract` returns `202 Accepted` immediately and runs in a detached goroutine with a 30-minute timeout (sized for 100+ chunk sessions that exceed Cloudflare-tunnel-style 100s origin timeouts)
  - Per-session `sync.Mutex` serializes overlapping cron + on-demand calls; idempotency guard skips chunks whose source UUIDs are already covered by a successful prior extraction row
  - Blended `/recall` — `GET /api/memory/search` now fans out to mem0 in parallel and returns a new `memory_hits[]` array alongside FTS5 `hits[]`; CLI renders distilled memories above transcript hits with `[memory] (repo)` prefix
  - `arc-sync memory extract <session-id>` CLI subcommand for forcing extraction
  - Spec at `docs/superpowers/specs/2026-04-28-arc-relay-memory-extraction-design.md`
- **Skill repository** - centralized Claude Code skill distribution
  - Three new tables (migration 015): `skills`, `skill_versions`, `skill_assignments` with public/restricted visibility, semver version pins, yank ≠ delete semantics
  - Tar.gz archives stored on disk under `<bundles_dir>/<slug>/<version>.tar.gz` (default `<db_dir>/skills`, mode 0700, atomic temp+rename); SHA-256 verified on download via `X-Skill-SHA256` response header
  - Validation gate on every upload: SKILL.md at archive root, YAML frontmatter parse, `name` field equals slug, no path-traversal entries, ≤5 MiB cap, semver `MAJOR.MINOR.PATCH` enforcement
  - Eight REST endpoints under `/api/skills/*` (admin-tier required for upload/delete; reads scoped per visibility ACL); `MaxBytesReader` enforces the 5 MiB cap pre-allocation
  - Web dashboard at `/skills`, `/skills/{slug}`, `/skills/new` with CSRF-checked admin yank/unyank/hard-delete + admin-only assignments table
  - `arc-sync skill {list,install,remove,sync,push}` subcommands; `.arc-sync-version` marker discriminates arc-sync-managed installs from hand-installed dirs (sync/remove refuse to touch unmarkered dirs); pinned-version preferred over latest
  - `arc-sync setup-claude` is now relay-first with embed fallback; prior embed-only installs are auto-upgraded to relay-managed on next run via SHA-check against the embed
  - Config: `ARC_RELAY_SKILLS_DIR` env var, `[skills] bundles_dir` TOML key
- **Centralized memory** - transcript ingestion + recall across machines
  - Separate SQLite file (`memory.db`, env `ARC_RELAY_MEMORY_DB_PATH`) with isolated WAL/VACUUM/backup so heavy ingest does not contend with auth-critical writes
  - External-content FTS5 + BM25 ranking; three-tier search escalation (raw FTS5 → quoted phrase → Go regex) handles unquoted hyphenated queries (`arc-relay`) and other FTS5 metacharacter edge cases
  - Parser registry pattern under `internal/memory/parser/` (claudecode v1; Codex/Gemini drop in via `register("<platform>", ...)`)
  - Five REST endpoints under `/api/memory/*`; native MCP server at `/mcp/memory` (8 tools, accepts API keys OR OAuth tokens); web dashboard at `/memory`, `/memory/sessions[/{id}]`, `/memory/search`
  - Per-user scoping at every read surface; user ID derived from authenticated context, never the request body
  - All read surfaces prepend a `## RESEARCH ONLY ...` banner so an LLM consumer cannot mistake recalled history for live instructions
  - `arc-sync memory watch` (launchd/systemd user service) tails `~/.claude/projects/**/*.jsonl`, watermarks per-file, reacts to a Stop-hook wakeup flag for instant ingest at session end
  - `arc-sync memory {search,list,stats,show}` and a `/recall` Claude Code slash command
  - Per-IP rate limits (commit `e9e1844`) on public auth-init endpoints: `/oauth/register` (10/hr), `/api/auth/device` (20/15min), `/api/auth/device/token` (60/min), `/api/auth/invite` (10/15min) — return HTTP 429 + `Retry-After` when exhausted
  - DB files now created mode 0600 via `os.Chmod` in `store.Open` plus `syscall.Umask(0o077)` in `main()` — defense in depth against accidentally permissive volume defaults
- **Stateful archive handoff** - "Set up the Comma Compliance Archive" flow now mints a server-side nonce before opening the compliance popup and validates it on the return trip
  - Without the nonce, any crafted `#mw-archive?...` fragment on an authenticated page could silently reconfigure archive credentials
  - New endpoints: `POST /api/archive/handoff/begin`, `POST /api/archive/handoff/complete`
  - In-memory nonce store with 10-minute TTL, bound to the initiating admin session
  - Fragment values are never applied directly from the browser; the client posts them to `/complete` where server-side validation is authoritative
  - See `docs/archive-handoff.md` for the protocol specification
- **Envelope schema v2** - NaCl Box archive envelopes now include `version` and `kid` (key fingerprint) fields
  - `version: "nacl-box-v1"` lets receivers dispatch on the version, not the presence of a ciphertext field
  - `kid = base64(blake2b-256(recipient_pub)[:8])` lets receivers route decryption through multiple keys during rotation
  - Shared `sealArchivePayload` helper - real traffic and synchronous test deliveries go through the same sealing code
  - Schema documented in `docs/archive-envelope.md`
- **Envelope encryption UI** - archive config section shows an "Envelope encrypted" indicator with fingerprint when a recipient key is configured, and a "Remove encryption" button for explicit plaintext downgrade
- `ValidateArchiveConfig` is extracted as a public function and called at save time
  - Rejects non-https URLs unless the host is localhost/loopback
  - Rejects unknown `auth_type` or `include` values
  - Rejects malformed `nacl_recipient_key` values before they reach the enqueue path
- **Tool Context Optimizer** - LLM-powered tool definition compression to reduce context token usage
  - Per-server opt-in: audit tool sizes, run LLM optimization, toggle serving optimized tools
  - Deterministic JSON Schema pruning plus LLM-based description compression via Anthropic API
  - Hash-based invalidation detects upstream tool changes, marks optimizations stale
  - Optimizer middleware intercepts tools/list responses when enabled
  - Before/after tool details table with per-tool savings and red/green coloring
  - Concurrent run guard, adaptive batch sizing for large schemas
  - Config: `ARC_RELAY_LLM_API_KEY`, `ARC_RELAY_LLM_MODEL` env vars
  - Migration 014: `tool_optimizations` table, `servers.optimize_enabled` column
- `scripts/lint.sh` - local lint script mirroring CI checks

### Fixed
- **`arc-sync memory watch` no longer wedges on a large backlog** - the watcher POSTed the whole delta from its watermark to EOF in one request. The relay caps ingest bodies at 10 MiB, and a rejected POST must not advance the watermark, so any delta over the cap was re-sent in full on every 30s scan and that file never made progress again. Seen in production as 2,653 × 413 against 632 × 200 over seven days, traced to one transcript with a 69.5 MiB backlog. The trigger is not large files but large *gaps* — watcher downtime, or a reset state file, while a session keeps growing.
  - Ingest now walks the delta in newline-aligned chunks of at most 4 MiB, advancing the watermark after each accepted chunk, so a backlog of any size drains a piece at a time. Chunks must be newline-aligned because the relay's parser is line-oriented and discards the fragments on both sides of a mid-record split.
  - A 413 is now treated as permanent for those bytes and skipped, so one oversized record cannot stall every later record in the file. Other failures (network, auth, 5xx) still hold the watermark for retry. `postIngest` returns a typed `ingestHTTPError` so this is a status-code check rather than string matching.
  - The watcher now stops at the last complete line. A transcript caught mid-append ended partway through a record; that fragment was sent *and* the watermark advanced past it, so the parser skipped both halves and the record was lost. Pre-existing bug, fixed here because chunking has to find line boundaries anyway.
  - `MemoryWatcher.MaxChunkBytes` overrides the 4 MiB default (zero = default).

## [1.0.0] - 2026-04-01

### Changed
- **Renamed from MCP Wrangler to Arc Relay** - new module path `github.com/comma-compliance/arc-relay`
- Binary names: `arc-relay` (server), `arc-sync` (CLI, formerly `mcp-sync`)
- Environment variables: `ARC_RELAY_*` (server), `ARC_SYNC_*` (CLI)
- Config directory: `~/.config/arc-sync/` (CLI)
- Docker image: `ghcr.io/comma-compliance/arc-relay`
- License changed from AGPL-3.0 to MIT

### Added
- **NaCl Box encryption** for archive webhook payloads (X25519 + XSalsa20-Poly1305)
- OSS documentation: AGENTS.md, CODE_OF_CONDUCT.md, SECURITY.md, GitHub issue/PR templates

## [0.3.0] - 2026-03-08

### Added
- **Proxy Middleware Pipeline** - bidirectional request/response processing for MCP traffic
- **Sanitizer middleware** — PII/secret redaction with configurable regex patterns (redact or block)
- **Content Sizer middleware** — response size limits with truncate/block/warn actions (default 500KB)
- **Alerter middleware** — pattern monitoring with log and webhook alert actions
- Middleware toggle UI on server detail page (per-server enable/disable)
- Middleware event log with event type badges (redacted, blocked, truncated, alert)
- `middleware_configs` and `middleware_events` database tables (migration 004)
- 10 unit tests covering all middleware and pipeline behavior

## [0.2.3] - 2026-03-08

### Fixed
- Docker API compatibility: probe daemon version via `/_ping` and pin client API version to match, bypassing the SDK's minimum version check (fixes Docker on Unraid 6.x / Docker 24.x with API 1.43)

## [0.2.2] - 2026-03-08

### Added
- OAuth auto-discovery + dynamic client registration for manual server entry (not just catalog)
- Client-side OAuth discovery triggers when switching auth type dropdown to "oauth"

### Changed
- Improved error message when OAuth auto-discovery fails

## [0.2.1] - 2026-03-03

### Fixed
- Docker startup no longer requires config.toml (env vars are sufficient)

## [0.2.0] - 2026-03-03

### Fixed
- Server edit now preserves status, timestamps, and OAuth tokens on update

## [0.1.1] - 2026-03-01

### Added
- Foundation: proxy, Docker lifecycle, web UI, API keys, health monitor

## [0.1.0] - 2026-02-28

### Added
- Initial open-source release with security hardening
