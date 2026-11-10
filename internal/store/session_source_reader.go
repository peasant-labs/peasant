package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// The store is the SessionSourceReader the scoped LOCATE reads through. Both
// statements are indexed reads: the first seeks the session primary key, the
// second drives from the named parents and seeks idx_sessions_parent and the
// durable target indexes.
var _ ingest.SessionSourceReader = (*Store)(nil)

// independentAdmissionHarnesses is the closed set of harnesses whose children
// are admitted on their own selection evidence and may therefore be stored
// before their parent. It is derived from the one predicate, never restated.
func independentAdmissionHarnesses() []ingest.Harness {
	harnesses := make([]ingest.Harness, 0, len(ingest.AllHarnesses))
	for _, harness := range ingest.AllHarnesses {
		if ingest.IndependentAdmissionHarness(harness) {
			harnesses = append(harnesses, harness)
		}
	}
	return harnesses
}

func storedSessionSourcesSQL(count int) string {
	return `SELECT s.session_id, s.model_harness, s.source_path, s.source_format, ` +
		LogicalParentSQL("s") + `, s.project_hash
FROM sessions s WHERE s.session_id IN (` + sqlPlaceholders(count) + `)
ORDER BY s.session_id`
}

// storedCachedChildrenSQL reads the cached half of StoredChildren: rows whose
// availability cache is populated, through the partial parent index. The
// logical-parent answer is selected beside the cache key so a row whose durable
// evidence disagrees still reports the authoritative parent.
func storedCachedChildrenSQL(count int) string {
	return `SELECT s.session_id, s.model_harness, s.source_path, s.source_format, s.parent_id, ` +
		LogicalParentSQL("s") + `, s.project_hash
FROM sessions s WHERE s.parent_id IN (` + sqlPlaceholders(count) + `)
ORDER BY s.parent_id, s.session_id`
}

// storedRelationshipChildrenSQL reads the durable half of StoredChildren for a
// generation that carries started_by evidence. The target list is the fixed
// outer loop, so the statement seeks idx_relationship_evidence_started_by_target
// rather than scanning the stored population. The trailing %s is the harness
// placeholder list, derived from independentAdmissionHarnesses.
var storedRelationshipChildrenSQL = `SELECT s.session_id, s.model_harness, s.source_path, s.source_format, r.target_local_id, s.project_hash
FROM json_each(?) AS target
CROSS JOIN session_relationship_evidence r
CROSS JOIN sessions s
WHERE r.kind = 'started_by' AND r.target_local_id = target.value
  AND s.session_id = r.session_id AND r.generation_id = ` + parentGenerationScopeSQL("s") + `
  AND target.value IS NOT NULL
  AND r.target_local_id IS NOT NULL AND r.target_local_id <> ''
  AND r.target_state IN ('target_known','target_known_retained')
  AND s.parent_id IS NULL
  AND s.model_harness IN (%s)
ORDER BY r.target_local_id, s.session_id`

// storedPublicationChildrenSQL reads the durable half of StoredChildren for a
// legacy pair with no generation evidence: the publication metadata's
// parentUuid, through its expression index. A session that carries relationship
// evidence is excluded, so the legacy snapshot can never resurrect a parent the
// generation cleared or pointed elsewhere. The trailing %s is the harness
// placeholder list.
var storedPublicationChildrenSQL = `SELECT s.session_id, s.model_harness, s.source_path, s.source_format, json_extract(p.metadata_json, '$.parentUuid'), s.project_hash
FROM json_each(?) AS target
CROSS JOIN session_publication_metadata p
CROSS JOIN sessions s
WHERE json_extract(p.metadata_json, '$.parentUuid') = target.value
  AND s.session_id = p.session_id
  AND json_extract(p.metadata_json, '$.parentUuid') IS NOT NULL
  AND target.value IS NOT NULL
  AND s.parent_id IS NULL
  AND s.model_harness IN (%s)
  AND NOT EXISTS (
    SELECT 1 FROM session_relationship_evidence r
    WHERE r.session_id = s.session_id
      AND r.generation_id = ` + parentGenerationScopeSQL("s") + `
  )
ORDER BY json_extract(p.metadata_json, '$.parentUuid'), s.session_id`

// storedChildrenSQL renders one durable-children statement for the harness
// count. Production and its query-plan guard call the same renderer.
func storedChildrenSQL(template string, harnessCount int) string {
	return fmt.Sprintf(template, strings.TrimSuffix(strings.Repeat("?,", harnessCount), ","))
}

// StoredSessionSources returns the stored rows named by ids. It is one batched
// primary-key read; no row outside ids is touched.
func (s *Store) StoredSessionSources(ctx context.Context, ids []ingest.SessionID) (map[ingest.SessionID]ingest.StoredSessionSource, error) {
	result := make(map[ingest.SessionID]ingest.StoredSessionSource, len(ids))
	unique := uniqueSessionIDs(ids)
	if len(unique) == 0 {
		return result, nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection to read stored session sources: %w; no source record was read; restore database access and retry", err)
	}
	defer s.pool.Put(conn)

	args := make([]any, len(unique))
	for index, id := range unique {
		args[index] = string(id)
	}
	if err := sqlitex.ExecuteTransient(conn, storedSessionSourcesSQL(len(unique)), &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			source, err := scanStoredSessionSource(stmt, 4, 5)
			if err != nil {
				return err
			}
			result[source.ID] = source
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: read stored session sources: %w; no source record was returned; repair database access and retry", err)
	}
	return result, nil
}

// StoredChildren returns the stored children of the named parents, keyed by
// the parent whose row named them.
//
// It reads both halves. The cached half seeks idx_sessions_parent. The durable
// half seeks the started_by target index and the publication-metadata
// parentUuid expression index, restricted to the independently admitted
// harnesses. A child that appears in both durable sources is returned once.
func (s *Store) StoredChildren(ctx context.Context, parents []ingest.SessionID) (map[ingest.SessionID][]ingest.StoredSessionSource, error) {
	result := make(map[ingest.SessionID][]ingest.StoredSessionSource, len(parents))
	uniqueParents := uniqueSessionIDs(parents)
	if len(uniqueParents) == 0 {
		return result, nil
	}
	harnesses := independentAdmissionHarnesses()
	if len(harnesses) == 0 {
		return result, nil
	}

	targets := make([]string, len(uniqueParents))
	for index, parent := range uniqueParents {
		targets[index] = string(parent)
	}
	targetsJSON, err := json.Marshal(targets)
	if err != nil {
		return nil, fmt.Errorf("store: encode stored parent targets for child reconciliation: %w; no child record was read; retry the read", err)
	}

	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection to read stored children: %w; no child record was read; restore database access and retry", err)
	}
	defer s.pool.Put(conn)

	// groupKey -> child ID -> one source. The group key is the parent whose
	// row or evidence named the child, so the map answers the caller's question
	// even when a cached row's logical-parent answer disagrees with its cache.
	groups := make(map[ingest.SessionID]map[ingest.SessionID]ingest.StoredSessionSource, len(uniqueParents))
	record := func(parent, child ingest.SessionID, source ingest.StoredSessionSource) {
		children, ok := groups[parent]
		if !ok {
			children = make(map[ingest.SessionID]ingest.StoredSessionSource)
			groups[parent] = children
		}
		children[child] = source
	}

	parentArgs := make([]any, len(uniqueParents))
	for index, parent := range uniqueParents {
		parentArgs[index] = string(parent)
	}
	if err := sqlitex.ExecuteTransient(conn, storedCachedChildrenSQL(len(uniqueParents)), &sqlitex.ExecOptions{
		Args: parentArgs,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			parent, err := scanSessionIDColumn(stmt, 4)
			if err != nil {
				return err
			}
			source, err := scanStoredSessionSource(stmt, 5, 6)
			if err != nil {
				return err
			}
			record(parent, source.ID, source)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: read cached stored children: %w; no child record was returned; repair database access and retry", err)
	}

	harnessArgs := make([]any, 0, len(harnesses))
	for _, harness := range harnesses {
		harnessArgs = append(harnessArgs, string(harness))
	}
	targetArgs := append([]any{string(targetsJSON)}, harnessArgs...)
	for _, statement := range []string{
		storedChildrenSQL(storedRelationshipChildrenSQL, len(harnesses)),
		storedChildrenSQL(storedPublicationChildrenSQL, len(harnesses)),
	} {
		if err := sqlitex.ExecuteTransient(conn, statement, &sqlitex.ExecOptions{
			Args: targetArgs,
			ResultFunc: func(stmt *sqlite.Stmt) error {
				parent, err := scanSessionIDColumn(stmt, 4)
				if err != nil {
					return err
				}
				source, err := scanStoredSessionSource(stmt, 4, 5)
				if err != nil {
					return err
				}
				record(parent, source.ID, source)
				return nil
			},
		}); err != nil {
			return nil, fmt.Errorf("store: read durable stored children: %w; no child record was returned; repair database access and retry", err)
		}
	}

	for parent, children := range groups {
		ordered := make([]ingest.StoredSessionSource, 0, len(children))
		for _, child := range children {
			ordered = append(ordered, child)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
		result[parent] = ordered
	}
	return result, nil
}

// scanStoredSessionSource reads the shared session-source columns. parentColumn
// is the column that carries the logical parent; -1 means the statement has
// none and the parent is left nil.
func scanStoredSessionSource(stmt *sqlite.Stmt, parentColumn, projectColumn int) (ingest.StoredSessionSource, error) {
	id, err := ingest.NewSessionID(stmt.ColumnText(0))
	if err != nil {
		return ingest.StoredSessionSource{}, fmt.Errorf("store: stored session source carries an invalid id %q: %w; the row cannot be located; repair the stored identity before retrying", stmt.ColumnText(0), err)
	}
	harness := ingest.Harness(stmt.ColumnText(1))
	if !harness.IsKnown() {
		return ingest.StoredSessionSource{}, fmt.Errorf("store: stored session source %s carries unknown harness %q; this build cannot re-read it; upgrade Peasant before retrying", id, stmt.ColumnText(1))
	}
	format := ingest.SourceFormat(stmt.ColumnText(3))
	if !format.IsValid() {
		return ingest.StoredSessionSource{}, fmt.Errorf("store: stored session source %s carries unknown source format %q; this build cannot re-read it; upgrade Peasant before retrying", id, stmt.ColumnText(3))
	}
	projectHash, err := ingest.NewProjectHash(stmt.ColumnText(projectColumn))
	if err != nil {
		return ingest.StoredSessionSource{}, fmt.Errorf("store: stored session source %s carries an invalid project hash %q: %w; the row cannot be located; repair the stored identity before retrying", id, stmt.ColumnText(projectColumn), err)
	}
	source := ingest.StoredSessionSource{
		ID:          id,
		Harness:     harness,
		Path:        ingest.ResolvedPath(stmt.ColumnText(2)),
		Format:      format,
		ProjectHash: projectHash,
	}
	if parentColumn >= 0 && stmt.ColumnType(parentColumn) != sqlite.TypeNull && stmt.ColumnText(parentColumn) != "" {
		parent, err := ingest.NewSessionID(stmt.ColumnText(parentColumn))
		if err != nil {
			return ingest.StoredSessionSource{}, fmt.Errorf("store: stored session source %s carries an invalid logical parent %q: %w; the row cannot be located; repair the stored identity before retrying", id, stmt.ColumnText(parentColumn), err)
		}
		source.Parent = &parent
	}
	return source, nil
}

func scanSessionIDColumn(stmt *sqlite.Stmt, column int) (ingest.SessionID, error) {
	id, err := ingest.NewSessionID(stmt.ColumnText(column))
	if err != nil {
		return "", fmt.Errorf("store: stored child row carries an invalid parent id %q: %w; the row cannot be grouped; repair the stored identity before retrying", stmt.ColumnText(column), err)
	}
	return id, nil
}

func uniqueSessionIDs(ids []ingest.SessionID) []ingest.SessionID {
	unique := make([]ingest.SessionID, 0, len(ids))
	seen := make(map[ingest.SessionID]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	return unique
}
