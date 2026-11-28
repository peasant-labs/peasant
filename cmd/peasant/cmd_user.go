package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/claimprompt"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/spf13/cobra"
)

// BuildUserCommand constructs the local ownership command group. It reads and
// writes the local store only; no Village login or network call is required
// for `show`, and `claim` uses the saved credential the person already has.
func BuildUserCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "user",
		Short:        "Inspect the local owner and claim local sessions",
		SilenceUsage: true,
		Long: `Inspect this data directory's permanent local owner and its append-only
claim log.

A claim assigns local sessions and annotations to the signed-in peasant online
account. Claims append a new entry; they never rewrite earlier entries. Pulled
transcripts and pulled annotations are foreign and are never claimed.`,
	}

	cmd.AddCommand(buildUserShowCommand())
	cmd.AddCommand(buildUserClaimCommand())
	return cmd
}

// userOwnerReport is the machine shape of the local owner. CreatedAt is an
// RFC 3339 time on the wire even though the store keeps Unix milliseconds.
type userOwnerReport struct {
	ID        string `json:"id"`
	CreatedAt string `json:"createdAt"`
}

// userClaimEntry is the machine shape of one claim-log entry.
type userClaimEntry struct {
	ID           string `json:"id"`
	LocalOwnerID string `json:"localOwnerId"`
	VillageURL   string `json:"villageUrl"`
	UserID       string `json:"userId"`
	Username     string `json:"username"`
	ClaimedAt    string `json:"claimedAt"`
	Source       string `json:"source"`
}

// userArtifactCounts is the machine shape of one artifact table's ownership.
// Total = Unclaimed + Mine + Others.
type userArtifactCounts struct {
	Total     int `json:"total"`
	Unclaimed int `json:"unclaimed"`
	Mine      int `json:"mine"`
	Others    int `json:"others"`
}

// userOwnershipReport is the machine shape of local ownership counts.
type userOwnershipReport struct {
	Sessions    userArtifactCounts `json:"sessions"`
	Annotations userArtifactCounts `json:"annotations"`
}

// userPromptState is the contract's LocalOwnerPromptState closed set.
type userPromptState string

const (
	userPromptStateNotApplicable userPromptState = "not_applicable"
	userPromptStateDue           userPromptState = "due"
	userPromptStateDeclined      userPromptState = "declined"
	userPromptStateCompleted     userPromptState = "completed"
)

// userPromptAccount is the selected account scope the prompt is computed for.
type userPromptAccount struct {
	VillageURL string `json:"villageUrl"`
	UserID     string `json:"userId"`
}

// userPromptReport is the contract's LocalOwnerClaimPrompt shape: state,
// account only while signed in, and decidedAt only for a settled prompt.
type userPromptReport struct {
	State     userPromptState    `json:"state"`
	Account   *userPromptAccount `json:"account,omitempty"`
	DecidedAt *string            `json:"decidedAt,omitempty"`
}

// userShowReport is the one document `peasant user show --json` writes. The
// field names and order follow the LocalOwnerStatus contract.
type userShowReport struct {
	Owner       userOwnerReport     `json:"owner"`
	Claims      []userClaimEntry    `json:"claims"`
	TotalClaims int                 `json:"totalClaims"`
	NextBefore  *string             `json:"nextBefore,omitempty"`
	Prompt      userPromptReport    `json:"prompt"`
	Ownership   userOwnershipReport `json:"ownership"`
}

// userClaimOutcome names what an explicit claim did, for a person reading the
// CLI. It is a CLI-only closed set, distinct from the contract's nested status.
type userClaimOutcome string

const (
	userClaimOutcomeClaimed       userClaimOutcome = "claimed"
	userClaimOutcomeEmpty         userClaimOutcome = "empty"
	userClaimOutcomeNothingToMove userClaimOutcome = "nothing_to_claim"
)

// userClaimOutcomeReport carries the CLI-only facts a person needs after a
// claim. It never replaces the contract's status, claimed, alreadyClaimed, or
// claimId fields.
type userClaimOutcomeReport struct {
	Kind       userClaimOutcome `json:"kind"`
	All        bool             `json:"all"`
	Stored     int              `json:"stored"`
	VillageURL string           `json:"villageUrl"`
	UserID     string           `json:"userId"`
	Username   string           `json:"username"`
}

// userClaimReport is the one document `peasant user claim --json` writes. The
// nested status and the counts follow the LocalOwnerClaimResult contract; the
// CLI-only outcome lives under its own typed field.
type userClaimReport struct {
	Status         userShowReport          `json:"status"`
	Claimed        int                     `json:"claimed"`
	AlreadyClaimed int                     `json:"alreadyClaimed"`
	ClaimID        string                  `json:"claimId"`
	Outcome        *userClaimOutcomeReport `json:"outcome,omitempty"`
}

func buildUserShowCommand() *cobra.Command {
	var jsonOutput bool
	var before string
	cmd := &cobra.Command{
		Use:          "show",
		Short:        "Show the local owner, claim log, and ownership counts",
		SilenceUsage: true,
		Long: `Show this data directory's permanent local owner, its newest-first claim
log, and how many local sessions and annotations carry an owner.

This reads local state only. It needs no peasant online login and makes no
network request, so a signed-out report is still a successful report. Pass
--before with a previous nextBefore value to read an older page.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUserShow(cmd, before, jsonOutput)
		},
	}
	cmd.Flags().StringVar(&before, "before", "", "Read the claim page older than this claim id (a previous nextBefore)")
	cmd.Flags().BoolVar(&jsonOutput, defaults.JSONFlagName, false, "Output as JSON instead of human-readable")
	return cmd
}

func buildUserClaimCommand() *cobra.Command {
	var all, yes, jsonOutput bool
	cmd := &cobra.Command{
		Use:          "claim",
		Short:        "Claim local sessions and annotations for the signed-in account",
		SilenceUsage: true,
		Long: `Claim this data directory's local sessions and annotations for the signed-in
peasant online account.

By default only unclaimed artifacts are claimed. --all is an explicit
reassignment of every local session and annotation; it appends a new claim
entry and never rewrites earlier entries. An explicit claim always appends an
entry, even when it finds nothing to move. A non-terminal run does not need
--yes: only an interactive terminal asks for confirmation. Pulled transcripts
and pulled annotations are foreign and are never claimed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUserClaim(cmd, all, yes, jsonOutput)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Reassign every local session and annotation, not only unclaimed ones")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Confirm --all on an interactive terminal (non-terminal runs do not ask)")
	cmd.Flags().BoolVar(&jsonOutput, defaults.JSONFlagName, false, "Output as JSON instead of human-readable")
	return cmd
}

func runUserShow(cmd *cobra.Command, before string, jsonOutput bool) error {
	ctx := cmd.Context()
	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	report, err := buildUserShowReport(ctx, cmd, db, before)
	if err != nil {
		return err
	}
	if jsonOutput {
		return encodeUserJSON(cmd.OutOrStdout(), report)
	}
	printUserShow(cmd.OutOrStdout(), report)
	return nil
}

// buildUserShowReport reads the LocalOwnerStatus facts. Both `user show` and
// `user claim` use it, so the text and JSON surfaces can never drift.
func buildUserShowReport(ctx context.Context, cmd *cobra.Command, db *store.Store, before string) (userShowReport, error) {
	owner, err := db.LocalOwner(ctx)
	if err != nil {
		return userShowReport{}, err
	}
	if owner == nil {
		return userShowReport{}, fmt.Errorf("user show: this store has no local owner; reopen the store to create one, then retry")
	}
	page, err := db.OwnerClaims(ctx, before)
	if err != nil {
		return userShowReport{}, err
	}
	totalClaims, err := db.OwnerClaimCount(ctx)
	if err != nil {
		return userShowReport{}, err
	}
	account, err := userOwnershipAccount(ctx, cmd)
	if err != nil {
		return userShowReport{}, err
	}
	counts, err := db.OwnershipCounts(ctx, account)
	if err != nil {
		return userShowReport{}, err
	}
	prompt, err := readUserPrompt(ctx, cmd, *owner)
	if err != nil {
		return userShowReport{}, err
	}
	createdAt, err := formatOwnerTime(owner.CreatedAt)
	if err != nil {
		return userShowReport{}, fmt.Errorf("user show: local owner created time: %w", err)
	}
	report := userShowReport{
		Owner:       userOwnerReport{ID: owner.ID, CreatedAt: createdAt},
		Claims:      make([]userClaimEntry, 0, len(page.Claims)),
		TotalClaims: totalClaims,
		Prompt:      prompt,
		Ownership: userOwnershipReport{
			Sessions:    userArtifactCounts{Total: counts.TotalSessions, Unclaimed: counts.UnclaimedSessions, Mine: counts.MineSessions, Others: counts.OthersSessions},
			Annotations: userArtifactCounts{Total: counts.TotalAnnotations, Unclaimed: counts.UnclaimedAnnotations, Mine: counts.MineAnnotations, Others: counts.OthersAnnotations},
		},
	}
	if page.NextBefore != "" {
		next := page.NextBefore
		report.NextBefore = &next
	}
	for _, claim := range page.Claims {
		claimedAt, err := formatOwnerTime(claim.ClaimedAt)
		if err != nil {
			return userShowReport{}, fmt.Errorf("user show: claim entry %s claimed time: %w", claim.ID, err)
		}
		report.Claims = append(report.Claims, userClaimEntry{
			ID:           claim.ID,
			LocalOwnerID: claim.LocalOwnerID,
			VillageURL:   claim.VillageURL,
			UserID:       claim.UserID,
			Username:     claim.UsernameSnapshot,
			ClaimedAt:    claimedAt,
			Source:       string(claim.Source),
		})
	}
	return report, nil
}

// userOwnershipAccount resolves the selected account for ownership counts. It
// returns nil while signed out, where Mine is zero.
func userOwnershipAccount(ctx context.Context, cmd *cobra.Command) (*store.OwnershipAccount, error) {
	snapshot, err := auth.CaptureCredentials(ctx, configDirOverride(cmd))
	if errors.Is(err, auth.ErrSignedOut) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the selected account for ownership counts: %w", err)
	}
	creds := snapshot.Credentials()
	base, err := auth.NormalizeVillageURL(creds.VillageURL)
	if err != nil {
		return nil, fmt.Errorf("read the selected account for ownership counts: normalize the saved Village URL: %w", err)
	}
	return &store.OwnershipAccount{VillageURL: base, UserID: creds.UserID}, nil
}

// readUserPrompt computes the contract's prompt state from the selected
// account and the local prompt-decision file. It never contacts Village.
func readUserPrompt(ctx context.Context, cmd *cobra.Command, owner store.LocalOwner) (userPromptReport, error) {
	snapshot, err := auth.CaptureCredentials(ctx, configDirOverride(cmd))
	if errors.Is(err, auth.ErrSignedOut) {
		return userPromptReport{State: userPromptStateNotApplicable}, nil
	}
	if err != nil {
		return userPromptReport{}, fmt.Errorf("read the local ownership prompt state: %w", err)
	}
	creds := snapshot.Credentials()
	base, err := auth.NormalizeVillageURL(creds.VillageURL)
	if err != nil {
		return userPromptReport{}, fmt.Errorf("read the local ownership prompt state: normalize the saved Village URL: %w", err)
	}
	state, err := claimprompt.Load(claimprompt.DefaultPath(defaults.ResolveStateDirPathWith(stateDirOverride(cmd)).String()))
	if err != nil {
		return userPromptReport{}, err
	}
	account := &userPromptAccount{VillageURL: base, UserID: creds.UserID}
	if decision, ok := state.Decisions[claimprompt.ScopeKey(owner.ID, base, creds.UserID)]; ok {
		decided, err := formatOwnerTime(decision.DecidedAt)
		if err != nil {
			return userPromptReport{}, fmt.Errorf("read the local ownership prompt state: decision time: %w", err)
		}
		prompt := userPromptReport{Account: account, DecidedAt: &decided, State: userPromptStateCompleted}
		if decision.Choice == claimprompt.NotNow {
			prompt.State = userPromptStateDeclined
		}
		return prompt, nil
	}
	return userPromptReport{State: userPromptStateDue, Account: account}, nil
}

func runUserClaim(cmd *cobra.Command, all, yes, jsonOutput bool) error {
	ctx := cmd.Context()
	if all && !yes && commandInputIsTerminal(cmd.InOrStdin()) {
		return fmt.Errorf("user claim --all reassigns every local session and annotation, including other accounts' claims; rerun with --yes to confirm, or omit --all to claim only unclaimed artifacts")
	}
	snapshot, err := auth.CaptureCredentials(ctx, configDirOverride(cmd))
	if errors.Is(err, auth.ErrSignedOut) {
		return fmt.Errorf("claim local artifacts: no valid saved peasant online credential; sign in with `peasant village login` and retry; nothing was claimed")
	}
	if err != nil {
		return fmt.Errorf("claim local artifacts: read the saved credential: %w; nothing was claimed", err)
	}
	creds := snapshot.Credentials()
	normalizedURL, err := auth.NormalizeVillageURL(creds.VillageURL)
	if err != nil {
		return fmt.Errorf("claim local artifacts: normalize the saved Village URL: %w; nothing was claimed", err)
	}

	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	scope := "unclaimed artifacts"
	if all {
		scope = "every local session and annotation"
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Claiming %s for %s...\n", scope, creds.Username)

	nowMs := time.Now().UnixMilli()
	result, err := db.ClaimArtifacts(ctx, snapshot, all, store.ClaimSourceManual, nowMs)
	if err != nil {
		return err
	}
	if err := recordClaimCompleted(ctx, cmd, db, normalizedURL, creds.UserID, all, nowMs); err != nil {
		return err
	}
	status, err := buildUserShowReport(ctx, cmd, db, "")
	if err != nil {
		return err
	}
	outcome := userClaimOutcomeClaimed
	switch {
	case result.Stored == 0:
		outcome = userClaimOutcomeEmpty
	case result.Claimed == 0:
		outcome = userClaimOutcomeNothingToMove
	}
	report := userClaimReport{
		Status:         status,
		Claimed:        result.Claimed,
		AlreadyClaimed: result.AlreadyClaimed,
		ClaimID:        result.ClaimID,
		Outcome: &userClaimOutcomeReport{
			Kind:       outcome,
			All:        all,
			Stored:     result.Stored,
			VillageURL: normalizedURL,
			UserID:     creds.UserID,
			Username:   creds.Username,
		},
	}
	if jsonOutput {
		return encodeUserJSON(cmd.OutOrStdout(), report)
	}
	printUserClaim(cmd.OutOrStdout(), report)
	return nil
}

// recordClaimCompleted marks the local owner plus account prompt completed after
// an explicit claim, using the same decision file and write the sign-in prompt
// uses. A completed choice replaces an earlier decline, so a later sign-in does
// not ask again.
func recordClaimCompleted(ctx context.Context, cmd *cobra.Command, db *store.Store, villageURL, userID string, all bool, nowMs int64) error {
	owner, err := db.LocalOwner(ctx)
	if err != nil {
		return err
	}
	if owner == nil {
		return fmt.Errorf("record the completed local ownership prompt: this store has no local owner; reopen the store to create one, then retry")
	}
	statePath := claimprompt.DefaultPath(defaults.ResolveStateDirPathWith(stateDirOverride(cmd)).String())
	state, err := claimprompt.Load(statePath)
	if err != nil {
		return err
	}
	choice := claimprompt.Unclaimed
	if all {
		choice = claimprompt.All
	}
	state.Record(owner.ID, villageURL, userID, choice, nowMs)
	return claimprompt.Save(statePath, state)
}

func printUserShow(out io.Writer, report userShowReport) {
	fmt.Fprintf(out, "Local owner: %s\n", report.Owner.ID)
	fmt.Fprintf(out, "Created:     %s\n", report.Owner.CreatedAt)
	if len(report.Claims) == 0 {
		fmt.Fprintln(out, "Claims:      none")
	} else {
		fmt.Fprintf(out, "Claims:      %d (showing %d)\n", report.TotalClaims, len(report.Claims))
		for _, claim := range report.Claims {
			fmt.Fprintf(out, "  %s  %s  %s  %s  %s\n", claim.ClaimedAt, claim.VillageURL, claim.UserID, claim.Username, claim.Source)
		}
	}
	if report.NextBefore != nil {
		fmt.Fprintf(out, "Older page:  pass --before %s\n", *report.NextBefore)
	}
	fmt.Fprintf(out, "Prompt:      %s", report.Prompt.State)
	if report.Prompt.Account != nil {
		fmt.Fprintf(out, " (%s %s)", report.Prompt.Account.VillageURL, report.Prompt.Account.UserID)
	}
	if report.Prompt.DecidedAt != nil {
		fmt.Fprintf(out, " decided %s", *report.Prompt.DecidedAt)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Sessions:    %d total, %d unclaimed, %d mine, %d other accounts\n", report.Ownership.Sessions.Total, report.Ownership.Sessions.Unclaimed, report.Ownership.Sessions.Mine, report.Ownership.Sessions.Others)
	fmt.Fprintf(out, "Annotations: %d total, %d unclaimed, %d mine, %d other accounts\n", report.Ownership.Annotations.Total, report.Ownership.Annotations.Unclaimed, report.Ownership.Annotations.Mine, report.Ownership.Annotations.Others)
}

func printUserClaim(out io.Writer, report userClaimReport) {
	if report.Outcome == nil {
		return
	}
	switch report.Outcome.Kind {
	case userClaimOutcomeEmpty:
		fmt.Fprintln(out, "There is nothing to claim yet: this store has no local sessions or annotations. The claim was still recorded.")
	case userClaimOutcomeNothingToMove:
		fmt.Fprintln(out, "Nothing to claim: every local session and annotation already has an owner. The claim was still recorded.")
	default:
		fmt.Fprintf(out, "Claimed %d local artifacts", report.Claimed)
		if report.AlreadyClaimed > 0 {
			fmt.Fprintf(out, " (%d already claimed)", report.AlreadyClaimed)
		}
		fmt.Fprintf(out, " for %s (%s %s).\n", report.Outcome.Username, report.Outcome.VillageURL, report.Outcome.UserID)
	}
	fmt.Fprintf(out, "Claim entry: %s\n", report.ClaimID)
}

func encodeUserJSON(out io.Writer, document any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(document)
}

// formatOwnerTime renders a stored Unix-millisecond time at full precision.
// A non-positive value is refused: it would emit an invalid timestamp rather
// than silently print an empty one.
func formatOwnerTime(ms int64) (string, error) {
	if ms <= 0 {
		return "", fmt.Errorf("time %d is not a positive wall-clock millisecond", ms)
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano), nil
}
