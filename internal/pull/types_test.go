package pull_test

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/schema"
)

// TestParseTranscriptRef drives the REAL pull.ParseTranscriptRef (the SUT) over
// the agentfilter-style YAML fixtures embedded in the github.com/peasant-labs/schema
// module (loaded via schema.LoadPullRefFixtures).
// The 15 valid cases are the id_casings x ref_forms cross-product; the invalid +
// negative-lookalike cases come from the fixture's categories. See that module's
// pull-fixture generators and the meta-tests that pin the fixture's structure/coverage.
func TestParseTranscriptRef(t *testing.T) {
	fix, err := schema.LoadPullRefFixtures()
	if err != nil {
		t.Fatalf("LoadPullRefFixtures: %v", err)
	}

	cases, err := fix.AllCases()
	if err != nil {
		t.Fatalf("AllCases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("AllCases returned no cases — fixture wiring is broken")
	}

	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			ref, err := pull.ParseTranscriptRef(tc.Input)
			if tc.WantErr {
				if err == nil {
					t.Fatalf("ParseTranscriptRef(%q) = %+v, want error", tc.Input, ref)
				}
				if tc.ErrContains != "" && !strings.Contains(err.Error(), tc.ErrContains) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.ErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTranscriptRef(%q) unexpected error: %v", tc.Input, err)
			}
			if string(ref.ID) != tc.WantID {
				t.Errorf("ID = %q, want %q", ref.ID, tc.WantID)
			}
			if ref.FromURL != tc.WantFromURL {
				t.Errorf("FromURL = %v, want %v", ref.FromURL, tc.WantFromURL)
			}
		})
	}
}

// TestPullStatus_String pins each typed PullStatus constant's String() against
// the wire string in pull_statuses.yaml. The typed constants are enumerated here
// (no bare literals); the expected wire string comes from the fixture, keyed by
// const_name.
func TestPullStatus_String(t *testing.T) {
	fix, err := schema.LoadPullStatusFixtures()
	if err != nil {
		t.Fatalf("LoadPullStatusFixtures: %v", err)
	}

	// Enumerate the typed constants -> their documented const_name. This is the
	// only place the Go constant <-> fixture row correspondence is asserted.
	constByName := map[string]pull.PullStatus{
		"PullStatusPulled":        pull.PullStatusPulled,
		"PullStatusUpToDate":      pull.PullStatusUpToDate,
		"PullStatusNotFound":      pull.PullStatusNotFound,
		"PullStatusNotLoggedIn":   pull.PullStatusNotLoggedIn,
		"PullStatusContractError": pull.PullStatusContractError,
		"PullStatusError":         pull.PullStatusError,
	}

	if len(constByName) != len(fix.Statuses) {
		t.Fatalf("typed constants (%d) and fixture rows (%d) disagree on count",
			len(constByName), len(fix.Statuses))
	}

	for constName, status := range constByName {
		want, ok := fix.WireFor(constName)
		if !ok {
			t.Errorf("no pull_statuses.yaml row for const %q", constName)
			continue
		}
		if got := status.String(); got != want {
			t.Errorf("%s.String() = %q, want %q", constName, got, want)
		}
	}
}
