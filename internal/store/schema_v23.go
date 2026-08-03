package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// buildMigrationV23 constructs the V23 migration SQL string.
//
// V23: Opaque PKs + canonical_cwd — this SQL portion creates the empty v2
// tables. The actual Go-driven data migration (HMAC computation) is performed
// by applyV23DataMigration, called from Open() after sqlitemigration.Migrate
// completes.
//
// Split design rationale: sqlitemigration.MigrationOptions does not support
// GoFunc (as of v1.4.2). HMAC-SHA256 computation requires Go code. We therefore
// split V23 into two phases:
//   - Phase 1 (SQL, in migration string): create v2 tables with new schema
//   - Phase 2 (Go, in Open()): populate v2 tables from old tables using HMAC,
//     drop old tables, rename v2 → final names
//
// Foreign keys are disabled via MigrationOptions.DisableForeignKeys (V16 precedent).
// PRAGMA legacy_alter_table = ON prevents FK auto-rewrite during ALTER TABLE RENAME.
func buildMigrationV23() string {
	var b strings.Builder

	// PRAGMA setup — same rationale as V16.
	b.WriteString("PRAGMA legacy_alter_table = ON;\n\n")

	// Create empty v2 tables with new schema.
	// Data population happens in applyV23DataMigration (Go-driven).
	b.WriteString(v23CreateHostSlugsV2)
	b.WriteString(";\n\n")
	b.WriteString(v23CreateProjectsV2)
	b.WriteString(";\n\n")
	b.WriteString(v23CreateSessionsV2)
	b.WriteString(";\n\n")
	b.WriteString(v23CreateDailySummaryByProjectV2)
	b.WriteString(";\n\n")
	b.WriteString(v23CreateFileFamiliarityV2)
	b.WriteString(";\n\n")

	// Restore PRAGMA.
	b.WriteString("PRAGMA legacy_alter_table = OFF")

	return b.String()
}

// applyV23DataMigration performs the Go-driven portion of migration V23.
//
// This function is called from Open() after sqlitemigration.Migrate completes.
// It is idempotent: checks whether old tables (pre-rename) are still present
// before running. If old tables have already been dropped and v2 tables renamed,
// this is a no-op.
//
// Migration steps:
//  1. Check if migration is needed (old 'host_slugs' table with 'host_slug' PK exists)
//  2. Load/generate salt via salt.Load(pool)
//  3. Read all host_slugs rows: compute opaque_id = HMAC-SHA256(salt, canonical_remote)
//  4. Read all projects rows: compute new project_hash = HMAC-SHA256(salt, canonical_remote)
//  5. Populate v2 tables (host_slugs_v2, projects_v2, sessions_v2,
//     daily_summary_by_project_v2, file_familiarity_v2)
//  6. Drop old tables (file_familiarity, daily_summary_by_project, sessions,
//     host_slugs, projects) in dependency order
//  7. Rename v2 tables to final names
//  8. Recreate indexes
//
// On error, returns a wrapped error describing what failed and how to fix it.
func applyV23DataMigration(pool *sqlitex.Pool) error {
	conn, err := pool.Take(context.Background())
	if err != nil {
		return fmt.Errorf(
			"store.applyV23DataMigration: take DB connection: %w — "+
				"ensure the SQLite pool is open before calling Open()",
			err,
		)
	}
	defer pool.Put(conn)

	// Step 0: Idempotency check — if host_slugs already has 'opaque_id' PK column,
	// the Go migration phase has already run (or this is a fresh install with the
	// new V23 schema). In either case the data migration is not needed.
	//
	// However, for a fresh install the V23 SQL phase still creates the _v2 tables
	// (sqlitemigration always runs every migration). We must clean them up here.
	var alreadyMigrated bool
	err = sqlitex.ExecuteTransient(conn,
		`SELECT COUNT(*) FROM pragma_table_info('host_slugs') WHERE name='opaque_id'`,
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				alreadyMigrated = stmt.ColumnInt(0) > 0
				return nil
			},
		},
	)
	if err != nil {
		return fmt.Errorf("store.applyV23DataMigration: check migration state: %w", err)
	}

	// Check if v2 tables exist (created by the SQL phase of V23).
	var v2TablesExist bool
	err = sqlitex.ExecuteTransient(conn,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='host_slugs_v2'`,
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				v2TablesExist = stmt.ColumnInt(0) > 0
				return nil
			},
		},
	)
	if err != nil {
		return fmt.Errorf("store.applyV23DataMigration: check v2 tables: %w", err)
	}

	if alreadyMigrated {
		// Data migration already done (or fresh install). But the SQL phase may have
		// left orphan _v2 tables (fresh install case). Drop them if present.
		if v2TablesExist {
			if err = dropV23TempTables(conn); err != nil {
				return fmt.Errorf("store.applyV23DataMigration: drop orphan v2 tables: %w", err)
			}
		}
		return nil
	}

	if !v2TablesExist {
		// V23 SQL migration hasn't run yet; nothing to do.
		return nil
	}

	// Step 1: Load/generate salt.
	s, _, err := salt.Load(pool)
	if err != nil {
		return fmt.Errorf(
			"store.applyV23DataMigration: load salt: %w — "+
				"what: failed to read or create the installation salt; "+
				"why: the _install_salt table may be inaccessible or the DB pool unavailable; "+
				"fix: ensure the database is open with write permissions before running migrations",
			err,
		)
	}

	// Step 2: Read all host_slugs rows and compute opaque_id.
	type hostSlugRow struct {
		hostSlug        string
		gitRemote       *string
		canonicalRemote string
		opaqueID        string
	}
	var hostSlugs []hostSlugRow
	err = sqlitex.ExecuteTransient(conn,
		`SELECT host_slug, git_remote FROM host_slugs`,
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				r := hostSlugRow{
					hostSlug: stmt.ColumnText(0),
				}
				if stmt.ColumnType(1) != sqlite.TypeNull {
					v := stmt.ColumnText(1)
					r.gitRemote = &v
				}

				// Compute canonical_remote and opaque_id.
				var hashInput string
				if r.gitRemote != nil && *r.gitRemote != "" {
					canonical, normErr := ingest.NormalizeRemoteURL(*r.gitRemote)
					if normErr == nil {
						r.canonicalRemote = canonical
						hashInput = canonical
					} else {
						// Normalize failed (e.g. file:// or unrecognized format).
						// Use raw git_remote as canonical_remote and hash input.
						r.canonicalRemote = *r.gitRemote
						hashInput = r.hostSlug
					}
				} else {
					// No git_remote: use host_slug as hash input.
					hashInput = r.hostSlug
				}

				ph, hashErr := s.Hash(hashInput)
				if hashErr != nil {
					return fmt.Errorf(
						"store.applyV23DataMigration: hash host_slug %q (input=%q): %w",
						r.hostSlug, hashInput, hashErr,
					)
				}
				r.opaqueID = string(ph)
				hostSlugs = append(hostSlugs, r)
				return nil
			},
		},
	)
	if err != nil {
		return fmt.Errorf(
			"store.applyV23DataMigration: read host_slugs: %w — "+
				"what: failed to read host_slugs rows; "+
				"why: the host_slugs table may be corrupt or inaccessible; "+
				"fix: inspect the host_slugs table via sqlite3",
			err,
		)
	}

	// Build host_slug → opaque_id and host_slug → canonicalRemote maps.
	hostSlugToOpaqueID := make(map[string]string, len(hostSlugs))
	hostSlugToCanonicalRemote := make(map[string]string, len(hostSlugs))
	for _, r := range hostSlugs {
		hostSlugToOpaqueID[r.hostSlug] = r.opaqueID
		if r.canonicalRemote != "" {
			hostSlugToCanonicalRemote[r.hostSlug] = r.canonicalRemote
		}
	}

	// Step 3: Read all projects rows and compute new project_hash.
	// canonical_remote is derived via session → host_slug → canonical_remote chain.
	projectCanonicalRemote := make(map[string]string) // old_project_hash → canonical_remote
	err = sqlitex.ExecuteTransient(conn,
		`SELECT DISTINCT s.project_hash, s.host_slug FROM sessions s`,
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				ph := stmt.ColumnText(0)
				hs := stmt.ColumnText(1)
				if cr, ok := hostSlugToCanonicalRemote[hs]; ok && cr != "" {
					// Use first canonical_remote found (all sessions for the same project
					// should resolve to the same canonical_remote).
					if _, exists := projectCanonicalRemote[ph]; !exists {
						projectCanonicalRemote[ph] = cr
					}
				}
				return nil
			},
		},
	)
	if err != nil {
		// Non-fatal: no sessions yet. Continue with no canonical_remote for projects.
		projectCanonicalRemote = make(map[string]string)
	}

	type projectRow struct {
		oldProjectHash  string
		projectPath     string
		canonicalCWD    string
		canonicalRemote string
		newProjectHash  string
	}
	var projects []projectRow
	err = sqlitex.ExecuteTransient(conn,
		`SELECT project_hash, project_path FROM projects`,
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				r := projectRow{
					oldProjectHash: stmt.ColumnText(0),
					projectPath:    stmt.ColumnText(1),
				}
				// canonical_cwd: use existing project_path.
				r.canonicalCWD = r.projectPath

				// canonical_remote: from sessions → host_slugs mapping.
				if cr, ok := projectCanonicalRemote[r.oldProjectHash]; ok {
					r.canonicalRemote = cr
				}

				// Compute new project_hash.
				var hashInput string
				if r.canonicalRemote != "" {
					hashInput = r.canonicalRemote
				} else {
					// No remote: use old project_hash as hash input.
					// This gives a deterministic but still salted output.
					hashInput = r.oldProjectHash
				}
				ph, hashErr := s.Hash(hashInput)
				if hashErr != nil {
					return fmt.Errorf(
						"store.applyV23DataMigration: hash project %q: %w",
						r.oldProjectHash, hashErr,
					)
				}
				r.newProjectHash = string(ph)
				projects = append(projects, r)
				return nil
			},
		},
	)
	if err != nil {
		return fmt.Errorf(
			"store.applyV23DataMigration: read projects: %w — "+
				"what: failed to read projects rows; "+
				"why: the projects table may be corrupt or inaccessible",
			err,
		)
	}

	// Build old→new project hash mapping.
	oldToNewProjectHash := make(map[string]string, len(projects))
	for _, r := range projects {
		oldToNewProjectHash[r.oldProjectHash] = r.newProjectHash
	}

	// All data reads are done. Now run the write phase in a single transaction.
	//
	// PRAGMA foreign_keys = OFF must be set BEFORE the transaction begins — it is
	// a no-op inside an active transaction (SQLite docs). We re-enable it via a
	// deferred call that runs AFTER the transaction commits/rolls back (LIFO order).
	var txnErr error
	if txnErr = sqlitex.ExecuteTransient(conn, "PRAGMA foreign_keys = OFF", nil); txnErr != nil {
		return fmt.Errorf("store.applyV23DataMigration: disable FK: %w", txnErr)
	}
	// Re-enable FK after the transaction ends (runs second due to LIFO).
	defer func() {
		if pragmaErr := sqlitex.ExecuteTransient(conn, "PRAGMA foreign_keys = ON", nil); pragmaErr != nil && txnErr == nil {
			txnErr = fmt.Errorf("store.applyV23DataMigration: re-enable FK: %w", pragmaErr)
		}
	}()

	endTxn := sqlitex.Transaction(conn)
	// endTxn runs first (LIFO), committing or rolling back before FK is re-enabled.
	defer endTxn(&txnErr)

	// Step 4: Populate host_slugs_v2.
	for _, r := range hostSlugs {
		var gitRemoteVal any
		if r.gitRemote != nil {
			gitRemoteVal = *r.gitRemote
		}
		var canonicalRemoteVal any
		if r.canonicalRemote != "" {
			canonicalRemoteVal = r.canonicalRemote
		}
		if txnErr = sqlitex.ExecuteTransient(conn,
			`INSERT INTO host_slugs_v2 (opaque_id, host_slug, git_remote, canonical_remote)
			VALUES (?, ?, ?, ?)`,
			&sqlitex.ExecOptions{
				Args: []any{r.opaqueID, r.hostSlug, gitRemoteVal, canonicalRemoteVal},
			},
		); txnErr != nil {
			return fmt.Errorf(
				"store.applyV23DataMigration: insert host_slugs_v2 %q: %w — "+
					"what: failed to insert row into host_slugs_v2; "+
					"why: the opaque_id %q may collide (HMAC collision is extremely unlikely) or the data is invalid; "+
					"fix: check that host_slugs has no duplicate entries before migrating",
				r.hostSlug, txnErr, r.opaqueID,
			)
		}
	}

	// Step 5: Populate projects_v2.
	for _, r := range projects {
		var canonicalCWDVal any
		if r.canonicalCWD != "" {
			canonicalCWDVal = r.canonicalCWD
		}
		var canonicalRemoteVal any
		if r.canonicalRemote != "" {
			canonicalRemoteVal = r.canonicalRemote
		}
		if txnErr = sqlitex.ExecuteTransient(conn,
			`INSERT INTO projects_v2 (project_hash, canonical_cwd, canonical_remote)
			VALUES (?, ?, ?)`,
			&sqlitex.ExecOptions{
				Args: []any{r.newProjectHash, canonicalCWDVal, canonicalRemoteVal},
			},
		); txnErr != nil {
			return fmt.Errorf(
				"store.applyV23DataMigration: insert projects_v2 %q → %q: %w — "+
					"what: failed to insert row into projects_v2; "+
					"why: the new project_hash %q may already exist; "+
					"fix: check that projects has no duplicate entries",
				r.oldProjectHash, r.newProjectHash, txnErr, r.newProjectHash,
			)
		}
	}

	// Step 6: Populate sessions_v2.
	if txnErr = sqlitex.ExecuteTransient(conn,
		`SELECT session_id, parent_id, model_harness, model_id, host_slug, project_hash,
		        start_ms, end_ms, ingested_ms, source_path, source_format,
		        schema_version, git_branch, git_worktree, git_tracking, tool_version,
		        pushed_at, tags, index_version, indexed_at
		FROM sessions`,
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				sessionID := stmt.ColumnText(0)
				var parentID any
				if stmt.ColumnType(1) != sqlite.TypeNull {
					parentID = stmt.ColumnText(1)
				}
				modelHarness := stmt.ColumnText(2)
				modelID := stmt.ColumnText(3)
				hostSlug := stmt.ColumnText(4)
				oldProjectHash := stmt.ColumnText(5)
				startMs := stmt.ColumnInt64(6)
				endMs := stmt.ColumnInt64(7)
				ingestedMs := stmt.ColumnInt64(8)
				sourcePath := stmt.ColumnText(9)
				sourceFormat := stmt.ColumnText(10)
				schemaVersion := stmt.ColumnInt(11)

				var gitBranch, gitWorktree, gitTracking, toolVersion any
				var pushedAt, indexedAt any
				var tags any

				if stmt.ColumnType(12) != sqlite.TypeNull {
					gitBranch = stmt.ColumnText(12)
				}
				if stmt.ColumnType(13) != sqlite.TypeNull {
					gitWorktree = stmt.ColumnText(13)
				}
				if stmt.ColumnType(14) != sqlite.TypeNull {
					gitTracking = stmt.ColumnText(14)
				}
				if stmt.ColumnType(15) != sqlite.TypeNull {
					toolVersion = stmt.ColumnText(15)
				}
				if stmt.ColumnType(16) != sqlite.TypeNull {
					pushedAt = stmt.ColumnInt64(16)
				}
				if stmt.ColumnType(17) != sqlite.TypeNull {
					tags = stmt.ColumnText(17)
				}
				indexVersion := stmt.ColumnInt(18)
				if stmt.ColumnType(19) != sqlite.TypeNull {
					indexedAt = stmt.ColumnInt64(19)
				}

				opaqueHostID, ok := hostSlugToOpaqueID[hostSlug]
				if !ok {
					// Orphaned session: host_slug not found in host_slugs.
					// Skip rather than aborting migration.
					return nil
				}
				newProjectHash, ok := oldToNewProjectHash[oldProjectHash]
				if !ok {
					// Orphaned session: project_hash not found in projects.
					return nil
				}

				return sqlitex.ExecuteTransient(conn,
					`INSERT INTO sessions_v2 (
						session_id, parent_id, model_harness, model_id,
						opaque_host_id, project_hash,
						start_ms, end_ms, ingested_ms, source_path, source_format,
						schema_version, git_branch, git_worktree, git_tracking, tool_version,
						pushed_at, tags, index_version, indexed_at
					) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					&sqlitex.ExecOptions{
						Args: []any{
							sessionID, parentID, modelHarness, modelID,
							opaqueHostID, newProjectHash,
							startMs, endMs, ingestedMs, sourcePath, sourceFormat,
							schemaVersion, gitBranch, gitWorktree, gitTracking, toolVersion,
							pushedAt, tags, indexVersion, indexedAt,
						},
					},
				)
			},
		},
	); txnErr != nil {
		return fmt.Errorf("store.applyV23DataMigration: migrate sessions: %w", txnErr)
	}

	// Step 7: Populate daily_summary_by_project_v2.
	if txnErr = sqlitex.ExecuteTransient(conn,
		`SELECT date_utc, project_hash, project_path, session_count, total_tokens,
		        total_tool_calls, total_duration_minutes, resolved_count, failed_count,
		        partial_count, avg_spec_quality, acceptance_rate, total_cost_usd
		FROM daily_summary_by_project`,
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				dateUTC := stmt.ColumnText(0)
				oldProjectHash := stmt.ColumnText(1)
				newProjectHash, ok := oldToNewProjectHash[oldProjectHash]
				if !ok {
					return nil // orphaned row
				}
				var projectPath any
				if stmt.ColumnType(2) != sqlite.TypeNull {
					projectPath = stmt.ColumnText(2)
				}
				sessionCount := stmt.ColumnInt(3)
				totalTokens := stmt.ColumnInt(4)
				totalToolCalls := stmt.ColumnInt(5)
				totalDurationMins := stmt.ColumnFloat(6)
				resolvedCount := stmt.ColumnInt(7)
				failedCount := stmt.ColumnInt(8)
				partialCount := stmt.ColumnInt(9)
				var avgSpecQuality, acceptanceRate, totalCostUSD any
				if stmt.ColumnType(10) != sqlite.TypeNull {
					avgSpecQuality = stmt.ColumnFloat(10)
				}
				if stmt.ColumnType(11) != sqlite.TypeNull {
					acceptanceRate = stmt.ColumnFloat(11)
				}
				if stmt.ColumnType(12) != sqlite.TypeNull {
					totalCostUSD = stmt.ColumnFloat(12)
				}
				return sqlitex.ExecuteTransient(conn,
					`INSERT INTO daily_summary_by_project_v2 (
						date_utc, project_hash, project_path, session_count, total_tokens,
						total_tool_calls, total_duration_minutes, resolved_count, failed_count,
						partial_count, avg_spec_quality, acceptance_rate, total_cost_usd
					) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					&sqlitex.ExecOptions{
						Args: []any{
							dateUTC, newProjectHash, projectPath, sessionCount, totalTokens,
							totalToolCalls, totalDurationMins, resolvedCount, failedCount,
							partialCount, avgSpecQuality, acceptanceRate, totalCostUSD,
						},
					},
				)
			},
		},
	); txnErr != nil {
		return fmt.Errorf("store.applyV23DataMigration: migrate daily_summary_by_project: %w", txnErr)
	}

	// Step 8: Populate file_familiarity_v2.
	if txnErr = sqlitex.ExecuteTransient(conn,
		`SELECT project_hash, file_path, familiarity_depth, session_count,
		        total_turns, total_human_turns, first_engaged_at, last_engaged_at
		FROM file_familiarity`,
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				oldProjectHash := stmt.ColumnText(0)
				newProjectHash, ok := oldToNewProjectHash[oldProjectHash]
				if !ok {
					return nil // orphaned row
				}
				filePath := stmt.ColumnText(1)
				familiarityDepth := stmt.ColumnInt(2)
				sessionCount := stmt.ColumnInt(3)
				totalTurns := stmt.ColumnInt(4)
				totalHumanTurns := stmt.ColumnInt(5)
				var firstEngagedAt, lastEngagedAt any
				if stmt.ColumnType(6) != sqlite.TypeNull {
					firstEngagedAt = stmt.ColumnText(6)
				}
				if stmt.ColumnType(7) != sqlite.TypeNull {
					lastEngagedAt = stmt.ColumnText(7)
				}
				return sqlitex.ExecuteTransient(conn,
					`INSERT INTO file_familiarity_v2 (
						project_hash, file_path, familiarity_depth, session_count,
						total_turns, total_human_turns, first_engaged_at, last_engaged_at
					) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
					&sqlitex.ExecOptions{
						Args: []any{
							newProjectHash, filePath, familiarityDepth, sessionCount,
							totalTurns, totalHumanTurns, firstEngagedAt, lastEngagedAt,
						},
					},
				)
			},
		},
	); txnErr != nil {
		return fmt.Errorf("store.applyV23DataMigration: migrate file_familiarity: %w", txnErr)
	}

	// Step 8.5: Migrate annotation_target_projects project_hash values.
	// annotation_target_projects (V16 TPT) references projects(project_hash).
	// V23 changes the project_hash values, so we must update FK references.
	// Gracefully skip if table doesn't exist (pre-V16 databases).
	var hasAnnotTargetProjects bool
	if txnErr = sqlitex.ExecuteTransient(conn,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='annotation_target_projects'`,
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				hasAnnotTargetProjects = stmt.ColumnInt(0) > 0
				return nil
			},
		},
	); txnErr != nil {
		return fmt.Errorf("store.applyV23DataMigration: check annotation_target_projects: %w", txnErr)
	}

	if hasAnnotTargetProjects {
		type annotTargetRow struct {
			annotationID   string
			oldProjectHash string
		}
		var annotRows []annotTargetRow
		if txnErr = sqlitex.ExecuteTransient(conn,
			`SELECT annotation_id, project_hash FROM annotation_target_projects`,
			&sqlitex.ExecOptions{
				ResultFunc: func(stmt *sqlite.Stmt) error {
					annotRows = append(annotRows, annotTargetRow{
						annotationID:   stmt.ColumnText(0),
						oldProjectHash: stmt.ColumnText(1),
					})
					return nil
				},
			},
		); txnErr != nil {
			return fmt.Errorf("store.applyV23DataMigration: read annotation_target_projects: %w", txnErr)
		}

		for _, r := range annotRows {
			newPH, ok := oldToNewProjectHash[r.oldProjectHash]
			if !ok {
				// Orphaned annotation: old project_hash not in projects table.
				// Delete the row rather than leaving a dangling FK.
				if txnErr = sqlitex.ExecuteTransient(conn,
					`DELETE FROM annotation_target_projects WHERE annotation_id = ?`,
					&sqlitex.ExecOptions{Args: []any{r.annotationID}},
				); txnErr != nil {
					return fmt.Errorf(
						"store.applyV23DataMigration: delete orphaned annotation_target_projects %q: %w",
						r.annotationID, txnErr,
					)
				}
				continue
			}
			if txnErr = sqlitex.ExecuteTransient(conn,
				`UPDATE annotation_target_projects SET project_hash = ? WHERE annotation_id = ?`,
				&sqlitex.ExecOptions{Args: []any{newPH, r.annotationID}},
			); txnErr != nil {
				return fmt.Errorf(
					"store.applyV23DataMigration: update annotation_target_projects %q: %w",
					r.annotationID, txnErr,
				)
			}
		}
	}

	// Step 9: Drop old tables in dependency order (children before parents).
	dropOldTables := []string{
		"DROP TABLE IF EXISTS file_familiarity",
		"DROP TABLE IF EXISTS daily_summary_by_project",
		"DROP TABLE IF EXISTS sessions",
		"DROP TABLE IF EXISTS host_slugs",
		"DROP TABLE IF EXISTS projects",
	}
	for _, stmt := range dropOldTables {
		if txnErr = sqlitex.ExecuteTransient(conn, stmt, nil); txnErr != nil {
			return fmt.Errorf(
				"store.applyV23DataMigration: %s: %w — "+
					"what: failed to drop old table; "+
					"why: the table may be referenced by an active query or FK constraint; "+
					"fix: ensure no other processes hold open queries on these tables",
				stmt, txnErr,
			)
		}
	}

	// Step 10: Rename v2 tables to final names.
	renameStmts := []string{
		"ALTER TABLE host_slugs_v2 RENAME TO host_slugs",
		"ALTER TABLE projects_v2 RENAME TO projects",
		"ALTER TABLE sessions_v2 RENAME TO sessions",
		"ALTER TABLE daily_summary_by_project_v2 RENAME TO daily_summary_by_project",
		"ALTER TABLE file_familiarity_v2 RENAME TO file_familiarity",
	}
	for _, stmt := range renameStmts {
		if txnErr = sqlitex.ExecuteTransient(conn, stmt, nil); txnErr != nil {
			return fmt.Errorf(
				"store.applyV23DataMigration: %s: %w — "+
					"what: failed to rename v2 table to final name; "+
					"why: the target name may already exist; "+
					"fix: check for duplicate table names in the database",
				stmt, txnErr,
			)
		}
	}

	// Step 11: Recreate indexes (one statement at a time — ExecuteTransient
	// does not support multi-statement strings).
	for _, idxStmt := range v23RecreateIndexStmts {
		if txnErr = sqlitex.ExecuteTransient(conn, idxStmt, nil); txnErr != nil {
			return fmt.Errorf(
				"store.applyV23DataMigration: recreate indexes: %s: %w — "+
					"what: failed to recreate session/summary indexes after V23 migration; "+
					"why: an index with the same name may already exist; "+
					"fix: drop duplicate indexes and re-run",
				idxStmt, txnErr,
			)
		}
	}

	return nil
}

// dropV23TempTables drops the temporary _v2 tables created by the V23 SQL phase.
// Called when the Go migration phase detects the data migration already ran (or
// on a fresh install where the base schema already has the V23 shape), but the
// SQL phase still created the orphan _v2 tables.
func dropV23TempTables(conn *sqlite.Conn) error {
	dropStmts := []string{
		"DROP TABLE IF EXISTS file_familiarity_v2",
		"DROP TABLE IF EXISTS daily_summary_by_project_v2",
		"DROP TABLE IF EXISTS sessions_v2",
		"DROP TABLE IF EXISTS host_slugs_v2",
		"DROP TABLE IF EXISTS projects_v2",
	}
	for _, stmt := range dropStmts {
		if err := sqlitex.ExecuteTransient(conn, stmt, nil); err != nil {
			return fmt.Errorf("dropV23TempTables: %s: %w", stmt, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// V23 DDL constants
// ---------------------------------------------------------------------------

// v23CreateHostSlugsV2 defines the new host_slugs table with opaque PK.
// opaque_id = HMAC-SHA256(salt, canonical_remote) — 64-char hex string.
const v23CreateHostSlugsV2 = `CREATE TABLE host_slugs_v2 (
    opaque_id        TEXT PRIMARY KEY,
    host_slug        TEXT NOT NULL,
    git_remote       TEXT,
    canonical_remote TEXT
) STRICT`

// v23CreateProjectsV2 defines the new projects table with salted HMAC PK.
// project_hash = HMAC-SHA256(salt, canonical_remote) — 64-char hex string.
// canonical_cwd: shortest observed CWD per project (populated from project_path).
const v23CreateProjectsV2 = `CREATE TABLE projects_v2 (
    project_hash     TEXT PRIMARY KEY,
    canonical_cwd    TEXT,
    canonical_remote TEXT
) STRICT`

// v23CreateSessionsV2 defines the sessions table with updated FKs.
// host_slug column replaced by opaque_host_id (FK → host_slugs_v2(opaque_id)).
const v23CreateSessionsV2 = `CREATE TABLE sessions_v2 (
    session_id     TEXT PRIMARY KEY,
    parent_id      TEXT REFERENCES sessions_v2(session_id),
    model_harness  TEXT NOT NULL CHECK (model_harness IN ('claude','opencode','codex','gemini')),
    model_id       TEXT NOT NULL,
    opaque_host_id TEXT NOT NULL REFERENCES host_slugs_v2(opaque_id),
    project_hash   TEXT NOT NULL REFERENCES projects_v2(project_hash),
    start_ms       INTEGER NOT NULL,
    end_ms         INTEGER NOT NULL,
    ingested_ms    INTEGER NOT NULL,
    source_path    TEXT NOT NULL,
    source_format  TEXT NOT NULL CHECK (source_format IN ('jsonl','json')),
    schema_version INTEGER NOT NULL DEFAULT 1,
    git_branch     TEXT,
    git_worktree   TEXT,
    git_tracking   TEXT,
    tool_version   TEXT,
    pushed_at      INTEGER,
    tags           TEXT,
    index_version  INTEGER NOT NULL DEFAULT 0,
    indexed_at     INTEGER
) STRICT`

// v23CreateDailySummaryByProjectV2 defines daily_summary_by_project with updated FK.
const v23CreateDailySummaryByProjectV2 = `CREATE TABLE daily_summary_by_project_v2 (
    date_utc       TEXT NOT NULL,
    project_hash   TEXT NOT NULL REFERENCES projects_v2(project_hash),
    project_path   TEXT,
    session_count  INTEGER NOT NULL DEFAULT 0,
    total_tokens   INTEGER NOT NULL DEFAULT 0,
    total_tool_calls INTEGER NOT NULL DEFAULT 0,
    total_duration_minutes REAL NOT NULL DEFAULT 0,
    resolved_count INTEGER NOT NULL DEFAULT 0,
    failed_count   INTEGER NOT NULL DEFAULT 0,
    partial_count  INTEGER NOT NULL DEFAULT 0,
    avg_spec_quality REAL,
    acceptance_rate REAL,
    total_cost_usd REAL,
    PRIMARY KEY (date_utc, project_hash)
) STRICT`

// v23CreateFileFamiliarityV2 defines file_familiarity with updated FK.
const v23CreateFileFamiliarityV2 = `CREATE TABLE file_familiarity_v2 (
    project_hash       TEXT NOT NULL REFERENCES projects_v2(project_hash),
    file_path          TEXT NOT NULL,
    familiarity_depth  INTEGER NOT NULL DEFAULT 0,
    session_count      INTEGER NOT NULL DEFAULT 0,
    total_turns        INTEGER NOT NULL DEFAULT 0,
    total_human_turns  INTEGER NOT NULL DEFAULT 0,
    first_engaged_at   TEXT,
    last_engaged_at    TEXT,
    PRIMARY KEY (project_hash, file_path)
) STRICT`

// v23RecreateIndexStmts recreates all indexes for the sessions and
// daily_summary_by_project tables after the V23 rename.
// Each statement is executed individually (ExecuteTransient does not support multi-statement strings).
var v23RecreateIndexStmts = []string{
	"CREATE INDEX IF NOT EXISTS idx_sessions_start    ON sessions(start_ms)",
	"CREATE INDEX IF NOT EXISTS idx_sessions_harness  ON sessions(model_harness)",
	"CREATE INDEX IF NOT EXISTS idx_sessions_project  ON sessions(project_hash)",
	"CREATE INDEX IF NOT EXISTS idx_sessions_host     ON sessions(opaque_host_id)",
	"CREATE INDEX IF NOT EXISTS idx_sessions_parent   ON sessions(parent_id) WHERE parent_id IS NOT NULL",
	"CREATE INDEX IF NOT EXISTS idx_daily_project_hash ON daily_summary_by_project(project_hash)",
}
