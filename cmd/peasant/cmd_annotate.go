package main

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/peasant-labs/peasant/internal/annotations"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

// humanCLIAnnotatorName is the annotator name used for human annotations created via the CLI.
const humanCLIAnnotatorName = "human-cli"

// BuildAnnotateCommand constructs the annotate command with list, create,
// list-project, and create-project subcommands.
// Alias: "react" (closer to end-user mental model for thumbs-up/down workflows).
// Follows the BuildSessionsCommand pattern (cmd/peasant/cmd_sessions.go).
func BuildAnnotateCommand() *cobra.Command {
	annotateCmd := &cobra.Command{
		Use:     "annotate",
		Aliases: []string{"react"},
		Short:   "Manage session and project annotations",
	}

	listCmd := &cobra.Command{
		Use:   "list <session-id>",
		Short: "List annotations for a session",
		Args:  cobra.ExactArgs(1),
		RunE:  runAnnotateList,
	}

	createCmd := &cobra.Command{
		Use:   "create <session-id> <type-id> <value>",
		Short: "Create a human annotation for a session",
		Args:  cobra.ExactArgs(3),
		RunE:  runAnnotateCreate,
	}

	listProjectCmd := &cobra.Command{
		Use:   "list-project <project-hash>",
		Short: "List annotations for a project",
		Args:  cobra.ExactArgs(1),
		RunE:  runAnnotateListProject,
	}

	createProjectCmd := &cobra.Command{
		Use:   "create-project <project-hash> <type-id> <value>",
		Short: "Create a human annotation for a project",
		Args:  cobra.ExactArgs(3),
		RunE:  runAnnotateCreateProject,
	}

	annotateCmd.AddCommand(listCmd, createCmd, listProjectCmd, createProjectCmd, buildAnnotateImportCommand(), buildAnnotatePruneCommand())
	BuildAnnotateResearchCommands(annotateCmd)
	return annotateCmd
}

// buildAnnotatePruneCommand creates the `annotate prune <annotator-name>` subcommand.
// Deletes all annotations by the named annotator, optionally scoped to specific sessions.
func buildAnnotatePruneCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prune <annotator-name>",
		Short: "Delete annotations by a specific annotator",
		Long: `Delete annotations created by the named annotator from the local database.

By default all annotations by the annotator are deleted. Use --session or
--session-from-file to scope the deletion to specific sessions. An explicitly
provided session file must contain at least one nonblank session ID.

With --dry-run the command prints how many annotations would be deleted
without modifying the database.`,
		Args: cobra.ExactArgs(1),
		RunE: runAnnotatePrune,
	}

	cmd.Flags().String("session", "", "Session ID to scope deletion to a single session")
	cmd.Flags().String("session-from-file", "", "Path to a newline-delimited file of session IDs to scope deletion")
	cmd.Flags().Bool("dry-run", false, "Print how many annotations would be deleted without making changes")

	return cmd
}

// runAnnotatePrune implements the handler for `annotate prune`.
func runAnnotatePrune(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	annotatorName := args[0]

	sessionIDs, err := resolveOptionalSessionIDs(cmd)
	if err != nil {
		return err
	}

	dryRun, err := cmd.Flags().GetBool("dry-run")
	if err != nil {
		return fmt.Errorf("annotate prune: read --dry-run flag: %w", err)
	}

	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	if dryRun {
		count, err := db.CountAnnotationsByAnnotator(ctx, annotatorName, sessionIDs)
		if err != nil {
			return fmt.Errorf("annotate prune: count annotations: %w", err)
		}
		if len(sessionIDs) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "would delete %d annotation(s) by %q (scoped to %d session(s))\n",
				count, annotatorName, len(sessionIDs))
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "would delete %d annotation(s) by %q\n", count, annotatorName)
		}
		return nil
	}

	count, err := db.DeleteAnnotationsByAnnotator(ctx, annotatorName, sessionIDs)
	if err != nil {
		return fmt.Errorf("annotate prune: %w", err)
	}
	if len(sessionIDs) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "deleted %d annotation(s) by %q (scoped to %d session(s))\n",
			count, annotatorName, len(sessionIDs))
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "deleted %d annotation(s) by %q\n", count, annotatorName)
	}
	return nil
}

// runAnnotateList queries and displays all non-superseded annotations for a session.
func runAnnotateList(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	sessionID := args[0]

	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	rows, err := db.GetAnnotationsForSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("list annotations: %w", err)
	}

	if len(rows) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "no annotations found")
		return nil
	}

	printAnnotationTable(cmd, rows)
	return nil
}

// runAnnotateCreate validates and creates a human annotation targeting a session.
func runAnnotateCreate(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	sessionID := args[0]
	typeID := args[1]
	value := args[2]

	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	registry := annotations.NewTypeReader(db)

	// Single DB lookup: get type summary (validates existence) and resolve UUID.
	typeSummary, err := registry.GetType(ctx, typeID)
	if err != nil {
		return fmt.Errorf("annotate create: %w", err)
	}

	// Validate value against the type domain.
	if err := schema.ValidateAnnotationValue(typeSummary.ValueDomain, value); err != nil {
		return fmt.Errorf("annotate create: %w", err)
	}

	annotatorID, err := ensureHumanCLIAnnotator(ctx, db)
	if err != nil {
		return err
	}

	id, err := db.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sessionID,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeSummary.ID,
		Value:            value,
	})
	if err != nil {
		return fmt.Errorf("annotate create: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "created annotation %s: %s = %q on session %s\n", id, typeID, value, sessionID)
	return nil
}

// runAnnotateListProject queries and displays all non-superseded annotations for a project.
func runAnnotateListProject(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	projectHash := args[0]

	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	rows, err := db.GetAnnotationsForProject(ctx, projectHash)
	if err != nil {
		return fmt.Errorf("list project annotations: %w", err)
	}

	if len(rows) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "no annotations found")
		return nil
	}

	printAnnotationTable(cmd, rows)
	return nil
}

// runAnnotateCreateProject validates and creates a human annotation targeting a project.
func runAnnotateCreateProject(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	projectHash := args[0]
	typeID := args[1]
	value := args[2]

	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	registry := annotations.NewTypeReader(db)

	// Single DB lookup: get type summary (validates existence) and resolve UUID.
	typeSummary, err := registry.GetType(ctx, typeID)
	if err != nil {
		return fmt.Errorf("annotate create-project: %w", err)
	}

	// Validate value against the type domain.
	if err := schema.ValidateAnnotationValue(typeSummary.ValueDomain, value); err != nil {
		return fmt.Errorf("annotate create-project: %w", err)
	}

	annotatorID, err := ensureHumanCLIAnnotator(ctx, db)
	if err != nil {
		return err
	}

	id, err := db.CreateAnnotation(ctx, store.CreateAnnotationParams{
		ProjectHash:      &projectHash,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeSummary.ID,
		Value:            value,
	})
	if err != nil {
		return fmt.Errorf("annotate create-project: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "created annotation %s: %s = %q on project %s\n", id, typeID, value, projectHash)
	return nil
}

// ensureHumanCLIAnnotator returns the DB ID for the "human-cli" annotator,
// creating it if it does not yet exist.
func ensureHumanCLIAnnotator(ctx context.Context, db *store.Store) (string, error) {
	annotator, err := db.GetAnnotator(ctx, humanCLIAnnotatorName)
	if err != nil {
		return "", fmt.Errorf("lookup annotator: %w", err)
	}
	if annotator != nil {
		return annotator.ID, nil
	}

	// Create the human-cli annotator on first use.
	id, err := db.CreateAnnotator(ctx, store.CreateAnnotatorParams{
		Kind:        schema.AnnotatorHuman,
		Name:        humanCLIAnnotatorName,
		DisplayName: "Human (CLI)",
		Description: "Human annotations created via the peasant annotate CLI.",
	})
	if err != nil {
		return "", fmt.Errorf("create annotator: %w", err)
	}
	return id, nil
}

// printAnnotationTable prints annotation rows as a tab-separated table.
func printAnnotationTable(cmd *cobra.Command, rows []store.AnnotationRow) {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TYPE_ID\tVALUE\tANNOTATOR\tKIND\tCREATED")
	for _, r := range rows {
		created := time.UnixMilli(r.CreatedAt).UTC().Format(time.RFC3339)
		confidence := ""
		if r.Confidence != nil {
			confidence = fmt.Sprintf(" (%.2f)", *r.Confidence)
		}
		fmt.Fprintf(w, "%s\t%s%s\t%s\t%s\t%s\n",
			r.TypeID,
			r.Value, confidence,
			r.AnnotatorName,
			r.AnnotatorKind.String(),
			created,
		)
	}
	w.Flush()

	fmt.Fprintf(cmd.OutOrStdout(), "\n%d annotation(s)\n", len(rows))
}
