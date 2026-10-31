package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/push_subagent_tree.yaml
var pushSubagentTreeYAML []byte

//go:embed testdata/push_subagent_tree_manifest.yaml
var pushSubagentTreeManifestYAML []byte

type subagentTreeCase struct {
	Name             string              `yaml:"name"`
	Requested        []string            `yaml:"requested"`
	WithoutSubagents bool                `yaml:"withoutSubagents"`
	Edges            map[string][]string `yaml:"edges"`
	Chain            int                 `yaml:"chain"`
	Want             []string            `yaml:"want"`
	WantCount        int                 `yaml:"wantCount"`
	Omitted          map[string]int      `yaml:"omitted"`
	WantError        string              `yaml:"wantError"`
}

// fakeChildReader answers ChildSessionsForParent from fixture edges.
type fakeChildReader map[string][]string

func (f fakeChildReader) ChildSessionsForParent(_ context.Context, parentID string) ([]store.ChildSessionRow, error) {
	var rows []store.ChildSessionRow
	for _, id := range f[parentID] {
		rows = append(rows, store.ChildSessionRow{SessionID: id})
	}
	return rows, nil
}

func loadSubagentTreeCases(t *testing.T) []subagentTreeCase {
	t.Helper()
	var fixture struct {
		Cases []subagentTreeCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(pushSubagentTreeYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode push_subagent_tree.yaml: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(pushSubagentTreeManifestYAML, "push subagent tree")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "push subagent tree"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

// TestExpandSubagentsWalksTheWholeTree pins the subagent expansion: the
// whole descendant tree under its requested root, cycle-safe, and bounded.
func TestExpandSubagentsWalksTheWholeTree(t *testing.T) {
	t.Parallel()
	for _, tc := range loadSubagentTreeCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			edges := fakeChildReader{}
			maps.Copy(edges, tc.Edges)
			parent := tc.Requested[0]
			for i := 1; i <= tc.Chain; i++ {
				child := fmt.Sprintf("c%d", i)
				edges[parent] = append(edges[parent], child)
				parent = child
			}
			got, err := expandSubagents(t.Context(), edges, tc.Requested, tc.WithoutSubagents)
			if tc.WantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.WantError) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.WantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.WantCount > 0 && len(got.sessions) != tc.WantCount {
				t.Fatalf("selected %d sessions, want %d", len(got.sessions), tc.WantCount)
			}
			if tc.Want != nil {
				var flat []string
				for _, s := range got.sessions {
					flat = append(flat, s.ID+":"+s.Parent)
				}
				if !slices.Equal(flat, tc.Want) {
					t.Fatalf("selection = %v, want %v", flat, tc.Want)
				}
			}
			for root, want := range tc.Omitted {
				if got.omitted[root] != want {
					t.Errorf("omitted[%s] = %d, want %d", root, got.omitted[root], want)
				}
			}
		})
	}
}
