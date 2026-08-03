-- ============================================================================
-- Peasant Database Query Scripts
-- Run with: sqlite3 ~/.local/share/peasant/peasant.db < query.sql
-- Or:       sqlite3 ~/.local/share/peasant/peasant.db -header -column
-- ============================================================================

-- ----------------------------------------------------------------------------
-- 1. DATABASE OVERVIEW
-- ----------------------------------------------------------------------------

-- List all tables
SELECT name FROM sqlite_master WHERE type='table' ORDER BY name;

-- Show schema for a specific table
.schema sessions
.schema session_entries
.schema session_metrics

-- Show table stats (row counts)
SELECT 'sessions' as table_name, COUNT(*) as rows FROM sessions
UNION ALL SELECT 'session_entries', COUNT(*) FROM session_entries
UNION ALL SELECT 'session_metrics', COUNT(*) FROM session_metrics
UNION ALL SELECT 'projects', COUNT(*) FROM projects
UNION ALL SELECT 'host_slugs', COUNT(*) FROM host_slugs
UNION ALL SELECT 'daily_summary', COUNT(*) FROM daily_summary
UNION ALL SELECT 'ingest_log', COUNT(*) FROM ingest_log
UNION ALL SELECT 'push_log', COUNT(*) FROM push_log
UNION ALL SELECT 'session_entries_ext', COUNT(*) FROM session_entries_ext
UNION ALL SELECT 'models', COUNT(*) FROM models
UNION ALL SELECT 'daily_summary_by_project', COUNT(*) FROM daily_summary_by_project;

-- ----------------------------------------------------------------------------
-- 2. SESSIONS
-- ----------------------------------------------------------------------------

-- All sessions with basic info
SELECT 
    session_id,
    model_harness,
    model_id,
    host_slug,
    project_hash,
    datetime(start_ms/1000, 'unixepoch') as start_time,
    datetime(end_ms/1000, 'unixepoch') as end_time,
    source_format,
    pushed_at
FROM sessions
ORDER BY start_ms DESC
LIMIT 20;

-- Sessions by provider
SELECT model_harness, COUNT(*) as count FROM sessions GROUP BY model_harness;

-- Recent sessions (last 7 days)
SELECT 
    session_id,
    model_harness,
    datetime(start_ms/1000, 'unixepoch') as start_time
FROM sessions
WHERE start_ms > (strftime('%s', 'now') * 1000 - 7*24*60*60*1000)
ORDER BY start_ms DESC;

-- Sessions with tags
SELECT session_id, tags FROM sessions WHERE tags IS NOT NULL;

-- Sessions that have been pushed to village
SELECT session_id, datetime(pushed_at/1000, 'unixepoch') as pushed_at 
FROM sessions 
WHERE pushed_at IS NOT NULL
ORDER BY pushed_at DESC;

-- ----------------------------------------------------------------------------
-- 3. SESSION ENTRIES (TRANSCRIPTS)
-- ----------------------------------------------------------------------------

-- Entry counts per session
SELECT 
    session_id,
    COUNT(*) as total_entries,
    SUM(has_tool_use) as tool_uses,
    SUM(has_thinking) as thinking,
    SUM(is_error) as errors
FROM session_entries
GROUP BY session_id
LIMIT 20;

-- Entries by type for a session
SELECT 
    session_id,
    entry_type,
    role,
    COUNT(*) as count
FROM session_entries
WHERE session_id = (SELECT session_id FROM sessions LIMIT 1)
GROUP BY entry_type, role;

-- Entries with depth > 0 (full-depth indexing - v4)
SELECT session_id, depth, COUNT(*) as count 
FROM session_entries 
WHERE depth > 0 
GROUP BY session_id, depth
LIMIT 20;

-- Tool names used per session
SELECT session_id, tool_names_csv, COUNT(*) as uses
FROM session_entries 
WHERE has_tool_use = 1
GROUP BY session_id, tool_names_csv
LIMIT 20;

-- ----------------------------------------------------------------------------
-- 4. SESSION METRICS
-- ----------------------------------------------------------------------------

-- Sessions with metrics
SELECT 
    s.session_id,
    s.model_harness,
    sm.turn_count,
    sm.tool_calls,
    sm.input_tokens,
    sm.output_tokens,
    sm.total_tokens,
    sm.duration_minutes,
    sm.outcome,
    sm.cost_total_usd
FROM sessions s
JOIN session_metrics sm ON s.session_id = sm.session_id
ORDER BY s.start_ms DESC
LIMIT 20;

-- Cost analytics (v7 columns)
SELECT 
    s.session_id,
    sm.cost_input_usd,
    sm.cost_output_usd,
    sm.cost_reasoning_usd,
    sm.cost_total_usd,
    sm.cost_model_id
FROM sessions s
JOIN session_metrics sm ON s.session_id = sm.session_id
WHERE sm.cost_total_usd IS NOT NULL
ORDER BY sm.cost_total_usd DESC
LIMIT 20;

-- Sessions by outcome
SELECT outcome, COUNT(*) as count FROM session_metrics WHERE outcome IS NOT NULL GROUP BY outcome;

-- ----------------------------------------------------------------------------
-- 5. DAILY SUMMARIES
-- ----------------------------------------------------------------------------

-- Daily summary by date
SELECT 
    date_utc,
    session_count,
    tokens_in,
    tokens_out,
    tokens_total,
    avg_duration_ms,
    avg_turns,
    tool_call_count,
    total_cost_usd
FROM daily_summary
ORDER BY date_utc DESC
LIMIT 30;

-- Daily summary by harness (provider)
SELECT 
    date_utc,
    model_harness,
    session_count,
    tokens_total,
    tool_call_count
FROM daily_summary_harness
ORDER BY date_utc DESC, model_harness
LIMIT 30;

-- Daily summary by project
SELECT 
    date_utc,
    project_hash,
    project_path,
    session_count,
    total_tokens,
    total_cost_usd,
    acceptance_rate
FROM daily_summary_by_project
ORDER BY date_utc DESC
LIMIT 30;

-- ----------------------------------------------------------------------------
-- 6. MODELS (V6 - models.dev)
-- ----------------------------------------------------------------------------

-- All models
SELECT 
    model_id,
    provider_key,
    display_name,
    family,
    context_window,
    max_output,
    reasoning,
    tool_call,
    cost_input_per_mtok,
    cost_output_per_mtok
FROM models
ORDER BY provider_key, family;

-- Models by provider
SELECT provider_key, COUNT(*) as count FROM models GROUP BY provider_key;

-- Models by family
SELECT family, COUNT(*) as count FROM models WHERE family IS NOT NULL GROUP BY family;

-- ----------------------------------------------------------------------------
-- 7. SESSION ENTRIES EXT (V5 - EAV)
-- ----------------------------------------------------------------------------

-- Extended attributes per session
SELECT 
    session_id,
    entry_index,
    key,
    value_text,
    value_int
FROM session_entries_ext
LIMIT 30;

-- Reasoning tokens per session
SELECT 
    session_id,
    SUM(value_int) as total_reasoning_tokens
FROM session_entries_ext
WHERE key = 'tokens_reasoning'
GROUP BY session_id
ORDER BY total_reasoning_tokens DESC
LIMIT 20;

-- Cache usage per session
SELECT 
    session_id,
    SUM(CASE WHEN key = 'cache_read' THEN value_int ELSE 0 END) as cache_read,
    SUM(CASE WHEN key = 'cache_write' THEN value_int ELSE 0 END) as cache_write
FROM session_entries_ext
GROUP BY session_id
LIMIT 20;

-- ----------------------------------------------------------------------------
-- 8. INGEST LOG
-- ----------------------------------------------------------------------------

-- Recent ingest runs
SELECT 
    id,
    datetime(started_at/1000, 'unixepoch') as started,
    datetime(finished_at/1000, 'unixepoch') as finished,
    sessions_new,
    sessions_updated,
    sessions_unchanged,
    sessions_error,
    indexed_count,
    computed_count,
    error_message,
    source_path
FROM ingest_log
ORDER BY started_at DESC
LIMIT 10;

-- ----------------------------------------------------------------------------
-- 9. PUSH LOG
-- ----------------------------------------------------------------------------

-- Recent push runs
SELECT 
    id,
    datetime(started_at/1000, 'unixepoch') as started,
    datetime(finished_at/1000, 'unixepoch') as finished,
    village_url,
    sessions_pushed,
    sessions_updated,
    sessions_skipped,
    sessions_failed,
    user_id,
    username,
    error_message
FROM push_log
ORDER BY started_at DESC
LIMIT 10;

-- ----------------------------------------------------------------------------
-- 10. DEBUG: Check for issues
-- ----------------------------------------------------------------------------

-- Sessions without metrics
SELECT s.session_id 
FROM sessions s 
LEFT JOIN session_metrics sm ON s.session_id = sm.session_id 
WHERE sm.session_id IS NULL
LIMIT 10;

-- Entries with NULL timestamps
SELECT session_id, entry_index, entry_type 
FROM session_entries 
WHERE timestamp_ms IS NULL
LIMIT 10;

-- Check for duplicate sessions
SELECT session_id, COUNT(*) as dupes 
FROM sessions 
GROUP BY session_id 
HAVING COUNT(*) > 1;

-- Check session_entries_ext FK integrity
SELECT COUNT(*) as orphaned_ext_rows
FROM session_entries_ext e
LEFT JOIN session_entries s ON e.session_id = s.session_id AND e.entry_index = s.entry_index
WHERE s.session_id IS NULL;
