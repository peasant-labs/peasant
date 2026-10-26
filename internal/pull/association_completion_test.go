package pull_test

import (
	"context"
	_ "embed"
	"errors"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/association-completion.yaml
var associationCompletionYAML []byte

type completionCase struct {
	Name      string                              `yaml:"name"`
	Path      string                              `yaml:"path"`
	State     schema.PullAssociationMetadataState `yaml:"state"`
	Issue     schema.OperationIssueCode           `yaml:"issue"`
	DryRun    bool                                `yaml:"dryRun"`
	WantCalls int                                 `yaml:"wantCalls"`
	WantError bool                                `yaml:"wantError"`
}

type completionObserver struct {
	result schema.PullAssociationMetadataResult
	calls  int
}

var _ pull.AssociationObserver = (*completionObserver)(nil)

func (o *completionObserver) ObserveAssociations(context.Context, schema.TranscriptID) (schema.PullAssociationMetadataResult, error) {
	o.calls++
	if o.result.Warning != nil {
		return o.result, errors.New("optional metadata dependency failed")
	}
	return o.result, nil
}

func TestAssociationCompletionEveryContentExit(t *testing.T) {
	var corpus struct {
		ObservedAt    time.Time        `yaml:"observedAt"`
		RequiredNames []string         `yaml:"requiredNames"`
		Cases         []completionCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(associationCompletionYAML, &corpus); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range corpus.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatal("empty/duplicate completion case")
		}
		names[c.Name] = true
	}
	for _, name := range corpus.RequiredNames {
		if !names[name] {
			t.Fatalf("required completion case missing: %s", name)
		}
	}
	for _, c := range corpus.Cases {
		t.Run(c.Name, func(t *testing.T) {
			filesystem := testutil.NewMemFS()
			st := &testutil.StubPullStore{}
			ref := newRef(t)
			if c.Path == "hash" || c.Path == "etag" || c.Path == "download" {
				if _, err := newPipeline(happyReader(), filesystem, st, testutil.NewFixedClock(), testCreds(), testPullsRoot).PullTranscript(t.Context(), ref, pull.PullOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			reader := happyReader()
			switch c.Path {
			case "etag":
				reader.Meta.ContentHash = ""
				reader.ContentErr = village.ErrNotModified
			case "download":
				reader.Meta.ContentHash = ""
			case "content-error":
				reader.ContentErr = village.ErrPullNotFound
			case "negotiate-error":
				reader.NegotiateErr = errors.New("required negotiation failed")
			case "index-error":
				st.CommitErr = errors.New("required content projection failed")
			}
			metadata := schema.PullAssociationMetadataResult{State: c.State}
			if c.State != schema.PullAssociationMetadataUnknown && c.State != "" {
				at := corpus.ObservedAt
				metadata.ObservedAt = &at
			}
			if c.Issue != "" {
				metadata.Warning = &schema.OperationIssue{Code: c.Issue, Operation: schema.CollectiveOperationPull, Message: "Optional association source failed", Retry: "Retry metadata fetch explicitly"}
			}
			observer := &completionObserver{result: metadata}
			p := pull.NewPipeline(reader, treeRenameFS{filesystem}, st, testutil.NewFixedClock(), testCreds(), testPullsRoot, pull.WithInstallSync(func(string) error { return nil }), pull.WithInstallLock(func(context.Context, string, schema.TranscriptID) (func() error, error) {
				return func() error { return nil }, nil
			}), pull.WithAssociationObserver(observer))
			result, err := p.PullTranscript(t.Context(), ref, pull.PullOptions{DryRun: c.DryRun})
			if (err != nil) != c.WantError {
				t.Fatalf("required content outcome changed: result=%+v err=%v", result, err)
			}
			if observer.calls != c.WantCalls {
				t.Fatalf("metadata calls=%d want=%d", observer.calls, c.WantCalls)
			}
			if c.WantCalls == 0 {
				if result.AssociationMetadata != nil {
					t.Fatal("metadata invented without observation")
				}
				return
			}
			if result.AssociationMetadata == nil {
				t.Fatal("successful content exit omitted independent metadata")
			}
			if err := result.AssociationMetadata.Validate(); err != nil {
				t.Fatal(err)
			}
			if result.AssociationMetadata.State != c.State {
				t.Fatalf("metadata state=%s want=%s", result.AssociationMetadata.State, c.State)
			}
			if c.Path == "hash" && reader.ContentCalls != 0 {
				t.Fatal("equal hash transferred content")
			}
		})
	}
}
