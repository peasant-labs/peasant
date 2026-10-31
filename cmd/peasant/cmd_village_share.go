package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/clicontract"
	"github.com/peasant-labs/peasant/internal/collectiveobservations"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// BuildVillageShareCommand constructs `peasant village share`.
func BuildVillageShareCommand() *cobra.Command {
	return buildVillageShareCommand(pushCommandDependencies{})
}

type villageShareOptions struct {
	collectives []string
	sessions    []string
	dryRun      bool
	jsonOutput  bool
	yes         bool
}

func buildVillageShareCommand(deps pushCommandDependencies) *cobra.Command {
	if deps.newVillageClient == nil {
		deps.newVillageClient = village.NewVillageClient
	}
	if deps.isTTY == nil {
		deps.isTTY = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	}
	var opts villageShareOptions
	cmd := &cobra.Command{
		Use:   "share [<transcript-uuid|url>]...",
		Short: "Offer transcripts you already published to a collective",
		Long: `Offer owned, already-published transcripts to one or more collectives.

Name a transcript by its UUID or page URL, or a local session by --session or
--sessions-from, which resolve through the publication receipt on this computer.
Its members can read a transcript the collective accepts, and a curated
collective holds it for its owner's approval.

The command asks once before it shares. --yes confirms in advance. Without --yes,
--json or a run with no terminal is refused and shares nothing. --dry-run never
asks and sends no change.

It uploads nothing, changes no visibility or license, reads no auto-publish rule,
and takes nothing back. A collective that already has a share of the transcript,
live, rejected or taken back, is skipped and not asked again.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runVillageShare(cmd, deps, opts, args)
		},
	}
	cmd.Flags().StringArrayVar(&opts.collectives, pushCollectiveFlag, nil, "Offer each transcript to this collective: a name, UUID or page URL (repeatable; required)")
	cmd.Flags().StringArrayVar(&opts.sessions, pushSessionFlag, nil, "Share the transcript this local session was published as (repeatable)")
	clicontract.RegisterSessionsFrom(cmd)
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Print the plan: read each transcript's visibility and share history, ask nothing and send no change")
	cmd.Flags().BoolVar(&opts.jsonOutput, "json", false, "Print one JSON document on stdout")
	clicontract.AddYesFlag(cmd, &opts.yes, "Share without asking")
	return cmd
}

// shareTarget is one transcript to share. SessionID is empty when the
// transcript was named directly.
type shareTarget struct {
	Transcript schema.TranscriptID
	SessionID  string
	URL        string
}

// shareTargetError is a named transcript or session that did not resolve.
type shareTargetError struct {
	Ref     string `json:"ref"`
	Message string `json:"message"`
}

// resolveShareTargets turns references and session ids into transcripts, each
// once, references first. A target that does not resolve is reported and the
// others continue. A session resolves only through this account's receipt.
func resolveShareTargets(ctx context.Context, db *store.Store, creds *auth.Credentials, refs []string, sessionIDs []string) ([]shareTarget, []shareTargetError) {
	var targets []shareTarget
	failed := []shareTargetError{}
	index := map[schema.TranscriptID]int{}
	add := func(target shareTarget) {
		if at, seen := index[target.Transcript]; seen {
			if targets[at].SessionID == "" {
				targets[at] = target
			}
			return
		}
		index[target.Transcript] = len(targets)
		targets = append(targets, target)
	}
	for _, raw := range refs {
		ref, err := pull.ParseTranscriptRef(raw)
		if err != nil {
			failed = append(failed, shareTargetError{Ref: raw, Message: err.Error()})
			continue
		}
		add(shareTarget{Transcript: ref.ID})
	}
	if len(sessionIDs) == 0 {
		return targets, failed
	}
	receipts, err := db.SessionPublications(ctx, creds.VillageURL, creds.UserID, sessionIDs)
	for _, id := range sessionIDs {
		if err != nil {
			failed = append(failed, shareTargetError{Ref: id, Message: "read the publication receipt: " + err.Error()})
			continue
		}
		receipt, ok := receipts[id]
		if !ok {
			failed = append(failed, shareTargetError{Ref: id, Message: fmt.Sprintf("no publication receipt for this account; run `peasant village push --session %s`", id)})
			continue
		}
		add(shareTarget{Transcript: receipt.Receipt.TranscriptID, SessionID: id, URL: receipt.Receipt.TranscriptURL})
	}
	return targets, failed
}

type shareCollective struct {
	ID   schema.VillageUUID `json:"id"`
	Name string             `json:"name"`
}

type shareTranscript struct {
	TranscriptID schema.TranscriptID                `json:"transcript_id"`
	SessionID    string                             `json:"session_id,omitempty"`
	URL          string                             `json:"url,omitempty"`
	Visibility   schema.VillageTranscriptVisibility `json:"visibility,omitempty"`
	Steps        []schema.SyncPushStepResult        `json:"steps"`
}

// shareDocument is the --json document. NeedsConfirmation is set on a dry run
// only: it says the same run without --dry-run needs a confirmation.
type shareDocument struct {
	DryRun            bool               `json:"dry_run"`
	NeedsConfirmation *bool              `json:"needsConfirmation,omitempty"`
	Collectives       []shareCollective  `json:"collectives"`
	Transcripts       []shareTranscript  `json:"transcripts"`
	Errors            []shareTargetError `json:"errors"`
	clicontract.Meta
	// Refusal names why the run stopped before it shared anything.
	Refusal string `json:"refusal,omitempty"`
}

func runVillageShare(cmd *cobra.Command, deps pushCommandDependencies, opts villageShareOptions, args []string) error {
	ctx := cmd.Context()
	warnings := clicontract.NewCollector(cmd.ErrOrStderr(), opts.jsonOutput)
	doc := shareDocument{DryRun: opts.dryRun, Collectives: []shareCollective{}, Transcripts: []shareTranscript{}, Errors: []shareTargetError{}}
	if opts.dryRun {
		needsConfirmation := !opts.yes
		doc.NeedsConfirmation = &needsConfirmation
	}
	// refuse ends a run that shared nothing, exit status 3.
	refuse := func(cause error) error {
		if opts.jsonOutput {
			doc.Meta = warnings.Meta("", "")
			doc.Refusal = cause.Error()
			if err := clicontract.WriteJSON(cmd.OutOrStdout(), doc); err != nil {
				return err
			}
		}
		return &pushNothingAttemptedError{cause: cause}
	}

	selection, _, err := resolveExplicitSelection(cmd)
	if err != nil {
		return refuse(err)
	}
	if len(opts.collectives) == 0 {
		return refuse(shareUsage(cmd, "no collective was named", "pass --collective with a name, UUID or page URL"))
	}
	if len(args) == 0 && len(selection.IDs) == 0 {
		return refuse(shareUsage(cmd, "no transcript was named", "pass a transcript UUID or URL, --session, or --sessions-from"))
	}

	// Decided before anything is read, so a run that cannot be confirmed makes
	// no request at all.
	decision := clicontract.PromptPolicy{DryRun: opts.dryRun, JSON: opts.jsonOutput, AssumeYes: opts.yes, IsTTY: deps.isTTY()}.Decide()
	if decision == clicontract.ConfirmRefusedNoTTY {
		return refuse(clicontract.ConfirmationRequired(cmd, "sharing with a collective"))
	}

	creds, err := auth.LoadCredentialsFrom(configDirOverride(cmd))
	if err != nil {
		return refuse(fmt.Errorf("load credentials: %w", err))
	}
	if creds == nil || !creds.IsValid() || creds.VillageURL == "" {
		return refuse(errors.New("not logged in; nothing was shared; run 'peasant village login' first"))
	}

	// A real run records its share events, so it needs the store whatever it
	// was given. A dry run opens it only to read receipts, and never writes.
	var db *store.Store
	if !opts.dryRun || len(selection.IDs) > 0 {
		var closeDB func()
		db, closeDB, err = openShareStore(cmd, opts.dryRun)
		if err != nil {
			return refuse(err)
		}
		defer closeDB()
	}

	client := deps.newVillageClient(creds.VillageURL, creds.APIKey, village.NewPooledHTTPClient(creds.VillageURL, 1))
	ids, names, err := resolveCollectiveRefs(ctx, client, opts.collectives)
	if err != nil {
		return refuse(fmt.Errorf("village share: %w", err))
	}
	for i, id := range ids {
		doc.Collectives = append(doc.Collectives, shareCollective{ID: id, Name: names[i]})
	}
	targets, targetErrors := resolveShareTargets(ctx, db, creds, args, selection.IDs)
	doc.Errors = targetErrors

	// The plan is a dry run of every target, so the prompt counts what would
	// really be asked. The real pass reads the history again, so a decision
	// made while the prompt was open is respected.
	for _, target := range targets {
		entry := shareTranscript{TranscriptID: target.Transcript, SessionID: target.SessionID, URL: target.URL}
		entry.Visibility, err = client.TranscriptVisibility(ctx, target.Transcript)
		if err == nil {
			entry.Steps, err = push.ShareExisting(ctx, client, target.Transcript, ids, true)
		}
		if err != nil {
			entry.Steps = failedShareSteps(ids, err)
		}
		doc.Transcripts = append(doc.Transcripts, entry)
	}

	if !opts.dryRun {
		asked, askedNames := shareAsked(doc.Transcripts, doc.Collectives)
		if asked > 0 {
			if decision == clicontract.ConfirmAsk {
				confirmed, promptErr := promptShareConfirmation(cmd.InOrStdin(), cmd.ErrOrStderr(), asked, askedNames)
				if promptErr != nil {
					return promptErr
				}
				if !confirmed {
					fmt.Fprintln(cmd.ErrOrStderr(), "village share: not confirmed; nothing was shared")
					return nil
				}
			}
			observations := collectiveobservations.New(collectiveobservations.Dependencies{Store: db, ConfigHome: configDirOverride(cmd), NewIdentityReader: func(auth.Credentials) collectiveobservations.IdentityReader { return client }})
			sharer, observeErr := push.ObserveSharer(ctx, observations, time.Now, creds, client)
			if observeErr != nil {
				return refuse(fmt.Errorf("village share: %w; nothing was shared", observeErr))
			}
			for i := range doc.Transcripts {
				entry := &doc.Transcripts[i]
				if !hasPlannedShare(entry.Steps) {
					continue
				}
				steps, shareErr := push.ShareExisting(ctx, sharer, entry.TranscriptID, ids, false)
				if shareErr != nil {
					steps = failedShareSteps(ids, shareErr)
				}
				entry.Steps = steps
			}
		}
	}

	empty, reason := clicontract.EmptyCode(""), ""
	if shareNothingToDo(doc) {
		empty = clicontract.EmptyNothingChanged
		reason = "every collective already has a share of every transcript, so nothing was asked"
	}
	doc.Meta = warnings.Meta(empty, reason)
	if opts.jsonOutput {
		if err := clicontract.WriteJSON(cmd.OutOrStdout(), doc); err != nil {
			return err
		}
	} else {
		printShareDocument(cmd.OutOrStdout(), cmd.ErrOrStderr(), doc, reason)
	}
	if failures := shareFailures(doc); failures > 0 {
		return fmt.Errorf("village share: %d of the requested shares failed; the rest are as listed", failures)
	}
	return nil
}

// openShareStore opens the store. A dry run opens it read-only: that runs no
// migration, so it can neither ask for consent nor write.
func openShareStore(cmd *cobra.Command, dryRun bool) (*store.Store, func(), error) {
	if !dryRun {
		return openDB(cmd)
	}
	db, err := store.OpenReadOnlyWithOptions(string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd))), contentIndexFormats()...)
	if err != nil {
		return nil, func() {}, fmt.Errorf("open analytics store: %w", err)
	}
	return db, func() { db.Close() }, nil
}

func shareUsage(cmd *cobra.Command, what, fix string) error {
	return clicontract.NewUsageError(fmt.Sprintf("%s: %s; nothing was shared; %s", cmd.CommandPath(), what, fix))
}

func failedShareSteps(ids []schema.VillageUUID, err error) []schema.SyncPushStepResult {
	steps := make([]schema.SyncPushStepResult, 0, len(ids))
	for _, id := range ids {
		target := id
		steps = append(steps, schema.SyncPushStepResult{Step: schema.SyncPushStepAddCollective, CollectiveID: &target, Outcome: schema.SyncPushStepFailed, Reason: "nothing was shared: " + err.Error()})
	}
	return steps
}

func hasPlannedShare(steps []schema.SyncPushStepResult) bool {
	for _, step := range steps {
		if step.Outcome == schema.SyncPushStepNotAttempted {
			return true
		}
	}
	return false
}

// shareAsked counts the transcripts the plan would really share, and names the
// collectives that would receive one, in the order given.
func shareAsked(transcripts []shareTranscript, collectives []shareCollective) (int, []string) {
	count := 0
	receiving := map[schema.VillageUUID]bool{}
	for _, entry := range transcripts {
		if !hasPlannedShare(entry.Steps) {
			continue
		}
		count++
		for _, step := range entry.Steps {
			if step.Outcome == schema.SyncPushStepNotAttempted {
				receiving[*step.CollectiveID] = true
			}
		}
	}
	var names []string
	for _, collective := range collectives {
		if receiving[collective.ID] {
			names = append(names, collective.Name)
		}
	}
	return count, names
}

// promptShareConfirmation asks once. It restates that members can read what is
// shared; Village decides who actually can.
func promptShareConfirmation(r io.Reader, w io.Writer, transcripts int, names []string) (bool, error) {
	noun, members := "transcripts", "Its"
	if transcripts == 1 {
		noun = "transcript"
	}
	if len(names) > 1 {
		members = "Their"
	}
	pronoun := "them"
	if transcripts == 1 {
		pronoun = "it"
	}
	fmt.Fprintf(w, "Share %d %s with %s? %s members will be able to read %s. [y/N] ",
		transcripts, noun, strings.Join(names, ", "), members, pronoun)
	return readConfirmation(r)
}

func shareFailures(doc shareDocument) int {
	failures := len(doc.Errors)
	for _, entry := range doc.Transcripts {
		for _, step := range entry.Steps {
			if step.Outcome == schema.SyncPushStepFailed {
				failures++
			}
		}
	}
	return failures
}

// shareNothingToDo reports a run in which every step was skipped.
func shareNothingToDo(doc shareDocument) bool {
	if len(doc.Errors) > 0 || len(doc.Transcripts) == 0 {
		return false
	}
	for _, entry := range doc.Transcripts {
		for _, step := range entry.Steps {
			if step.Outcome != schema.SyncPushStepSkipped {
				return false
			}
		}
	}
	return true
}

func printShareDocument(stdout, stderr io.Writer, doc shareDocument, emptyReason string) {
	names := make(map[schema.VillageUUID]string, len(doc.Collectives))
	for _, collective := range doc.Collectives {
		names[collective.ID] = collective.Name
	}
	for _, entry := range doc.Transcripts {
		label := entry.TranscriptID.String()
		if entry.SessionID != "" {
			label += " (session " + entry.SessionID + ")"
		}
		for _, step := range entry.Steps {
			line := fmt.Sprintf("%s: %s: %s", label, names[*step.CollectiveID], step.Outcome)
			if step.Reason != "" {
				line += ": " + step.Reason
			}
			fmt.Fprintln(stdout, line)
		}
	}
	for _, failed := range doc.Errors {
		fmt.Fprintf(stderr, "village share: %s: %s\n", failed.Ref, failed.Message)
	}
	if emptyReason != "" {
		fmt.Fprintln(stderr, emptyReason)
	}
}
