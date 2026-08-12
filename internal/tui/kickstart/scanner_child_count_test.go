package kickstart_test

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
)

//go:embed testdata/child_counts.yaml
var childCountData []byte

// childCountRow is one expected session row of the folded branch: its id, the
// child-session count its Meta must carry ("" for none), and whether the row is
// marked as already imported.
type childCountRow struct {
	SessionID  string `yaml:"sessionId"`
	ChildCount string `yaml:"childCount"`
	Ingested   bool   `yaml:"ingested"`
}

// childCountCase is one whole discovery listing and the branch rows BuildForest
// must fold it into, in render order.
type childCountCase struct {
	Name         string                `yaml:"name"`
	Ingested     []string              `yaml:"ingested"`
	Listings     []ftue.SessionListing `yaml:"listings"`
	ExpectedRows []childCountRow       `yaml:"expectedRows"`
}

// childCountDocument is the whole fixture plus its row-count guard.
type childCountDocument struct {
	ExpectedCaseCount int              `yaml:"expectedCaseCount"`
	Cases             []childCountCase `yaml:"cases"`
}

func loadChildCounts(t *testing.T) childCountDocument {
	t.Helper()
	var doc childCountDocument
	dec := yaml.NewDecoder(bytes.NewReader(childCountData))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/child_counts.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		t.Fatalf("child_counts.yaml must hold exactly one document: %v", err)
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		t.Fatalf("expectedCaseCount=%d but %d cases present", doc.ExpectedCaseCount, len(doc.Cases))
	}
	for _, c := range doc.Cases {
		if len(c.ExpectedRows) == 0 {
			t.Fatalf("child count fixture case %q declares no expected rows; an empty row list turns the case into a guaranteed pass", c.Name)
		}
		if len(c.Listings) == 0 {
			t.Fatalf("child count fixture case %q declares no listings to fold", c.Name)
		}
		for i, row := range c.ExpectedRows {
			// A blank session id would match whatever the fold happened to emit.
			testutil.RequireFixtureFields(t, "child count", c.Name, []testutil.FixtureField{
				{Key: fmt.Sprintf("expectedRows[%d].sessionId", i), Value: row.SessionID},
			})
		}
	}
	return doc
}

// TestBuildForest_ChildCountsAndImportGrouping proves the real fold: a parent
// session stays a LEAF row carrying the number of subagent sessions discovered
// transitively beneath it (never another level of nesting), an undiscovered or
// cyclic subagent reference cannot inflate or hang that count, and a branch
// lists its not-yet-imported sessions before its already-imported ones.
func TestBuildForest_ChildCountsAndImportGrouping(t *testing.T) {
	t.Parallel()
	doc := loadChildCounts(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			source := kickstart.NewScannerTreeSource(
				c.Listings,
				withFixturePathResolver(),
				kickstart.WithIngestedSessionIDs(c.Ingested),
			)
			roots, err := source.Load(context.Background())
			if err != nil {
				t.Fatalf("scanner load: %v", err)
			}
			if len(roots) != 1 || len(roots[0].Children) != 1 {
				t.Fatalf("want one project with one branch, got %d projects", len(roots))
			}
			rows := roots[0].Children[0].Children
			if len(rows) != len(c.ExpectedRows) {
				t.Fatalf("branch has %d session rows, want %d", len(rows), len(c.ExpectedRows))
			}
			for i, want := range c.ExpectedRows {
				row := rows[i]
				if row.ID != want.SessionID {
					t.Fatalf("row %d = %q, want %q (render order)", i, row.ID, want.SessionID)
				}
				if len(row.Children) != 0 {
					t.Errorf("session %q must be a leaf, got %d children", row.ID, len(row.Children))
				}
				if got := row.Meta[settings.MetaChildCount]; got != want.ChildCount {
					t.Errorf("session %q child count = %q, want %q", row.ID, got, want.ChildCount)
				}
				wantMark := ""
				if want.Ingested {
					wantMark = settings.MetaIngestedValue
				}
				if got := row.Meta[settings.MetaIngested]; got != wantMark {
					t.Errorf("session %q ingested mark = %q, want %q", row.ID, got, wantMark)
				}
			}
		})
	}
}
