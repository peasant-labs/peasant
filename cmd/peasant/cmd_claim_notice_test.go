package main

import (
	"bytes"
	"context"
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/claimprompt"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/claim_notice_cases.yaml
var claimNoticeCasesYAML []byte

type claimNoticeFixture struct {
	RequiredNames []string          `yaml:"requiredNames"`
	Cases         []claimNoticeCase `yaml:"cases"`
}

type claimNoticeCase struct {
	Name            string `yaml:"name"`
	InteractiveRuns int    `yaml:"interactiveRuns"`
	WantNotices     int    `yaml:"wantNotices"`
}

// TestClaimExistingInstallPromptedOnce is the named notice case: two
// interactive runs on an already-signed-in installation print exactly one
// notice, and the notice does not settle the real prompt.
func TestClaimExistingInstallPromptedOnce(t *testing.T) {
	t.Parallel()
	var fixture claimNoticeFixture
	decoder := yaml.NewDecoder(bytes.NewReader(claimNoticeCasesYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode claim_notice_cases.yaml: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		names = append(names, testCase.Name)
	}
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: fixture.RequiredNames}, names, "claim_notice_cases.yaml"); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
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
			notices := 0
			for run := 0; run < testCase.InteractiveRuns; run++ {
				var out bytes.Buffer
				if err := announceUnclaimedClaim(t.Context(), &out, db, deps); err != nil {
					t.Fatalf("run %d: %v", run+1, err)
				}
				if strings.Contains(out.String(), "notice:") {
					notices++
					if !strings.Contains(out.String(), "2 local sessions and annotations are not claimed") || !strings.Contains(out.String(), "peasant user claim") {
						t.Fatalf("notice = %q, want the unclaimed count and claim command", out.String())
					}
				}
			}
			if notices != testCase.WantNotices {
				t.Fatalf("notices across %d runs = %d, want %d", testCase.InteractiveRuns, notices, testCase.WantNotices)
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
		})
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
