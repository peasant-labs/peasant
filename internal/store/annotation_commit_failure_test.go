package store_test

import (
	"context"
	_ "embed"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
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

// authorizeCommitFailure verifies that the two checked-out connections exhaust
// the pool before installing the authorizer on both of them.
func authorizeCommitFailure(t *testing.T, pool *sqlitex.Pool, auth sqlite.AuthorizeFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var held []*sqlite.Conn
	defer func() {
		for _, conn := range held {
			pool.Put(conn)
		}
	}()
	for range 2 {
		conn, err := pool.Take(ctx)
		if err != nil {
			t.Fatalf("take connection from expected two-connection pool: %v", err)
		}
		held = append(held, conn)
	}
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer probeCancel()
	extra, err := pool.Take(probeCtx)
	if extra != nil {
		held = append(held, extra)
		t.Fatal("expected a two-connection pool, but a third connection was available")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("checking pool exhaustion: got %v, want deadline exceeded", err)
	}
	for _, conn := range held {
		if err := conn.SetAuthorizer(auth); err != nil {
			t.Fatal(err)
		}
	}
}

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
			pool := s.PoolForTest()
			denied := 0
			auth := sqlite.AuthorizeFunc(func(action sqlite.Action) sqlite.AuthResult {
				if action.Type() == sqlite.OpTransaction && action.Operation() == "COMMIT" {
					denied++
					return sqlite.AuthResultDeny
				}
				return sqlite.AuthResultOK
			})
			authorizeCommitFailure(t, pool, auth)
			var err error
			switch tc.Operation {
			case annotationDelete:
				var count int64
				count, err = s.DeleteAnnotationsByAnnotator(ctx, pruneAnnotatorName, nil)
				if count != 0 {
					t.Errorf("delete count on commit failure = %d, want 0", count)
				}
			case annotationCreate:
				var result []string
				result, err = s.BatchCreateAnnotations(ctx, []store.CreateAnnotationParams{p})
				if result != nil {
					t.Errorf("batch result on commit failure = %v, want nil", result)
				}
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
