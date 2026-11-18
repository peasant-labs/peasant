package ingest_test

import (
	_ "embed"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/unchanged_settlement_refresh.yaml
var unchangedSettlementRefreshYAML []byte

//go:embed testdata/unchanged_settlement_refresh.manifest.yaml
var unchangedSettlementRefreshManifest []byte

type unchangedSettlementRefreshFixture struct {
	Cases []struct {
		Name     string `yaml:"name"`
		Baseline string `yaml:"baseline"`
	} `yaml:"cases"`
}

func TestUnchangedSettlementSecondRun(t *testing.T) {
	var fixture unchangedSettlementRefreshFixture
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(unchangedSettlementRefreshYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(unchangedSettlementRefreshManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	required := map[string]bool{}
	for _, name := range manifest.RequiredNames {
		required[name] = false
	}
	baseline := loadRefreshLaneFixture(t)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			if _, ok := required[tc.Name]; !ok || required[tc.Name] {
				t.Fatalf("unexpected or duplicate case %q", tc.Name)
			}
			required[tc.Name] = true
			var row *refreshLaneCase
			for i := range baseline.Cases {
				if baseline.Cases[i].Name == tc.Baseline {
					row = &baseline.Cases[i]
					break
				}
			}
			if row == nil {
				t.Fatalf("baseline %q not found", tc.Baseline)
			}
			root := filepath.Join(t.TempDir(), "artifacts")
			artifacts, err := store.NewOSGenerationArtifactStore(root)
			if err != nil {
				t.Fatal(err)
			}
			locker, err := store.NewFileSessionLocker(root)
			if err != nil {
				t.Fatal(err)
			}
			b := newRefreshLaneBenchWithVersions(t, baseline, *row, ingest.GenerationTargets(cloneHarvesterVersions()), store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locker))
			target := b.versions[b.harness]
			target.AdapterVersion++
			b.versions[b.harness] = target
			b.mutateMetadataWithoutBinding(func(m *ingest.UnifiedMetadata) { m.AdapterVersion = &target.AdapterVersion })
			data, err := b.fs.ReadFile(b.metadataPath)
			if err != nil {
				t.Fatal(err)
			}
			var metadata ingest.UnifiedMetadata
			if err = json.Unmarshal(data, &metadata); err != nil {
				t.Fatal(err)
			}
			state, err := b.database.ReadIndexState(t.Context(), b.sessionID)
			if err != nil || state == nil || state.IndexedInputHash == nil {
				t.Fatalf("read index state: %+v, %v", state, err)
			}
			if state.PublicationCaptureRevision <= 0 || state.PublicationBound {
				t.Fatalf("expected refreshed, unbound publication capture: %+v", state)
			}
			beforeDecision := ingest.StoredDecisionForTest(b.pipeline(false, false), ingest.DiscoveredSession{SessionID: b.sessionID, Harness: b.harness}, b.location())
			if beforeDecision.Reason != ingest.RefreshReasonPublicationUnbound {
				t.Fatalf("pre-settlement decision = %+v, want publication unbound", beforeDecision)
			}
			conn, err := b.database.Pool().Take(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var generationID string
			err = sqlitex.Execute(conn, `SELECT active_generation_id FROM sessions WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{string(b.sessionID)}, ResultFunc: func(st *sqlite.Stmt) error { generationID = st.ColumnText(0); return nil }})
			b.database.Pool().Put(conn)
			if err != nil || generationID == "" {
				t.Fatalf("read generation: %q, %v", generationID, err)
			}
			request := store.ManagedGenerationUnchangedProof{SessionID: b.sessionID, GenerationID: generationID, Metadata: metadata, InputDigest: *state.IndexedInputHash, IndexerTarget: b.versions[b.harness].IndexerVersion, FormatTarget: 2, Expected: state, UpdatedAtMs: time.Now().UnixMilli()}
			proof, err := b.database.ProveManagedGenerationUnchanged(t.Context(), request)
			if err != nil || !proof.Proven {
				t.Fatalf("prove: %+v, %v", proof, err)
			}
			settled, err := b.database.SettleManagedGenerationUnchanged(t.Context(), request)
			if err != nil || settled.Disposition != store.ManagedGenerationRefreshUnchanged {
				t.Fatalf("settle: %+v, %v", settled, err)
			}
			state, err = b.database.ReadIndexState(t.Context(), b.sessionID)
			if err != nil || state == nil || !state.PublicationBound {
				t.Fatalf("settlement did not bind publication: %+v, %v", state, err)
			}
			result := b.run(false, false)
			if result.Summary.StoreError != nil || result.Summary.Errors != 0 || result.Summary.Updated != 0 || result.Summary.Indexed != 0 || result.Summary.New != 0 {
				t.Fatalf("second stored-only run did work: %+v", result.Summary)
			}
			decision := ingest.StoredDecisionForTest(b.pipeline(false, false), ingest.DiscoveredSession{SessionID: b.sessionID, Harness: b.harness}, b.location())
			if decision.Reason != ingest.RefreshReasonNone || decision.Lane != ingest.RefreshLaneNone {
				t.Fatalf("second run decision = %+v", decision)
			}
		})
	}
	for name, seen := range required {
		if !seen {
			t.Errorf("missing case %q", name)
		}
	}
}
