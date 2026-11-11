package discovery_test

import (
	_ "embed"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/preview_text.yaml
var previewTextData []byte

type previewTextSize struct {
	Bytes int64  `yaml:"bytes"`
	Want  string `yaml:"want"`
}

type previewTextContext struct {
	Kind           string   `yaml:"kind"`
	Project        string   `yaml:"project"`
	Branch         string   `yaml:"branch"`
	Harnesses      []string `yaml:"harnesses"`
	Remotes        []string `yaml:"remotes"`
	GitDirectories []string `yaml:"gitDirectories"`
	ClonePaths     []string `yaml:"clonePaths"`
	Branches       []string `yaml:"branches"`
	SessionCount   int      `yaml:"sessionCount"`
}

type previewTextCase struct {
	Name        string             `yaml:"name"`
	Op          string             `yaml:"op"`
	Context     previewTextContext `yaml:"context"`
	Lines       []string           `yaml:"lines"`
	Value       string             `yaml:"value"`
	Want        string             `yaml:"want"`
	Consumed    int64              `yaml:"consumed"`
	Total       int64              `yaml:"total"`
	More        bool               `yaml:"more"`
	CanLoadMore bool               `yaml:"canLoadMore"`
	Sizes       []previewTextSize  `yaml:"sizes"`
}

type previewTextDocument struct {
	RequiredNames []string          `yaml:"requiredNames"`
	Cases         []previewTextCase `yaml:"cases"`
}

func (c previewTextContext) preview(t *testing.T) discovery.PreviewContext {
	t.Helper()
	kinds := map[string]discovery.PreviewKind{"project": discovery.PreviewProject, "branch": discovery.PreviewBranch}
	kind, ok := kinds[c.Kind]
	if !ok {
		t.Fatalf("fixture preview kind %q is not project or branch", c.Kind)
	}
	return discovery.PreviewContext{
		Kind: kind, Project: c.Project, Branch: c.Branch, Harnesses: c.Harnesses, Remotes: c.Remotes,
		GitDirectories: c.GitDirectories, ClonePaths: c.ClonePaths, Branches: c.Branches, SessionCount: c.SessionCount,
	}
}

// TestPreviewText runs the shared preview sentences: the exact lines the
// terminal pane and the setup page both show for a project or a branch row, and
// the notice for a transcript cut short.
func TestPreviewText(t *testing.T) {
	t.Parallel()
	var document previewTextDocument
	if err := testutil.DecodeFixtureYAML(previewTextData, &document); err != nil {
		t.Fatalf("decode preview_text.yaml: %v", err)
	}
	present := make(map[string]bool, len(document.Cases))
	for _, row := range document.Cases {
		if present[row.Name] {
			t.Fatalf("preview_text.yaml repeats case %q", row.Name)
		}
		present[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("internal/discovery/testdata/preview_text.yaml", "case", document.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			switch row.Op {
			case "repository":
				got := discovery.RepositoryPreviewLines(row.Context.preview(t), discovery.CleanPreviewValue)
				if !reflect.DeepEqual(got, row.Lines) {
					t.Fatalf("lines = %q, want %q", got, row.Lines)
				}
			case "clean":
				if got := discovery.CleanPreviewValue(row.Value); got != row.Want {
					t.Fatalf("CleanPreviewValue(%q) = %q, want %q", row.Value, got, row.Want)
				}
			case "cut":
				if got := discovery.PreviewCutNotice(row.Consumed, row.Total, row.More, row.CanLoadMore); got != row.Want {
					t.Fatalf("PreviewCutNotice = %q, want %q", got, row.Want)
				}
			case "size":
				for _, size := range row.Sizes {
					if got := discovery.PreviewByteSize(size.Bytes); got != size.Want {
						t.Fatalf("PreviewByteSize(%d) = %q, want %q", size.Bytes, got, size.Want)
					}
				}
			default:
				t.Fatalf("fixture case %q has op %q, want repository, clean, cut, or size", row.Name, row.Op)
			}
		})
	}
}
