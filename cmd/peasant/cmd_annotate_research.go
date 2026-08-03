package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// BuildAnnotateResearchCommands adds research-specific subcommands to the annotate command.
func BuildAnnotateResearchCommands(annotateCmd *cobra.Command) {
	sampleCmd := &cobra.Command{
		Use:   "sample",
		Short: "Sample random sessions for annotation",
		RunE:  runAnnotateSample,
	}
	sampleCmd.Flags().IntP("count", "n", 15, "Number of sessions to sample")
	sampleCmd.Flags().Int("min-user-turns", 3, "Minimum depth-0 user turns (from indexed session_entries, not raw transcript)")
	sampleCmd.Flags().Int("max-total-turns", 2000, "Maximum total indexed turns (all roles, all depths)")
	sampleCmd.Flags().Bool("only-parent-sessions", true, "Only sample parent sessions (exclude subagents/teammates)")
	sampleCmd.Flags().String("project", "", "Filter to a specific project (substring match)")
	sampleCmd.Flags().Bool("unannotated", false, "Only sessions without friction episode annotations")
	sampleCmd.Flags().Int64("seed", 0, "Random seed for reproducible sampling (0 = random)")
	sampleCmd.Flags().String("session-from-file", "", "File of existing session IDs (one per line) to include and exclude from new sampling")

	annotateCmd.AddCommand(sampleCmd)
}

// readSessionIDsFromFile reads one session ID per line, skipping empty lines and whitespace.
func readSessionIDsFromFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open session file: %w", err)
	}
	defer f.Close()

	var ids []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			ids = append(ids, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read session file: %w", err)
	}
	return ids, nil
}

func runAnnotateSample(cmd *cobra.Command, _ []string) error {
	count, _ := cmd.Flags().GetInt("count")
	minUserTurns, _ := cmd.Flags().GetInt("min-user-turns")
	maxTotalTurns, _ := cmd.Flags().GetInt("max-total-turns")
	onlyParentSessions, _ := cmd.Flags().GetBool("only-parent-sessions")
	project, _ := cmd.Flags().GetString("project")
	unannotated, _ := cmd.Flags().GetBool("unannotated")
	seed, _ := cmd.Flags().GetInt64("seed")
	sessionFromFile, _ := cmd.Flags().GetString("session-from-file")

	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	// Load existing session IDs from file (if provided)
	var existingIDs []string
	if sessionFromFile != "" {
		var readErr error
		existingIDs, readErr = readSessionIDsFromFile(sessionFromFile)
		if readErr != nil {
			return readErr
		}
		if len(existingIDs) >= count {
			return fmt.Errorf("file contains %d sessions but requested count is %d — nothing new to sample", len(existingIDs), count)
		}
	}
	newCount := count - len(existingIDs)

	// Build query with filters
	var conditions []string
	var args []any

	// Filter on depth-0 user turns from session_entries (our indexed schema, correct roles).
	if minUserTurns > 0 {
		conditions = append(conditions,
			`(SELECT COUNT(*) FROM session_entries se
			  WHERE se.session_id = s.session_id AND se.role = 'user' AND se.depth = 0) >= ?`)
		args = append(args, minUserTurns)
	}
	// Filter on total indexed turns from session_entries (all roles, all depths).
	if maxTotalTurns > 0 {
		conditions = append(conditions,
			`(SELECT COUNT(*) FROM session_entries se
			  WHERE se.session_id = s.session_id) <= ?`)
		args = append(args, maxTotalTurns)
	}
	if project != "" {
		conditions = append(conditions, "COALESCE(p.canonical_cwd, p.project_hash) LIKE '%' || ? || '%'")
		args = append(args, project)
	}
	if onlyParentSessions {
		conditions = append(conditions, "s.parent_id IS NULL")
	}

	if unannotated {
		conditions = append(conditions,
			`s.session_id NOT IN (
				SELECT DISTINCT ats.session_id FROM annotations a
				JOIN annotation_target_sessions ats ON ats.annotation_id = a.id
				JOIN annotation_types t ON t.id = a.annotation_type_id
				WHERE t.type_id = 'research.friction_episode'
				AND a.superseded_by IS NULL
			)`)
	}

	// Exclude already-sampled sessions from the new sampling pool
	if len(existingIDs) > 0 {
		placeholders := make([]string, len(existingIDs))
		for i, id := range existingIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		conditions = append(conditions,
			fmt.Sprintf("s.session_id NOT IN (%s)", strings.Join(placeholders, ",")))
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	// Use seed for reproducible random ordering
	orderBy := "ORDER BY RANDOM()"
	if seed != 0 {
		// SQLite doesn't have seeded RANDOM, but we can hash session_id with the seed
		// for deterministic ordering
		orderBy = fmt.Sprintf("ORDER BY hex(substr(s.session_id, 1, 8) || '%d')", seed)
	}

	// The session info query (shared between new sampling and existing lookups)
	selectCols := `s.session_id, COALESCE(p.canonical_cwd, p.project_hash),
		(SELECT COUNT(*) FROM session_entries se WHERE se.session_id = s.session_id AND se.role = 'user' AND se.depth = 0) as user_turns,
		(SELECT COUNT(*) FROM session_entries se WHERE se.session_id = s.session_id) as total_turns,
		COALESCE(m.input_tokens,0)+COALESCE(m.output_tokens,0) as tokens,
		m.duration_minutes, m.tool_calls`

	joinClause := `FROM sessions s
		JOIN session_metrics m ON s.session_id = m.session_id
		JOIN projects p ON s.project_hash = p.project_hash`

	// SQLite evaluates WHERE before LIMIT, so filtering happens before sampling —
	// ORDER BY RANDOM() LIMIT ? samples only from the already-filtered rows.
	query := fmt.Sprintf(`SELECT %s %s %s %s LIMIT ?`, selectCols, joinClause, where, orderBy)
	args = append(args, newCount)

	pool := db.Pool()
	conn, connErr := pool.Take(cmd.Context())
	if connErr != nil {
		return fmt.Errorf("take connection: %w", connErr)
	}
	defer pool.Put(conn)

	type sessionInfo struct {
		ID         string
		Project    string
		UserTurns  int
		TotalTurns int
		Tokens     int
		Duration   float64
		Tools      int
	}

	// Helper to run a query and collect sessionInfo rows
	execQuery := func(q string, qArgs []any) ([]sessionInfo, error) {
		stmt, _, stmtErr := conn.PrepareTransient(q)
		if stmtErr != nil {
			return nil, fmt.Errorf("prepare query: %w", stmtErr)
		}
		defer stmt.Finalize()

		for i, arg := range qArgs {
			switch v := arg.(type) {
			case int:
				stmt.BindInt64(i+1, int64(v))
			case int64:
				stmt.BindInt64(i+1, v)
			case string:
				stmt.BindText(i+1, v)
			}
		}

		var results []sessionInfo
		for {
			hasRow, stepErr := stmt.Step()
			if stepErr != nil {
				return nil, fmt.Errorf("step: %w", stepErr)
			}
			if !hasRow {
				break
			}
			results = append(results, sessionInfo{
				ID:         stmt.ColumnText(0),
				Project:    stmt.ColumnText(1),
				UserTurns:  stmt.ColumnInt(2),
				TotalTurns: stmt.ColumnInt(3),
				Tokens:     stmt.ColumnInt(4),
				Duration:   stmt.ColumnFloat(5),
				Tools:      stmt.ColumnInt(6),
			})
		}
		return results, nil
	}

	// Sample new sessions
	newSessions, execErr := execQuery(query, args)
	if execErr != nil {
		return fmt.Errorf("sample query: %w", execErr)
	}

	// Look up existing sessions from file (if any)
	var existingSessions []sessionInfo
	if len(existingIDs) > 0 {
		placeholders := make([]string, len(existingIDs))
		var existingArgs []any
		for i, id := range existingIDs {
			placeholders[i] = "?"
			existingArgs = append(existingArgs, id)
		}
		existingQuery := fmt.Sprintf(`SELECT %s %s WHERE s.session_id IN (%s)`,
			selectCols, joinClause, strings.Join(placeholders, ","))
		var lookupErr error
		existingSessions, lookupErr = execQuery(existingQuery, existingArgs)
		if lookupErr != nil {
			return fmt.Errorf("existing session lookup: %w", lookupErr)
		}
	}

	allSessions := append(existingSessions, newSessions...)

	if len(allSessions) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No sessions match the criteria.")
		return nil
	}

	// Output summary
	if len(existingIDs) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "Existing: %d from file, New: %d sampled, Total: %d session(s):\n\n",
			len(existingSessions), len(newSessions), len(allSessions))
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Sampled %d session(s):\n\n", len(allSessions))
	}
	for _, s := range allSessions {
		project := s.Project
		// Show just the basename
		if idx := strings.LastIndex(project, "/"); idx >= 0 {
			project = project[idx+1:]
		}
		fmt.Fprintf(cmd.OutOrStdout(), "  %s  %s  %d user turns  %d total  %d tools  %.0fm\n",
			s.ID, project, s.UserTurns, s.TotalTurns, s.Tools, s.Duration)
	}

	// Also print just the IDs for piping
	fmt.Fprintln(cmd.OutOrStdout(), "\nSession IDs (for piping):")
	for _, s := range allSessions {
		fmt.Fprintln(cmd.OutOrStdout(), s.ID)
	}

	return nil
}
