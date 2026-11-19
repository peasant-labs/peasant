package upgrade

import (
	"bytes"
	_ "embed"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/producer_version_ledger.yaml
var producerVersionLedgerYAML []byte

//go:embed testdata/producer_version_ledger.manifest.yaml
var producerVersionLedgerManifestYAML []byte

//go:embed testdata/producer_version_ledger_invalid.yaml
var invalidProducerVersionLedgerYAML []byte

//go:embed testdata/producer_ledger_agreement_invalid.yaml
var invalidProducerLedgerAgreementYAML []byte

// ledgerRow is one producer bump. The declaration fields restate what the
// bump changed; reason and lane restate what it invalidated for a stored
// session.
type ledgerRow struct {
	ID          string               `yaml:"id"`
	Harness     ingest.Harness       `yaml:"harness,omitempty"`
	Name        string               `yaml:"name,omitempty"`
	Kind        ProducerKind         `yaml:"kind"`
	From        ProducerVersion      `yaml:"from,omitempty"`
	To          ProducerVersion      `yaml:"to"`
	ChangeScope ChangeScope          `yaml:"change_scope"`
	Input       ProducerInput        `yaml:"input"`
	OutputProof []ProofName          `yaml:"output_proof"`
	Precheck    PrecheckName         `yaml:"precheck"`
	Fallback    StepAction           `yaml:"fallback"`
	Reason      ingest.RefreshReason `yaml:"reason"`
	Lane        ingest.RefreshLane   `yaml:"lane"`
}

// producerKey names the producer a row belongs to, spelled the way a
// declaration names itself.
func (row ledgerRow) producerKey() string {
	name := row.Name
	if row.Kind.PerHarness() {
		name = string(row.Harness)
	}
	return ProducerDeclaration{Kind: row.Kind, Name: name}.String()
}

func (row ledgerRow) validate() error {
	if row.ID == "" {
		return fmt.Errorf("a ledger row has no id; give every bump a unique id")
	}
	if _, err := NewProducerKind(string(row.Kind)); err != nil {
		return fmt.Errorf("row %s kind: %w", row.ID, err)
	}
	switch {
	case row.Kind.PerHarness():
		if row.Harness == "" || row.Name != "" {
			return fmt.Errorf("row %s is a per-harness %s and names no harness, or also names %q; name the harness only", row.ID, row.Kind, row.Name)
		}
		if !row.Harness.IsKnown() {
			return fmt.Errorf("row %s names unknown harness %q; use a registered harness", row.ID, row.Harness)
		}
	case row.Kind.PerContentCapability():
		if row.Harness != "" || row.Name == "" || row.From != "" || ProducerVersion(row.Name) != row.To {
			return fmt.Errorf("row %s is a content model and names no capability, or names a harness, a prior version, or a version other than its token; name the capability and set to to the same token", row.ID)
		}
	default:
		if row.Harness != "" || row.Name != "" {
			return fmt.Errorf("row %s is the single %s producer and names harness %q or name %q; name neither", row.ID, row.Kind, row.Harness, row.Name)
		}
	}
	if _, err := NewChangeScope(string(row.ChangeScope)); err != nil {
		return fmt.Errorf("row %s: %w", row.ID, err)
	}
	if _, err := NewProducerInput(string(row.Input)); err != nil {
		return fmt.Errorf("row %s: %w", row.ID, err)
	}
	for _, proof := range row.OutputProof {
		if _, err := NewProofName(string(proof)); err != nil {
			return fmt.Errorf("row %s: %w", row.ID, err)
		}
	}
	if _, err := NewPrecheckName(string(row.Precheck)); err != nil {
		return fmt.Errorf("row %s: %w", row.ID, err)
	}
	if _, err := NewStepAction(string(row.Fallback)); err != nil {
		return fmt.Errorf("row %s fallback: %w", row.ID, err)
	}
	if _, err := ingest.NewRefreshReason(string(row.Reason)); err != nil {
		return fmt.Errorf("row %s reason: %w", row.ID, err)
	}
	if _, err := ingest.NewRefreshLane(string(row.Lane)); err != nil {
		return fmt.Errorf("row %s lane: %w", row.ID, err)
	}
	return nil
}

func decodeProducerVersionLedger(data []byte) ([]ledgerRow, error) {
	var rows []ledgerRow
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&rows); err != nil {
		return nil, fmt.Errorf("decode producer_version_ledger.yaml: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("producer_version_ledger.yaml declares no rows; add a version history")
	}
	for _, row := range rows {
		if err := row.validate(); err != nil {
			return nil, fmt.Errorf("producer_version_ledger.yaml %w", err)
		}
	}
	return rows, nil
}

func loadProducerVersionLedger(t *testing.T) []ledgerRow {
	t.Helper()
	rows, err := decodeProducerVersionLedger(producerVersionLedgerYAML)
	if err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool, len(rows))
	for _, row := range rows {
		if present[row.ID] {
			t.Fatalf("producer_version_ledger.yaml repeats id %q; give every bump a unique id", row.ID)
		}
		present[row.ID] = true
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := testutil.DecodeFixtureYAML(producerVersionLedgerManifestYAML, &manifest); err != nil {
		t.Fatalf("decode producer_version_ledger manifest: %v", err)
	}
	if err := testutil.RequireFixtureNames("producer_version_ledger.yaml", "rows", manifest.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	return rows
}

// versionAdvances reports whether to is a newer version than from: a larger
// revision number, a higher semantic version, or a capability token with no
// prior version.
func versionAdvances(from, to ProducerVersion) bool {
	if from == "" {
		return to != ""
	}
	fromRevision, fromErr := strconv.Atoi(string(from))
	toRevision, toErr := strconv.Atoi(string(to))
	if fromErr == nil && toErr == nil {
		return toRevision > fromRevision
	}
	fromSemver, toSemver := "v"+string(from), "v"+string(to)
	if semver.IsValid(fromSemver) && semver.IsValid(toSemver) {
		return semver.Compare(toSemver, fromSemver) > 0
	}
	return false
}

// checkLedgerAgreement is the table-versus-ledger gate. Every declared
// producer has a contiguous chain of real bumps; every version its registries
// hand this build is on that chain; the chain ends at the newest one; the
// newest row restates the declaration; and every chain belongs to a declared
// producer.
func checkLedgerAgreement(registries producerRegistries, rows []ledgerRow) error {
	declarations, err := declareProducers(registries)
	if err != nil {
		return err
	}
	chains := make(map[string][]ledgerRow)
	for _, row := range rows {
		if !versionAdvances(row.From, row.To) {
			return fmt.Errorf("ledger row %s is not a bump: %q -> %q", row.ID, row.From, row.To)
		}
		key := row.producerKey()
		if chain := chains[key]; len(chain) > 0 && chain[len(chain)-1].To != row.From {
			return fmt.Errorf("%s ledger is not a contiguous chain at %s: %q does not follow %q", key, row.ID, row.From, chain[len(chain)-1].To)
		}
		chains[key] = append(chains[key], row)
	}
	declared := make(map[string]bool, len(declarations))
	for _, declaration := range declarations {
		key := declaration.String()
		if declared[key] {
			return fmt.Errorf("%s is declared twice; a producer has exactly one declaration", key)
		}
		declared[key] = true
		chain := chains[key]
		if len(chain) == 0 {
			return fmt.Errorf("%s is declared but the ledger has no rows for it; add its version history to producer_version_ledger.yaml", key)
		}
		newest := chain[len(chain)-1]
		if newest.To != declaration.Current() {
			return fmt.Errorf("%s chain ends at %s but the registry declares %s; add a ledger row for the bump", key, newest.To, declaration.Current())
		}
		onChain := map[ProducerVersion]bool{}
		for _, row := range chain {
			onChain[row.From], onChain[row.To] = true, true
		}
		for _, version := range declaration.Versions {
			if !onChain[version] {
				return fmt.Errorf("%s registry version %s is absent from the ledger chain %v; add the missing bump", key, version, chain)
			}
		}
		if newest.ChangeScope != declaration.ChangeScope || newest.Input != declaration.Input ||
			!slices.Equal(newest.OutputProof, declaration.OutputProof) || newest.Precheck != declaration.Precheck ||
			newest.Fallback != declaration.Fallback {
			return fmt.Errorf("ledger row %s disagrees with the declaration of %s: row states %s/%s/%v/%s/%s, declaration states %s/%s/%v/%s/%s (scope/input/proof/precheck/fallback); update the row or the declaration so they state the same producer",
				newest.ID, key, newest.ChangeScope, newest.Input, newest.OutputProof, newest.Precheck, newest.Fallback,
				declaration.ChangeScope, declaration.Input, declaration.OutputProof, declaration.Precheck, declaration.Fallback)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(chains)) {
		if !declared[key] {
			return fmt.Errorf("ledger has rows for %s but no registry declares it; register the producer or remove its rows", key)
		}
	}
	return nil
}

// TestLedgerAgreesWithTheDeclarationTable proves the runtime declaration table
// and the live registries it reads agree with the recorded version history.
func TestLedgerAgreesWithTheDeclarationTable(t *testing.T) {
	if err := checkLedgerAgreement(currentProducerRegistries(), loadProducerVersionLedger(t)); err != nil {
		t.Fatal(err)
	}
}

// TestEveryLedgerBumpNamesAReasonAndALane states the ledger's second half:
// every row names a defined decision reason and the lane that reason takes,
// and the pair matches what the bump's producer reads.
func TestEveryLedgerBumpNamesAReasonAndALane(t *testing.T) {
	for _, row := range loadProducerVersionLedger(t) {
		reason, lane := row.Reason, row.Lane
		if reason == ingest.RefreshReasonNone {
			if lane != ingest.RefreshLaneNone {
				t.Fatalf("ledger row %s schedules no session work but names lane %s", row.ID, lane)
			}
		} else if !reason.Refreshing() || reason.OutcomeReason().RefreshLane() != lane {
			t.Fatalf("ledger row %s names %s on %s, which is not a refresh reason and its lane", row.ID, reason, lane)
		}
		switch row.Kind {
		case ProducerKindAdapter:
			switch row.Input {
			case ProducerInputStoredTranscript:
				if reason != ingest.RefreshReasonAdapterBehind {
					t.Fatalf("ledger row %s is an adapter bump that reads the stored transcript and must name adapter_behind, not %s", row.ID, reason)
				}
			case ProducerInputNativeSource:
				if reason != ingest.RefreshReasonAdapterBehindNativeOnly {
					t.Fatalf("ledger row %s is an adapter bump that reads the native source and must name adapter_behind_native_only, not %s", row.ID, reason)
				}
			default:
				t.Fatalf("ledger row %s is an adapter bump reading %s; an adapter reads a transcript", row.ID, row.Input)
			}
		case ProducerKindIndexer, ProducerKindFormat:
			if reason != ingest.RefreshReasonIndexerBehind {
				t.Fatalf("ledger row %s is a %s bump and must name indexer_behind, not %s", row.ID, row.Kind, reason)
			}
		case ProducerKindMetadataSchema:
			from, err := strconv.Atoi(string(row.From))
			if err != nil {
				t.Fatalf("ledger row %s metadata schema version %q is not a number", row.ID, row.From)
			}
			want := ingest.RefreshReasonMetadataSchema
			if ingest.MetadataSchemaVersionIsCurrent(from) {
				want = ingest.RefreshReasonNone
			}
			if reason != want {
				t.Fatalf("ledger row %s bumps the metadata schema from %d and must name %s, not %s", row.ID, from, want, reason)
			}
		case ProducerKindContentModel, ProducerKindRedactionRules, ProducerKindSchema:
			if reason != ingest.RefreshReasonNone {
				t.Fatalf("ledger row %s is a %s bump, which schedules no session work of its own, and must name none, not %s", row.ID, row.Kind, reason)
			}
		default:
			t.Fatalf("ledger row %s has kind %s, which this test does not cover", row.ID, row.Kind)
		}
	}
}

// TestLedgerRejectsUnknownAxes proves a typo in any field is rejected rather
// than silently recording a different producer.
func TestLedgerRejectsUnknownAxes(t *testing.T) {
	var invalid struct {
		RequiredNames []string `yaml:"required_names"`
		Cases         []struct {
			Name          string    `yaml:"name"`
			Patch         yaml.Node `yaml:"patch"`
			ErrorContains string    `yaml:"error_contains"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(invalidProducerVersionLedgerYAML, &invalid); err != nil {
		t.Fatalf("decode invalid ledger fixtures: %v", err)
	}
	baseline := loadProducerVersionLedger(t)[0]
	names := make(map[string]bool, len(invalid.Cases))
	for _, testCase := range invalid.Cases {
		if testCase.Name == "" || names[testCase.Name] {
			t.Fatalf("invalid ledger fixtures have an empty or repeated name %q", testCase.Name)
		}
		names[testCase.Name] = true
		t.Run(testCase.Name, func(t *testing.T) {
			input := baseline
			input.OutputProof = slices.Clone(baseline.OutputProof)
			if err := testCase.Patch.Decode(&input); err != nil {
				t.Fatalf("decode invalid row patch: %v", err)
			}
			data, err := yaml.Marshal([]ledgerRow{input})
			if err != nil {
				t.Fatalf("encode invalid row: %v", err)
			}
			_, err = decodeProducerVersionLedger(data)
			if err == nil || !strings.Contains(err.Error(), testCase.ErrorContains) {
				t.Fatalf("validation error = %v, want it to name %q", err, testCase.ErrorContains)
			}
		})
	}
	if err := testutil.RequireFixtureNames("producer_version_ledger_invalid.yaml", "cases", invalid.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
}

type harvesterVersionsPatch struct {
	Harness ingest.Harness `yaml:"harness"`
	Adapter int            `yaml:"adapter"`
	Indexer int            `yaml:"indexer"`
	Format  int            `yaml:"format"`
}

func (patch harvesterVersionsPatch) versions() ingest.HarvesterVersions {
	return ingest.HarvesterVersions{AdapterVersion: patch.Adapter, IndexerVersion: patch.Indexer, IndexVersion: patch.Format}
}

type registryMutation struct {
	SetBaseline           *harvesterVersionsPatch  `yaml:"set_baseline"`
	SetManaged            *harvesterVersionsPatch  `yaml:"set_managed"`
	DropHarness           ingest.Harness           `yaml:"drop_harness"`
	DropVersions          ingest.Harness           `yaml:"drop_versions"`
	DropAdapter           ingest.Harness           `yaml:"drop_adapter"`
	AddContentCapability  schema.ContentCapability `yaml:"add_content_capability"`
	DropContentCapability schema.ContentCapability `yaml:"drop_content_capability"`
	MetadataSchema        int                      `yaml:"metadata_schema"`
	RedactionRules        string                   `yaml:"redaction_rules"`
	StoreSchema           int                      `yaml:"store_schema"`
}

// apply returns changed copies of the registries; the live ones are never
// written.
func (mutation registryMutation) apply(registries producerRegistries) producerRegistries {
	changed := registries
	changed.baseline = maps.Clone(registries.baseline)
	changed.managed = maps.Clone(registries.managed)
	changed.adapters = maps.Clone(registries.adapters)
	changed.contentCapabilities = slices.Clone(registries.contentCapabilities)
	if patch := mutation.SetBaseline; patch != nil {
		changed.baseline[patch.Harness] = patch.versions()
	}
	if patch := mutation.SetManaged; patch != nil {
		changed.managed[patch.Harness] = patch.versions()
	}
	for _, harness := range []ingest.Harness{mutation.DropHarness, mutation.DropVersions} {
		if harness != "" {
			delete(changed.baseline, harness)
			delete(changed.managed, harness)
		}
	}
	for _, harness := range []ingest.Harness{mutation.DropHarness, mutation.DropAdapter} {
		if harness != "" {
			delete(changed.adapters, harness)
		}
	}
	if mutation.AddContentCapability != "" {
		changed.contentCapabilities = append(changed.contentCapabilities, mutation.AddContentCapability)
	}
	if mutation.DropContentCapability != "" {
		changed.contentCapabilities = slices.DeleteFunc(changed.contentCapabilities, func(capability schema.ContentCapability) bool {
			return capability == mutation.DropContentCapability
		})
	}
	if mutation.MetadataSchema != 0 {
		changed.metadataSchema = mutation.MetadataSchema
	}
	if mutation.RedactionRules != "" {
		changed.redactionRules = mutation.RedactionRules
	}
	if mutation.StoreSchema != 0 {
		changed.storeSchema = mutation.StoreSchema
	}
	return changed
}

type ledgerMutation struct {
	DropRows  []string `yaml:"drop_rows"`
	PatchRows []struct {
		ID    string    `yaml:"id"`
		Patch yaml.Node `yaml:"patch"`
	} `yaml:"patch_rows"`
}

func (mutation ledgerMutation) apply(t *testing.T, rows []ledgerRow) []ledgerRow {
	t.Helper()
	changed := slices.DeleteFunc(slices.Clone(rows), func(row ledgerRow) bool { return slices.Contains(mutation.DropRows, row.ID) })
	for _, patch := range mutation.PatchRows {
		index := slices.IndexFunc(changed, func(row ledgerRow) bool { return row.ID == patch.ID })
		if index < 0 {
			t.Fatalf("patch names ledger row %q, which the ledger does not have", patch.ID)
		}
		row := changed[index]
		row.OutputProof = slices.Clone(row.OutputProof)
		if err := patch.Patch.Decode(&row); err != nil {
			t.Fatalf("decode ledger row patch for %s: %v", patch.ID, err)
		}
		changed[index] = row
	}
	for _, id := range mutation.DropRows {
		if slices.ContainsFunc(rows, func(row ledgerRow) bool { return row.ID == id }) {
			continue
		}
		t.Fatalf("drop names ledger row %q, which the ledger does not have", id)
	}
	return changed
}

// TestLedgerAgreementFailsLoudly proves a producer missing from the ledger or
// from a registry, and a chain that disagrees with its declaration, each fail
// the agreement gate with a message naming the producer.
func TestLedgerAgreementFailsLoudly(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"required_names"`
		Cases         []struct {
			Name          string           `yaml:"name"`
			Registry      registryMutation `yaml:"registry"`
			Ledger        ledgerMutation   `yaml:"ledger"`
			ErrorContains string           `yaml:"error_contains"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(invalidProducerLedgerAgreementYAML, &fixture); err != nil {
		t.Fatalf("decode ledger agreement fixtures: %v", err)
	}
	rows := loadProducerVersionLedger(t)
	names := make(map[string]bool, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" || names[testCase.Name] {
			t.Fatalf("ledger agreement fixtures have an empty or repeated name %q", testCase.Name)
		}
		names[testCase.Name] = true
		t.Run(testCase.Name, func(t *testing.T) {
			registries := testCase.Registry.apply(currentProducerRegistries())
			err := checkLedgerAgreement(registries, testCase.Ledger.apply(t, rows))
			if err == nil || !strings.Contains(err.Error(), testCase.ErrorContains) {
				t.Fatalf("agreement error = %v, want it to name %q", err, testCase.ErrorContains)
			}
		})
	}
	if err := testutil.RequireFixtureNames("producer_ledger_agreement_invalid.yaml", "cases", fixture.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
}
