SELECT
    f.session_id,
    f.entry_index,
    COALESCE(e.role, b.role) AS role,
    COALESCE(p.canonical_cwd, s.project_hash, '') AS project,
    s.project_hash,
    snippet(session_search_fts, -1, '[', ']', '…', 12) AS snippet,
    bm25(session_search_fts) AS rank,
    s.model_harness,
    COALESCE(s.git_branch, ''),
    COALESCE(p.canonical_remote, ''),
    COALESCE(p.canonical_cwd, ''),
    COALESCE(s.git_worktree, '')
FROM session_search_fts f
LEFT JOIN session_entries e ON e.rowid = f.rowid AND f.rowid < 1125899906842624
LEFT JOIN session_entry_bodies b ON b.body_id = f.rowid AND f.rowid >= 1125899906842624
JOIN sessions s        ON s.session_id = f.session_id
LEFT JOIN projects p   ON p.project_hash = s.project_hash
WHERE session_search_fts MATCH ?
  AND (
    (
      f.rowid < 1125899906842624 AND e.session_id IS NOT NULL
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
    )
    OR
    (
      f.rowid >= 1125899906842624 AND b.session_id IS NOT NULL
      AND COALESCE(b.part_type, '') <> 'pi.carrier'
      AND EXISTS (
        SELECT 1 FROM sessions s2
        JOIN session_generations g ON g.session_id = s2.session_id AND g.generation_id = s2.active_generation_id
        JOIN session_generation_entries m ON m.session_id = b.session_id AND m.generation_id = g.generation_id AND m.partition_id = 0 AND m.body_digest = b.body_digest
        WHERE s2.session_id = b.session_id
      )
      AND NOT (
        b.depth > 0
        AND b.content_preview IS NOT NULL
        AND b.parent_index IS NOT NULL
        AND b.content_preview = (
          SELECT pb.content_preview
          FROM session_generation_entries pm
          JOIN session_entry_bodies pb ON pb.session_id = pm.session_id AND pb.body_digest = pm.body_digest
          WHERE pm.session_id = b.session_id
            AND pm.generation_id = (SELECT active_generation_id FROM sessions WHERE session_id = b.session_id)
            AND pm.partition_id = 0
            AND pm.entry_index = b.parent_index
        )
      )
    )
  )
ORDER BY rank, f.session_id, f.entry_index
LIMIT ? OFFSET ?