package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/claimprompt"
	"github.com/peasant-labs/peasant/internal/store"
	"golang.org/x/term"
)

// signInClaimDeps holds the seams the one-time post-login claim prompt uses.
// Production wires the saved credential snapshot and the analytics store;
// tests can drive each outcome without a network login.
type signInClaimDeps struct {
	in          io.Reader
	out         io.Writer
	interactive bool
	statePath   string
	now         func() int64
	capture     func(context.Context) (auth.CredentialSnapshot, error)
	claim       func(context.Context, auth.CredentialSnapshot, bool) (store.ClaimResult, error)
}

// promptSignInClaim asks once per account scope and data directory whether the
// signed-in account should claim local artifacts. A non-terminal invocation
// asks nothing and records nothing. A decline is recorded but is not a claim.
// A claim failure records no decision, so `peasant user claim` can retry.
func promptSignInClaim(ctx context.Context, creds *auth.Credentials, deps signInClaimDeps) error {
	if creds == nil || !deps.interactive {
		return nil
	}
	base, err := auth.NormalizeVillageURL(creds.VillageURL)
	if err != nil {
		return err
	}
	state, err := claimprompt.Load(deps.statePath)
	if err != nil {
		return err
	}
	if state.Decided(base, creds.UserID) {
		return nil
	}
	choice, err := claimprompt.Prompt(deps.in, deps.out, creds.Username)
	if err != nil {
		return err
	}
	now := deps.now
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	switch choice {
	case claimprompt.NotNow:
		state.Record(base, creds.UserID, choice, now())
		return claimprompt.Save(deps.statePath, state)
	case claimprompt.Unclaimed, claimprompt.All:
		snapshot, err := deps.capture(ctx)
		if err != nil {
			return fmt.Errorf("capture the signed-in account before claiming: %w", err)
		}
		result, err := deps.claim(ctx, snapshot, choice == claimprompt.All)
		if err != nil {
			return fmt.Errorf("claim local artifacts: %w", err)
		}
		writeClaimOutcome(deps.out, result)
		state.Record(base, creds.UserID, choice, now())
		return claimprompt.Save(deps.statePath, state)
	default:
		return fmt.Errorf("ask the local ownership question: unsupported answer %q; nothing was claimed", choice)
	}
}

// writeClaimOutcome reports one completed claim on the progress stream.
func writeClaimOutcome(out io.Writer, result store.ClaimResult) {
	switch {
	case result.ClaimID == "":
		fmt.Fprintln(out, "Nothing to claim: every local session and annotation already has an owner.")
	case result.Claimed == 0:
		fmt.Fprintln(out, "No local artifacts needed this claim; earlier claim entries were kept.")
	default:
		fmt.Fprintf(out, "Claimed %d local artifacts", result.Claimed)
		if result.AlreadyClaimed > 0 {
			fmt.Fprintf(out, " (%d already claimed)", result.AlreadyClaimed)
		}
		fmt.Fprintln(out, ".")
	}
}

// commandInputIsTerminal reports whether the command's input is a terminal.
// A non-terminal invocation must not prompt and records no decision.
func commandInputIsTerminal(in io.Reader) bool {
	file, ok := in.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

// claimPromptWarning describes a prompt or claim failure without failing a
// completed sign-in. The local retry path is named so the person is not stuck.
func claimPromptWarning(err error) string {
	message := strings.TrimSpace(err.Error())
	return "warning: local ownership was not claimed after sign-in: " + message + "\nRun `peasant user claim` to claim your local sessions and annotations."
}
