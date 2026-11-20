package store_test

import (
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"gopkg.in/yaml.v3"
)

type annotationOperation string

const (
	annotationDelete annotationOperation = "delete"
	annotationCreate annotationOperation = "create"
)

//go:embed testdata/annotation_commit_failure.yaml
var annotationCommitFailureYAML []byte

//go:embed testdata/annotation_commit_failure.manifest.yaml
var annotationCommitFailureManifestYAML []byte

func TestAnnotationCommitFailures(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name         string              `yaml:"name"`
			Operation    annotationOperation `yaml:"operation"`
			InitialCount int64               `yaml:"initial_count"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(annotationCommitFailureYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tc := range fixture.Cases {
		if seen[tc.Name] {
			t.Fatalf("duplicate case %q", tc.Name)
		}
		seen[tc.Name] = true
		t.Run(tc.Name, func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t)
			seedTestProjectForStore(t, s)
			hash := testProjectHash
			p := store.CreateAnnotationParams{ProjectHash: &hash, AnnotatorID: seedAnnotatorIDForTest(t, s), AnnotationTypeID: seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome), Value: "resolved"}
			if tc.InitialCount == 1 {
				if _, err := s.CreateAnnotation(ctx, p); err != nil {
					t.Fatal(err)
				}
			}
			// Pool connections are all checked out together, so whichever one the
			// production method takes will reject only COMMIT, permitting rollback.
			pool := s.PoolForTest()
			a := takeConn(t, pool)
			b := takeConn(t, pool)
			denied := 0
			auth := sqlite.AuthorizeFunc(func(action sqlite.Action) sqlite.AuthResult {
				if action.Type() == sqlite.OpTransaction && action.Operation() == "COMMIT" {
					denied++
					return sqlite.AuthResultDeny
				}
				return sqlite.AuthResultOK
			})
			for _, c := range []*sqlite.Conn{a, b} {
				if err := c.SetAuthorizer(auth); err != nil {
					t.Fatal(err)
				}
				pool.Put(c)
			}
			var err error
			switch tc.Operation {
			case annotationDelete:
				_, err = s.DeleteAnnotationsByAnnotator(ctx, pruneAnnotatorName, nil)
			case annotationCreate:
				_, err = s.BatchCreateAnnotations(ctx, []store.CreateAnnotationParams{p})
			default:
				t.Fatalf("unknown operation %q", tc.Operation)
			}
			if denied != 1 {
				t.Fatalf("COMMIT denials = %d, want 1; operation error: %v", denied, err)
			}
			if err == nil || !strings.Contains(err.Error(), "COMMIT") {
				t.Errorf("expected COMMIT error, got %v", err)
			}
			count, countErr := s.CountAnnotationsByAnnotator(ctx, pruneAnnotatorName, nil)
			if countErr != nil || count != tc.InitialCount {
				t.Errorf("after rollback count = %d, err %v; want %d", count, countErr, tc.InitialCount)
			}
		})
	}
	var manifest struct {
		RequiredNames []string `yaml:"required_names"`
	}
	if err := yaml.Unmarshal(annotationCommitFailureManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	required := make(map[string]bool, len(manifest.RequiredNames))
	for _, name := range manifest.RequiredNames {
		if required[name] {
			t.Errorf("duplicate required name %q", name)
		}
		required[name] = true
		if !seen[name] {
			t.Errorf("missing case %q", name)
		}
	}
	for name := range seen {
		if !required[name] {
			t.Errorf("undeclared case %q", name)
		}
	}
}
