package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

// villageListDefaultLimit is the page size requested for the remote pullable
// listing. The remote list fetches the first page only (MVP); paging beyond it
// is a possible FOLLOWUP.
const villageListDefaultLimit = 50

// buildVillageTranscriptsListCommand constructs `village transcripts list`
// (L2). DEFAULT (no --local) lists the REMOTE pullable transcripts (own +
// group-shared) and REQUIRES login — it contacts the village (NegotiatePull is
// called exactly once before the listing GET). With --local it lists the LOCAL
// pulled inventory from the V34 tables OFFLINE (no network, no credentials).
// Output conventions mirror push: the table goes to stdout, --json is parseable.
func buildVillageTranscriptsListCommand() *cobra.Command {
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
		Long: `List transcripts you can pull from the village (the default — own and
group-shared transcripts; requires login), or the transcripts you have ALREADY
pulled locally (--local; reads only local data and works offline).

--collective narrows the remote listing to one or more collectives you belong
to, named by a collective name, its UUID, or its /groups/<uuid> page URL, and
pages to the end so the listing matches what a pull would take. --user keeps only
the transcripts one village account contributed: combine it with --collective for
a single collective, or pass it alone to span every collective you can pull from.
With --local, --user filters already-pulled transcripts by their stored owner
username, without login or network access. --limit sets the remote page size;
--collective and --limit apply to the remote listing only.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true

			if local {
				if len(collectiveRefs) > 0 || cmd.Flags().Changed("limit") {
					return fmt.Errorf("--collective and --limit apply to the remote listing; --local reads only the local inventory")
				}
				return runVillageTranscriptsListLocal(cmd, jsonOutput, user)
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
// inventory. NO credentials are loaded — this is a purely-local read.
func runVillageTranscriptsListLocal(cmd *cobra.Command, jsonOutput bool, owner string) error {
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

	if jsonOutput {
		return printLocalListJSON(cmd, rows)
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
	TranscriptID    string `json:"transcriptId"`
	VillageHost     string `json:"villageHost"`
	Owner           string `json:"owner"`
	Title           string `json:"title,omitempty"`
	Harness         string `json:"harness,omitempty"`
	LocalSessionID  string `json:"localSessionId,omitempty"`
	License         string `json:"license,omitempty"`
	AnnotationCount int    `json:"annotationCount"`
	LastPulledAt    int64  `json:"lastPulledAt"`
}

// printLocalListJSON writes the local inventory as JSON.
func printLocalListJSON(cmd *cobra.Command, rows []store.PulledTranscriptRow) error {
	out := make([]jsonLocalListRow, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, jsonLocalListRow{
			TranscriptID:    r.TranscriptID.String(),
			VillageHost:     r.VillageHost,
			Owner:           r.OwnerUsername,
			Title:           r.Title,
			Harness:         r.Harness.String(),
			LocalSessionID:  r.LocalSessionID,
			License:         string(r.License),
			AnnotationCount: r.AnnotationCount,
			LastPulledAt:    r.LastPulledAt,
		})
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(out)
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
		fmt.Fprintln(w, "No pullable transcripts available on the village.")
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
