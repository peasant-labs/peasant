package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

// BuildVillageTranscriptsCommand constructs the `peasant village transcripts`
// command group (pull, list, context). These names are part of the public CLI.
// Authentication scope:
//   - pull and remote `list` CONTACT THE VILLAGE → login required (the gate
//     mirrors cmd_push.go:77-86 exactly);
//   - `list --local` and `context` are purely-local reads → offline OK, no
//     credentials required.
func BuildVillageTranscriptsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "transcripts",
		Short: "Pull, list, and view transcripts from the Peasant village",
		Long: "Pull a transcript (and its annotations) from the Peasant village, list pullable or " +
			"already-pulled transcripts, and render a pulled transcript in the terminal.\n\n" +
			"Auth: `pull` and remote `list` contact the village and require login " +
			"(run 'peasant village login'). `list --local` and `context` read only " +
			"local data and work offline.",
	}

	cmd.AddCommand(buildVillageTranscriptsPullCommand())
	cmd.AddCommand(buildVillageTranscriptsListCommand())
	cmd.AddCommand(buildVillageTranscriptsContextCommand())

	return cmd
}

// buildVillageTranscriptsContextCommand constructs `village transcripts context
// <ref>`. It is an offline command: it resolves the ref, locates
// the pulled directory via the V34 store (no network, no login), reads the served
// blob, decodes the TranscriptContent envelope, projects the turns back to flat
// entries via TurnsToEntries, and renders them through the EXISTING
// renderSessionContextHuman / renderToolBox path (no forked renderer).
//
// Flag parity: this command supports exactly the
// `sessions context` rendering flags that apply to a whole pulled transcript:
//
//	--turn N            center / highlight the entry at index N (default: -1 = no center marker; render the whole transcript)
//	-C, --context K     when --turn is set, show K entries before and after it (default: whole transcript)
//	--format-tool-calls how to render tool calls: verbose | compact | quiet (default: verbose)
//	--json              emit the projected entries as JSON instead of the human render
//
// It deliberately does NOT take --session (the ref IS the transcript) and the
// default (no --turn) renders the WHOLE transcript, which is the share use-case
// ergonomic.
//
// LEGACY pre-envelope blob handling: when the manifest records an empty/unknown
// BlobContractVersion (or the blob fails envelope decode), the command returns an
// actionable "unsupported blob contract" error (what/why/where/how) — pull
// itself is unaffected; full legacy rendering is DEFERRED (user-confirmed).
func buildVillageTranscriptsContextCommand() *cobra.Command {
	var (
		turn        int
		radius      int
		asJSON      bool
		formatTCRaw string
	)

	cmd := &cobra.Command{
		Use:   "context <uuid|url>",
		Short: "Render a pulled transcript in the terminal (offline)",
		Long: `Render a previously pulled village transcript in the terminal.

The reference is the transcript UUID or a pasted village web URL — the same form
'village transcripts pull' accepts. The transcript must already be pulled
locally; this command reads only local data and works offline.

By default the WHOLE transcript is rendered. Pass --turn N to highlight (and,
with -C K, window around) a specific entry. Tool calls are rendered with the
same --format-tool-calls options as 'peasant sessions context'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Runtime errors (bad ref, not-pulled, unsupported blob contract) are
			// not usage errors — suppress cobra's Usage/Flags dump so the actionable
			// one-liner stands alone. Genuine arg/flag misuse is still reported with
			// usage by cobra before RunE runs.
			cmd.SilenceUsage = true

			// Validate --format-tool-calls before any I/O so an invalid value
			// fails fast.
			formatTC := defaults.ToolCallFormat(formatTCRaw)
			if !formatTC.IsValid() {
				return fmt.Errorf("invalid --format-tool-calls value %q: must be one of {verbose, compact, quiet}", formatTCRaw)
			}

			ref, err := pull.ParseTranscriptRef(args[0])
			if err != nil {
				return err
			}

			db, cleanup, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			row, err := locatePulledTranscript(cmd, db, ref.ID)
			if err != nil {
				return err
			}

			entries, err := loadPulledTranscriptEntries(ref.ID, row.PullDir)
			if err != nil {
				return err
			}

			// --turn defaults to -1 (no center) so the whole-transcript render
			// has no spurious "◀ center" marker. When the user passes --turn, we
			// window [turn-C, turn+C] over the projected entries.
			if cmd.Flags().Changed("turn") {
				entries = windowEntries(entries, turn, radius)
			}

			if asJSON {
				return renderPulledContextJSON(cmd, ref.ID, row, entries)
			}

			// Provenance header identifies the rendered data as a
			// foreign pulled transcript, distinct from a local session. Printed to
			// stdout above the render so the human output is self-describing.
			printPulledProvenanceHeader(cmd, ref.ID, row)

			// Reuse the EXISTING renderer UNCHANGED. The pulled transcript carries
			// no local SessionID; pass the transcript ID as the sid label (the
			// renderer only uses sid for the unused-by-body parameter). projectDir
			// is empty (pulled blobs are already redacted/relativized at push).
			renderSessionContextHuman(cmd, schema.SessionID(ref.ID.String()), centerTurn(cmd, turn), entries, "", formatTC)
			return nil
		},
	}

	cmd.Flags().IntVar(&turn, "turn", -1,
		"Highlight the entry at this index (default: render the whole transcript with no center marker)")
	cmd.Flags().IntVarP(&radius, "context", "C", defaults.SessionContextDefaultRadius,
		"With --turn, number of entries to show before and after the target")
	cmd.Flags().BoolVar(&asJSON, defaults.JSONFlagName, false, "Output the projected entries as JSON")
	cmd.Flags().StringVar(&formatTCRaw, "format-tool-calls",
		string(defaults.ToolCallFormatVerbose),
		"How to render tool calls: verbose (full box), compact (one-line), or quiet (hidden)")

	return cmd
}

// centerTurn returns the entry index the renderer should mark as the center. When
// --turn was not passed it returns -1, which never matches an EntryIndex, so no
// "◀ center" marker is printed (whole-transcript render).
func centerTurn(cmd *cobra.Command, turn int) int {
	if cmd.Flags().Changed("turn") {
		return turn
	}
	return -1
}

// windowEntries returns the slice of entries whose EntryIndex falls within
// [center-radius, center+radius]. Because TurnsToEntries assigns monotonically
// increasing indices, this is a contiguous window. Out-of-range bounds simply
// yield whatever entries exist.
func windowEntries(entries []schema.SessionEntry, center, radius int) []schema.SessionEntry {
	lo := center - radius
	hi := center + radius
	out := entries[:0:0]
	for _, e := range entries {
		if e.EntryIndex >= lo && e.EntryIndex <= hi {
			out = append(out, e)
		}
	}
	return out
}

// locatePulledTranscript finds the pulled_transcripts row for the given
// transcript ID by scanning the offline V34 inventory. Context is host-agnostic
// (it does not require login, so it cannot know the active village host); a
// transcript ID is unique per village, and in practice a user pulls from one
// village, so the first match by ID is returned. Returns an actionable
// not-pulled error when no local copy exists.
func locatePulledTranscript(cmd *cobra.Command, db *store.Store, id schema.TranscriptID) (*store.PulledTranscriptRow, error) {
	rows, err := db.ListPulledTranscripts(cmd.Context())
	if err != nil {
		return nil, fmt.Errorf("list pulled transcripts: %w", err)
	}
	for i := range rows {
		if rows[i].TranscriptID == id {
			return &rows[i], nil
		}
	}
	return nil, fmt.Errorf(
		"transcript %s is not pulled locally\n"+
			"  what: no pulled copy of this transcript exists in the local inventory\n"+
			"  why:  `village transcripts context` renders only already-pulled transcripts (offline)\n"+
			"  where: searched the local pulled-transcripts index (%s)\n"+
			"  fix:  run `peasant village transcripts pull %s` first, then re-run context",
		id, string(defaults.ResolveVillagePullsDirPathWith(dataDirOverride(cmd))), id)
}

// loadPulledTranscriptEntries reads the served blob from the pull directory,
// decodes the TranscriptContent envelope, and projects it to flat entries for
// the renderer. It enforces the legacy-blob contract: a manifest recording an
// empty/unknown BlobContractVersion, or a blob that fails envelope decode, yields
// the actionable "unsupported blob contract" error.
func loadPulledTranscriptEntries(id schema.TranscriptID, pullDir string) ([]schema.SessionEntry, error) {
	if pullDir == "" {
		return nil, fmt.Errorf(
			"transcript %s has no recorded pull directory\n"+
				"  what: the pulled-transcripts index row carries an empty pull_dir\n"+
				"  why:  the local index is corrupt or was written by an older version\n"+
				"  fix:  re-pull with `peasant village transcripts pull %s --force`",
			id, id)
	}

	manifestPath := filepath.Join(pullDir, pull.ManifestFilename)
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf(
			"read pull manifest for transcript %s: %w\n"+
				"  where: %s\n"+
				"  fix:   re-pull with `peasant village transcripts pull %s --force`",
			id, err, manifestPath, id)
	}
	var manifest pull.PullManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf(
			"decode pull manifest for transcript %s: %w\n"+
				"  where: %s\n"+
				"  fix:   re-pull with `peasant village transcripts pull %s --force`",
			id, err, manifestPath, id)
	}

	// LEGACY DETECTION (manifest axis): an empty/unknown BlobContractVersion
	// marks a pre-envelope raw provider blob. Report the actionable
	// unsupported-blob-contract error before attempting to decode.
	if manifest.BlobContractVersion == "" {
		return nil, unsupportedBlobContractErr(id, "the manifest records no blob contract version (pre-envelope raw provider blob)")
	}

	blobPath := filepath.Join(pullDir, pull.TranscriptFilename)
	blobBytes, err := os.ReadFile(blobPath)
	if err != nil {
		return nil, fmt.Errorf(
			"read pulled transcript blob for %s: %w\n"+
				"  where: %s\n"+
				"  fix:   re-pull with `peasant village transcripts pull %s --force`",
			id, err, blobPath, id)
	}

	// LEGACY DETECTION (decode axis): a blob that does not decode to a
	// session_detail TranscriptContent envelope is treated as legacy/unsupported.
	var envelope schema.TranscriptContent
	if err := json.Unmarshal(blobBytes, &envelope); err != nil {
		return nil, unsupportedBlobContractErr(id, fmt.Sprintf("the blob is not a TranscriptContent envelope (%v)", err))
	}
	if envelope.Kind != schema.ContentKindSessionDetail || envelope.SessionDetail == nil {
		return nil, unsupportedBlobContractErr(id, fmt.Sprintf("the blob envelope kind is %q, not %q", envelope.Kind, schema.ContentKindSessionDetail))
	}

	return TurnsToEntries(envelope.SessionDetail.Turns), nil
}

// unsupportedBlobContractErr builds the actionable legacy-blob error shared by
// the manifest-axis and decode-axis detection paths (C-actionable-errors).
func unsupportedBlobContractErr(id schema.TranscriptID, detail string) error {
	return fmt.Errorf(
		"unsupported blob contract for transcript %s\n"+
			"  what: %s\n"+
			"  why:  this transcript was published before the structured TranscriptContent envelope; rendering legacy raw blobs is not yet supported\n"+
			"  where: village transcripts context (pulled blob decode)\n"+
			"  fix:  ask the owner to re-push the transcript from source with a current peasant, or await legacy-blob rendering support",
		id, detail)
}

// pulledContextJSON is the JSON envelope for `context --json`. It mirrors the
// sessions-context JSON shape (entries + the transcript provenance) so tooling
// gets a parseable view of the projected, renderable entries.
type pulledContextJSON struct {
	TranscriptID  string                `json:"transcriptId"`
	VillageHost   string                `json:"villageHost"`
	OwnerUsername string                `json:"ownerUsername"`
	LocalID       string                `json:"localSessionId,omitempty"`
	License       string                `json:"license,omitempty"`
	Entries       []schema.SessionEntry `json:"entries"`
}

// renderPulledContextJSON writes the projected entries + provenance as JSON.
func renderPulledContextJSON(cmd *cobra.Command, id schema.TranscriptID, row *store.PulledTranscriptRow, entries []schema.SessionEntry) error {
	out := pulledContextJSON{
		TranscriptID:  id.String(),
		VillageHost:   row.VillageHost,
		OwnerUsername: row.OwnerUsername,
		LocalID:       row.LocalSessionID,
		License:       string(row.License),
		Entries:       entries,
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// printPulledProvenanceHeader prints the pinned provenance header above the human
// render so the output is self-describing as a FOREIGN pulled transcript (not a
// local session). The output-equivalence golden test pins this behavior:
// the golden asserts the body below this header equals the sessions-context body.
func printPulledProvenanceHeader(cmd *cobra.Command, id schema.TranscriptID, row *store.PulledTranscriptRow) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "pulled transcript %s\n", id)
	if row.OwnerUsername != "" {
		fmt.Fprintf(w, "  owner:   @%s\n", row.OwnerUsername)
	}
	if row.VillageHost != "" {
		fmt.Fprintf(w, "  village: %s\n", row.VillageHost)
	}
	if row.Title != "" {
		fmt.Fprintf(w, "  title:   %s\n", row.Title)
	}
	// Unconditional (unlike the omit-empty fields above): the ABSENCE of a
	// license is itself legal information — no grant means default copyright.
	license := "none (all rights reserved)"
	if row.License != "" {
		license = row.License.String()
	}
	fmt.Fprintf(w, "  license: %s\n", license)
	fmt.Fprintln(w)
}
