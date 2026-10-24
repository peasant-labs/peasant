package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/peasant-labs/peasant/internal/collectiveobservations"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/observationinventory"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

func villageObservationService(cmd *cobra.Command) (*collectiveobservations.Service, func(), error) {
	fetchTimeout, err := collectiveobservations.RuntimeFetchTimeout()
	if err != nil {
		return nil, func() {}, err
	}
	db, closeDB, err := openDB(cmd)
	if err != nil {
		return nil, closeDB, err
	}
	return collectiveobservations.New(collectiveobservations.Dependencies{Store: db, ConfigHome: configDirOverride(cmd), LocallyHeldTranscriptIDs: observationinventory.Held(db), FetchTimeout: fetchTimeout}), closeDB, nil
}

func printCollectiveObservations(cmd *cobra.Command, response schema.CollectiveObservationResponse, jsonOutput bool) error {
	if err := response.Validate(); err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(response)
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "collective\tid\tstate\treasons\tobserved")
	for _, row := range response.Collectives {
		reasons := make([]string, 0, len(row.Reasons))
		for _, reason := range row.Reasons {
			reasons = append(reasons, string(reason))
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", row.Name, row.ID, row.State, strings.Join(reasons, ","), row.ObservedAt.Format(time.RFC3339Nano))
	}
	return w.Flush()
}

// BuildVillageCollectivesCommand shares the service with the Local API. Offline
// reads never call the local HTTP server, login, or contact Village implicitly.
func BuildVillageCollectivesCommand() *cobra.Command {
	return buildVillageCollectivesCommand(villageObservationService)
}

type villageObservationServiceFactory func(*cobra.Command) (*collectiveobservations.Service, func(), error)

func buildVillageCollectivesCommand(newService villageObservationServiceFactory) *cobra.Command {
	var jsonOutput bool
	group := &cobra.Command{Use: "collectives", Short: "Inspect saved collective metadata and explicit tracking"}
	group.PersistentFlags().BoolVar(&jsonOutput, defaults.JSONFlagName, false, "Output the Local API JSON envelope")
	group.AddCommand(&cobra.Command{Use: "fetch", Short: "Fetch metadata only; independently install complete catalog, history and disclosed associations", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		service, closeDB, err := newService(cmd)
		if err != nil {
			return err
		}
		defer closeDB()
		report, fetchErr := service.Fetch(cmd.Context())
		if err := report.Validate(); err != nil {
			if fetchErr != nil {
				return fetchErr
			}
			return err
		}
		if jsonOutput {
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(report); err != nil {
				return err
			}
		} else {
			for _, domain := range report.Domains {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: %s (%s), coverage %s\n", domain.Domain, domain.State, domain.Commit, domain.Coverage)
			}
		}
		return fetchErr
	}})
	group.AddCommand(&cobra.Command{Use: "list", Short: "List saved relevant collectives offline (no network or login)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		service, closeDB, err := newService(cmd)
		if err != nil {
			return err
		}
		defer closeDB()
		response, err := service.List(cmd.Context())
		if err != nil {
			return err
		}
		return printCollectiveObservations(cmd, response, jsonOutput)
	}})
	group.AddCommand(&cobra.Command{Use: "track <uuid|name|url>", Short: "Verify visibility online, then save explicit tracking (does not join or publish)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
		defer cancel()
		service, closeDB, err := newService(cmd)
		if err != nil {
			return err
		}
		defer closeDB()
		snapshot, err := service.SelectedCredentials(ctx)
		if err != nil {
			return err
		}
		id, err := service.ResolveReferenceFor(ctx, snapshot, args[0], true)
		if err != nil {
			return err
		}
		response, err := service.TrackFor(ctx, snapshot, id)
		if err != nil {
			return err
		}
		return printCollectiveObservations(cmd, response, jsonOutput)
	}})
	group.AddCommand(&cobra.Command{Use: "untrack <uuid|name|url>", Short: "Remove explicit tracking offline using the saved account (does not leave a collective)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		service, closeDB, err := newService(cmd)
		if err != nil {
			return err
		}
		defer closeDB()
		snapshot, err := service.SelectedCredentials(cmd.Context())
		if err != nil {
			return err
		}
		id, err := service.ResolveReferenceFor(cmd.Context(), snapshot, args[0], false)
		if err != nil {
			return err
		}
		response, err := service.UntrackFor(cmd.Context(), snapshot, id)
		if err != nil {
			return err
		}
		return printCollectiveObservations(cmd, response, jsonOutput)
	}})
	return group
}

func BuildVillageContributionsCommand() *cobra.Command {
	return buildVillageContributionsCommand(villageObservationService)
}

func buildVillageContributionsCommand(newService villageObservationServiceFactory) *cobra.Command {
	group := &cobra.Command{Use: "contributions", Short: "Inspect bounded saved contribution evidence offline"}
	var kind, collective, revision string
	var page, limit int
	var jsonOutput bool
	list := &cobra.Command{Use: "list", Short: "List one saved evidence category offline; page >1 requires its traversal revision", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		q := schema.LocalContributionHistoryQuery{Page: page, Limit: limit}
		for _, known := range schema.AllLocalContributionEvidenceKinds {
			if string(known) == kind {
				q.Kind = known
			}
		}
		if !q.Kind.IsValid() {
			return fmt.Errorf("unknown contribution kind %q; no history read; use remote-attempts, receipts, or operation-diagnostics", kind)
		}
		service, closeDB, err := newService(cmd)
		if err != nil {
			return err
		}
		defer closeDB()
		snapshot, err := service.SelectedCredentials(cmd.Context())
		if err != nil {
			return err
		}
		if collective != "" {
			id, err := service.ResolveReferenceFor(cmd.Context(), snapshot, collective, false)
			if err != nil {
				return err
			}
			q.CollectiveID = &id
		}
		if cmd.Flags().Changed("revision") {
			token := schema.ContributionTraversalRevision(revision)
			q.Revision = &token
		}
		response, err := service.HistoryFor(cmd.Context(), snapshot, q)
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(response)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s: page %d, limit %d, saved total %d, coverage %s\nrevision: %s\n", response.Kind, response.Page, response.Limit, response.Total, response.Coverage, response.TraversalRevision)
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		switch response.Kind {
		case schema.LocalContributionEvidenceRemoteAttempts:
			fmt.Fprintln(w, "collective\ttranscript\tattempt\tstatus\trecorded")
			for _, row := range response.RemoteAttempts {
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", row.CollectiveID, row.TranscriptID, row.Attempt.EventNum, row.Attempt.Status, row.Attempt.RecordedAt.Format(time.RFC3339Nano))
			}
		case schema.LocalContributionEvidenceReceipts:
			fmt.Fprintln(w, "receipt\ttranscript\tactual effect\tconfirmed")
			for _, row := range response.Receipts {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", row.ReceiptID, row.TranscriptID, row.Outcome, row.ConfirmedAt.Format(time.RFC3339Nano))
			}
		case schema.LocalContributionEvidenceOperationDiagnostics:
			fmt.Fprintln(w, "operation\tkind\toutcome\tattempted\tissue")
			for _, row := range response.OperationDiagnostics {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", row.OperationID, row.Operation, row.Outcome, row.AttemptedAt.Format(time.RFC3339Nano), row.Issue.Message)
			}
		}
		return w.Flush()
	}}
	list.Flags().StringVar(&kind, "kind", string(schema.LocalContributionEvidenceRemoteAttempts), "remote-attempts, receipts, or operation-diagnostics")
	list.Flags().StringVar(&collective, "collective", "", "Filter using an exact saved collective reference (offline)")
	list.Flags().IntVar(&page, "page", 1, "Page number (page >1 requires --revision)")
	list.Flags().IntVar(&limit, "limit", 50, "Page size, 1..200")
	list.Flags().StringVar(&revision, "revision", "", "Previous selected-category traversalRevision, not cacheRevision")
	list.Flags().BoolVar(&jsonOutput, defaults.JSONFlagName, false, "Output the strict Local API evidence envelope")
	group.AddCommand(list)
	return group
}
