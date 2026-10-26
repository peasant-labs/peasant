package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/collectiveobservations"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

// collectivePullFlags carries the pull command's flags into the collective
// driver, so the driver does not reach back into the command builder's closure.
type collectivePullFlags struct {
	Original     *auth.CredentialSnapshot
	Observations *collectiveobservations.Service
	Force        bool
	DryRun       bool
	JSON         bool
	Level        outputLevel
	// User, when non-empty, keeps only the transcripts contributed by that village
	// account. It is applied over the fully-paged listing, so it is exact.
	User string
}

// collectivePullOutcome is one transcript's result in a bulk pull. Result is nil
// only when the pipeline failed before it could classify an outcome.
type collectivePullOutcome struct {
	Ref    pull.TranscriptRef
	Result *pull.PullResult
	Err    error
}

// failed reports whether the outcome is a failure. An error is always a failure;
// a result whose typed status is neither pulled nor up-to-date is one too.
func (o collectivePullOutcome) failed() bool {
	if o.Err != nil {
		return true
	}
	if o.Result == nil {
		return true
	}
	switch o.Result.Status {
	case pull.PullStatusPulled, pull.PullStatusUpToDate:
		return false
	default:
		return true
	}
}

// runVillageTranscriptsPullCollectives pulls every transcript an accessible
// collective holds.
//
// It asks the village as few times as the pull surface allows. ONE listing pass
// returns each transcript's metadata, including the served-blob hash, so no
// per-transcript metadata read is needed. ONE skip-gate request answers, for
// every transcript this database already holds, whether the stored blob and its
// annotations are still current, so a current transcript is not touched at all.
// The pull-window preflight runs once here, not once per transcript.
//
// Explicit live references resolve through the canonical original-account
// service, including server-disclosed nonmember collectives. Bare-user discovery
// and dry-run keep their existing membership listing. After negotiation and the
// complete required listing, explicit live selections are tracked even when the
// eligible shelf is empty; tracking never joins or publishes to the collective.
//
// A per-transcript failure does not stop the run. Each outcome is reported and
// the command fails at the end, because a bulk pull that pulled nine of ten
// transcripts is neither a success nor a silent partial.
//
// SIGINT and SIGTERM stop the run cleanly through the signal-aware context:
// the loop checks it before each transcript and after each pipeline call, so
// an interrupt lands at a transcript boundary with no partial copy (the
// pipeline removes its staging directory on any failure before the write
// stage). The run then reports how many of the listed transcripts completed
// and exits non-zero; a re-run resumes because current transcripts are
// skipped by the skip-gate and the pipeline diff.
func runVillageTranscriptsPullCollectives(cmd *cobra.Command, db *store.Store, client *village.VillageClient, pipeline collectivePuller, values []string, flags collectivePullFlags) error {
	signalCtx, stopSignals := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancelOperation := context.WithCancel(signalCtx)
	defer cancelOperation()
	if err := ctx.Err(); err != nil {
		cmd.SilenceUsage = true
		return collectivePullCancellationError(0, 0, nil, flags.User, err)
	}
	writer := cmd.OutOrStdout()

	var collectives []schema.VillageUUID
	var names []string
	var err error
	var observations *collectiveobservations.Service
	if len(values) > 0 && !flags.DryRun && flags.Original != nil {
		observations = flags.Observations
		if observations == nil {
			return fmt.Errorf("resolve explicit collective selection: observation service is not configured; no references or preferences changed; construct the pull action with its original account service")
		}
		seen := map[schema.VillageUUID]bool{}
		for _, value := range values {
			var id schema.VillageUUID
			id, err = observations.ResolveReferenceFor(ctx, *flags.Original, value, true)
			if err != nil {
				break
			}
			if !seen[id] {
				seen[id] = true
				collectives = append(collectives, id)
				names = append(names, id.String())
			}
		}
	} else {
		// Bare --user retains membership-based discovery; dry-run retains its
		// existing read-only path without new auth/detail or tracking requests.
		collectives, names, err = resolveCollectiveRefs(ctx, client, values)
	}
	if err != nil {
		if ctx.Err() != nil {
			cmd.SilenceUsage = true
			return collectivePullCancellationError(0, 0, names, flags.User, ctx.Err())
		}
		return err
	}

	// NEGOTIATE once, before any download: an incompatible pull window aborts the
	// whole run instead of failing transcript by transcript. A village that
	// predates the collectives filter advertises an older window here, which is
	// what stops this CLI from over-pulling.
	envelope, err := client.NegotiatePull(ctx)
	if err != nil {
		if ctx.Err() != nil {
			cmd.SilenceUsage = true
			return collectivePullCancellationError(0, 0, names, flags.User, ctx.Err())
		}
		return err
	}

	listed, err := collectCollectiveTranscripts(ctx, client, collectives, flags.User)
	if err != nil {
		if ctx.Err() != nil {
			cmd.SilenceUsage = true
			return collectivePullCancellationError(0, 0, names, flags.User, ctx.Err())
		}
		return err
	}
	// Resolve the entire explicit selection before writing any preference. Track
	// even an empty eligible shelf, but never an unresolved reference or dry run.
	if observations != nil {
		for i, id := range collectives {
			response, err := observations.TrackFor(ctx, *flags.Original, id)
			if err != nil {
				return fmt.Errorf("record explicit collective selection %s: %w; no publication or membership change was requested", id, err)
			}
			for _, row := range response.Collectives {
				if row.ID == id {
					names[i] = row.Name
					break
				}
			}
		}
	}

	if len(listed) == 0 {
		if flags.JSON {
			return encodeCollectivePullJSON(cmd, collectives, names, nil)
		}
		fmt.Fprintf(writer, "no pullable transcripts %s\n", selectionLabel(names, flags.User))
		return nil
	}

	// ONE skip-gate question prunes whatever this database already holds and the
	// village still has current. --force means re-pull regardless, so it is not
	// asked.
	current := map[schema.TranscriptID]store.PulledTranscriptRow{}
	if !flags.Force {
		current, err = currentHeldTranscripts(ctx, db, client, listed)
		if err != nil {
			if ctx.Err() != nil {
				cmd.SilenceUsage = true
				return collectivePullCancellationError(0, len(listed), names, flags.User, ctx.Err())
			}
			// The gate is an optimization, never a correctness dependency: a village
			// that cannot answer it must not cost the pull. Say so and check every
			// transcript individually, which is what the pipeline does anyway.
			fmt.Fprintf(cmd.ErrOrStderr(), "notice: the pull skip-gate did not answer, so every transcript is checked individually: %v\n", err)
			current = map[schema.TranscriptID]store.PulledTranscriptRow{}
		}
	}

	if flags.DryRun && !flags.JSON && flags.Level != outputQuiet {
		fmt.Fprintf(cmd.ErrOrStderr(), "Dry run — no transcript will be written\n\n")
	}

	// The stderr bar shows done/total with the elapsed run time while the run
	// proceeds, throttled to one refresh per floor interval plus the initial
	// and final bars. It stays silent when stderr is not a terminal and under
	// --json; the per-transcript lines and the closing summary keep printing
	// either way because they travel on stdout.
	//
	// Nothing here wipes the panel. The bar and its final footer are the ending
	// state: a user who looks back after a cancelled run must still find them on
	// the terminal, so every write appends and the last one to land stays.
	progress := newCollectivePullProgress(cmd.ErrOrStderr(), flags.JSON, pullProgressTheme(cmd))
	progress.SetTotal(len(listed))

	// Paint the canceling state the moment the interrupt arrives, while the
	// transcript in flight finishes. Without it the bar sits on its last refresh
	// for the whole unwind, which reads as an unresponsive command. The watcher
	// yields to the final state; Cancel is the last panel to land.
	cancelWatch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			progress.MarkCanceling()
		case <-cancelWatch:
		}
	}()
	defer close(cancelWatch)

	outcomes := make([]collectivePullOutcome, 0, len(listed))
	for _, row := range listed {
		if err := ctx.Err(); err != nil {
			progress.Cancel()
			cmd.SilenceUsage = true
			return collectivePullCancellationError(len(outcomes), len(listed), names, flags.User, err)
		}
		ref := pull.TranscriptRef{ID: row.TranscriptID}

		// Already current per the gate: report it without touching the village.
		if local, held := current[row.TranscriptID]; held {
			outcome := collectivePullOutcome{Ref: ref, Result: &pull.PullResult{
				Ref:            ref,
				Status:         pull.PullStatusUpToDate,
				VillageHost:    local.VillageHost,
				PullDir:        local.PullDir,
				ServedBlobHash: local.ContentHash,
				License:        local.License,
				DryRun:         flags.DryRun,
			}}
			if observer, ok := pipeline.(interface {
				ObservePulledAssociations(context.Context, *pull.PullResult)
			}); ok {
				observer.ObservePulledAssociations(ctx, outcome.Result)
			}
			printPullMetadataWarning(cmd, outcome.Result)
			outcomes = append(outcomes, outcome)
			if !flags.JSON {
				renderCollectivePullOutcome(cmd, outcome, flags.DryRun, flags.Level)
			}
			progress.Advance(len(outcomes))
			continue
		}

		// Hand the pipeline the metadata the listing already returned, and tell it
		// the window was checked. Both spare a per-transcript request.
		meta := row
		result, pullErr := pipeline.PullTranscript(ctx, ref, pull.PullOptions{
			Force:           flags.Force,
			DryRun:          flags.DryRun,
			Meta:            &meta,
			SkipNegotiate:   true,
			EnvelopeVersion: envelope,
		})
		printPullMetadataWarning(cmd, result)
		if err := ctx.Err(); err != nil {
			// The interrupt wins over the pipeline result: a failure here is
			// the cancellation, not a transcript failure. When the
			// transcript landed just before the interrupt raced it, keep
			// that outcome so the report counts what is on disk; otherwise
			// the attempt left no partial copy (the pipeline removes its
			// staging directory on any failure before the write stage) and
			// is simply not recorded.
			if landedBeforeCancel(result, pullErr) {
				outcome := collectivePullOutcome{Ref: ref, Result: result, Err: pullErr}
				outcomes = append(outcomes, outcome)
				if !flags.JSON {
					renderCollectivePullOutcome(cmd, outcome, flags.DryRun, flags.Level)
				}
				progress.Advance(len(outcomes))
			}
			progress.Cancel()
			cmd.SilenceUsage = true
			return collectivePullCancellationError(len(outcomes), len(listed), names, flags.User, err)
		}
		outcome := collectivePullOutcome{Ref: ref, Result: result, Err: pullErr}
		outcomes = append(outcomes, outcome)
		if !flags.JSON {
			renderCollectivePullOutcome(cmd, outcome, flags.DryRun, flags.Level)
		}
		progress.Advance(len(outcomes))
	}

	if flags.JSON {
		if err := encodeCollectivePullJSON(cmd, collectives, names, outcomes); err != nil {
			return err
		}
	} else {
		printCollectivePullSummary(cmd, names, outcomes, flags.DryRun, flags.User)
	}

	failures := 0
	for _, outcome := range outcomes {
		if outcome.failed() {
			failures++
		}
	}
	if failures > 0 {
		return fmt.Errorf(
			"%d of %d transcripts did not pull\n"+
				"  what: the run reported each failure above and left the successful transcripts in place\n"+
				"  why:  a bulk pull reports the truth about the whole set rather than a partial success\n"+
				"  fix:  re-run the same command — current transcripts are skipped, so only the failures repeat",
			failures, len(outcomes))
	}
	return nil
}

// currentHeldTranscripts asks the village, in ONE request, which of the listed
// transcripts this database already holds are still current in content AND
// annotations, and returns those ids with their local rows.
//
// An id the village does not answer is treated as NOT current: the response
// withholds non-pullable ids by omission, so an absent answer must never be read
// as "current".
func currentHeldTranscripts(ctx context.Context, db *store.Store, client *village.VillageClient, listed []schema.PullTranscriptInfo) (map[schema.TranscriptID]store.PulledTranscriptRow, error) {
	held, err := db.ListPulledTranscripts(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the local pulled inventory: %w", err)
	}
	if len(held) == 0 {
		return nil, nil
	}
	heldByID := make(map[schema.TranscriptID]store.PulledTranscriptRow, len(held))
	for _, row := range held {
		heldByID[row.TranscriptID] = row
	}

	// Group the listed ids this database holds by the host their copy lives in, so
	// the annotation-hash read runs once per host (in practice once).
	idsByHost := make(map[string][]schema.TranscriptID)
	for _, row := range listed {
		if local, ok := heldByID[row.TranscriptID]; ok {
			idsByHost[local.VillageHost] = append(idsByHost[local.VillageHost], row.TranscriptID)
		}
	}
	if len(idsByHost) == 0 {
		return nil, nil
	}

	items := make([]schema.PullSkipGateItem, 0, len(listed))
	for host, ids := range idsByHost {
		hashes, err := db.ListPulledAnnotationHashesByTranscript(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			items = append(items, schema.PullSkipGateItem{
				TranscriptID:     id,
				ContentHash:      heldByID[id].ContentHash,
				AnnotationHashes: hashes[id],
			})
		}
	}

	answer, err := client.PullSkipGate(ctx, schema.NewPullSkipGateRequest(items))
	if err != nil {
		return nil, err
	}

	listedByID := make(map[schema.TranscriptID]schema.PullTranscriptInfo, len(listed))
	for _, row := range listed {
		listedByID[row.TranscriptID] = row
	}
	current := make(map[schema.TranscriptID]store.PulledTranscriptRow)
	for _, result := range answer.Results {
		if !result.ContentCurrent || !result.AnnotationsCurrent {
			continue
		}
		local, ok := heldByID[result.TranscriptID]
		if !ok {
			continue
		}
		// The gate compares hashes, not people: a held copy whose owner or
		// annotation authors the village may now show differently goes to the
		// pipeline, which rewrites what it no longer shows.
		manifest, err := os.ReadFile(filepath.Join(local.PullDir, pull.ManifestFilename))
		if err != nil || !pull.HeldIdentityCurrent(manifest, listedByID[result.TranscriptID]) {
			continue
		}
		current[result.TranscriptID] = local
	}
	return current, nil
}

// collectCollectiveTranscripts pages the village's pullable listing and returns
// the listed rows in first-seen order with duplicates removed. The rows carry
// the metadata the pull needs — including the served-blob hash — so no
// per-transcript metadata read follows. Duplicates are removed client-side so a
// transcript held by two listed collectives is pulled once.
//
// An empty collectives slice lists everything the caller may pull (own plus
// approved shares), which is what `--user` alone means: one contributor across
// every collective the caller can pull from. A non-empty owner keeps only that
// contributor's rows, filtered here because the listing pages to the end, so the
// caller holds every row the filter must consider.
func collectCollectiveTranscripts(ctx context.Context, client *village.VillageClient, collectives []schema.VillageUUID, owner string) ([]schema.PullTranscriptInfo, error) {
	const pageLimit = 100

	listed := make([]schema.PullTranscriptInfo, 0)
	seen := make(map[schema.TranscriptID]struct{})
	for page := 1; ; page++ {
		response, err := client.ListPullableTranscripts(ctx, village.PullListOptions{
			Page:        page,
			Limit:       pageLimit,
			Collectives: collectives,
		})
		if err != nil {
			return nil, fmt.Errorf("list the pullable transcripts of the selected collectives: %w", err)
		}
		if response == nil || len(response.Transcripts) == 0 {
			break
		}
		for i := range response.Transcripts {
			row := response.Transcripts[i]
			if _, duplicate := seen[row.TranscriptID]; duplicate {
				continue
			}
			seen[row.TranscriptID] = struct{}{}
			listed = append(listed, row)
		}
		// Stop on a short page (the end of the listing) or once the accumulated set
		// reaches the server's total for this filter.
		if len(response.Transcripts) < pageLimit || len(listed) >= response.Total {
			break
		}
	}
	return filterByOwner(listed, owner), nil
}

// renderCollectivePullOutcome writes one transcript's outcome line, matching the
// single-transcript rendering.
func renderCollectivePullOutcome(cmd *cobra.Command, outcome collectivePullOutcome, dryRun bool, level outputLevel) {
	printPullSummary(cmd, outcome.Ref, outcome.Result, dryRun, level)
}

// landedBeforeCancel reports whether a pipeline result counts as landed work
// when an interrupt races it. Only a successful pulled or up-to-date result
// is kept in the cancellation report; any failure left no partial copy
// because the pipeline removes its staging directory on any failure before
// the write stage.
func landedBeforeCancel(result *pull.PullResult, pullErr error) bool {
	if pullErr != nil || result == nil {
		return false
	}
	return result.Status == pull.PullStatusPulled ||
		result.Status == pull.PullStatusUpToDate
}

// printCollectivePullSummary writes the closing count line for a bulk pull.
func printCollectivePullSummary(cmd *cobra.Command, names []string, outcomes []collectivePullOutcome, dryRun bool, owner string) {
	var pulled, upToDate, failed int
	for _, outcome := range outcomes {
		switch {
		case outcome.failed():
			failed++
		case outcome.Result != nil && outcome.Result.Status == pull.PullStatusUpToDate:
			upToDate++
		default:
			pulled++
		}
	}
	verb := "pulled"
	if dryRun {
		verb = "would pull"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s %d, up to date %d, failed %d of %d %s\n",
		verb, pulled, upToDate, failed, len(outcomes), selectionLabel(names, owner))
}

// selectionLabel names what a bulk pull selected: the collectives when any were
// given, and the contributor when one was.
func selectionLabel(names []string, owner string) string {
	label := "in the selected collectives"
	switch len(names) {
	case 0:
	case 1:
		label = fmt.Sprintf("in collective %s", names[0])
	default:
		label = fmt.Sprintf("in %d collectives (%s)", len(names), joinNames(names))
	}
	if owner == "" {
		return label
	}
	if len(names) == 0 {
		return fmt.Sprintf("contributed by %s across your collectives", owner)
	}
	return fmt.Sprintf("%s contributed by %s", label, owner)
}

// joinNames renders names for a one-line summary.
func joinNames(names []string) string {
	out := ""
	for i, name := range names {
		if i > 0 {
			out += ", "
		}
		out += name
	}
	return out
}

// jsonPullCollective is the JSON envelope for a bulk pull: the resolved
// collectives, the per-transcript results, and the totals.
type jsonPullCollective struct {
	Collectives []jsonPulledCollective   `json:"collectives"`
	Totals      jsonPullCollectiveTotals `json:"totals"`
	Results     []jsonPullResult         `json:"results"`
}

type jsonPulledCollective struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type jsonPullCollectiveTotals struct {
	Pulled   int `json:"pulled"`
	UpToDate int `json:"upToDate"`
	Failed   int `json:"failed"`
	Total    int `json:"total"`
}

// encodeCollectivePullJSON writes the aggregate. It never reports a partial
// view: every outcome is present, with the totals beside it.
func encodeCollectivePullJSON(cmd *cobra.Command, collectives []schema.VillageUUID, names []string, outcomes []collectivePullOutcome) error {
	out := jsonPullCollective{
		Collectives: make([]jsonPulledCollective, 0, len(collectives)),
		Results:     make([]jsonPullResult, 0, len(outcomes)),
	}
	for i, id := range collectives {
		name := ""
		if i < len(names) {
			name = names[i]
		}
		out.Collectives = append(out.Collectives, jsonPulledCollective{ID: id.String(), Name: name})
	}
	for _, outcome := range outcomes {
		result := buildJSONPullResult(outcome.Ref, outcome.Result, outcome.Err)
		out.Results = append(out.Results, result)
		switch {
		case outcome.failed():
			out.Totals.Failed++
		case outcome.Result != nil && outcome.Result.Status == pull.PullStatusUpToDate:
			out.Totals.UpToDate++
		default:
			out.Totals.Pulled++
		}
	}
	out.Totals.Total = len(outcomes)

	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(out)
}
