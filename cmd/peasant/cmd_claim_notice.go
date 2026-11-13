package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/claimprompt"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
)

// claimNoticeDeps holds the seams the one-time already-signed-in notice uses.
// It is best-effort and never interrupts the command that triggered it.
type claimNoticeDeps struct {
	interactive bool
	statePath   string
	now         func() int64
	capture     func(context.Context) (auth.CredentialSnapshot, error)
}

// announceUnclaimedClaim prints one stderr notice on an interactive command
// when the installation is already signed in, has unclaimed local rows, and has
// no prompt decision or earlier notice for this local owner and account scope.
// The notice is recorded separately from a prompt decision, so it never
// suppresses the real sign-in prompt.
func announceUnclaimedClaim(ctx context.Context, out io.Writer, db *store.Store, deps claimNoticeDeps) error {
	if db == nil || !deps.interactive {
		return nil
	}
	capture := deps.capture
	if capture == nil {
		return nil
	}
	snapshot, err := capture(ctx)
	if errors.Is(err, auth.ErrSignedOut) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check for unclaimed local ownership: %w", err)
	}
	creds := snapshot.Credentials()
	base, err := auth.NormalizeVillageURL(creds.VillageURL)
	if err != nil {
		return fmt.Errorf("check for unclaimed local ownership: normalize the saved Village URL: %w", err)
	}
	owner, err := db.LocalOwner(ctx)
	if err != nil {
		return fmt.Errorf("check for unclaimed local ownership: %w", err)
	}
	if owner == nil {
		return nil
	}
	state, err := claimprompt.Load(deps.statePath)
	if err != nil {
		return fmt.Errorf("check for unclaimed local ownership: %w", err)
	}
	if state.Decided(owner.ID, base, creds.UserID) || state.Noticed(owner.ID, base, creds.UserID) {
		return nil
	}
	counts, err := db.OwnershipCounts(ctx)
	if err != nil {
		return fmt.Errorf("check for unclaimed local ownership: %w", err)
	}
	unclaimed := counts.UnclaimedSessions + counts.UnclaimedAnnotations
	if unclaimed == 0 {
		return nil
	}
	fmt.Fprintf(out, "notice: %d local sessions and annotations are not claimed for %s. Run `peasant user claim` to claim them, or `peasant user claim --all` to reassign every local artifact.\n", unclaimed, creds.Username)
	now := deps.now
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	state.RecordNotice(owner.ID, base, creds.UserID, now())
	if err := claimprompt.Save(deps.statePath, state); err != nil {
		return fmt.Errorf("record the one-time local ownership notice: %w", err)
	}
	return nil
}

// announceUnclaimedClaimOnCommand wires the notice to a harvested command's
// credentials, state directory, and stderr. A failure is reported as a warning
// by the caller; it never changes the command's outcome.
func announceUnclaimedClaimOnCommand(cmd interface {
	Context() context.Context
	InOrStdin() io.Reader
	ErrOrStderr() io.Writer
}, db *store.Store, configHome, stateHome string) error {
	return announceUnclaimedClaim(cmd.Context(), cmd.ErrOrStderr(), db, claimNoticeDeps{
		interactive: commandInputIsTerminal(cmd.InOrStdin()),
		statePath:   claimprompt.DefaultPath(defaults.ResolveStateDirPathWith(stateHome).String()),
		now:         func() int64 { return time.Now().UnixMilli() },
		capture: func(ctx context.Context) (auth.CredentialSnapshot, error) {
			return auth.CaptureCredentials(ctx, configHome)
		},
	})
}
