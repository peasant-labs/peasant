package main

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
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
		local      bool
		jsonOutput bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List pullable (remote, login required) or already-pulled (--local, offline) transcripts",
		Long: `List transcripts you can pull from the village (the default — own and
group-shared transcripts; requires login), or the transcripts you have ALREADY
pulled locally (--local; reads only local data and works offline).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Runtime errors (the remote auth gate, an unreachable village, a local
			// DB read) are not usage errors — suppress cobra's Usage/Flags dump so
			// the actionable one-liner stands alone. Genuine flag misuse is still
			// reported with usage by cobra before RunE runs.
			cmd.SilenceUsage = true

			if local {
				return runVillageTranscriptsListLocal(cmd, jsonOutput)
			}
			return runVillageTranscriptsListRemote(cmd, jsonOutput)
		},
	}

	cmd.Flags().BoolVar(&local, "local", false, "List already-pulled transcripts from the local inventory (offline; no login)")
	cmd.Flags().BoolVar(&jsonOutput, defaults.JSONFlagName, false, "Output as JSON instead of a table")

	return cmd
}

// runVillageTranscriptsListLocal renders the offline pulled-transcripts
// inventory. NO credentials are loaded — this is a purely-local read.
func runVillageTranscriptsListLocal(cmd *cobra.Command, jsonOutput bool) error {
	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	rows, err := db.ListPulledTranscripts(cmd.Context())
	if err != nil {
		return fmt.Errorf("list pulled transcripts: %w", err)
	}

	if jsonOutput {
		return printLocalListJSON(cmd, rows)
	}
	printLocalListTable(cmd, rows)
	return nil
}

// runVillageTranscriptsListRemote fetches and renders the village's pullable
// listing. Login is REQUIRED. NegotiatePull is called EXACTLY ONCE before the
// listing GET used by the transcript list command.
func runVillageTranscriptsListRemote(cmd *cobra.Command, jsonOutput bool) error {
	creds, err := requireVillageCredentials(cmd)
	if err != nil {
		return err
	}

	client := newVillageClientFromCreds(creds)
	if err := client.NegotiatePull(cmd.Context()); err != nil {
		return err
	}

	resp, err := client.ListPullableTranscripts(cmd.Context(), 1, villageListDefaultLimit)
	if err != nil {
		return fmt.Errorf("list pullable transcripts: %w", err)
	}

	if jsonOutput {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}
	printRemoteListTable(cmd, resp)
	return nil
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
