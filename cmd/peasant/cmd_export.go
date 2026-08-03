package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// BuildExportCommand constructs the export command tree.
func BuildExportCommand() *cobra.Command {
	exportCmd := &cobra.Command{
		Use:   "export",
		Short: "Export session data, annotations, and schemas",
	}

	exportCmd.AddCommand(
		buildExportSessionsCommand(),    // session transcript export
		buildExportAnnotationsCommand(), // annotation JSONL export
		buildExportSchemaCommand(),      // annotation schema export (renamed from eee)
		buildExportFrictionCommand(),    // friction episode export (moved from annotate export)
	)
	return exportCmd
}

// buildExportSchemaCommand constructs the `peasant export schema` subcommand.
// The --shape flag selects the output schema; currently only "eee" (EveryEvalEver) is supported.
func buildExportSchemaCommand() *cobra.Command {
	var (
		jsonOutput   bool
		statusFilter string
		includeAll   bool
		shape        string
	)

	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Export annotation types as structured schema configs",
		Long: `Export Peasant annotation types to a structured schema format.

Use --shape to select the output format:
  eee  (default) — EveryEvalEver metric configs

Maps Peasant annotation types to EEE metric types:
  - enumerated + text        -> categorical (nominal or ordinal)
  - described  + real/integer -> numeric (continuous)
  - described/enumerated + boolean -> boolean
  - described  + text        -> categorical (nominal)

Output includes configs array and summary statistics.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if shape != "eee" {
				return fmt.Errorf(
					"export schema: unsupported --shape %q\n"+
						"What went wrong: the requested schema shape is not recognized.\n"+
						"Where: export schema command, --shape flag.\n"+
						"Fix: use --shape=eee (the only currently supported value).",
					shape,
				)
			}

			db, cleanup, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			ctx := cmd.Context()

			// Build filter from flags.
			var status, origin string
			if statusFilter != "" {
				status = statusFilter
			} else if !includeAll {
				status = string(schema.StatusActive)
			}
			_ = origin // origin filter not exposed via CLI yet

			types, err := db.ListAnnotationTypes(ctx, status, origin)
			if err != nil {
				return fmt.Errorf("list annotation types: %w", err)
			}

			result, err := export.ExportEEE(types)
			if err != nil {
				return fmt.Errorf("export: %w", err)
			}

			if jsonOutput {
				return printExportJSON(cmd.OutOrStdout(), result)
			}
			printExportSummary(cmd.OutOrStdout(), result)
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, defaults.JSONFlagName, false, "Output as JSON instead of human-readable")
	cmd.Flags().StringVar(&statusFilter, "status", "", "Filter by annotation type status (active, proposed, deprecated, retired)")
	cmd.Flags().BoolVar(&includeAll, "all", false, "Include deprecated and retired types")
	cmd.Flags().StringVar(&shape, "shape", "eee", "Schema shape to export (currently only 'eee')")

	return cmd
}

// buildExportFrictionCommand constructs the `peasant export friction` subcommand.
// Exports friction episode annotations as CSV or JSON.
// This was previously available as `peasant annotate export`.
func buildExportFrictionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "friction",
		Short: "Export friction episode annotations as CSV or JSON",
		RunE:  runAnnotateExport,
	}
	cmd.Flags().Bool(defaults.JSONFlagName, false, "Output as JSON instead of CSV")
	cmd.Flags().String("annotator", "", "Filter by annotator name (e.g., 'llm-judge:claude-opus-4-6', 'human-cli')")
	cmd.Flags().String("session", "", "Filter to a specific session ID")
	cmd.Flags().Bool("paired", false, "Pair human and agent annotations per session for agreement analysis")
	cmd.Flags().Bool("summary", false, "Output per-session summary instead of per-episode detail")

	return cmd
}

// frictionExportRow is the per-annotation row shape for friction episode exports.
type frictionExportRow struct {
	SessionID     string   `json:"session_id"`
	Value         string   `json:"value"`
	Annotator     string   `json:"annotator"`
	AnnotatorKind string   `json:"annotator_kind"`
	Confidence    *float64 `json:"confidence,omitempty"`
	Reason        string   `json:"reason,omitempty"`
	StartEntry    *int     `json:"start_entry,omitempty"`
	EndEntry      *int     `json:"end_entry,omitempty"`
	CreatedAt     int64    `json:"created_at"`
}

// runAnnotateExport queries friction episode annotations and outputs as CSV or JSON.
func runAnnotateExport(cmd *cobra.Command, _ []string) error {
	asJSON, _ := cmd.Flags().GetBool(defaults.JSONFlagName)
	annotator, _ := cmd.Flags().GetString("annotator")
	sessionFilter, _ := cmd.Flags().GetString("session")
	paired, _ := cmd.Flags().GetBool("paired")
	summary, _ := cmd.Flags().GetBool("summary")

	rows, err := queryFrictionAnnotations(cmd, annotator, sessionFilter)
	if err != nil {
		return err
	}

	if len(rows) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No friction episode annotations found.")
		return nil
	}

	if summary {
		return exportSessionSummary(cmd, rows, asJSON)
	}

	if paired {
		return exportPaired(cmd, rows, asJSON)
	}

	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}

	// CSV output
	w := csv.NewWriter(cmd.OutOrStdout())
	w.Write([]string{"session_id", "value", "annotator", "annotator_kind", "confidence", "reason", "start_entry", "end_entry", "created_at"})
	for _, r := range rows {
		conf := ""
		if r.Confidence != nil {
			conf = strconv.FormatFloat(*r.Confidence, 'f', 2, 64)
		}
		start, end := "", ""
		if r.StartEntry != nil {
			start = strconv.Itoa(*r.StartEntry)
		}
		if r.EndEntry != nil {
			end = strconv.Itoa(*r.EndEntry)
		}
		w.Write([]string{
			r.SessionID, r.Value, r.Annotator, r.AnnotatorKind,
			conf, r.Reason, start, end,
			strconv.FormatInt(r.CreatedAt, 10),
		})
	}
	w.Flush()
	return nil
}

// queryFrictionAnnotations builds and executes the friction episode annotation query.
func queryFrictionAnnotations(cmd *cobra.Command, annotator, sessionFilter string) ([]frictionExportRow, error) {
	db, cleanup, err := openDB(cmd)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	var conditions []string
	var args []any

	conditions = append(conditions, "t.type_id = 'research.friction_episode'")
	conditions = append(conditions, "a.superseded_by IS NULL")

	if annotator != "" {
		conditions = append(conditions, "ann.name = ?")
		args = append(args, annotator)
	}
	if sessionFilter != "" {
		conditions = append(conditions, "COALESCE(ats.session_id, ate.session_id) = ?")
		args = append(args, sessionFilter)
	}

	where := "WHERE " + strings.Join(conditions, " AND ")

	query := fmt.Sprintf(`SELECT
		COALESCE(ats.session_id, ate.session_id) as session_id,
		a.value,
		ann.name as annotator_name,
		ak.name as annotator_kind,
		a.confidence,
		a.reason,
		ate.entry_index,
		ate.end_index,
		a.created_at
	FROM annotations a
	JOIN annotation_types t ON t.id = a.annotation_type_id
	JOIN annotators ann ON ann.id = a.annotator_id
	JOIN annotator_kinds ak ON ak.id = ann.kind_id
	LEFT JOIN annotation_target_sessions ats ON ats.annotation_id = a.id
	LEFT JOIN annotation_target_entries ate ON ate.annotation_id = a.id
	%s
	ORDER BY COALESCE(ats.session_id, ate.session_id), a.created_at`, where)

	pool := db.Pool()
	conn, connErr := pool.Take(cmd.Context())
	if connErr != nil {
		return nil, fmt.Errorf("take connection: %w", connErr)
	}
	defer pool.Put(conn)

	var rows []frictionExportRow
	stmt, _, stmtErr := conn.PrepareTransient(query)
	if stmtErr != nil {
		return nil, fmt.Errorf("prepare export query: %w", stmtErr)
	}
	defer stmt.Finalize()

	for i, arg := range args {
		switch v := arg.(type) {
		case string:
			stmt.BindText(i+1, v)
		}
	}

	for {
		hasRow, stepErr := stmt.Step()
		if stepErr != nil {
			return nil, fmt.Errorf("step: %w", stepErr)
		}
		if !hasRow {
			break
		}
		row := frictionExportRow{
			SessionID:     stmt.ColumnText(0),
			Value:         stmt.ColumnText(1),
			Annotator:     stmt.ColumnText(2),
			AnnotatorKind: stmt.ColumnText(3),
			Reason:        stmt.ColumnText(5),
			CreatedAt:     stmt.ColumnInt64(8),
		}
		if stmt.ColumnType(4) != sqlite.TypeNull { // confidence not null
			c := stmt.ColumnFloat(4)
			row.Confidence = &c
		}
		if stmt.ColumnType(6) != sqlite.TypeNull { // entry_index not null
			idx := stmt.ColumnInt(6)
			row.StartEntry = &idx
		}
		if stmt.ColumnType(7) != sqlite.TypeNull { // entry_end_index not null
			end := stmt.ColumnInt(7)
			row.EndEntry = &end
		}
		// COALESCE in the query handles both session-targeted and entry-targeted annotations.
		rows = append(rows, row)
	}

	return rows, nil
}

// exportPaired groups annotations by session and shows human vs agent side by side.
func exportPaired(cmd *cobra.Command, rows []frictionExportRow, asJSON bool) error {
	type pairedSession struct {
		SessionID string              `json:"session_id"`
		Human     []frictionExportRow `json:"human"`
		Agent     []frictionExportRow `json:"agent"`
	}

	sessions := make(map[string]*pairedSession)
	var order []string

	for _, r := range rows {
		ps, ok := sessions[r.SessionID]
		if !ok {
			ps = &pairedSession{SessionID: r.SessionID}
			sessions[r.SessionID] = ps
			order = append(order, r.SessionID)
		}
		if r.AnnotatorKind == "human" {
			ps.Human = append(ps.Human, r)
		} else {
			ps.Agent = append(ps.Agent, r)
		}
	}

	var result []pairedSession
	for _, sid := range order {
		result = append(result, *sessions[sid])
	}

	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	// Text summary
	for _, ps := range result {
		fmt.Fprintf(cmd.OutOrStdout(), "\n=== %s ===\n", ps.SessionID)
		fmt.Fprintf(cmd.OutOrStdout(), "  Human annotations: %d\n", len(ps.Human))
		for _, a := range ps.Human {
			fmt.Fprintf(cmd.OutOrStdout(), "    - %s: %s\n", a.Value, a.Reason)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "  Agent annotations: %d\n", len(ps.Agent))
		for _, a := range ps.Agent {
			fmt.Fprintf(cmd.OutOrStdout(), "    - %s: %s\n", a.Value, a.Reason)
		}
	}
	return nil
}

// sessionSummaryRow is the per-session aggregation for summary exports.
type sessionSummaryRow struct {
	SessionID     string  `json:"session_id"`
	Project       string  `json:"project"`
	UserMessages  int     `json:"user_messages"`
	TotalTurns    int     `json:"total_turns"`
	DurationMins  int     `json:"duration_mins"`
	Injected      bool    `json:"injected"`
	Episodes      int     `json:"episodes"`
	BadHandoff    int     `json:"bad_handoff"`
	BadOutput     int     `json:"bad_output"`
	BadProcess    int     `json:"bad_process"`
	AvgConfidence float64 `json:"avg_confidence"`
}

// exportSessionSummary aggregates friction episodes per session and outputs as CSV or JSON.
func exportSessionSummary(cmd *cobra.Command, rows []frictionExportRow, asJSON bool) error {
	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx := cmd.Context()

	// Get injection windows for tagging.
	windows, _ := db.InjectionWindows(ctx)

	sessions := make(map[string]*sessionSummaryRow)
	var order []string

	for _, r := range rows {
		s, ok := sessions[r.SessionID]
		if !ok {
			s = &sessionSummaryRow{SessionID: r.SessionID}
			sessions[r.SessionID] = s
			order = append(order, r.SessionID)
		}
		s.Episodes++
		switch r.Value {
		case "bad_handoff":
			s.BadHandoff++
		case "bad_output":
			s.BadOutput++
		case "bad_process":
			s.BadProcess++
		}
		if r.Confidence != nil {
			s.AvgConfidence += *r.Confidence
		}
	}

	// Enrich with session metadata from DB.
	pool := db.Pool()
	conn, connErr := pool.Take(ctx)
	if connErr != nil {
		return fmt.Errorf("take connection: %w", connErr)
	}
	defer pool.Put(conn)

	for _, sid := range order {
		s := sessions[sid]

		// Get session metadata.
		sqlitex.ExecuteTransient(conn, `
			SELECT
				COALESCE(p.canonical_cwd, ''),
				s.start_ms,
				s.end_ms
			FROM sessions s
			LEFT JOIN projects p ON s.project_hash = p.project_hash
			WHERE s.session_id = ?
		`, &sqlitex.ExecOptions{
			Args: []any{sid},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				cwd := stmt.ColumnText(0)
				// Extract project name from path.
				if cwd != "" {
					parts := strings.Split(cwd, "/")
					s.Project = parts[len(parts)-1]
				}
				startMs := stmt.ColumnInt64(1)
				endMs := stmt.ColumnInt64(2)
				if endMs > startMs {
					s.DurationMins = int((endMs - startMs) / 60000)
				}
				// Check injection status.
				for _, w := range windows {
					wEnd := w.EndMs
					if wEnd == 0 {
						wEnd = time.Now().UnixMilli()
					}
					if startMs >= w.StartMs && startMs <= wEnd {
						s.Injected = true
						break
					}
				}
				return nil
			},
		})

		// Get turn counts.
		sqlitex.ExecuteTransient(conn, `
			SELECT
				COUNT(*) FILTER (WHERE role = 'user' AND depth = 0),
				COUNT(*)
			FROM session_entries WHERE session_id = ?
		`, &sqlitex.ExecOptions{
			Args: []any{sid},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				s.UserMessages = stmt.ColumnInt(0)
				s.TotalTurns = stmt.ColumnInt(1)
				return nil
			},
		})
	}

	// Finalize averages.
	for _, s := range sessions {
		if s.Episodes > 0 {
			s.AvgConfidence /= float64(s.Episodes)
		}
	}

	var result []sessionSummaryRow
	for _, sid := range order {
		result = append(result, *sessions[sid])
	}

	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	w := csv.NewWriter(cmd.OutOrStdout())
	w.Write([]string{"session_id", "project", "user_messages", "total_turns", "duration_mins", "injected", "episodes", "bad_handoff", "bad_output", "bad_process", "avg_confidence"})
	for _, s := range result {
		injected := "false"
		if s.Injected {
			injected = "true"
		}
		w.Write([]string{
			s.SessionID,
			s.Project,
			strconv.Itoa(s.UserMessages),
			strconv.Itoa(s.TotalTurns),
			strconv.Itoa(s.DurationMins),
			injected,
			strconv.Itoa(s.Episodes),
			strconv.Itoa(s.BadHandoff),
			strconv.Itoa(s.BadOutput),
			strconv.Itoa(s.BadProcess),
			strconv.FormatFloat(s.AvgConfidence, 'f', 2, 64),
		})
	}
	w.Flush()
	return nil
}

// printExportJSON outputs the EEE export result as JSON.
func printExportJSON(w io.Writer, result *export.ExportResult[export.EEEMetricConfig]) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

// printExportSummary outputs the human-readable EEE export summary.
func printExportSummary(w io.Writer, result *export.ExportResult[export.EEEMetricConfig]) {
	fmt.Fprintf(w, "EveryEvalEver Export\n")
	fmt.Fprintf(w, "====================\n\n")

	if result.TotalTypes == 0 {
		fmt.Fprintln(w, "No annotation types found matching the filter.")
		return
	}

	fmt.Fprintf(w, "Metric Configs (%d total):\n\n", result.TotalTypes)

	var categorical, numeric, boolean int
	for _, mc := range result.Configs {
		fmt.Fprintf(w, "  %-40s  type=%-12s", mc.Name, mc.Type)
		if mc.Scale != "" {
			fmt.Fprintf(w, "  scale=%s", mc.Scale)
		}
		if len(mc.Categories) > 0 {
			fmt.Fprintf(w, "  categories=%v", mc.Categories)
		}
		if mc.Range != nil {
			fmt.Fprintf(w, "  range=[%.1f, %.1f]", mc.Range.Min, mc.Range.Max)
		}
		if mc.LowerIsBetter != nil {
			fmt.Fprintf(w, "  lower_is_better=%v", *mc.LowerIsBetter)
		}
		fmt.Fprintln(w)

		switch mc.Type {
		case export.EEEMetricCategorical:
			categorical++
		case export.EEEMetricNumeric:
			numeric++
		case export.EEEMetricBoolean:
			boolean++
		}
	}

	fmt.Fprintf(w, "\nSummary: %d categorical, %d numeric, %d boolean\n",
		categorical, numeric, boolean)
}

// buildExportSessionsCommand constructs the `peasant export sessions` subcommand.
// Exports session transcripts as JSON files with full turn content (not truncated).
// Each session is written to <output-dir>/<session-id>.json.
func buildExportSessionsCommand() *cobra.Command {
	var outputDir string

	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Export session transcripts as JSON",
		Long: `Export session transcripts as JSON files with full turn content.

Each session is re-indexed from its original source file with full content
extraction (no truncation), producing a JSON envelope with metadata and turns.

Requires either --session for a single session or --session-from-file for a batch.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionIDs, err := resolveSessionIDs(cmd)
			if err != nil {
				return err
			}

			if err := ensureOutputDir(outputDir); err != nil {
				return fmt.Errorf(
					"export sessions: create output directory %q: %w\n"+
						"What went wrong: could not create the output directory.\n"+
						"Fix: check that the parent directory exists and is writable.",
					outputDir, err,
				)
			}

			db, cleanup, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			ctx := cmd.Context()
			fs := &ingest.OSFileSystem{}

			var succeeded, failed int
			for _, sid := range sessionIDs {
				exported, exportErr := export.ExportSession(ctx, db, fs, sid)
				if exportErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: session %s: %v\n", sid, exportErr)
					failed++
					continue
				}

				outPath := filepath.Join(outputDir, sid+".json")
				data, marshalErr := json.MarshalIndent(exported, "", "  ")
				if marshalErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: session %s: marshal JSON: %v\n", sid, marshalErr)
					failed++
					continue
				}

				if writeErr := os.WriteFile(outPath, data, 0644); writeErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: session %s: write %s: %v\n", sid, outPath, writeErr)
					failed++
					continue
				}

				fmt.Fprintf(cmd.OutOrStdout(), "exported %s -> %s (%d turns)\n", sid, outPath, exported.TurnCount)
				succeeded++
			}

			if succeeded == 0 && failed > 0 {
				return fmt.Errorf(
					"export sessions: all %d session(s) failed to export\n"+
						"What went wrong: no sessions were successfully exported.\n"+
						"Fix: check the warnings above for details on each failure.",
					failed,
				)
			}

			return nil
		},
	}

	cmd.Flags().String("session", "", "Single session ID to export")
	cmd.Flags().String("session-from-file", "", "Path to a newline-delimited file of session IDs to export")
	cmd.Flags().StringVar(&outputDir, "output-dir", ".", "Directory to write exported session JSON files")

	return cmd
}

// buildExportAnnotationsCommand constructs the `peasant export annotations` subcommand.
// Exports all non-superseded annotations (session-level and entry-level) for the
// specified session(s) as JSONL files: one JSON object per line per annotation.
func buildExportAnnotationsCommand() *cobra.Command {
	var outputDir string

	cmd := &cobra.Command{
		Use:   "annotations",
		Short: "Export session annotations as JSONL",
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionIDs, err := resolveSessionIDs(cmd)
			if err != nil {
				return err
			}

			if err := ensureOutputDir(outputDir); err != nil {
				return fmt.Errorf(
					"export annotations: create output directory %q: %w\n"+
						"What went wrong: the output directory could not be created.\n"+
						"Fix: verify the path is writable and parent directories exist.",
					outputDir, err,
				)
			}

			db, cleanup, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			ctx := cmd.Context()
			w := cmd.OutOrStdout()
			errW := cmd.ErrOrStderr()

			var exported int
			for _, sid := range sessionIDs {
				annotations, err := export.ExportAnnotations(ctx, db, sid)
				if err != nil {
					fmt.Fprintf(errW, "warning: export annotations for session %s: %v\n", sid, err)
					continue
				}

				outPath := filepath.Join(outputDir, sid+"--annotations.jsonl")
				if err := writeJSONL(outPath, annotations); err != nil {
					fmt.Fprintf(errW, "warning: write %s: %v\n", outPath, err)
					continue
				}
				exported++
			}

			if exported == 0 {
				return fmt.Errorf("export annotations: all %d session(s) failed", len(sessionIDs))
			}
			fmt.Fprintf(w, "exported annotations for %d of %d session(s) to %s\n", exported, len(sessionIDs), outputDir)
			return nil
		},
	}

	cmd.Flags().String("session", "", "Single session ID whose annotations to export")
	cmd.Flags().String("session-from-file", "", "Path to a newline-delimited file of session IDs")
	cmd.Flags().StringVar(&outputDir, "output-dir", ".", "Directory to write exported annotation JSONL files")

	return cmd
}

// writeJSONL writes a slice of values as newline-delimited JSON to the given path.
// Each value is marshalled as a single JSON line. An empty slice produces an empty file.
func writeJSONL[T any](path string, records []T) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()

	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, rec := range records {
		if err := enc.Encode(rec); err != nil {
			return err
		}
	}
	return nil
}
