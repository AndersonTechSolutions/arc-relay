# Arc Relay Memory Phase 0 + Phase 3: pipeline hardening, then Codex transcript ingestion

- **Date:** 2026-09-17
- **Status:** rev 5, ready for implementation planning. Four adversarial review passes: Codex gpt-6-astra (high ×2, low ×1) and Claude Opus 5 (×1), all read-only against the code, 2,027 Codex rollouts, and the Cursor DBs on Mint. Production facts verified read-only by Claude. See §10.
- **Predecessors:** `2026-04-26-arc-relay-memory-pivot-design.md` (Phase 1), `2026-04-27-arc-relay-memory-dashboard-design.md` (Phase 2), `2026-04-28-arc-relay-memory-extraction-design.md` (Phase B)

## 1. Problem

Agent threads run through T3 Code on the Mint dev box (`dev@10.10.0.177`) against Claude, Codex, and Cursor. The memory pipeline (watcher → `/api/memory/ingest` → FTS5 store → extraction → mem0 `transcripts-<repo>`) only understands Claude Code transcripts. Codex and Cursor threads reach `code-memory` only when the model voluntarily calls `add_memory` per the shared `AGENTS.md` protocol (stopgap deployed 2026-09-17).

T3 threads on Mint: `codex` 365, `claudeAgent` 88, `cursor` 1. Mint holds 2,027 Codex rollouts (1,311 MB raw; 575 with mtime in the last 30 days; the largest is about 53 MB). Codex is the only unrecorded source with real volume.

**Goal:** every Codex session on a machine running `arc-sync memory watch` is ingested and extracted automatically and safely, with backfill that cannot duplicate data or burst extraction spend.

**The existing pipeline has correctness defects** (§2.2). Adding a high-volume source on top would amplify them, so **Phase 0 (hardening) is a hard prerequisite.**

**Scope decisions:**
- **Phase 3a = Codex.** Ships in this spec.
- **Phase 3b = Cursor. Deferred** (§5.4). There is one real Cursor thread out of 454; every Cursor DB on Mint was a probe created while writing this spec; the format drifted between review rounds. Observations are retained so the work can resume when there is real usage.

**Non-goals:**
- Gemini CLI.
- T3 `state.sqlite` as a source (§8).
- Changing mem0 prompts or classification.
- Secret redaction (follow-up, §7.5).
- Codex compaction epochs (Codex rows use epoch 0; documented limitation).
- Manual extraction of subagent sessions.
- Repairing `transcripts-<x>` namespaces of already-extracted sessions (§5.2).

## 2. Current architecture: verified facts and defects

### 2.1 Facts

- **Parsers:** `parser.Parser` = `Platform()` + stateless `Parse(io.Reader)`, registered via `init()`. `parser.Get` returns nil for unknown platforms, which becomes HTTP 400.
- **Ingest:** `Service.Ingest` upserts `memory_sessions`, **then** parses and `BulkInsert`s in a separate transaction.
- **Empty UUIDs** are stored as NULL (`memory_messages.go:247-252`).
- **Session upsert:** `ON CONFLICT(session_id)` updates mtime/seen/bytes only (`memory_sessions.go:37-42`). `user_id`, `project_dir`, and `platform` are insert-only.
- **FTS:** `memory_messages_fts` is external-content FTS5, synced by AFTER INSERT/DELETE triggers (`001_memory.sql:63-70`). The delete trigger issues the FTS `'delete'` command with `old.content`, which is only correct if the index entry matches. A non-`MATCH` count cannot validate the index; `INSERT INTO memory_messages_fts(memory_messages_fts, rank) VALUES('integrity-check', 1)` is the content-aware check (verified: detects a stale entry; the no-arg form does not).
- **Boot:** `store.Open` runs `PRAGMA integrity_check` synchronously (`db.go:39`, B-tree only, not FTS) and then all pending migrations in one transaction (`db.go:172-189`), before HTTP starts. There is no maintenance mode.
- **Backups:** `memDB.StartBackup(6h)` runs `VACUUM INTO` (`cmd/arc-relay/main.go:186`).
- **Extraction:**
  - `Filter` (`filter.go:118-122`) drops `tool`/`system` rows before chunking. The Claude parser never emits those roles (it inlines tool use as `[TOOL_USE:…]`/`[TOOL_RESULT]` in assistant text, `claudecode.go:145-152`), so Claude tool content *is* extracted today.
  - Chunk target 3,000 chars (`extractor.go:81`); oversized single messages are kept whole (`chunk.go:73`).
  - `coveredUUIDs` (`extractor.go:231-252`) decodes **every** `memory_extractions` row for the session, including error rows, on every pass.
  - `callAddMemoryWithRetry` (`extractor.go:281-286`) makes up to two attempts and returns `ctx.Err()` immediately once the context is done.
  - `MarkExtracted` is stamped unconditionally after a pass (`extractor.go:215`).
  - `Derive(project_dir)` produces `transcripts-<basename>`; `callAddMemory` writes `metadata.platform`.
  - `GetSession` returns messages in `id` order (`memory_messages.go:204`).
- **Extract triggers and timeouts:**
  - Watcher quiescence POSTs `/api/memory/extract`; each POST spawns a goroutine with a **30-minute** per-session context (`memory_handlers.go:333-334`). The handler first calls `GetSessionWithMessages(userID, id, 0)` purely for the ownership check (`memory_handlers.go:319`), materializing the whole session.
  - Cron: 50 sessions/cycle, **15-minute** per-session context (`cron.go:61`). The only rate limit today.
  - `arc-sync memory extract <id>` calls the same endpoint with only `session_id`. `--all-stale` prints a hint and exits 2 (`main.go:1852`).
- **Watcher:**
  - Single `RootDir`; `addRecursive` runs once at startup (`memory.go:136`).
  - `.jsonl` filter; the whole unread tail is read into RAM.
  - `chunkEnd` emits oversized lines whole; a 413 skips them; any other non-2xx leaves the watermark and retries next scan (`memory.go:274-282,312`).
  - State file written by in-place truncate, with no lock; an unknown shape silently resets to empty (`memory.go:83-103`).
- **`decodeProjectDir`:** lossy (`arc-relay` → `arc/relay`, `memory_test.go:190`).
- **Stats:** `Stats()` is server-side only (`service.go:216-241`). "Last ingest" = `MAX(last_seen_at)` (client mtime). `Platforms` = registered parsers. There is no watcher-side status surface.

### 2.2 Defects (fixed in Phase 0)

| # | Defect | Evidence |
|---|---|---|
| D1 | **Ingest is not idempotent.** `idx_memory_messages_uuid` is a plain partial index and inserts are plain `INSERT`. The service comment claiming uniqueness is wrong. | `001_memory.sql:41`, `memory_messages.go:48`. **Prod `memory.db` 2026-09-17 (read-only):** 699,256 rows; 0 NULL/empty uuids; 23,433 duplicate `(session_id, uuid)` groups = **27,587 extra rows**, all `claude-code`, **0 groups with differing content**; 673 uuids legitimately span sessions (resumed sessions). |
| D2 | **Session ownership unchecked:** a foreign `session_id` appends into another user's session. | `service.go:66`, `memory_sessions.go:37` |
| D3 | **Partial extraction failure is never retried:** `last_extracted_at` is stamped regardless, and cron needs it `< last_seen_at`. | `extractor.go:193-215` |
| D4 | **Extraction can see half-ingested sessions:** session row is published before messages, and backfill chunks share one mtime. | `service.go` ordering |
| D5 | **No global extraction admission:** watcher triggers bypass the cron cap. | `memory_handlers.go:333`, `memory.go:301` |
| D6 | **Unsafe watcher state:** non-atomic, no lock; unknown shape silently resets → full replay (compounds D1). | `memory.go:83-103` |
| D7 | **New directories never watched.** | `memory.go:136` |
| D8 | **Oversized records silently lost; unbounded tail read into RAM.** | `memory.go:274,312` |
| D9 | **Extract authorization loads the whole session.** | `memory_handlers.go:319` |

## 3. Source formats (verified on Mint)

### 3.1 Codex CLI

- **Path:** `~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl`. Append-only JSONL, no per-line uuid.
- **Line 1:** `session_meta`. `payload`: `id`, `session_id`, `cwd`, `git` (`repository_url` often absent), `originator`, `source`, `parent_thread_id`, `agent_nickname`, `agent_path` (hierarchical, e.g. `/root/prod_safety_auditor`; present in 441 of 581 children, absent on top-level rollouts), `multi_agent_version` (`v2` = 441, `disabled` = 140, on all children), `subagent_history_start_ordinal` (506 of 581 children), `forked_from_id` (418 rollouts, 71 of them top-level), `base_instructions`, `timestamp`. **4 files contain a second `session_meta` line.**
- **Identity:** `payload.id` is unique across all 2,027 rollouts. All 211 shared-`session_id` groups are parent+children families; no two top-level rollouts share a `session_id`.
- **Subagents:** 581 child rollouts (474 MB, 36 % of the corpus), each with `parent_thread_id`. Children copy parent history (with new timestamps) up to `subagent_history_start_ordinal`. One family has 1 parent + 63 children. All children sort after their parent on disk in this corpus, but nothing guarantees it.
- **Line types (counts):** `response_item` (`message`, `agent_message` 7,856 with `author`/`recipient` agent paths — 5,186 have `recipient == /root`; `reasoning`; `function_call(_output)`; `custom_tool_call(_output)`), `turn_context` 4,443, `token_usage_record` 4,953, `event_msg.*`, `world_state`, `inter_agent_communication_metadata`, `compacted` 408. (There is no `token_count` type.)
- **Inter-agent tool surface:** `spawn_agent` (outputs `{agent_id,nickname}` 418 vs `{task_name}` 406), `wait_agent` (`{status,timed_out}` 442 vs `{message,timed_out}` 12,239), `send_message` 4,759, `followup_task` 1,457, `list_agents` 1,164, `interrupt_agent` 162, `resume_agent` 32, `graph_*` 408. Findings for the parent travel as `agent_message` rows in the v2 generation; `multi_agent_version` says which generation a rollout is.
- **Leading tags on user-role text parts (census of 8,864 parts):**

| leading tag | count | class |
|---|---|---|
| `<environment_context>` | 1,880 | injected |
| `# AGENTS.md instructions for …` + `<INSTRUCTIONS>` | 1,695 | injected composite (`<INSTRUCTIONS>` never appears standalone) |
| `<recommended_plugins>` | 1,269 | injected |
| `<subagent_notification>` | 327 | injected |
| `<grove-meta>` | 322 | injected |
| `<turn_aborted>` | 320 | injected ("The user interrupted the previous turn…") |
| `<user_action>` | 170 | injected (`<context>…</context><action>review</action>`) |
| `<task>` | 108 | genuine subagent assignment (children only) |
| none | 2,773 | genuine user input |

`<user_instructions>` occurs 0 times.
- **Text sizes:** 32,974 user/assistant/`agent_message` parts, 40.3 MB total; p50 354 B, p99 9.3 KB, **100 parts > 16 KiB**, max 225,709 B (`2026/07/25/rollout-…019f9b05…` line 9419).
- **Extraction volume:** top-level rollouts carry 17.0 M extractable chars ≈ 5,681 chunks. Largest sessions: 310, 222, 193, 193, 174 chunks; 9 sessions need > 75 chunks, 5 need > 150.
- **Worktree cwds:** `~/.grove/worktrees/<hash>/<task>` (28, all with `git.repository_url`), `.claude/worktrees/…`, `/var/tmp/vibe-kanban/worktrees/…` (174). 1,206 of 2,027 cwds still exist on disk.

### 3.2 Cursor CLI (`cursor-agent`) — retained for Phase 3b

- **Path:** `~/.cursor/chats/<workspaceHash>/<agentId>/store.db` + `meta.json` (`{schemaVersion, createdAtMs, updatedAtMs, hasConversation, cwd, subagentInfo?}`). One dir has `meta.json` + `prompt_history.json` and **no `store.db`** (`hasConversation:false`).
- **`store.db`:** `blobs(id TEXT PRIMARY KEY, data BLOB)` + `meta(key, value)`; `meta` value is hex JSON with `agentId`, `latestRootBlobId`, `blobEncryptionKey` (**present and non-empty in every DB while blobs are plaintext** — encryption is configured but unused).
- **Root:** protobuf; field 1 = ordered 32-byte refs to committed messages (final root of `a31d0056-…` holds 10; a newer chat holds 12). Earlier roots hold pending turn content in field 4.
- **Message blobs:** plaintext JSON `{role, content}`. Roles observed: `system`, `user`, `assistant`, **`tool`** (tool results are top-level `role:"tool"` blobs with `content:[{type:"tool-result", toolCallId, toolName, result: string|object, experimental_content}]`). Assistant parts: `text`, **`reasoning`** (not `redacted-reasoning`), `tool-call`.
- **Preamble:** user blobs without `<user_query>` that begin `<user_info>` are injected (one is 46,182 chars). Real turns wrap text in `<user_query>` and carry `<timestamp>`.
- **All 7 chat dirs on Mint were created 2026-09-17 00:15–00:42 by probes while writing this spec.** There is no real local corpus.

## 4. Phase 0: pipeline hardening

### 4.1 Server

**P0-S1: session-scoped identity + idempotency (D1).**

**Migration `003_memory_hardening.sql`:**
1. `DELETE FROM memory_messages WHERE uuid IS NOT NULL AND id NOT IN (SELECT MIN(id) FROM memory_messages WHERE uuid IS NOT NULL GROUP BY session_id, uuid)`. NULL uuids are never touched.
2. `INSERT INTO memory_messages_fts(memory_messages_fts) VALUES('rebuild')`. The post-state is then correct regardless of whether the delete trigger's `'delete'` commands matched the prior index.
3. `CREATE UNIQUE INDEX idx_memory_messages_session_uuid ON memory_messages(session_id, uuid)`. Non-partial: SQLite unique indexes allow multiple NULLs, and prod has 0 NULLs.
4. Session columns (all insert-only on upsert unless stated):
   - `raw_cwd TEXT`, `project_key TEXT`
   - `is_subagent INTEGER NOT NULL DEFAULT 0`, `source_parent_session_id TEXT`
   - `last_activity_at REAL` (updated on every ingest), `last_ingested_at REAL` (updated on every ingest)
   - `extracted_through_msg_id INTEGER` (NULL for all existing rows)
   - `extract_attempts INTEGER NOT NULL DEFAULT 0`, `last_extract_attempt_at REAL`, `extract_poisoned_at REAL`
   - `client_counters TEXT` (JSON, replaced on every ingest)
5. Backfill `last_activity_at` = `MAX(timestamp)` per session, parsed by a Go migration step (not SQL) that accepts RFC3339 with `Z` or offset; unparseable → NULL (treated as "old" by the age gate).

**Inserts:** `INSERT … ON CONFLICT(session_id, uuid) DO NOTHING`. `MessagesAdded` = sum of `RowsAffected`.

**Migration procedure** (no maintenance mode exists; `store.Open` runs `integrity_check` + migrations before HTTP):
1. **Dry run on a consistent copy** (`.backup` of prod while running is consistent). On the copy, in order:
   - `INSERT INTO memory_messages_fts(memory_messages_fts, rank) VALUES('integrity-check', 1)` — **abort the plan if this fails** before anything destructive.
   - preflight: 0 content-differing duplicate groups; expected removal ≈ 27,587 + growth.
   - run migration 003 and **record three durations:** `PRAGMA integrity_check`, the DELETE, the FTS `'rebuild'`. The disk high-water mark is `.backup` + `.backup.prev` + `memory.pre-003.db` + live DB + WAL growth ≈ 4× 1.8 GB + WAL; the volume has 73 GB free.
2. **Confirm the Komodo stack's restart policy and healthcheck** for `arc-relay` will not kill a container that spends the measured time in `store.Open`. If they would, raise the healthcheck start period for the deploy.
3. **Stop** the `arc-relay` container. **This is an outage for all MCP proxying, not just memory ingest**; watchers retry without data loss (non-2xx leaves the watermark).
4. **Backup** of the stopped DB: `sqlite3 memory.db ".backup memory.pre-003.db"` (the only supported path).
5. Start the new image; the migration runs in `store.Open`. Kill mid-migration rolls back (one transaction); a rollback also takes time — do not restart the container during the window.
6. **Verify:** 0 duplicate `(session_id, uuid)` groups; `integrity-check, 1` passes; 5 known-phrase `MATCH` queries return expected sessions; `/api/memory/stats` reachable.
7. **Restore path:** stop; remove stale `-wal`/`-shm`; `sqlite3 memory.pre-003.db ".backup memory.db"`; start the previous image tag.

**P0-S2: atomic ingest + ownership (D2, D4, D9).**
- One transaction: ownership check → session upsert → message inserts → `last_ingested_at`/`last_activity_at`/`client_counters` update.
- If `session_id` exists with a different `user_id`: **409**, nothing written.
- `HandleExtract` authorizes with `SELECT user_id FROM memory_sessions WHERE session_id = ?`, never by loading messages.

**P0-S3: timestamps + stats.** Stats "Last ingest" = `MAX(last_ingested_at)`. `platform` is denormalized onto `memory_messages` (migration 003 adds the column and backfills it from sessions) so per-platform counts are indexed, not a join scan. Stats also report per-session `client_counters` aggregates (`skipped_records`, `rejected_records`, `ownership_conflicts`, `unknown_leading_tag{name}`).

**P0-S4: bounded passes + retry + poison (D3).**

*Pass:*
- Load `WHERE session_id=? AND id > COALESCE(extracted_through_msg_id,0) ORDER BY id LIMIT 400`; `through_id` = max id actually loaded. **Every pass is bounded** and terminates well inside any timeout; large sessions walk forward across passes.
- Messages removed by `Filter` count as processed. `coveredUUIDs` stays as a guard within the loaded range but reads **only** `WHERE error IS NULL` rows, in SQL.
- On completion:
  - **All chunks succeeded, or zero chunks after filtering:** set `extracted_through_msg_id = through_id`, `last_extracted_at`, `extract_attempts = 0`, in one transaction with the final provenance insert. A failed provenance insert aborts that transaction.
  - **Any chunk failed:** watermark unchanged; `extract_attempts += 1`; failure rows recorded.
  - **`ctx.DeadlineExceeded`:** abort the pass immediately (no failure row per remaining chunk); counts as one failed attempt.
- **Poison:** when `extract_attempts` reaches 4, set `extract_poisoned_at`, advance `extracted_through_msg_id = through_id` (skipping the failing range), reset attempts, log, and count `poisoned_ranges` in stats. The session stays eligible for later messages. Error provenance rows older than 30 days are pruned by the backup job.
- **Initialization:** all existing sessions start with `extracted_through_msg_id = NULL`. Sessions inside the age window get one pass per 400-message window; `coveredUUIDs` prevents re-spend on chunks that already succeeded, so the first cycles are mostly no-op passes.

*Eligibility predicate* `eligible_auto(s)` (one function, used at enqueue and dequeue):
- `is_subagent = 0` AND `platform ∈ ARC_RELAY_EXTRACT_PLATFORMS` (default `claude-code`)
- AND `last_activity_at ≥ now − ARC_RELAY_EXTRACT_MAX_AGE_DAYS` (default 30; NULL activity = ineligible)
- AND `EXISTS(message id > COALESCE(extracted_through_msg_id, 0))`
- AND `last_ingested_at < now − 60s` (NULL = quiet)
- AND backoff since `last_extract_attempt_at` has elapsed: attempts 1/2/3 → 5 m / 30 m / 2 h.

**P0-S5: admission queue + call-rate limit (D5).**
- **One in-process queue** fed by watcher requests and cron: capacity 1,000; dedup across queued + running by `session_id`; workers `ARC_RELAY_EXTRACT_WORKERS` (default 2).
- **Per-session pass timeout** for queue workers: 15 minutes (the 400-message bound makes a pass ≤ ~135 chunks ≈ 27 minutes at worst under the rate limit, so a pass that hits the timeout simply resumes from its watermark next time). The existing cron and HTTP contexts are replaced by the worker timeout.
- **Call-rate limit** (token bucket): refill `ARC_RELAY_EXTRACT_CALLS_PER_HOUR` (default 300), burst 60, startup balance 0. One token per **attempted** backend call including retries, taken before the classifier. This is a rate limit, not a dollar bound; single messages are hard-truncated at 12,000 chars before chunking.
- **Cron:** every 30 min, enqueues up to 50 eligible sessions ordered by `COALESCE(last_extract_attempt_at, 0) ASC` (least recently attempted first; no starvation).
- **`POST /api/memory/extract`** body `{"session_id", "mode": "auto"|"manual"}`; **missing `mode` = `auto`**. Response (always 202 when authorized): `{"session_id", "queued": bool, "reason": "eligible"|"ineligible:<gate>"|"queue_full"|"duplicate"}`; `arc-sync memory extract` prints it.
  - `manual` skips age/platform/quiet gates, not `is_subagent` or ownership; best-effort across restarts (documented in CLI output).
  - `--all-stale` stays unimplemented; cron is the bulk path.

### 4.2 Watcher (arc-sync)

**P0-W1: state v2 (D6).**
- `memory_state.v2.json`, imported once from v1; v1 never written again (harmless replay on downgrade after P0-S1).
- Atomic tmp + fsync + rename; `flock` on `memory_state.lock`; a second `watch`/`--once` exits with an error.
- Unknown/corrupt state refuses to start (`--reset-state` to override).
- Per-file state includes `raw_cwd` (populated by a bounded 64 KiB head-read on first sight — required because resumes start mid-file) and, for Codex, the cached `session_meta` identity.

**P0-W2: directory discovery (D7).** Handle directory `Create` before file matching and add watches recursively. The 30 s tick reconciles watches and discovers roots created after startup. Real loop tests cover new dir → file → ingest, and quiescence.

**P0-W3: bounded reads + permanent errors (D8).**
- Tail read in ≤4 MiB windows. **Hard record ceiling 64 MiB:** a longer line is skipped by scanning to the next newline without buffering, counted (`skipped_records`), and logged with path + offset.
- **413** on a multi-record unit halves the unit and retries; a single-record 413 is a counted skip.
- **409** is permanent for that file: quarantine the file (no further reads), count `ownership_conflicts`, log once with path + session. This can happen when two arc-sync installs authenticated as different relay users see the same transcript tree.
- **Counters travel with ingest:** every ingest request carries `client_counters` (the per-file counters above plus normalizer counters); the relay stores them per session and aggregates them in stats. There is no separate watcher status surface.

## 5. Phase 3a: Codex source

### 5.1 Client-side normalization

Codex normalization runs in **arc-sync**, because it needs state across POST boundaries (session identity, `call_id → name` map) that the stateless server parser can't hold. The server registers one thin `codex` parser for **normalized JSONL**:

```json
{"uuid":"…","parent_uuid":"…","role":"user|assistant|tool","content":"…","timestamp":"RFC3339"}
```

The server sets `epoch = 0`, validates the shape (role enum, uuid non-empty, content ≤64 KiB), rejects malformed lines individually, and returns `rejected_rows` (count + first 3 uuids) in the ingest response; the watcher adds them to `rejected_records`.

**Invariant: one source record never spans a POST.** A record's normalized output is emitted as one unit (or appended whole to the current unit if it fits). If a single record normalizes to more than 1 MiB it is a counted skip. No intra-record checkpoint exists. (Corpus: max record normalizes to ~225 KB.)

**Fragmentation:** any row whose content exceeds 16 KiB UTF-8 is split at the last rune boundary ≤16 KiB into rows `<uuid>:frag:<n>` with `parent_uuid` = first fragment (100 parts in the corpus).

**Client interface:**

```go
type Source interface {
    Platform() string
    Roots() []string
    Match(path string) bool
    Next(st SourceFileState, path string) (*Unit, error) // one bounded POST; nil when caught up
}

type Unit struct {
    SessionID, RawCwd, ProjectKey, SourceParentSessionID string
    IsSubagent   bool
    FilePath     string
    BytesSeen    int64
    Payload      []byte
    Counters     map[string]int64
    NextState    SourceFileState // persisted only after 2xx
}
```

`ingestRequest` gains `raw_cwd`, `project_key`, `is_subagent`, `source_parent_session_id`, `client_counters`. `claudeCodeSource` wraps today's behavior; golden tests prove POST payload bytes are identical to pre-refactor apart from the new envelope fields.

### 5.2 Project identity

- **`raw_cwd`:** the transcript's own cwd (Codex `session_meta.cwd`; Claude: first record carrying `cwd`, from the head-read in P0-W1). Stored at ingest, insert-only.
- **`project_key`** (resolved client-side, cached per cwd in state), first of:
  1. If `<cwd>/.git` is a **file** (worktree), read its `gitdir:` and the `commondir` file to find the main repo dir → basename. If `.git` is a directory → basename of `cwd`. **No `git` subprocess is run** (a `.git/config` in a historical worktree could execute hooks/pagers as the watcher user).
  2. Codex `git.repository_url` basename (covers deleted worktrees and all observed Grove sessions).
  3. Basename of `raw_cwd`.
- **Extraction:** `Derive` uses `project_key` when set, else today's `project_dir` behavior.
- **No repair of history.** Sessions ingested before this change keep their `project_dir`-derived namespace; mem0 memories under old `transcripts-<worktree-id>` names stay. (The `reproject` endpoint from rev 3/4 was cut as three moving parts for a cosmetic fix.)

### 5.3 Codex normalization rules

**Identity:**
- `session_id` = `codex:<payload.id>`.
- `is_subagent` = `parent_thread_id != null`; `source_parent_session_id` = `codex:<parent_thread_id>`, stored as-is. The parent need not exist.
- A **second `session_meta`** line in a file is ignored for identity (the first wins) and counted (`duplicate_session_meta`).

**Skip copied history:**
- **Children:** records with ordinal `< subagent_history_start_ordinal` are skipped when the field is present (506 of 581 children). Removes ~474 MB of duplicated ingest and the up-to-64× FTS duplication.
- **Forks:** top-level rollouts with `forked_from_id` (71) copy another session's history under new hashes and would be extracted twice. If a fork-start ordinal field exists it is used identically; otherwise **the duplication is accepted and measured** (`fork_sessions` counter, listed in §7).

**Message identity:** `uuid` = `sha256(raw_line)[:32]`; derived rows add `:result:<n>`, `:part:<n>`, `:frag:<n>`. Scoped per session.

**User messages:** one segmentation pass per text part, in order:
1. **Protect fenced code:** spans inside ``` fences are opaque to steps 2–3.
2. **Strip injected segments:** (a) a `# AGENTS.md instructions for <path>` header together with the `<INSTRUCTIONS>` block that follows it; (b) any standalone block from the table: `environment_context`, `recommended_plugins`, `subagent_notification`, `grove-meta`, `turn_aborted`, `user_action`. (`user_instructions` does not occur; standalone `INSTRUCTIONS` does not occur; `task` is genuine and kept.)
3. **Keep the remainder** if non-empty after trimming; else drop the part. Unknown tags are kept.
4. **Any part whose leading tag is not in the table and not `task`** increments `unknown_leading_tag{name}` in counters, so the next injected wrapper shows up in stats instead of in mem0.

Fixtures: one per table entry; composite header+wrapper with and without trailing request; known wrapper inside a code fence (kept); unknown tag (kept + counted).

**Other records:**

| Record | Output |
|---|---|
| assistant `message` | `assistant` text parts |
| `agent_message` | `input_text` parts → `assistant`, `"[agent <author>→<recipient>] …"`. `encrypted_content` dropped. **Kept when `recipient` equals this rollout's `agent_path`, or `/root` when `agent_path` is absent** (top-level rollouts). Otherwise dropped (it's addressed to another agent and will appear in that agent's rollout). |
| `function_call` / `custom_tool_call` | `tool` `"<name>: <args ≤500>"`; client state records `call_id → name` |
| `*_call_output` | `tool`, output ≤2,000 chars |
| `wait_agent` output when `multi_agent_version != "v2"` and `status.<id>.completed` present | Decode JSON before truncation; emit `assistant` `"[subagent <nickname|id> result] <text>"` per completed agent (fragmented; `:result:<n>`). `not_found`/timeout dropped. No repeat-delivery dedup (442 occurrences corpus-wide; the `:result:<n>` identity is enough). |
| `wait_agent` output when `v2` | Dropped (acknowledgment; findings arrive as `agent_message`) |
| `compacted`, `reasoning`, `turn_context`, `event_msg.*`, `token_usage_record`, `world_state`, `inter_agent_communication_metadata`, developer/system | Dropped |
| unknown or malformed | Skipped, counted (`unknown_record_type{type}`), never fatal |

**Tool rows are FTS-only.** `Filter` drops `role = tool` before extraction, so Codex tool args/outputs are searchable but never reach mem0. This is a deliberate divergence from Claude (whose parser inlines tool content into assistant text and therefore extracts it); the ≤500/≤2,000 truncation exists to bound DB size. Revisit if Codex extractions turn out to lack the outcomes Claude's carry.

**Client state per rollout:** `{byte_offset, raw_cwd, session identity (id, parent, agent_path, multi_agent_version, history_start_ordinal, repository_url), call_id→{name, nickname}, counters}`, persisted with each acked unit.

**Subagents:** child rollouts are ingested (searchable, minus copied history) and never extracted (`is_subagent`; manual rejected). Parent extraction carries findings via the `agent_message` / v1 result rows above.

### 5.4 Phase 3b: Cursor (deferred)

Not implemented in this spec. When there is real Cursor usage, the design must address, beyond rev 4's field-1 decoder and append-only quarantine: the missing-`store.db` chat shape; `meta.json.subagentInfo` → `is_subagent`; `role:"tool"` blobs; the `reasoning` part type and an explicit unknown-part policy; treating encryption onset (`blobEncryptionKey` in use) as an alert, not a counter; and a binary-size decision for `modernc.org/sqlite`. Fallback if the format proves too unstable: a T3 `state.sqlite` source filtered to `provider = cursor`.

### 5.5 Server

1. `codex` parser for normalized JSONL.
2. Migration 003 already carries the session columns.
3. Dashboard platform filter.
4. `eligible_auto` and the manual path reject `is_subagent = 1`.

## 6. Rollout (strict order: relay first, then clients, then gates)

1. **PR-0a (relay):** P0-S1…S5 + migration 003. Dry run on a DB copy (with the three measured durations + the FTS preflight), restart-policy check, then the maintenance procedure (§4.1), deploy (`komodo execute RunBuild` + `DeployStackService`), and verification. `ARC_RELAY_EXTRACT_PLATFORMS=claude-code`.
2. **PR-0b (arc-sync):** P0-W1…W3. Self-update Mac + Mint. Verify v2 import + lock; `last_ingested_at` advances; forced `--once` replay adds 0 rows. **PR-0b must be on every client before PR-2 Stage B** (old clients send no `mode`, which defaults to `auto` and is gated — safe, but their quiescence triggers are silently ineligible until then).
3. **PR-1 (arc-sync):** `Source` interface, `claudeCodeSource`, project resolver. Golden tests.
4. **PR-2 (relay, then arc-sync):** Codex.
   - **Stage A:** ingest all history with `codex` absent from `ARC_RELAY_EXTRACT_PLATFORMS`. Expected: ≈ +165 K rows (+24 %). Spot-check normalization on 10 real sessions (including one `disabled`-generation and one `v2` subagent parent) and review `unknown_leading_tag` counts.
   - **Stage B:** add `codex` to the gate. Full ≤30-day backlog ≈ 5,681 chunks ≈ 19 h at 300 calls/hour.
5. **Per-stage verification:** per-platform stored counts; search for a phrase from a recent T3 thread; one session's provenance → mem0 `transcripts-<repo>` with `metadata.platform`; 24 h journal clean; `skipped_records` / `rejected_records` / `poisoned_ranges` / `unknown_leading_tag` reviewed.

## 7. Risks

1. **Codex format drift:** unknown record types and leading tags are counted, never fatal; the wrapper table and `agent_message` rules need upkeep; fixtures per generation.
2. **Migration downtime = MCP outage** for every client, for `integrity_check` + DELETE + `'rebuild'` (measured in the dry run). Watchers retry without data loss. Boot-loop risk if the stack's healthcheck is shorter than the migration — checked before the window.
3. **Spend:** the call-rate limit is the bound (300/h default). Estimate chunk counts from Stage A before Stage B.
4. **Fork duplication:** 71 top-level forked sessions (~5 %) may be extracted twice unless a fork-start ordinal exists; measured and surfaced.
5. **Secrets in transcripts:** pre-existing, widened by Codex tool outputs (FTS only). Follow-up spec for client-side redaction.
6. **Namespace history:** memories extracted before `project_key` stay under `transcripts-<worktree-id>` names; accepted.
7. **Poisoned ranges** skip up to 400 messages of a session after 4 failed attempts; surfaced in stats for manual extraction.

## 8. Alternatives considered

- **T3 `state.sqlite` as the single source:** alpha schema, T3-only, duplicates Claude. Kept as the Cursor fallback.
- **Server-side normalization:** needs cross-chunk state the stateless parser lacks. Rejected.
- **AGENTS.md instructions only:** the current stopgap; best-effort.
- **Codex notify hooks:** no backfill, per-host config. Possible later complement.
- **Cursor in this phase:** rejected for now (§1).

## 9. Test plan

- **Migration:** fixture with same-session and cross-session duplicates plus NULL-uuid rows → only same-session non-NULL dups removed; FTS `'rebuild'` then `integrity-check, 1` passes; `MATCH` results unchanged; `ON CONFLICT(session_id, uuid) DO NOTHING` works against the non-partial index; `last_activity_at` backfill across `Z`/offset timestamps and an unparseable value.
- **Ingest:** `RowsAffected` counts; 409 on a foreign session with nothing written; parse failure rolls back the session upsert; `client_counters` stored; `platform` denormalized onto messages.
- **Extraction:** pass loads ≤400 and advances `through_id`; tool-only range (zero chunks) advances; partial failure → attempts++ and retry after backoff, no re-spend; failed provenance insert leaves the watermark; `DeadlineExceeded` aborts without per-chunk failure rows; 4th failure poisons and advances; `coveredUUIDs` ignores error rows; NULL-initialized covered session → no-op pass, no backend calls; dequeue re-check; token bucket startup 0 / retries counted / classifier gated; 12,000-char truncation; cron order least-recently-attempted; `HandleExtract` does not load messages; missing `mode` = auto; response `reason` values.
- **Watcher:** v1→v2 import; downgrade leaves v1; corrupt state refuses; lock contention; new-dir loop test; 64 MiB+ line skipped without buffering; multi-record 413 halving; 409 quarantine + counter; `raw_cwd` head-read on first sight; golden Claude payloads.
- **Codex:** each wrapper entry; composite header+wrapper with/without trailing request; known wrapper inside a code fence kept; unknown tag kept + counted; `agent_message` kept for own `agent_path`/`/root`, dropped otherwise; `disabled`-generation `wait_agent` completed result → rows; `v2` wait acknowledgment dropped; spawn in POST 1 / result in POST 2 across a restart; child with `subagent_history_start_ordinal` skips the prefix; child without it ingests fully; child before parent → `is_subagent = 1`, never auto-extracted; second `session_meta` ignored + counted; 225 KB part → `:frag:<n>` rows; > 1 MiB normalized record → counted skip; `turn_context`/unknown types dropped + counted.

## 10. Review log

### Review 1 (Codex astra high; rev 1 → rev 2)

| # | Sev | Finding | Status |
|---|---|---|---|
| 1 | Blocker | Non-unique uuid index; duplicates | P0-S1 (prod: 27,587 dup rows) |
| 2 | Blocker | Backfill bypasses extraction cap | P0-S5 queue + rate limit + Stage A/B |
| 3 | Major | Session ownership unchecked | P0-S2 |
| 4 | Major | Unsafe state migration | P0-W1 |
| 5 | Major | Partial extraction not retried; extraction on half-ingested sessions | P0-S2, P0-S4 |
| 6 | Major | Injected-context filtering misses real inputs | §5.3 segmentation + census |
| 7 | Major | Subagent outcomes lost (tool rows filtered) | §5.3 `agent_message` / v1 result rows |
| 8 | Major | Children copy parent history | `is_subagent` never extracted; history-start ordinal skip |
| 9 | Major | Cursor content is a part union | deferred (§5.4) |
| 10 | Major | Cursor committed boundary / snapshot | deferred (§5.4) |
| 11 | Major | Source interface metadata / commit ambiguity | §5.1 unit = one POST |
| 12 | Major | Project resolution incomplete | §5.2 |
| 13 | Major | No new-dir watching | P0-W2 |
| 14 | Major | Oversized records / RAM | P0-W3 |
| 15 | Major | Manual vs auto; mtime ≠ activity | P0-S5 `mode`, `last_activity_at` |
| 16 | Minor | Factual corrections | §2.1, §3 |

### Review 2 (Codex astra high; rev 2 → rev 3)

| # | Sev | Finding | Status |
|---|---|---|---|
| 1 | Blocker | `ON CONFLICT` vs partial unique index | Non-partial index |
| 2 | Major | Dedup NULLs; FTS count check meaningless; no 503 mode; backup consistency | Non-NULL only; content-aware check; stop-relay procedure; `.backup` |
| 3 | Major | Watermark ignores filtered rows / init / partial success | P0-S4 |
| 4 | Major | Queue semantics / spend undefined | P0-S5 |
| 5 | Major | Derived rows collide; real result shapes | Suffix identities; generation via `multi_agent_version` |
| 6 | Major | Requiring parent breaks child classification | `is_subagent` from source metadata |
| 7 | Major | Cursor rewrite replay; occurrence identity | deferred |
| 8 | Major | Stateless parser can't hold cross-chunk state | Client-side normalization |
| 9 | Major | Project repair order/info; Grove path wrong | Resolver rewritten; repair cut |
| 10 | Major | Oversized decode unbounded; batch 413 | 64 MiB ceiling; 413 halving |
| 11 | Major | Composite Codex preambles | Segmentation pass |
| 12 | Simplify | Cut dual Cursor backend, rewrite replay, manual child extraction, epochs, broad reproject, `--all-stale` claims | Accepted |

### Review 3 (Codex astra low; rev 3 → rev 4)

| # | Sev | Finding | Status |
|---|---|---|---|
| 1 | Major | Timestamp-based watermark init skips messages | All sessions init NULL; load by id range |
| 2 | Major | "Small by construction" untrue; record spanning POSTs | 16 KiB fragmentation; (intra-record checkpoint later cut in rev 5 for a one-record-per-POST invariant) |
| 3 | Major | Token bucket burst/startup undefined; not a spend bound | Call-rate limit: burst 60, startup 0, per attempt, before classifier |
| 4 | Major | AGENTS-prefix rule drops trailing requests | Single segmentation pass |
| 5.5 | Partial | Repeated subagent results | (`seen_results` later cut in rev 5: 442 occurrences, `:result:<n>` suffices) |
| 6 | Simplify | `.backup` only; dry-run non-mutating; `overwrite` API field; drop `epoch` | Accepted (repair endpoint itself cut in rev 5) |

### Review 4 (Claude Opus 5; rev 4 → rev 5)

| # | Sev | Finding | Resolution |
|---|---|---|---|
| 1 | Blocker | "Advance only on full success" + 300 calls/h + existing 15/30-min contexts = permanent livelock for 9 measured Codex sessions | Passes bounded to 400 messages; `DeadlineExceeded` aborts the pass; worker timeout replaces cron/HTTP contexts |
| 2 | Blocker | Destructive DELETE runs through FTS triggers; index integrity never checked before the point of no return | Preflight `integrity-check, 1` on the copy; post-delete FTS `'rebuild'` |
| 3 | Major | Unbounded failure rows; `coveredUUIDs` decodes all; no give-up → head-of-line blocking | `extract_attempts` + poison after 4; SQL `error IS NULL` filter; 30-day error-row pruning |
| 4 | Major | 409 is a new permanent error the watcher retries forever | 409 quarantines the file + `ownership_conflicts` |
| 5 | Major | Default `mode` undefined between PR-0a and PR-0b | Missing = `auto`; response carries `queued` + `reason`; PR-0b before Stage B |
| 6 | Major | Wrapper list incomplete (`turn_aborted` 320, `user_action` 170 = 14.5 % of kept parts) and two entries don't exist | Table rebuilt from the census; `unknown_leading_tag` counter |
| 7 | Major | `forked_from_id` sessions re-extract copied history | Measured, surfaced, accepted unless a fork ordinal exists (§7.4) |
| 8 | Major | Extract endpoint loads the whole session to authorize | `SELECT user_id` (D9) |
| 9 | Major | Cursor design built on probe data; part types, `tool` role, subagents, missing `store.db`, encryption all wrong or absent | Cursor deferred to Phase 3b with corrected observations |
| 10 | Major | Client counters have no surface | `client_counters` piggybacked on ingest, persisted per session, aggregated in stats |
| 11 | Major | Subagent attribution mechanism unnamed | `agent_path` / `/root` rule |
| 12 | Major | `subagent_history_start_ordinal` and `multi_agent_version` unused; 474 MB duplicate ingest; shape sniffing | Both fields used |
| 13 | Minor | `git -C <cwd>` executes config from historical worktrees | No git subprocess; parse `.git`/`commondir` directly |
| 14 | Minor | Cron ordering starves | Least-recently-attempted order |
| 15 | Minor | Claude `raw_cwd` unobtainable on mid-file resume | Head-read on first sight, persisted in state |
| 16 | Minor | "Eligible once" contradiction; no defaults for gates | Rewritten; defaults stated |
| 17 | Minor | Codex tool rows silently never extracted | Stated as a deliberate divergence |
| 18 | Minor | Per-platform counts = full join scan | `platform` denormalized onto messages |
| 19 | Minor | Migration window unbounded; boot-loop risk; MCP outage unstated | Measured durations, restart-policy check, outage stated |
| 20 | Minor | Wire shape omits epoch; new columns' upsert semantics unstated | `epoch = 0` server-side; insert-only stated |
| 21 | Nit | Counts and line numbers drifted; `turn_context` missing; `token_count` doesn't exist; second `session_meta` | Corrected throughout; second-meta rule added |
| S1–S6 | Simplify | Cut intra-record checkpoint, `seen_results`, Cursor, `reproject`, shape sniffing; state tool-truncation purpose | All accepted |
