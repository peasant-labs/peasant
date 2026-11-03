SELECT
    f.session_id,
    f.entry_index,
    e.role,
    COALESCE(p.canonical_cwd, s.project_hash, '') AS project,
    s.project_hash,
    snippet(session_entries_fts, -1, '[', ']', '…', 12) AS snippet,
    bm25(session_entries_fts) AS rank,
    s.model_harness,
    COALESCE(s.git_branch, ''),
    COALESCE(p.canonical_remote, ''),
    COALESCE(p.canonical_cwd, ''),
    COALESCE(s.git_worktree, '')
FROM session_entries_fts f
JOIN session_entries e ON e.rowid = f.rowid
JOIN sessions s        ON s.session_id = f.session_id
LEFT JOIN projects p   ON p.project_hash = s.project_hash
WHERE session_entries_fts MATCH ?
  AND COALESCE(e.part_type, '') <> 'pi.carrier'
  AND NOT EXISTS (
    SELECT 1 FROM sessions s2
    JOIN session_generations g ON g.session_id = s2.session_id AND g.generation_id = s2.active_generation_id
    WHERE s2.session_id = e.session_id
  )
  AND NOT (
    e.depth > 0
    AND e.content_preview IS NOT NULL
    AND e.content_preview = (
      SELECT pe.content_preview FROM session_entries pe
      WHERE pe.session_id = e.session_id AND pe.entry_index = e.parent_index
    )
  )
ORDER BY rank, f.session_id, f.entry_index
LIMIT ? OFFSET ?