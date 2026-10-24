package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/collectiveobservations"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

// villageListDefaultLimit is the page size requested for the remote pullable
// listing. The remote list fetches the first page only (MVP); paging beyond it
// is a possible FOLLOWUP.
const villageListDefaultLimit = 50

// villageListDependencies keeps provenance I/O replaceable without replacing the
// command, account-scoped service, Store reads or generation guard.
type villageListDependencies struct {
	NewProvenanceReader func(string) func(string) ([]byte, error)
}

// buildVillageTranscriptsListCommand constructs the production local/remote
// transcript listing. Local collective disclosures use a saved account scope;
// the unfiltered inventory needs no credentials. Both local forms stay offline.
func buildVillageTranscriptsListCommand(options ...villageListDependencies) *cobra.Command {
	dependencies := villageListDependencies{NewProvenanceReader: func(root string) func(string) ([]byte, error) { return pull.NewFileSystem(root).ReadFile }}
	if len(options) > 0 && options[0].NewProvenanceReader != nil {
		dependencies = options[0]
	}
	var (
		local          bool
		jsonOutput     bool
		limit          int
		collectiveRefs []string
		user           string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List pullable (remote, login required) or already-pulled (--local, offline) transcripts",
		Long: `List transcripts you can pull from peasant online (the default — own and
group-shared transcripts; requires login), or the transcripts you have ALREADY
pulled locally (--local; reads only local data and works offline).

--collective narrows the remote listing to one or more collectives you belong
to, named by a collective name, its UUID, or its /groups/<uuid> page URL, and
pages to the end so the listing matches what a pull would take. --user keeps only
the transcripts one peasant online account contributed: combine it with --collective for
a single collective, or pass it alone to span every collective you can pull from.
With --local, --user filters stored owner usernames without credentials or network.
--collective instead selects saved per-row viewer disclosures in the selected
account scope, still offline. Collective values are ORed, then --user intersects.
Missing or invalidated disclosures never match; stale disclosures keep their
original date. Signed-out collective filtering explains how to use accountless
inventory instead. --limit sets the remote page size only.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true

			if local {
				if cmd.Flags().Changed("limit") {
					return fmt.Errorf("--limit values apply to the remote listing; --local reads the saved inventory")
				}
				return runVillageTranscriptsListLocal(cmd, jsonOutput, user, collectiveRefs, dependencies)
			}
			return runVillageTranscriptsListRemote(cmd, jsonOutput, collectiveRefs, user, limit)
		},
	}

	cmd.Flags().BoolVar(&local, "local", false, "List already-pulled transcripts from the local inventory (offline; no login)")
	cmd.Flags().BoolVar(&jsonOutput, defaults.JSONFlagName, false, "Output as JSON instead of a table")
	cmd.Flags().IntVar(&limit, "limit", villageListDefaultLimit, "Page size for the remote listing")
	cmd.Flags().StringArrayVar(&collectiveRefs, "collective", nil,
		"List the transcripts a collective holds (a name, its UUID, or its /groups/<uuid> page URL). Repeatable; several values list the union")
	cmd.Flags().StringVar(&user, "user", "",
		"Keep only this contributor's transcripts; with --local, filter the stored owner username offline")

	return cmd
}

// runVillageTranscriptsListLocal renders the offline pulled-transcripts
// inventory, selecting a saved account only for private collective disclosures.
func runVillageTranscriptsListLocal(cmd *cobra.Command, jsonOutput bool, owner string, collectiveRefs []string, dependencies villageListDependencies) error {
	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	rows, err := db.ListPulledTranscripts(cmd.Context())
	if err != nil {
		return fmt.Errorf("list pulled transcripts: %w", err)
	}
	if owner != "" {
		selected := make([]store.PulledTranscriptRow, 0, len(rows))
		for _, row := range rows {
			if strings.EqualFold(row.OwnerUsername, strings.TrimSpace(owner)) {
				selected = append(selected, row)
			}
		}
		rows = selected
	}

	if len(collectiveRefs) > 0 {
		return renderLocalCollectiveObservations(cmd, db, rows, collectiveRefs, jsonOutput, dependencies)
	}
	if jsonOutput {
		return printLocalListJSON(cmd, rows, nil)
	}
	printLocalListTable(cmd, rows)
	return nil
}

// runVillageTranscriptsListRemote fetches and renders the village's pullable
// listing. Login is REQUIRED. NegotiatePull is called EXACTLY ONCE before the
// listing GET used by the transcript list command. A collective listing, and any
// listing narrowed to one contributor, follows the pages to the end, so the
// listing matches what a pull would take; the plain listing keeps its
// single-page behavior.
func runVillageTranscriptsListRemote(cmd *cobra.Command, jsonOutput bool, collectiveRefs []string, owner string, limit int) error {
	creds, err := requireVillageCredentials(cmd)
	if err != nil {
		return err
	}

	client := newVillageClientFromCreds(creds)

	var collectives []schema.VillageUUID
	if len(collectiveRefs) > 0 {
		// Resolve against the caller's own collectives before the listing call, so
		// a mistyped name fails with the names the caller can see.
		collectives, _, err = resolveCollectiveRefs(cmd.Context(), client, collectiveRefs)
		if err != nil {
			return err
		}
	}

	if err := client.NegotiatePull(cmd.Context()); err != nil {
		return err
	}

	// A contributor filter is applied here, so the listing must be complete first:
	// filtering a single page would under-report and make the total a lie.
	allPages := len(collectiveRefs) > 0 || owner != ""
	response, err := listPullableTranscripts(cmd.Context(), client, collectives, limit, allPages)
	if err != nil {
		return fmt.Errorf("list pullable transcripts: %w", err)
	}

	if owner != "" {
		response.Transcripts = filterByOwner(response.Transcripts, owner)
		response.Total = len(response.Transcripts)
	}

	if jsonOutput {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(response)
	}
	printRemoteListTable(cmd, response)
	return nil
}

// listPullableTranscripts reads the pullable listing. With allPages false it
// returns the single page it read. With allPages true it follows the pages to
// the end and returns one merged response, with duplicates collapsed, so a
// multi-page collective listing is complete rather than silently truncated.
func listPullableTranscripts(ctx context.Context, client *village.VillageClient, collectives []schema.VillageUUID, limit int, allPages bool) (*schema.PullListResponse, error) {
	if limit <= 0 {
		limit = villageListDefaultLimit
	}

	first, err := client.ListPullableTranscripts(ctx, village.PullListOptions{Page: 1, Limit: limit, Collectives: collectives})
	if err != nil {
		return nil, err
	}
	if !allPages || first == nil {
		return first, nil
	}

	merged := &schema.PullListResponse{
		Transcripts: append([]schema.PullTranscriptInfo(nil), first.Transcripts...),
		Page:        1,
		Limit:       first.Limit,
		Total:       first.Total,
	}
	seen := make(map[schema.TranscriptID]struct{}, len(merged.Transcripts))
	for i := range merged.Transcripts {
		seen[merged.Transcripts[i].TranscriptID] = struct{}{}
	}
	for page := 2; len(merged.Transcripts) < merged.Total; page++ {
		next, err := client.ListPullableTranscripts(ctx, village.PullListOptions{Page: page, Limit: limit, Collectives: collectives})
		if err != nil {
			return nil, err
		}
		if next == nil || len(next.Transcripts) == 0 {
			break
		}
		for i := range next.Transcripts {
			row := next.Transcripts[i]
			if _, duplicate := seen[row.TranscriptID]; duplicate {
				continue
			}
			seen[row.TranscriptID] = struct{}{}
			merged.Transcripts = append(merged.Transcripts, row)
		}
		// A short page means the end of the listing.
		if len(next.Transcripts) < limit {
			break
		}
	}
	return merged, nil
}

// jsonLocalListRow is the JSON-safe local inventory row.
type jsonLocalListRow struct {
	TranscriptID          string                          `json:"transcriptId"`
	VillageHost           string                          `json:"villageHost"`
	Owner                 string                          `json:"owner"`
	Title                 string                          `json:"title,omitempty"`
	Harness               string                          `json:"harness,omitempty"`
	LocalSessionID        string                          `json:"localSessionId,omitempty"`
	License               string                          `json:"license,omitempty"`
	AnnotationCount       int                             `json:"annotationCount"`
	LastPulledAt          int64                           `json:"lastPulledAt"`
	CollectiveObservation *jsonLocalCollectiveObservation `json:"collectiveObservation,omitempty"`
}

// This CLI composition uses canonical scope, disclosure and metadata components;
// it adds no wire enums or account-private fields to the content inventory.
type jsonLocalCollectiveObservation struct {
	Scope       schema.CollectiveObservationScope    `json:"scope"`
	Metadata    schema.PullAssociationMetadataResult `json:"metadata"`
	Collectives []schema.VillageTranscriptCollective `json:"collectives"`
}

// printLocalListJSON writes the local inventory as JSON.
func printLocalListJSON(cmd *cobra.Command, rows []store.PulledTranscriptRow, observations map[schema.TranscriptID]*jsonLocalCollectiveObservation) error {
	out := make([]jsonLocalListRow, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, jsonLocalListRow{
			TranscriptID:          r.TranscriptID.String(),
			VillageHost:           r.VillageHost,
			Owner:                 r.OwnerUsername,
			Title:                 r.Title,
			Harness:               r.Harness.String(),
			LocalSessionID:        r.LocalSessionID,
			License:               string(r.License),
			AnnotationCount:       r.AnnotationCount,
			LastPulledAt:          r.LastPulledAt,
			CollectiveObservation: observations[r.TranscriptID],
		})
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func renderLocalCollectiveObservations(cmd *cobra.Command, db *store.Store, rows []store.PulledTranscriptRow, refs []string, jsonOutput bool, dependencies villageListDependencies) error {
	ctx := cmd.Context()
	service := collectiveobservations.New(collectiveobservations.Dependencies{Store: db, ConfigHome: configDirOverride(cmd)})
	snapshot, err := service.SelectedCredentials(ctx)
	if err != nil {
		return fmt.Errorf("local collective filtering needs a saved account scope: %w; unfiltered --local and --local --user remain available without login or network", err)
	}
	scope, err := service.SelectedScope(ctx)
	if err != nil {
		return err
	}
	wanted := map[schema.VillageUUID]bool{}
	for _, ref := range refs {
		id, err := service.ResolveReference(ctx, ref, false)
		if err != nil {
			return err
		}
		wanted[id] = true
	}
	ids := make([]schema.TranscriptID, 0, len(rows))
	seen := map[schema.TranscriptID]bool{}
	for _, row := range rows {
		if !seen[row.TranscriptID] {
			ids = append(ids, row.TranscriptID)
			seen[row.TranscriptID] = true
		}
	}
	evidence := map[schema.TranscriptID]store.TranscriptCollectiveObservation{}
	for offset := 0; offset < len(ids); offset += schema.MaxCollectiveFetchItems {
		end := offset + schema.MaxCollectiveFetchItems
		if end > len(ids) {
			end = len(ids)
		}
		batch, err := service.Associations(ctx, ids[offset:end])
		if err != nil {
			return err
		}
		for id, row := range batch {
			evidence[id] = row
		}
	}
	selected := make([]store.PulledTranscriptRow, 0, len(rows))
	views := map[schema.TranscriptID]*jsonLocalCollectiveObservation{}
	readProvenance := dependencies.NewProvenanceReader(string(defaults.ResolveVillagePullsDirPathWith(dataDirOverride(cmd))))
	for _, row := range rows {
		association := evidence[row.TranscriptID]
		if association.Metadata == nil || association.Metadata.State == schema.PullAssociationMetadataInvalidated {
			continue
		}
		match := false
		for _, collective := range association.Collectives {
			if wanted[collective.ID] {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		// A UUID alone cannot attach this account's private disclosure to another
		// Village's content row. Compare existing canonical manifest provenance.
		data, err := readProvenance(filepath.Join(row.PullDir, pull.ManifestFilename))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read pulled provenance for local collective filtering: %w; re-pull explicitly to repair this inventory row", err)
		}
		var manifest pull.PullManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return err
		}
		if manifest.VillageURL == "" {
			continue
		}
		base, err := auth.NormalizeVillageURL(manifest.VillageURL)
		if err != nil {
			return err
		}
		if base != scope.VillageURL {
			continue
		}
		if manifest.TranscriptID != row.TranscriptID {
			return fmt.Errorf("pulled manifest differs from the local inventory identity; re-pull explicitly before filtering private observations")
		}
		selected = append(selected, row)
		views[row.TranscriptID] = &jsonLocalCollectiveObservation{Scope: scope, Metadata: *association.Metadata, Collectives: association.Collectives}
	}
	// All operations above are offline reads. A changed generation discards the
	// entire view before any private row reaches stdout. Keep the guard through
	// emission too, so a credential change cannot interleave with private output.
	return snapshot.WithCurrent(ctx, func() error {
		if jsonOutput {
			return printLocalListJSON(cmd, selected, views)
		}
		printLocalCollectiveListTable(cmd, selected, views)
		return nil
	})
}

func printLocalCollectiveListTable(cmd *cobra.Command, rows []store.PulledTranscriptRow, views map[schema.TranscriptID]*jsonLocalCollectiveObservation) {
	if len(rows) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No pulled transcripts match the saved collective observations; unfiltered --local still lists content.")
		return
	}
	first := views[rows[0].TranscriptID]
	fmt.Fprintf(cmd.OutOrStdout(), "saved collective observations: account %s at %s (offline; not a current permission check)\n", first.Scope.UserID, first.Scope.VillageURL)
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TRANSCRIPT\tOWNER\tCOLLECTIVES\tMETADATA\tOBSERVED")
	for _, row := range rows {
		view := views[row.TranscriptID]
		names := make([]string, 0, len(view.Collectives))
		for _, collective := range view.Collectives {
			names = append(names, collective.Name)
		}
		at := "-"
		if view.Metadata.ObservedAt != nil {
			at = view.Metadata.ObservedAt.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%s\t@%s\t%s\t%s\t%s\n", row.TranscriptID, row.OwnerUsername, strings.Join(names, ", "), view.Metadata.State, at)
	}
	_ = w.Flush()
}

// printLocalListTable writes the local inventory as a human-readable table.
func printLocalListTable(cmd *cobra.Command, rows []store.PulledTranscriptRow) {
	w := cmd.OutOrStdout()
	if len(rows) == 0 {
		fmt.Fprintln(w, "No transcripts pulled yet. Run `peasant village transcripts pull <uuid>` to pull one.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TRANSCRIPT\tOWNER\tHARNESS\tLICENSE\tANNOTATIONS\tPULLED")
	for i := range rows {
		r := &rows[i]
		fmt.Fprintf(tw, "%s\t@%s\t%s\t%s\t%d\t%s\n",
			r.TranscriptID, r.OwnerUsername, r.Harness, formatLicense(r.License), r.AnnotationCount, formatUnixMillis(r.LastPulledAt))
	}
	_ = tw.Flush()
}

// printRemoteListTable writes the village's pullable listing as a table.
func printRemoteListTable(cmd *cobra.Command, resp *schema.PullListResponse) {
	w := cmd.OutOrStdout()
	if resp == nil || len(resp.Transcripts) == 0 {
		fmt.Fprintln(w, "No pullable transcripts available on peasant online.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TRANSCRIPT\tOWNER\tHARNESS\tVISIBILITY\tLICENSE\tANNOTATIONS\tTITLE")
	for i := range resp.Transcripts {
		t := &resp.Transcripts[i]
		fmt.Fprintf(tw, "%s\t@%s\t%s\t%s\t%s\t%d\t%s\n",
			t.TranscriptID, t.OwnerUsername, t.Harness, t.Visibility, formatLicense(t.License), t.AnnotationCount, t.Title)
	}
	_ = tw.Flush()
	if resp.Total > len(resp.Transcripts) {
		fmt.Fprintf(w, "\nShowing %d of %d pullable transcripts.\n", len(resp.Transcripts), resp.Total)
	}
}

// formatUnixMillis renders a unix-millis timestamp as a local datetime, or "-"
// for the zero value.
func formatUnixMillis(ms int64) string {
	if ms == 0 {
		return "-"
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04")
}

// formatLicense renders a license id as a table cell, or "-" when the
// transcript carries none. Deliberately distinct from the context header's
// "none (all rights reserved)" long form — table cells stay terse.
func formatLicense(l schema.License) string {
	if l == "" {
		return "-"
	}
	return l.String()
}
