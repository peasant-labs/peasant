package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/claude_validation_oracle.yaml
var claudeValidationOracleYAML []byte

//go:embed testdata/claude_validation_oracle.manifest.yaml
var claudeValidationOracleManifestYAML []byte

//go:embed testdata/metadata_field_inventory.yaml
var metadataFieldInventoryYAML []byte

type oracleCorpusFile struct {
	Path       string `yaml:"path"`
	Transcript string `yaml:"transcript"`
}

type oracleExpectation struct {
	Validator string `yaml:"validator"`
	Parse     string `yaml:"parse"`
}

type oracleCase struct {
	Name                   string                       `yaml:"name"`
	Change                 string                       `yaml:"change"`
	Sessions               []string                     `yaml:"sessions"`
	SeedWithout            []string                     `yaml:"seedWithout"`
	CreateBeforeRefresh    []string                     `yaml:"createBeforeRefresh"`
	RemoveBeforeValidation []string                     `yaml:"removeBeforeValidation"`
	Expect                 map[string]oracleExpectation `yaml:"expect"`
	// InstallFirst parses the change in before the oracle's refresh, so the
	// oracle compares against a generation that already carries it.
	InstallFirst bool `yaml:"installFirst"`

	// inventory is set for a generated inventory case.
	inventory *metadataInventoryRow
}

type oracleWindow struct {
	Name                   string   `yaml:"name"`
	RemoveBeforeValidation []string `yaml:"removeBeforeValidation"`
	RestoreAfterValidation []string `yaml:"restoreAfterValidation"`
	RemoveAfterValidation  []string `yaml:"removeAfterValidation"`
	Session                string   `yaml:"session"`
	Validator              string   `yaml:"validator"`
	Settled                bool     `yaml:"settled"`
	Parsed                 bool     `yaml:"parsed"`
}

type oracleDocument struct {
	Corpus  []oracleCorpusFile `yaml:"corpus"`
	Cases   []oracleCase       `yaml:"cases"`
	Windows []oracleWindow     `yaml:"windows"`
}

type metadataInventoryRow struct {
	Field    string `yaml:"field"`
	Mutation string `yaml:"mutation"`
}

func loadOracleDocument(t *testing.T) oracleDocument {
	t.Helper()
	var document oracleDocument
	if err := testutil.DecodeFixtureYAML(claudeValidationOracleYAML, &document); err != nil {
		t.Fatalf("decode the validator oracle fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(claudeValidationOracleManifestYAML, "claude validation oracle")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range document.Cases {
		names = append(names, c.Name)
	}
	for _, w := range document.Windows {
		names = append(names, w.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "claude validation oracle"); err != nil {
		t.Fatal(err)
	}
	if len(document.Corpus) == 0 {
		t.Fatal("the validator oracle corpus is empty")
	}
	return document
}

// metadataLeafPaths reflects schema.UnifiedMetadata into its leaves, named by
// their dotted JSON paths: a struct (or pointer to a struct) is descended, and
// everything else is a leaf.
func metadataLeafPaths(t reflect.Type, prefix string, out *[]string) {
	for i := range t.NumField() {
		field := t.Field(i)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "-" || !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		kind := field.Type
		if kind.Kind() == reflect.Pointer && kind.Elem().Kind() == reflect.Struct {
			kind = kind.Elem()
		}
		if kind.Kind() == reflect.Struct {
			metadataLeafPaths(kind, prefix+name+".", out)
			continue
		}
		*out = append(*out, prefix+name)
	}
}

func loadMetadataInventory(t *testing.T) []metadataInventoryRow {
	t.Helper()
	var fixture struct {
		Fields []metadataInventoryRow `yaml:"fields"`
	}
	if err := testutil.DecodeFixtureYAML(metadataFieldInventoryYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	var leaves []string
	metadataLeafPaths(reflect.TypeOf(schema.UnifiedMetadata{}), "", &leaves)
	var listed []string
	for _, row := range fixture.Fields {
		listed = append(listed, row.Field)
		if _, err := metadataLeaf(&schema.UnifiedMetadata{}, row.Field); err != nil {
			t.Fatalf("inventory row %s: %v", row.Field, err)
		}
	}
	sort.Strings(leaves)
	sorted := slices.Clone(listed)
	sort.Strings(sorted)
	if len(leaves) == 0 || !slices.Equal(leaves, sorted) {
		t.Fatalf("metadata inventory fields differ from the leaves of schema.UnifiedMetadata:\nleaves %v\nlisted %v", leaves, sorted)
	}
	return fixture.Fields
}

// metadataLeaf resolves a dotted JSON path to the settable value of one leaf,
// allocating a nil pointer-to-struct on the way.
func metadataLeaf(metadata *schema.UnifiedMetadata, path string) (reflect.Value, error) {
	value := reflect.ValueOf(metadata).Elem()
	for _, part := range strings.Split(path, ".") {
		if value.Kind() == reflect.Pointer {
			if value.IsNil() {
				value.Set(reflect.New(value.Type().Elem()))
			}
			value = value.Elem()
		}
		found := false
		for i := range value.NumField() {
			field := value.Type().Field(i)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "" {
				name = field.Name
			}
			if name == part {
				value = value.Field(i)
				found = true
				break
			}
		}
		if !found {
			return reflect.Value{}, &json.UnsupportedValueError{Str: "no field " + part + " in " + path}
		}
	}
	return value, nil
}

// applyInventoryMutation sets one leaf to the row's mutation value and fails
// when the value equals the session's corpus value: the mutation would test
// nothing.
func applyInventoryMutation(t *testing.T, row metadataInventoryRow, sid string, metadata *schema.UnifiedMetadata) {
	t.Helper()
	leaf, err := metadataLeaf(metadata, row.Field)
	if err != nil {
		t.Fatal(err)
	}
	mutated := reflect.New(leaf.Type())
	if err := json.Unmarshal([]byte(row.Mutation), mutated.Interface()); err != nil {
		t.Fatalf("inventory row %s: decode the mutation %s: %v", row.Field, row.Mutation, err)
	}
	if reflect.DeepEqual(leaf.Interface(), mutated.Elem().Interface()) {
		t.Fatalf("inventory row %s: the mutation equals the corpus value of session %s", row.Field, sid)
	}
	leaf.Set(mutated.Elem())
}

// seamClaudeIndexer runs a seam before each managed build, the point in a
// parse that matches the point before the validator in a validated refresh.
type seamClaudeIndexer struct {
	*ingest.ClaudeIndexer
	before func()
}

func (i seamClaudeIndexer) BuildGeneration(ctx context.Context, input ingest.GenerationBuildInput) (ingest.GenerationCandidate, error) {
	if i.before != nil {
		i.before()
	}
	return i.ClaudeIndexer.BuildGeneration(ctx, input)
}

// oracleProveSeam runs a seam before each cheap proof, after the capture.
type oracleProveSeam struct {
	*halfValidatingStore
	before func()
}

func (s *oracleProveSeam) ProveCapturedGenerationUnchanged(ctx context.Context, request ingest.ManagedGenerationUnchangedProof) (ingest.UnchangedProof, error) {
	if s.before != nil {
		s.before()
	}
	return s.halfValidatingStore.ProveCapturedGenerationUnchanged(ctx, request)
}

// runOraclePipeline runs one Claude pipeline over source into output.
func runOraclePipeline(t *testing.T, db managedRefreshPipelineStore, source, output string, disableSkips bool, beforeBuild func()) *ingest.PipelineResult {
	t.Helper()
	fs := &ingest.OSFileSystem{}
	cfg := makePipelineConfig(output)
	if validator, ok := db.(ingest.ManagedGenerationValidator); ok {
		cfg.Validator = validator
	}
	cfg.Sources = map[ingest.Harness]ingest.SourceConfig{
		ingest.HarnessClaudeCode: {Paths: []ingest.ResolvedPath{ingest.ResolvedPath(source)}, Enabled: true},
	}
	cfg.StalenessThreshold = 0
	cfg.DisableRefreshSkips = disableSkips
	indexers := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})
	indexers[ingest.HarnessClaudeCode] = seamClaudeIndexer{ClaudeIndexer: ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullDepth(true)), before: beforeBuild}
	pipeline, err := newTestPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg,
		ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithSalt(db.InstallationSalt()),
		ingest.WithIndexers(indexers), ingest.WithLogger(db), ingest.WithIndexLogger(db),
	)
	if err != nil {
		t.Fatalf("build the oracle pipeline: %v", err)
	}
	result, err := pipeline.Run(t.Context())
	if err != nil {
		t.Fatalf("run the oracle pipeline: %v", err)
	}
	return result
}

// oracleSeed is one seeded Claude store, ready for the refresh.
type oracleSeed struct {
	root, source, output, empty string
	db                          *store.Store
	before                      map[string]string
}

func seedOracleStore(t *testing.T, document oracleDocument, c oracleCase) oracleSeed {
	t.Helper()
	root := t.TempDir()
	seed := oracleSeed{root: root, source: filepath.Join(root, "source"), output: filepath.Join(root, "managed"), empty: filepath.Join(root, "empty")}
	mustMkdir(t, seed.empty)
	for _, file := range document.Corpus {
		if slices.Contains(c.SeedWithout, file.Path) {
			continue
		}
		writeOracleFile(t, seed.source, document, file.Path)
	}
	seed.db = openManagedRefreshPipelineStore(t, filepath.Join(root, "db"), seed.output)
	t.Cleanup(func() { _ = seed.db.Close() })
	first := runOraclePipeline(t, seed.db, seed.source, seed.output, false, nil)
	if first.Summary.Indexed == 0 {
		t.Fatalf("the first import indexed nothing: %+v", first.Summary)
	}
	target := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[ingest.HarnessClaudeCode].AdapterVersion - 1
	managedRefreshPipelineDowngrade(t, seed.db, target)
	rewriteManagedMetadata(t, seed.output, func(sid string, metadata *schema.UnifiedMetadata) {
		metadata.AdapterVersion = &target
		switch {
		case c.inventory != nil:
			applyInventoryMutation(t, *c.inventory, sid, metadata)
		case c.Change == "record-context-from":
			source := schema.SessionID("22222222-2222-4222-8222-222222222222")
			metadata.Relationships = append(metadata.Relationships, schema.SessionRelationship{
				Kind: schema.SessionRelationshipContextFrom, TargetState: schema.RelationshipTargetKnown,
				TargetLocalID: &source, Evidence: schema.EvidenceNativeTyped,
				Anchor: &schema.PublicSourceAnchor{Kind: schema.PublicSourceAnchorGeneral},
			})
		case c.Change == "record-started-by":
			owner := schema.SessionID("22222222-2222-4222-8222-222222222222")
			metadata.Relationships = append(metadata.Relationships, schema.SessionRelationship{
				Kind: schema.SessionRelationshipStartedBy, TargetState: schema.RelationshipTargetKnown,
				TargetLocalID: &owner, Evidence: schema.EvidenceNativeTyped,
			})
		case c.Change == "none" || c.Change == "":
		default:
			t.Fatalf("unknown oracle change %q", c.Change)
		}
	}, c.Sessions...)
	if c.inventory != nil && c.inventory.Field == "metadataHash" {
		// The rewrite re-seals the hash; this row forges it after.
		rewriteManagedMetadataRaw(t, seed.output, func(document map[string]any) {
			var forged string
			if err := json.Unmarshal([]byte(c.inventory.Mutation), &forged); err != nil {
				t.Fatal(err)
			}
			document["metadataHash"] = forged
		})
	}
	for _, path := range c.CreateBeforeRefresh {
		writeOracleFile(t, seed.source, document, path)
	}
	if c.InstallFirst {
		installed := runOraclePipeline(t, seed.db, seed.empty, seed.output, true, nil)
		if installed.Summary.Errors != 0 {
			t.Fatalf("the installing refresh failed: %+v", installed.Summary)
		}
		managedRefreshPipelineDowngrade(t, seed.db, target)
		rewriteManagedMetadata(t, seed.output, func(_ string, metadata *schema.UnifiedMetadata) { metadata.AdapterVersion = &target })
	}
	seed.before = oracleActiveGenerations(t, seed.db)
	return seed
}

func writeOracleFile(t *testing.T, source string, document oracleDocument, path string) {
	t.Helper()
	for _, file := range document.Corpus {
		if file.Path != path {
			continue
		}
		full := filepath.Join(source, filepath.FromSlash(path))
		mustMkdir(t, filepath.Dir(full))
		if err := os.WriteFile(full, []byte(file.Transcript), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("no corpus file %s", path)
}

func removeOracleFiles(t *testing.T, source string, paths []string) {
	t.Helper()
	for _, path := range paths {
		if err := os.Remove(filepath.Join(source, filepath.FromSlash(path))); err != nil && !os.IsNotExist(err) {
			t.Error(err)
		}
	}
}

// rewriteManagedMetadataRaw changes every sidecar as a generic document,
// without re-sealing its hash.
func rewriteManagedMetadataRaw(t *testing.T, output string, change func(map[string]any)) {
	t.Helper()
	err := filepath.WalkDir(output, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), "--metadata.json") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var document map[string]any
		if err := json.Unmarshal(data, &document); err != nil {
			return err
		}
		change(document)
		encoded, err := json.Marshal(document)
		if err != nil {
			return err
		}
		return os.WriteFile(path, encoded, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func oracleActiveGenerations(t *testing.T, db *store.Store) map[string]string {
	t.Helper()
	active := map[string]string{}
	for _, row := range managedRefreshPipelineRows(t, db, `SELECT session_id || '=' || COALESCE(active_generation_id,'') FROM sessions`) {
		sid, generation, _ := strings.Cut(row, "=")
		active[sid] = generation
	}
	return active
}

// oracleParseOutcome reads what fork P's parse did to one session.
func oracleParseOutcome(t *testing.T, seed oracleSeed, result *ingest.PipelineResult, sid string) string {
	t.Helper()
	for _, entry := range result.IndexLog {
		if string(entry.SessionID) == sid && entry.Reason != nil && *entry.Reason == "projection unchanged" {
			return "skip"
		}
	}
	after := oracleActiveGenerations(t, seed.db)
	if after[sid] != "" && after[sid] != seed.before[sid] {
		return "install"
	}
	return "refuse"
}

// oracleValidatorOutcome reads fork V's validator answer for one session.
func oracleValidatorOutcome(store *halfValidatingStore, sid string) string {
	result, asked := store.outcome(ingest.SessionID(sid))
	if !asked {
		return "not-asked"
	}
	return string(result.Outcome)
}

// oracleObservation is both forks' result for every session of one case.
type oracleObservation map[string]oracleExpectation

func runOracleCase(t *testing.T, document oracleDocument, c oracleCase) oracleObservation {
	t.Helper()
	refreshSource := func(seed oracleSeed) string {
		if len(c.CreateBeforeRefresh) != 0 {
			return seed.source
		}
		return seed.empty
	}
	v := seedOracleStore(t, document, c)
	validating := newHalfValidatingStore(v.db, v.output, &ingest.OSFileSystem{})
	validating.forceValidation = true
	remove := func(seed oracleSeed) func() {
		var once sync.Once
		return func() { once.Do(func() { removeOracleFiles(t, seed.source, c.RemoveBeforeValidation) }) }
	}
	removeV := remove(v)
	runOraclePipeline(t, &oracleProveSeam{halfValidatingStore: validating, before: removeV}, refreshSource(v), v.output, false, removeV)

	p := seedOracleStore(t, document, c)
	parsed := runOraclePipeline(t, p.db, refreshSource(p), p.output, true, remove(p))

	observed := oracleObservation{}
	sessions := map[string]bool{}
	for sid := range v.before {
		sessions[sid] = true
	}
	for sid := range p.before {
		sessions[sid] = true
	}
	for sid := range oracleActiveGenerations(t, p.db) {
		sessions[sid] = true
	}
	for sid := range sessions {
		observed[sid] = oracleExpectation{Validator: oracleValidatorOutcome(validating, sid), Parse: oracleParseOutcome(t, p, parsed, sid)}
	}
	return observed
}

// oracleViolations lists the sessions where the two implications fail.
func oracleViolations(observed oracleObservation) []string {
	var violations []string
	for sid, o := range observed {
		proven := o.Validator == string(ingest.ValidationProven)
		unsupported := o.Validator == string(ingest.ValidationUnsupported) || o.Validator == "not-asked"
		skip := o.Parse == "skip"
		if proven && !skip {
			violations = append(violations, sid+": proven but the parse did not skip ("+o.Parse+")")
		}
		if skip && !unsupported && !proven {
			violations = append(violations, sid+": the parse skipped but the validator answered "+o.Validator)
		}
	}
	sort.Strings(violations)
	return violations
}

// oracleCases is every oracle case: the inventory rows and the named cases.
func oracleCases(t *testing.T, document oracleDocument) []oracleCase {
	t.Helper()
	var cases []oracleCase
	for _, row := range loadMetadataInventory(t) {
		cases = append(cases, oracleCase{Name: "inventory/" + row.Field, inventory: &row})
	}
	return append(cases, document.Cases...)
}

// TestValidatorAgreesWithTheParseSkip is the validator oracle on real Claude
// stores: for every inventory field, every named relationship case and the
// filesystem cases, proven implies the parse would skip, and a parse skip
// with a supported validator implies proven.
func TestValidatorAgreesWithTheParseSkip(t *testing.T) {
	document := loadOracleDocument(t)
	for _, c := range oracleCases(t, document) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			observed := runOracleCase(t, document, c)
			t.Logf("observed %v", observed)
			if violations := oracleViolations(observed); len(violations) != 0 {
				t.Fatalf("validator and parse skip disagree: %v (observed %+v)", violations, observed)
			}
			for sid, want := range c.Expect {
				if got := observed[sid]; got != want {
					t.Errorf("session %s: observed %+v, want %+v", sid, got, want)
				}
			}
		})
	}
}

// TestClaudeMetadataHalfMutationsFailTheOracle applies each metadata-half
// mutation and requires the oracle to report a disagreement on at least one
// case. A mutation whose branch no pipeline-written store can reach is listed
// in the mutations fixture as differential-only.
func TestClaudeMetadataHalfMutationsFailTheOracle(t *testing.T) {
	document := loadOracleDocument(t)
	for _, mutation := range loadClaudeMetadataHalfMutations(t) {
		t.Run(mutation.Name, func(t *testing.T) {
			ingest.SetClaudeMetadataHalfMutationForTest(t, claudeHalfMutationHook(t, mutation.Name))
			var caught []string
			for _, c := range document.Cases {
				observed := runOracleCase(t, document, c)
				t.Logf("%s observed %v", c.Name, observed)
				if violations := oracleViolations(observed); len(violations) != 0 {
					caught = append(caught, c.Name+": "+strings.Join(violations, "; "))
				}
			}
			if mutation.OracleUnreachable != "" {
				if len(caught) != 0 {
					t.Fatalf("the mutation is listed as unreachable by the oracle (%s) but the oracle caught it: %v", mutation.OracleUnreachable, caught)
				}
				return
			}
			if len(caught) == 0 {
				t.Fatal("the mutation passed the oracle on every case")
			}
			t.Logf("caught: %v", caught)
		})
	}
}

// TestValidatedSettleCandidateWindow runs the race variants: the filesystem
// changes between the validator's answer and the settle, and the settle uses
// the candidate computed before, never a recomputed one.
func TestValidatedSettleCandidateWindow(t *testing.T) {
	document := loadOracleDocument(t)
	if len(document.Windows) == 0 {
		t.Fatal("no candidate-window cases")
	}
	for _, w := range document.Windows {
		t.Run(w.Name, func(t *testing.T) {
			t.Parallel()
			seed := seedOracleStore(t, document, oracleCase{Name: w.Name, Change: "none"})
			validating := newHalfValidatingStore(seed.db, seed.output, &ingest.OSFileSystem{})
			validating.forceValidation = true
			sid := ingest.SessionID(w.Session)
			var removed sync.Once
			validating.afterValidation = func(got ingest.SessionID) {
				if got != sid {
					return
				}
				for _, path := range w.RestoreAfterValidation {
					writeOracleFile(t, seed.source, document, path)
				}
				removeOracleFiles(t, seed.source, w.RemoveAfterValidation)
			}
			prove := &oracleProveSeam{halfValidatingStore: validating, before: func() {
				removed.Do(func() { removeOracleFiles(t, seed.source, w.RemoveBeforeValidation) })
			}}
			result := runOraclePipeline(t, prove, seed.empty, seed.output, false, nil)
			if got := oracleValidatorOutcome(validating, w.Session); got != w.Validator {
				t.Fatalf("validator = %s, want %s (half error %v)", got, w.Validator, validating.halfErrors[sid])
			}
			validating.mu.Lock()
			settleErr, settled := validating.settled[sid]
			validating.mu.Unlock()
			if (settled && settleErr == nil) != w.Settled {
				t.Fatalf("settled = %t (error %v), want %t", settled && settleErr == nil, settleErr, w.Settled)
			}
			parsed := false
			for _, entry := range result.IndexLog {
				if entry.SessionID == sid && entry.ParseDuration != nil && (entry.Reason == nil || !strings.HasSuffix(*entry.Reason, "(unchanged)")) {
					parsed = true
				}
			}
			if parsed != w.Parsed {
				t.Fatalf("parsed = %t, want %t (index log %+v)", parsed, w.Parsed, result.IndexLog)
			}
			for _, session := range result.Sessions {
				if session.SessionID == sid && session.Error != nil {
					t.Fatalf("session %s failed: %v", sid, session.Error)
				}
			}
			if w.Settled && oracleActiveGenerations(t, seed.db)[w.Session] != seed.before[w.Session] {
				t.Fatal("the validated settle moved the active generation")
			}
		})
	}
}
