package ingest_test

import (
	_ "embed"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/harvester_version_ledger.yaml
var harvesterVersionLedgerYAML []byte

// harvesterVersionKind is the closed set of producer kinds the ledger records.
type harvesterVersionKind string

const (
	harvesterVersionAdapter harvesterVersionKind = "adapter"
	harvesterVersionIndexer harvesterVersionKind = "indexer"
	harvesterVersionFormat  harvesterVersionKind = "format"
)

var allHarvesterVersionKinds = []harvesterVersionKind{
	harvesterVersionAdapter, harvesterVersionIndexer, harvesterVersionFormat,
}

func (kind harvesterVersionKind) Validate() error {
	if slices.Contains(allHarvesterVersionKinds, kind) {
		return nil
	}
	return fmt.Errorf("unknown producer kind %q; use one of %q in harvester_version_ledger.yaml", kind, allHarvesterVersionKinds)
}

type harvesterVersionLedgerRow struct {
	ID          string               `yaml:"id"`
	Harness     ingest.Harness       `yaml:"harness"`
	Kind        harvesterVersionKind `yaml:"kind"`
	From        int                  `yaml:"from"`
	To          int                  `yaml:"to"`
	Reason      ingest.RefreshReason `yaml:"reason"`
	Lane        ingest.RefreshLane   `yaml:"lane"`
	ChangeScope string               `yaml:"change_scope"`
	OutputProof []string             `yaml:"output_proof"`
	Precheck    string               `yaml:"precheck"`
	Fallback    string               `yaml:"fallback"`
}

type harvesterVersionLedger struct {
	Rows []harvesterVersionLedgerRow `yaml:"-"`
}

func decodeHarvesterVersionLedger(data []byte) (harvesterVersionLedger, error) {
	var rows []harvesterVersionLedgerRow
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&rows); err != nil {
		return harvesterVersionLedger{}, fmt.Errorf("decode harvester_version_ledger.yaml: %w", err)
	}
	if len(rows) == 0 {
		return harvesterVersionLedger{}, fmt.Errorf("harvester_version_ledger.yaml declares no rows; add a version history")
	}
	for _, row := range rows {
		if !row.Harness.IsKnown() {
			return harvesterVersionLedger{}, fmt.Errorf("harvester_version_ledger.yaml row %s names unknown harness %q; use a registered harness", row.ID, row.Harness)
		}
		if err := row.Kind.Validate(); err != nil {
			return harvesterVersionLedger{}, fmt.Errorf("harvester_version_ledger.yaml row %s: %w", row.ID, err)
		}
		if _, err := ingest.NewRefreshReason(string(row.Reason)); err != nil {
			return harvesterVersionLedger{}, fmt.Errorf("harvester_version_ledger.yaml row %s reason: %w", row.ID, err)
		}
		if _, err := ingest.NewRefreshLane(string(row.Lane)); err != nil {
			return harvesterVersionLedger{}, fmt.Errorf("harvester_version_ledger.yaml row %s lane: %w", row.ID, err)
		}
	}
	return harvesterVersionLedger{Rows: rows}, nil
}

func loadHarvesterVersionLedger(t *testing.T) harvesterVersionLedger {
	t.Helper()
	ledger, err := decodeHarvesterVersionLedger(harvesterVersionLedgerYAML)
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

// ledgerKey names one producer chain. A baseline registry value and the
// managed-generation override are both checkpoints on the same chain.
type ledgerKey struct {
	harness ingest.Harness
	kind    harvesterVersionKind
}

func (key ledgerKey) String() string {
	return fmt.Sprintf("%s/%s", key.harness, key.kind)
}

func sortLedgerRows(rows []harvesterVersionLedgerRow) {
	slices.SortStableFunc(rows, func(a, b harvesterVersionLedgerRow) int {
		if a.From != b.From {
			return a.From - b.From
		}
		return a.To - b.To
	})
}

// registryValues returns every value a chain must carry: the baseline value and
// the managed-generation value when the harness declares one. A harness/kind
// absent from both registries has no value.
func registryValues() map[ledgerKey]map[int]bool {
	values := make(map[ledgerKey]map[int]bool)
	add := func(harness ingest.Harness, versions ingest.HarvesterVersions) {
		for kind, value := range map[harvesterVersionKind]int{
			harvesterVersionAdapter: versions.AdapterVersion,
			harvesterVersionIndexer: versions.IndexerVersion,
			harvesterVersionFormat:  versions.IndexVersion,
		} {
			key := ledgerKey{harness: harness, kind: kind}
			if values[key] == nil {
				values[key] = make(map[int]bool)
			}
			values[key][value] = true
		}
	}
	for harness, versions := range ingest.HarvesterVersionRegistry {
		add(harness, versions)
	}
	for harness, versions := range ingest.GenerationRepairTargets {
		add(harness, versions)
	}
	return values
}

// TestHarvesterVersionLedgerMatchesTheRegistry is the registry-vs-ledger gate:
// every value either registry can hand a run appears on the matching chain,
// each chain is contiguous, and its last row is the highest value any registry
// declares for that harness and kind.
func TestHarvesterVersionLedgerMatchesTheRegistry(t *testing.T) {
	ledger := loadHarvesterVersionLedger(t)
	byKey := make(map[ledgerKey][]harvesterVersionLedgerRow)
	seenIDs := make(map[string]bool, len(ledger.Rows))
	for _, row := range ledger.Rows {
		if row.ID == "" || seenIDs[row.ID] {
			t.Fatalf("ledger has an empty or repeated id %q", row.ID)
		}
		seenIDs[row.ID] = true
		key := ledgerKey{harness: row.Harness, kind: row.Kind}
		byKey[key] = append(byKey[key], row)
	}
	values := registryValues()
	for key, wantValues := range values {
		rows := byKey[key]
		if len(rows) == 0 {
			t.Fatalf("registry declares %s but the ledger has no rows for it", key)
		}
		sortLedgerRows(rows)
		for index := 1; index < len(rows); index++ {
			if rows[index].From != rows[index-1].To {
				t.Fatalf("%s ledger is not a contiguous chain at %s: %d does not follow %d", key, rows[index].ID, rows[index].From, rows[index-1].To)
			}
		}
		present := make(map[int]bool, len(rows))
		highest := 0
		for _, row := range rows {
			present[row.From] = true
			present[row.To] = true
			if row.To > highest {
				highest = row.To
			}
		}
		for value := range wantValues {
			if !present[value] {
				t.Fatalf("%s registry value %d is absent from the ledger chain (rows %+v)", key, value, rows)
			}
		}
		if highest != rows[len(rows)-1].To {
			t.Fatalf("%s highest value %d is not the last row %d", key, highest, rows[len(rows)-1].To)
		}
	}
	for key := range byKey {
		if _, known := values[key]; !known {
			t.Fatalf("ledger has rows for %s but no registry declares it", key)
		}
	}
}

// TestEveryVersionBumpNamesAReasonAndALane states the ledger's second half:
// every row is a real bump, names a defined decision reason and lane, and the
// lane matches what the bump's producer reads.
func TestEveryVersionBumpNamesAReasonAndALane(t *testing.T) {
	ledger := loadHarvesterVersionLedger(t)
	for _, row := range ledger.Rows {
		if row.To <= row.From {
			t.Fatalf("ledger row %s is not a bump: %d -> %d", row.ID, row.From, row.To)
		}
		reason, lane := row.Reason, row.Lane
		if !reason.Refreshing() {
			t.Fatalf("ledger row %s names %q, which writes no refresh outcome row", row.ID, reason)
		}
		switch row.Kind {
		case harvesterVersionAdapter:
			if row.ChangeScope != "metadata-only" || !slices.Equal(row.OutputProof, []string{"content-hash", "metadata-context-digest"}) || row.Precheck != "adapter-context-equivalence" || row.Fallback != "parse" {
				t.Fatalf("ledger row %s does not declare the adapter proof and fallback: %+v", row.ID, row)
			}
			switch lane {
			case ingest.RefreshLaneStoredMetadata:
				if reason != ingest.RefreshReasonAdapterBehind {
					t.Fatalf("ledger row %s is an adapter bump on %s and must name adapter_behind, not %s", row.ID, lane, reason)
				}
			case ingest.RefreshLaneNative:
				if reason != ingest.RefreshReasonAdapterBehindNativeOnly {
					t.Fatalf("ledger row %s is an adapter bump on %s and must name adapter_behind_native_only, not %s", row.ID, lane, reason)
				}
			default:
				t.Fatalf("ledger row %s is an adapter bump on lane %s; an adapter produces the pair and cannot use that lane", row.ID, lane)
			}
		case harvesterVersionIndexer, harvesterVersionFormat:
			if reason != ingest.RefreshReasonIndexerBehind || lane != ingest.RefreshLaneStoredIndex {
				t.Fatalf("ledger row %s is a %s bump and must name indexer_behind on stored-index, not %s/%s", row.ID, row.Kind, reason, lane)
			}
		}
	}
}
