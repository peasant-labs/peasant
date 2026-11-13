package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/claimprompt"
	"github.com/peasant-labs/peasant/internal/store/storetest"
)

func TestAnnounceUnclaimedClaimOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := openUserCommandStore(t, dir)
	storetest.SeedSession(t, db, "notice-session-a")
	storetest.SeedSession(t, db, "notice-session-b")
	saveUserCommandCredentials(t, dir)
	snapshot, err := auth.CaptureCredentials(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), claimprompt.FileName)
	deps := claimNoticeDeps{
		interactive: true,
		statePath:   statePath,
		now:         func() int64 { return 1234 },
		capture: func(context.Context) (auth.CredentialSnapshot, error) {
			return snapshot, nil
		},
	}
	var out bytes.Buffer
	if err := announceUnclaimedClaim(t.Context(), &out, db, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "2 local sessions and annotations are not claimed") || !strings.Contains(out.String(), "peasant user claim") {
		t.Fatalf("notice = %q, want the unclaimed count and claim command", out.String())
	}
	state, err := claimprompt.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := db.LocalOwner(t.Context())
	if err != nil || owner == nil {
		t.Fatalf("read owner = %+v, %v", owner, err)
	}
	if !state.Noticed(owner.ID, "https://village.example/team", "11111111-1111-4111-8111-111111111111") {
		t.Fatal("the notice was not recorded")
	}
	if state.Decided(owner.ID, "https://village.example/team", "11111111-1111-4111-8111-111111111111") {
		t.Fatal("the notice settled the prompt; the real prompt must remain due")
	}

	out.Reset()
	if err := announceUnclaimedClaim(t.Context(), &out, db, deps); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("second notice = %q, want it shown once", out.String())
	}
}

func TestAnnounceUnclaimedClaimNonInteractiveRecordsNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := openUserCommandStore(t, dir)
	storetest.SeedSession(t, db, "notice-session-a")
	statePath := filepath.Join(t.TempDir(), claimprompt.FileName)
	called := false
	deps := claimNoticeDeps{
		interactive: false,
		statePath:   statePath,
		capture: func(context.Context) (auth.CredentialSnapshot, error) {
			called = true
			return auth.CredentialSnapshot{}, nil
		},
	}
	var out bytes.Buffer
	if err := announceUnclaimedClaim(t.Context(), &out, db, deps); err != nil {
		t.Fatal(err)
	}
	if called || out.Len() != 0 {
		t.Fatal("a non-interactive command checked or printed the notice")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("a non-interactive command wrote prompt state: %v", err)
	}
}
