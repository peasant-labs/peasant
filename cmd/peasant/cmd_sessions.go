package main

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/spf13/cobra"
)

// tagManager abstracts session tag CRUD for testability.
// *store.Store satisfies this interface.
type tagManager interface {
	GetTags(ctx context.Context, sessionID ingest.SessionID) ([]string, error)
	SetTags(ctx context.Context, sessionID ingest.SessionID, tags []string) error
	AddTag(ctx context.Context, sessionID ingest.SessionID, tag string) error
	RemoveTag(ctx context.Context, sessionID ingest.SessionID, tag string) error
}

// Compile-time guard: *store.Store must satisfy tagManager.
var _ tagManager = (*store.Store)(nil)

// BuildSessionsCommand constructs the sessions command with tag subcommands.
func BuildSessionsCommand() *cobra.Command {
	sessionsCmd := &cobra.Command{
		Use:   "sessions",
		Short: "Manage sessions",
		// R15: bare `peasant sessions` prints help then shows a recent sessions listing.
		RunE: func(cmd *cobra.Command, args []string) error {
			// Print help text above the listing (R15 behavior).
			if err := cmd.Help(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout())

			db, cleanup, err := openDB(cmd)
			if err != nil {
				// Non-fatal: if the DB doesn't exist yet, show a friendly message.
				fmt.Fprintf(cmd.OutOrStdout(), "(no sessions — run `peasant ingest` to import sessions)\n")
				return nil
			}
			defer cleanup()

			f := store.SessionListFilter{
				SortField: defaults.SessionSortDate,
				SortDesc:  true,
				Limit:     defaults.SessionsBareDefaultLimit,
			}
			return listSessionsShared(cmd, db, f, false)
		},
	}

	tagCmd := &cobra.Command{
		Use:   "tag",
		Short: "Manage session tags",
	}

	addCmd := &cobra.Command{
		Use:   "add <session-id> <tag>",
		Short: "Add a tag to a session",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			sid, err := ingest.NewSessionID(args[0])
			if err != nil {
				return fmt.Errorf("invalid session ID: %w", err)
			}
			db, cleanup, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			if err := db.AddTag(ctx, sid, args[1]); err != nil {
				return fmt.Errorf("add tag: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "tag %q added to %s\n", args[1], sid)
			return nil
		},
	}

	removeCmd := &cobra.Command{
		Use:   "remove <session-id> <tag>",
		Short: "Remove a tag from a session",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			sid, err := ingest.NewSessionID(args[0])
			if err != nil {
				return fmt.Errorf("invalid session ID: %w", err)
			}
			db, cleanup, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			if err := db.RemoveTag(ctx, sid, args[1]); err != nil {
				return fmt.Errorf("remove tag: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "tag %q removed from %s\n", args[1], sid)
			return nil
		},
	}

	listCmd := &cobra.Command{
		Use:   "list <session-id>",
		Short: "List tags for a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			sid, err := ingest.NewSessionID(args[0])
			if err != nil {
				return fmt.Errorf("invalid session ID: %w", err)
			}
			db, cleanup, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			tags, err := db.GetTags(ctx, sid)
			if err != nil {
				return fmt.Errorf("get tags: %w", err)
			}
			if len(tags) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no tags")
				return nil
			}
			for _, tag := range tags {
				fmt.Fprintln(cmd.OutOrStdout(), tag)
			}
			return nil
		},
	}

	setCmd := &cobra.Command{
		Use:   "set <session-id> <tag1> [tag2] ...",
		Short: "Replace all tags for a session",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			sid, err := ingest.NewSessionID(args[0])
			if err != nil {
				return fmt.Errorf("invalid session ID: %w", err)
			}
			tags := args[1:]
			db, cleanup, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			if err := db.SetTags(ctx, sid, tags); err != nil {
				return fmt.Errorf("set tags: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "tags set on %s\n", sid)
			return nil
		},
	}

	tagCmd.AddCommand(addCmd, removeCmd, listCmd, setCmd)
	sessionsCmd.AddCommand(tagCmd)
	sessionsCmd.AddCommand(buildSessionsListCommand())
	sessionsCmd.AddCommand(buildSessionsContextCommand())
	return sessionsCmd
}
