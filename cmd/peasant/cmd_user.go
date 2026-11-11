package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/spf13/cobra"
)

// BuildUserCommand constructs the local ownership command group. It reads and
// writes the local store only; no Village login or network call is required
// for `show`, and `claim` uses the saved credential the person already has.
func BuildUserCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Inspect the local owner and claim local sessions",
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

// userOwnerReport is the machine shape of the local owner.
type userOwnerReport struct {
	ID        string `json:"id"`
	CreatedAt string `json:"createdAt"`
}

// userClaimEntry is the machine shape of one ordered claim-log entry.
type userClaimEntry struct {
	ID         string `json:"id"`
	VillageURL string `json:"villageUrl"`
	UserID     string `json:"userId"`
	Username   string `json:"username"`
	ClaimedAt  string `json:"claimedAt"`
	Source     string `json:"source"`
}

// userArtifactCounts is the machine shape of one artifact table's ownership.
type userArtifactCounts struct {
	Total     int `json:"total"`
	Claimed   int `json:"claimed"`
	Unclaimed int `json:"unclaimed"`
}

// userOwnershipReport is the machine shape of local ownership counts.
type userOwnershipReport struct {
	Sessions    userArtifactCounts `json:"sessions"`
	Annotations userArtifactCounts `json:"annotations"`
}

// userShowReport is the one document `peasant user show --json` writes.
type userShowReport struct {
	LocalOwner userOwnerReport     `json:"localOwner"`
	Claims     []userClaimEntry    `json:"claims"`
	Ownership  userOwnershipReport `json:"ownership"`
}

// userClaimReport is the one document `peasant user claim --json` writes.
type userClaimReport struct {
	All            bool   `json:"all"`
	Claimed        int    `json:"claimed"`
	AlreadyClaimed int    `json:"alreadyClaimed"`
	ClaimID        string `json:"claimId,omitempty"`
	VillageURL     string `json:"villageUrl"`
	UserID         string `json:"userId"`
	Username       string `json:"username"`
}

func buildUserShowCommand() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show the local owner, claim log, and ownership counts",
		Long: `Show this data directory's permanent local owner, its ordered claim log, and
how many local sessions and annotations carry an owner.

This reads local state only. It needs no peasant online login and makes no
network request, so a signed-out report is still a successful report.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUserShow(cmd, jsonOutput)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, defaults.JSONFlagName, false, "Output as JSON instead of human-readable")
	return cmd
}

func buildUserClaimCommand() *cobra.Command {
	var all, jsonOutput bool
	cmd := &cobra.Command{
		Use:   "claim",
		Short: "Claim local sessions and annotations for the signed-in account",
		Long: `Claim this data directory's local sessions and annotations for the signed-in
peasant online account.

By default only unclaimed artifacts are claimed. --all is an explicit
reassignment of every local session and annotation; it appends a new claim
entry and never rewrites earlier entries. Pulled transcripts and pulled
annotations are foreign and are never claimed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUserClaim(cmd, all, jsonOutput)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Reassign every local session and annotation, not only unclaimed ones")
	cmd.Flags().BoolVar(&jsonOutput, defaults.JSONFlagName, false, "Output as JSON instead of human-readable")
	return cmd
}

func runUserShow(cmd *cobra.Command, jsonOutput bool) error {
	ctx := cmd.Context()
	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	owner, err := db.EnsureLocalOwner(ctx, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	claims, err := db.OwnerClaims(ctx)
	if err != nil {
		return err
	}
	counts, err := db.OwnershipCounts(ctx)
	if err != nil {
		return err
	}

	report := userShowReport{
		LocalOwner: userOwnerReport{ID: owner.ID, CreatedAt: formatOwnerTime(owner.CreatedAt)},
		Claims:     make([]userClaimEntry, 0, len(claims)),
		Ownership: userOwnershipReport{
			Sessions:    userArtifactCounts{Total: counts.TotalSessions, Claimed: counts.ClaimedSessions, Unclaimed: counts.UnclaimedSessions},
			Annotations: userArtifactCounts{Total: counts.TotalAnnotations, Claimed: counts.ClaimedAnnotations, Unclaimed: counts.UnclaimedAnnotations},
		},
	}
	for _, claim := range claims {
		report.Claims = append(report.Claims, userClaimEntry{
			ID:         claim.ID,
			VillageURL: claim.VillageURL,
			UserID:     claim.UserID,
			Username:   claim.UsernameSnapshot,
			ClaimedAt:  formatOwnerTime(claim.ClaimedAt),
			Source:     claim.Source,
		})
	}
	if jsonOutput {
		return encodeUserJSON(cmd.OutOrStdout(), report)
	}
	printUserShow(cmd.OutOrStdout(), report)
	return nil
}

func runUserClaim(cmd *cobra.Command, all, jsonOutput bool) error {
	ctx := cmd.Context()
	snapshot, err := auth.CaptureCredentials(ctx, configDirOverride(cmd))
	if errors.Is(err, auth.ErrSignedOut) {
		return fmt.Errorf("claim local artifacts: no valid saved peasant online credential; sign in with `peasant village login` and retry; nothing was claimed")
	}
	if err != nil {
		return fmt.Errorf("claim local artifacts: read the saved credential: %w; nothing was claimed", err)
	}
	creds := snapshot.Credentials()

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

	result, err := db.ClaimArtifacts(ctx, snapshot, all, store.ClaimSourceManual, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	report := userClaimReport{
		All:            all,
		Claimed:        result.Claimed,
		AlreadyClaimed: result.AlreadyClaimed,
		ClaimID:        result.ClaimID,
		VillageURL:     creds.VillageURL,
		UserID:         creds.UserID,
		Username:       creds.Username,
	}
	if jsonOutput {
		return encodeUserJSON(cmd.OutOrStdout(), report)
	}
	printUserClaim(cmd.OutOrStdout(), report)
	return nil
}

func printUserShow(out io.Writer, report userShowReport) {
	fmt.Fprintf(out, "Local owner: %s\n", report.LocalOwner.ID)
	fmt.Fprintf(out, "Created:     %s\n", report.LocalOwner.CreatedAt)
	if len(report.Claims) == 0 {
		fmt.Fprintln(out, "Claims:      none")
	} else {
		fmt.Fprintf(out, "Claims:      %d\n", len(report.Claims))
		for _, claim := range report.Claims {
			fmt.Fprintf(out, "  %s  %s  %s  %s  %s\n", claim.ClaimedAt, claim.VillageURL, claim.UserID, claim.Username, claim.Source)
		}
	}
	fmt.Fprintf(out, "Sessions:    %d total, %d claimed, %d unclaimed\n", report.Ownership.Sessions.Total, report.Ownership.Sessions.Claimed, report.Ownership.Sessions.Unclaimed)
	fmt.Fprintf(out, "Annotations: %d total, %d claimed, %d unclaimed\n", report.Ownership.Annotations.Total, report.Ownership.Annotations.Claimed, report.Ownership.Annotations.Unclaimed)
}

func printUserClaim(out io.Writer, report userClaimReport) {
	switch {
	case report.ClaimID == "":
		fmt.Fprintln(out, "Nothing to claim: every local session and annotation already has an owner.")
	case report.Claimed == 0:
		fmt.Fprintln(out, "No local artifacts needed this claim; earlier claim entries were kept.")
	default:
		fmt.Fprintf(out, "Claimed %d local artifacts", report.Claimed)
		if report.AlreadyClaimed > 0 {
			fmt.Fprintf(out, " (%d already claimed)", report.AlreadyClaimed)
		}
		fmt.Fprintf(out, " for %s.\n", report.Username)
	}
}

func encodeUserJSON(out io.Writer, document any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(document)
}

func formatOwnerTime(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
