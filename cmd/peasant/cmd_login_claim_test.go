package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/claimprompt"
	"github.com/peasant-labs/peasant/internal/store"
)

func signInClaimTestCreds() *auth.Credentials {
	return &auth.Credentials{
		APIKey:     "fixture-api-key",
		KeyID:      "fixture-key-id",
		UserID:     "11111111-1111-4111-8111-111111111111",
		Username:   "alice",
		VillageURL: "https://village.example/team",
	}
}

func signInClaimTestDeps(t *testing.T, input string, interactive bool, claim func(context.Context, auth.CredentialSnapshot, bool) (store.ClaimResult, error)) (signInClaimDeps, string) {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), claimprompt.FileName)
	var out bytes.Buffer
	return signInClaimDeps{
		in:          strings.NewReader(input),
		out:         &out,
		interactive: interactive,
		statePath:   statePath,
		now:         func() int64 { return 1234 },
		capture: func(context.Context) (auth.CredentialSnapshot, error) {
			return auth.CredentialSnapshot{}, nil
		},
		claim: claim,
	}, statePath
}

func readSignInClaimState(t *testing.T, path string) claimprompt.State {
	t.Helper()
	state, err := claimprompt.Load(path)
	if err != nil {
		t.Fatalf("load prompt state: %v", err)
	}
	return state
}

func TestPromptSignInClaimNonTerminalRecordsNothing(t *testing.T) {
	t.Parallel()
	called := false
	deps, statePath := signInClaimTestDeps(t, "2\n", false, func(context.Context, auth.CredentialSnapshot, bool) (store.ClaimResult, error) {
		called = true
		return store.ClaimResult{}, nil
	})
	if err := promptSignInClaim(context.Background(), signInClaimTestCreds(), deps); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("a non-terminal invocation claimed")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("non-terminal invocation wrote prompt state: %v", err)
	}
}

func TestPromptSignInClaimDeclineIsNotAClaim(t *testing.T) {
	t.Parallel()
	called := false
	deps, statePath := signInClaimTestDeps(t, "3\n", true, func(context.Context, auth.CredentialSnapshot, bool) (store.ClaimResult, error) {
		called = true
		return store.ClaimResult{}, nil
	})
	if err := promptSignInClaim(context.Background(), signInClaimTestCreds(), deps); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("declining the prompt claimed artifacts")
	}
	state := readSignInClaimState(t, statePath)
	decision, ok := state.Decisions[claimprompt.ScopeKey("https://village.example/team", "11111111-1111-4111-8111-111111111111")]
	if !ok || decision.Choice != claimprompt.NotNow {
		t.Fatalf("decision = %+v, %v; want a recorded decline", decision, ok)
	}
}

func TestPromptSignInClaimDefaultAndAll(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		input string
		all   bool
	}{
		{name: "default-unclaimed", input: "\n", all: false},
		{name: "claim-all", input: "2\n", all: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var gotAll bool
			deps, statePath := signInClaimTestDeps(t, test.input, true, func(_ context.Context, _ auth.CredentialSnapshot, all bool) (store.ClaimResult, error) {
				gotAll = all
				return store.ClaimResult{ClaimID: "oc_fixture", Claimed: 3}, nil
			})
			if err := promptSignInClaim(context.Background(), signInClaimTestCreds(), deps); err != nil {
				t.Fatal(err)
			}
			if gotAll != test.all {
				t.Fatalf("claim all = %v, want %v", gotAll, test.all)
			}
			state := readSignInClaimState(t, statePath)
			decision, ok := state.Decisions[claimprompt.ScopeKey("https://village.example/team", "11111111-1111-4111-8111-111111111111")]
			if !ok || decision.Choice == claimprompt.NotNow {
				t.Fatalf("decision = %+v, %v; want a recorded claim", decision, ok)
			}
		})
	}
}

func TestPromptSignInClaimFailureStaysRetryable(t *testing.T) {
	t.Parallel()
	deps, statePath := signInClaimTestDeps(t, "1\n", true, func(context.Context, auth.CredentialSnapshot, bool) (store.ClaimResult, error) {
		return store.ClaimResult{}, errors.New("store is unavailable")
	})
	err := promptSignInClaim(context.Background(), signInClaimTestCreds(), deps)
	if err == nil || !strings.Contains(err.Error(), "store is unavailable") {
		t.Fatalf("error = %v, want the claim failure", err)
	}
	if _, statErr := os.Stat(statePath); !os.IsNotExist(statErr) {
		t.Fatalf("a failed claim recorded a decision, so the prompt cannot retry: %v", statErr)
	}
}

func TestPromptSignInClaimAlreadyDecidedSkips(t *testing.T) {
	t.Parallel()
	statePath := filepath.Join(t.TempDir(), claimprompt.FileName)
	state, err := claimprompt.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	state.Record("https://village.example/team", "11111111-1111-4111-8111-111111111111", claimprompt.NotNow, 1)
	if err := claimprompt.Save(statePath, state); err != nil {
		t.Fatal(err)
	}
	called := false
	deps := signInClaimDeps{
		in:          strings.NewReader("2\n"),
		out:         &bytes.Buffer{},
		interactive: true,
		statePath:   statePath,
		capture: func(context.Context) (auth.CredentialSnapshot, error) {
			return auth.CredentialSnapshot{}, nil
		},
		claim: func(context.Context, auth.CredentialSnapshot, bool) (store.ClaimResult, error) {
			called = true
			return store.ClaimResult{}, nil
		},
	}
	if err := promptSignInClaim(context.Background(), signInClaimTestCreds(), deps); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("an already-decided scope prompted or claimed again")
	}
}
