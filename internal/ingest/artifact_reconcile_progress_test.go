package ingest_test

import (
	"bytes"
	_ "embed"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/artifact_reconcile_progress.yaml
var artifactReconcileProgressYAML []byte

type artifactReconcileProgressFixtures struct {
	RequiredNames []string `yaml:"requiredNames"`
	Transcript    string   `yaml:"transcript"`
	Cases         []struct {
		Name      string `yaml:"name"`
		SessionID string `yaml:"sessionID"`
	} `yaml:"cases"`
}

func loadArtifactReconcileProgressFixtures(t *testing.T) artifactReconcileProgressFixtures {
	t.Helper()
	var fixture artifactReconcileProgressFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(artifactReconcileProgressYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("retained reconciliation progress fixture requires one YAML document")
	}
	required := []string{"settled-retained-session-alpha", "settled-retained-session-bravo", "settled-retained-session-charlie"}
	if !reflect.DeepEqual(required, fixture.RequiredNames) {
		t.Fatal("retained reconciliation progress required-name manifest changed")
	}
	seen := make(map[string]bool, len(fixture.Cases))
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid retained reconciliation progress case %q", row.Name)
		}
		if row.SessionID == "" {
			t.Fatalf("retained reconciliation progress case %q has no session id", row.Name)
		}
		seen[row.Name] = true
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("missing retained reconciliation progress case %q", name)
		}
	}
	if fixture.Transcript == "" {
		t.Fatal("retained reconciliation progress fixture has no transcript body")
	}
	return fixture
}

// reconcileProgressObservations records what the reconciliation stage reported
// while the walk was inside a session, one observation per retained session.
//
// The observation is taken when the walk first opens a session's committed
// metadata, which is the work the counter describes, so the recorded Done is
// the number of sessions the walk had already finished at that moment.
type reconcileProgressObservations struct {
	mu       sync.Mutex
	progress *ingest.ProgressState
	seen     map[string]bool
	observed []ingest.StageProgress
}

func newReconcileProgressObservations(progress *ingest.ProgressState) *reconcileProgressObservations {
	return &reconcileProgressObservations{progress: progress, seen: map[string]bool{}}
}

func (o *reconcileProgressObservations) observe(path string) {
	stage := o.progress.Snapshot()[ingest.StageReconcile]
	// Only the walk reports this stage. Metadata the harvest opens before the
	// stage starts, or after it ends, belongs to another phase and is not
	// progress of this one.
	if !stage.Started || stage.Ended {
		return
	}
	name := filepath.Base(path)
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seen[name] {
		return
	}
	o.seen[name] = true
	o.observed = append(o.observed, stage)
}

func (o *reconcileProgressObservations) snapshot() []ingest.StageProgress {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]ingest.StageProgress(nil), o.observed...)
}

// reconcileProgressFS watches the production reconciliation walk through the
// real durable filesystem, so the observations come from the path a harvest runs.
type reconcileProgressFS struct {
	*ingest.OSFileSystem
	observations *reconcileProgressObservations
}

var _ ingest.DurableFileSystem = (*reconcileProgressFS)(nil)

func (f *reconcileProgressFS) OpenArtifactRoot(path string) (ingest.ArtifactRoot, error) {
	root, err := f.OSFileSystem.OpenArtifactRoot(path)
	if err != nil {
		return nil, err
	}
	return &reconcileProgressRoot{ArtifactRoot: root, observations: f.observations}, nil
}

func (f *reconcileProgressFS) CreateArtifactRoot(path string) (ingest.ArtifactRoot, error) {
	root, err := f.OSFileSystem.CreateArtifactRoot(path)
	if err != nil {
		return nil, err
	}
	return &reconcileProgressRoot{ArtifactRoot: root, observations: f.observations}, nil
}

type reconcileProgressRoot struct {
	ingest.ArtifactRoot
	observations *reconcileProgressObservations
}

func (r *reconcileProgressRoot) ReadFile(path string) ([]byte, error) {
	if strings.HasSuffix(path, defaults.MetadataSuffix) {
		r.observations.observe(path)
	}
	return r.ArtifactRoot.ReadFile(path)
}

// TestReconcileWalkReportsProgressForEveryRetainedSession pins the progress the
// reconciliation walk owes the user. The walk runs before discovery and is
// proportional to the whole retained tree, so a tree of any size used to be a
// blank screen for as long as it took. The stage must start with the retained
// inventory as its total, advance once per session, and end with Done equal to
// that total.
func TestReconcileWalkReportsProgressForEveryRetainedSession(t *testing.T) {
	fixture := loadArtifactReconcileProgressFixtures(t)
	root := t.TempDir()
	output := filepath.Join(root, "managed")
	database, err := store.Open(filepath.Join(root, "peasant.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	sessions := make([]ingest.DiscoveredSession, 0, len(fixture.Cases))
	metadata := make(map[ingest.SessionID]*ingest.UnifiedMetadata, len(fixture.Cases))
	for index, row := range fixture.Cases {
		source := filepath.Join(root, row.SessionID+".jsonl")
		if err := os.WriteFile(source, []byte(fixture.Transcript), 0600); err != nil {
			t.Fatal(err)
		}
		session := makeDiscoveredSession(t, row.SessionID, source, time.Now().Add(-time.Duration(index+1)*time.Hour))
		sessions = append(sessions, session)
		metadata[session.SessionID] = makeReindexMeta(t, row.SessionID, source)
	}

	config := makePipelineConfig(output)
	seed, err := ingest.NewPipeline(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(sessions, metadata),
	}, config, ingest.WithStore(database), ingest.WithMetricsStore(database),
		ingest.WithIndexers(ingest.NewIndexerRegistry(&ingest.OSFileSystem{}, ingest.IndexerRegistryOptions{})))
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := seed.Run(t.Context())
	if err != nil || seeded.Summary.New != len(sessions) || seeded.Summary.Errors != 0 {
		t.Fatalf("seed the retained corpus: %+v %v", seeded, err)
	}

	progress := ingest.NewProgressState()
	observations := newReconcileProgressObservations(progress)
	filesystem := &reconcileProgressFS{OSFileSystem: &ingest.OSFileSystem{}, observations: observations}
	config.Progress = progress
	config.Sources = nil
	config.SessionFilter = func(ingest.DiscoveredSession) bool { return false }
	harvest, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(), map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter {
			return &metadataPolicyAdapter{StubAdapter: &testutil.StubAdapter{ProviderValue: ingest.HarnessClaudeCode}}
		},
	}, config, ingest.WithStore(database), ingest.WithMetricsStore(database),
		ingest.WithIndexers(ingest.NewIndexerRegistry(filesystem, ingest.IndexerRegistryOptions{})))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harvest.Run(t.Context()); err != nil {
		t.Fatal(err)
	}

	total := len(fixture.Cases)
	final := progress.Snapshot()[ingest.StageReconcile]
	if !final.Started {
		t.Fatalf("the reconciliation walk reported no progress stage at all: %+v", final)
	}
	if !final.Ended {
		t.Errorf("the reconciliation stage never ended: %+v", final)
	}
	if final.HasErr {
		t.Errorf("the reconciliation stage ended with an error over a settled corpus: %+v", final)
	}
	if final.Total != total {
		t.Errorf("the reconciliation stage reported total %d, want the retained inventory %d", final.Total, total)
	}
	if final.Done != total {
		t.Errorf("the reconciliation stage ended at %d of %d, want done to reach the total", final.Done, final.Total)
	}

	observed := observations.snapshot()
	if len(observed) != total {
		t.Fatalf("the walk was observed inside %d retained sessions, want %d", len(observed), total)
	}
	for index, stage := range observed {
		if stage.Total != total {
			t.Errorf("while reconciling retained session %d the stage total was %d, want %d", index+1, stage.Total, total)
		}
		if stage.Done != index {
			t.Errorf("while reconciling retained session %d the stage reported %d done, want %d (the counter advances once per session)", index+1, stage.Done, index)
		}
	}

	if !reflect.DeepEqual(ingest.StageOrder[:2], []ingest.Stage{ingest.StageReconcile, ingest.StageDiscover}) {
		t.Errorf("stage display order starts %v, want the reconciliation walk before discovery", ingest.StageOrder[:2])
	}
	if len(ingest.IndexProfileStageOrder) == 0 || ingest.IndexProfileStageOrder[0] != ingest.StageReconcile {
		t.Errorf("profile stage order starts %v, want the reconciliation walk first", ingest.IndexProfileStageOrder)
	}
}
