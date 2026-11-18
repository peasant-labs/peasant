package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/claimprompt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

const signInClaimTestOwner = "lo_sign_in_claim_fixture"

//go:embed testdata/sign_in_claim_cases.yaml
var signInClaimCasesYAML []byte

type signInClaimChoiceFixture struct {
	RequiredNames []string                `yaml:"requiredNames"`
	Cases         []signInClaimChoiceCase `yaml:"cases"`
}

type signInClaimChoiceCase struct {
	Name  string `yaml:"name"`
	Input string `yaml:"input"`
	All   bool   `yaml:"all"`
}

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
		in:           strings.NewReader(input),
		out:          &out,
		interactive:  interactive,
		statePath:    statePath,
		localOwnerID: signInClaimTestOwner,
		now:          func() int64 { return 1234 },
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
	decision, ok := state.Decisions[claimprompt.ScopeKey(signInClaimTestOwner, "https://village.example/team", "11111111-1111-4111-8111-111111111111")]
	if !ok || decision.Choice != claimprompt.NotNow {
		t.Fatalf("decision = %+v, %v; want a recorded decline", decision, ok)
	}
}

func TestPromptSignInClaimDefaultAndAll(t *testing.T) {
	t.Parallel()
	var fixture signInClaimChoiceFixture
	decoder := yaml.NewDecoder(bytes.NewReader(signInClaimCasesYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode sign_in_claim_cases.yaml: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, test := range fixture.Cases {
		names = append(names, test.Name)
	}
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: fixture.RequiredNames}, names, "sign_in_claim_cases.yaml"); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.Cases {
		test := test
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			var gotAll bool
			deps, statePath := signInClaimTestDeps(t, test.Input, true, func(_ context.Context, _ auth.CredentialSnapshot, all bool) (store.ClaimResult, error) {
				gotAll = all
				return store.ClaimResult{ClaimID: "oc_fixture", Claimed: 3}, nil
			})
			if err := promptSignInClaim(context.Background(), signInClaimTestCreds(), deps); err != nil {
				t.Fatal(err)
			}
			if gotAll != test.All {
				t.Fatalf("claim all = %v, want %v", gotAll, test.All)
			}
			state := readSignInClaimState(t, statePath)
			decision, ok := state.Decisions[claimprompt.ScopeKey(signInClaimTestOwner, "https://village.example/team", "11111111-1111-4111-8111-111111111111")]
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
	state.Record(signInClaimTestOwner, "https://village.example/team", "11111111-1111-4111-8111-111111111111", claimprompt.NotNow, 1)
	if err := claimprompt.Save(statePath, state); err != nil {
		t.Fatal(err)
	}
	called := false
	deps := signInClaimDeps{
		in:           strings.NewReader("2\n"),
		out:          &bytes.Buffer{},
		interactive:  true,
		statePath:    statePath,
		localOwnerID: signInClaimTestOwner,
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

func TestWriteClaimOutcome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		result store.ClaimResult
		want   string
	}{
		{name: "empty-store", result: store.ClaimResult{}, want: "nothing to claim yet"},
		{name: "nothing-needed", result: store.ClaimResult{ClaimID: "oc_fixture", AlreadyClaimed: 3}, want: "No local artifacts needed this claim"},
		{name: "claimed", result: store.ClaimResult{ClaimID: "oc_fixture", Claimed: 2, AlreadyClaimed: 1}, want: "Claimed 2 local artifacts (1 already claimed)"},
	}
	for _, test := range cases {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			writeClaimOutcome(&out, test.result)
			if !strings.Contains(out.String(), test.want) {
				t.Fatalf("writeClaimOutcome = %q, want %q", out.String(), test.want)
			}
		})
	}
}
