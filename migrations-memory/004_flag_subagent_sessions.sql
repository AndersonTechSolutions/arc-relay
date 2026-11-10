-- migrations-memory/004_flag_subagent_sessions.sql
-- Watchers before this fix never sent is_subagent, so subagent transcripts
-- (<project>/<parent-session>/subagents/agent-*.jsonl, project_dir
-- "/subagents") were auto-extracted into mem0 as agent_id
-- "transcripts-subagents". Flag the existing sessions so the auto gate
-- (is_subagent = 0) skips them from now on. Ingest infers the flag from the
-- path for new sessions (memory.isSubagentPath).
UPDATE memory_sessions
SET is_subagent = 1
WHERE is_subagent = 0
  AND (file_path LIKE '%/subagents/%' OR project_dir = '/subagents');
