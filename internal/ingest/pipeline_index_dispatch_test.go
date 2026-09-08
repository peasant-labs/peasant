package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/index_dispatch.yaml
var indexDispatchFixtureData []byte

// The rejection fixtures each hold a full corpus with exactly ONE thing wrong, so
// the evidence that the loader is strict sits beside the corpus it protects and
// its own header can say what it is for.
var (
	//go:embed testdata/index_dispatch-reject-uncovered-kind.yaml
	indexDispatchRejectUncoveredKindData []byte
	//go:embed testdata/index_dispatch-reject-silent-refusal.yaml
	indexDispatchRejectSilentRefusalData []byte
	//go:embed testdata/index_dispatch-reject-uncovered-exit.yaml
	indexDispatchRejectUncoveredExitData []byte
)

const indexDispatchFixturePath = "internal/ingest/testdata/index_dispatch.yaml"

// sourceKindName is the fixture's spelling of an ingest.TranscriptSourceKind.
//
// It is its own type with an explicit lookup rather than an integer the YAML
// could carry, because a numeric kind in a corpus is unreadable and, worse, a
// missing or misspelled one would decode to 0 - which is the UNDECLARED kind, a
// real arm. A row would then silently exercise the refusal path while reading as
// though it exercised something else.
type sourceKindName string

const (
	sourceKindUnknown   sourceKindName = "unknown"
	sourceKindFile      sourceKindName = "file"
	sourceKindDirectory sourceKindName = "directory"
)

// fixtureSourceKinds maps every kind a corpus may name to the production value it
// must equal. It is exhaustive over ingest.AllTranscriptSourceKinds, and the
// loader checks that: a kind added to production with no spelling here cannot be
// covered by any row, so the corpus would have no way to satisfy the coverage
// requirement below.
var fixtureSourceKinds = map[sourceKindName]ingest.TranscriptSourceKind{
	sourceKindUnknown:   ingest.TranscriptSourceKindUnknown,
	sourceKindFile:      ingest.TranscriptSourceFile,
	sourceKindDirectory: ingest.TranscriptSourceDirectory,
}

// entryPointName identifies the captured parser input used by the pipeline.
type entryPointName string

const (
	// entryPointBytes is IndexTranscriptBytes: the in-memory path that exists to
	// avoid a second disk read.
	entryPointBytes entryPointName = "bytes"
	// entryPointTree is the canonical OpenCode parser over its captured tree.
	entryPointTree entryPointName = "tree"
	// entryPointNone is neither, which is only correct for a refusal.
	entryPointNone entryPointName = "none"
)

var allEntryPoints = []entryPointName{entryPointBytes, entryPointTree, entryPointNone}

// indexOutcomeName is what the run records for the session.
type indexOutcomeName string

const (
	outcomeIndexed indexOutcomeName = "indexed"
	outcomeError   indexOutcomeName = "error"
)

type indexDispatchDocument struct {
	RequiredNames    []string            `yaml:"requiredNames"`
	DirectoryPreview string              `yaml:"directoryPreview"`
	LastGoodPreview  string              `yaml:"lastGoodPreview"`
	Cases            []indexDispatchCase `yaml:"cases"`
}

type indexDispatchCase struct {
	Name          string           `yaml:"name"`
	SourceKind    sourceKindName   `yaml:"sourceKind"`
	OriginalRoot  presence         `yaml:"originalRoot"`
	NativeLocator presence         `yaml:"nativeLocator"`
	Bytes         presence         `yaml:"transcriptBytes"`
	EntryPoint    entryPointName   `yaml:"entryPoint"`
	IndexOutcome  indexOutcomeName `yaml:"indexOutcome"`
	ErrorContains string           `yaml:"errorContains,omitempty"`
}

// presence is whether an input the dispatch reads is there.
type presence string

const (
	present presence = "present"
	absent  presence = "absent"
)

// dispatchExit distinguishes input availability before capture. Both file cases
// reach the bytes parser, independently of whether extraction supplied bytes.
type dispatchExit string

const (
	exitDirectoryMissingRoot dispatchExit = "directory refuses unavailable native context"
	exitDirectoryIndexes     dispatchExit = "directory indexes from its provider tree"
	exitFileFromBytes        dispatchExit = "file indexes from the bytes in hand"
	exitFileFromDisk         dispatchExit = "file captures what was written before parsing bytes"
	exitUndeclaredRefused    dispatchExit = "an undeclared kind is refused"
	// exitUnhandledKind is the default arm. It is NOT in requiredDispatchExits and
	// cannot be: fixtureSourceKinds is exhaustive over AllTranscriptSourceKinds by
	// construction and the loader rejects any other spelling, so no row can reach
	// it while that guard holds. Named so it does not read as an untested arm.
	exitUnhandledKind dispatchExit = "a kind the dispatch does not handle"
)

// requiredDispatchExits is every exit a corpus row can actually drive.
func requiredDispatchExits() []dispatchExit {
	return []dispatchExit{
		exitDirectoryMissingRoot, exitDirectoryIndexes,
		exitFileFromBytes, exitFileFromDisk, exitUndeclaredRefused,
	}
}

// dispatchExitOf derives the exit a row takes from the row's own inputs, so a
// row cannot claim an exit it does not drive.
func dispatchExitOf(testCase indexDispatchCase) dispatchExit {
	switch fixtureSourceKinds[testCase.SourceKind] {
	case ingest.TranscriptSourceDirectory:
		if testCase.OriginalRoot == absent && testCase.NativeLocator == absent {
			return exitDirectoryMissingRoot
		}
		return exitDirectoryIndexes
	case ingest.TranscriptSourceFile:
		if testCase.Bytes == absent {
			return exitFileFromDisk
		}
		return exitFileFromBytes
	default:
		return exitUndeclaredRefused
	}
}

// loadIndexDispatchFixture decodes and fully validates the corpus.
//
// The closed-set guard is the point of this loader: the corpus must cover every
// member of ingest.AllTranscriptSourceKinds. A dispatch written for the kinds
// somebody remembered passes for those and says nothing about the rest, which is
// how the undeclared kind spent its life folded into the file arm.
func loadIndexDispatchFixture(data []byte) (indexDispatchDocument, error) {
	var document indexDispatchDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, indexDispatchRuleError(
			"typed YAML fields must match the document schema",
			"loader=first-document decode",
			fmt.Sprintf("fix=remove unknown fields and match the typed schema: %v", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found another YAML document")
		}
		return document, indexDispatchRuleError(
			"exactly one YAML document is allowed; cases below a second one prove nothing",
			"loader=end-of-document check",
			fmt.Sprintf("fix=remove the second document so the next decode returns EOF: %v", err))
	}
	if len(document.Cases) == 0 {
		return document, indexDispatchRuleError(
			"the fixture has no cases",
			"loader=case validation",
			"fix=restore the named dispatch cases")
	}
	seen := map[string]bool{}
	coveredExits := map[dispatchExit]bool{}
	var coveredKinds []ingest.TranscriptSourceKind
	for index, testCase := range document.Cases {
		if strings.TrimSpace(testCase.Name) == "" || seen[testCase.Name] {
			return document, indexDispatchRuleError(
				fmt.Sprintf("case name %q is missing or duplicated", testCase.Name),
				fmt.Sprintf("loader=case index %d", index),
				"fix=give every case a unique, behaviour-naming name")
		}
		seen[testCase.Name] = true
		kind, known := fixtureSourceKinds[testCase.SourceKind]
		if !known {
			return document, indexDispatchRuleError(
				fmt.Sprintf("case %q names the source kind %q, which the dispatch does not define", testCase.Name, testCase.SourceKind),
				fmt.Sprintf("loader=case index %d", index),
				fmt.Sprintf("fix=use one of %s, %s, %s; a blank or invented value would decode as the UNDECLARED kind while "+
					"reading as a deliberate choice", sourceKindUnknown, sourceKindFile, sourceKindDirectory))
		}
		if !slices.Contains(allEntryPoints, testCase.EntryPoint) {
			return document, indexDispatchRuleError(
				fmt.Sprintf("case %q expects the entry point %q, which is not one the pipeline can call", testCase.Name, testCase.EntryPoint),
				fmt.Sprintf("loader=case index %d", index),
				fmt.Sprintf("fix=use one of %s, %s, %s", entryPointBytes, entryPointTree, entryPointNone))
		}
		switch testCase.IndexOutcome {
		case outcomeError:
			// A refusal that nothing pins the wording of asserts only that indexing
			// stopped. Stopping silently is the defect this dispatch replaced.
			if strings.TrimSpace(testCase.ErrorContains) == "" {
				return document, indexDispatchRuleError(
					fmt.Sprintf("case %q expects a refusal but pins none of what it says", testCase.Name),
					fmt.Sprintf("loader=case index %d", index),
					"fix=set errorContains to a phrase the recorded message must carry; a refusal nobody can read is barely "+
						"better than the silent empty import it replaced")
			}
			if testCase.EntryPoint != entryPointNone {
				return document, indexDispatchRuleError(
					fmt.Sprintf("case %q expects a refusal but also expects the %q entry point to be called",
						testCase.Name, testCase.EntryPoint),
					fmt.Sprintf("loader=case index %d", index),
					"fix=set entryPoint to none; a refused dispatch must not reach an indexer at all")
			}
		case outcomeIndexed:
			if testCase.ErrorContains != "" {
				return document, indexDispatchRuleError(
					fmt.Sprintf("case %q indexes successfully but also pins the error text %q", testCase.Name, testCase.ErrorContains),
					fmt.Sprintf("loader=case index %d", index),
					"fix=drop errorContains; a successful index records no error")
			}
			if testCase.EntryPoint == entryPointNone {
				return document, indexDispatchRuleError(
					fmt.Sprintf("case %q indexes successfully with no entry point called", testCase.Name),
					fmt.Sprintf("loader=case index %d", index),
					"fix=name the method the pipeline must call; entries cannot appear without one")
			}
		default:
			return document, indexDispatchRuleError(
				fmt.Sprintf("case %q names the outcome %q, which is not one this corpus can assert", testCase.Name, testCase.IndexOutcome),
				fmt.Sprintf("loader=case index %d", index),
				fmt.Sprintf("fix=use %s or %s", outcomeIndexed, outcomeError))
		}
		for label, value := range map[string]presence{"originalRoot": testCase.OriginalRoot, "nativeLocator": testCase.NativeLocator, "transcriptBytes": testCase.Bytes} {
			if value != present && value != absent {
				return document, indexDispatchRuleError(
					fmt.Sprintf("case %q gives %s the value %q", testCase.Name, label, value),
					fmt.Sprintf("loader=case index %d", index),
					"fix=use present or absent; a blank value would decode as neither and silently pick an exit")
			}
		}
		expectedInput := entryPointBytes
		switch dispatchExitOf(testCase) {
		case exitDirectoryIndexes:
			expectedInput = entryPointTree
		case exitDirectoryMissingRoot, exitUndeclaredRefused:
			expectedInput = entryPointNone
		}
		if testCase.EntryPoint != expectedInput {
			return document, indexDispatchRuleError(
				fmt.Sprintf("case %q expects %s but its available source requires %s", testCase.Name, testCase.EntryPoint, expectedInput),
				"loader=captured-input contract",
				"fix=expect captured bytes for files, a captured tree for available directory input, or no parser for refusal")
		}
		coveredKinds = append(coveredKinds, kind)
		coveredExits[dispatchExitOf(testCase)] = true
	}
	// The closed set is walked from the production enumeration, so a kind added to
	// the dispatch fails here rather than shipping with an arm nobody exercises.
	for _, kind := range ingest.AllTranscriptSourceKinds {
		if !slices.Contains(coveredKinds, kind) {
			return document, indexDispatchRuleError(
				fmt.Sprintf("no case declares the %q source kind", kind),
				"loader=closed-set coverage",
				"fix=add a case for it; an uncovered kind is an arm whose dispatch nobody checks, and the last one to go "+
					"uncovered was folded into the file arm and handed bytes it had to discard")
		}
	}
	// EXIT coverage, walked from the decisions the function makes rather than from
	// the kinds it switches on. Anchoring on kinds left two arms unreached.
	for _, exit := range requiredDispatchExits() {
		if !coveredExits[exit] {
			return document, indexDispatchRuleError(
				fmt.Sprintf("no case drives the exit where %s", exit),
				"loader=exit coverage",
				"fix=add one; this dispatch makes more decisions than it has source kinds, and an unreached arm can be "+
					"deleted with every test still green - which for both of these restores an indexer being handed an "+
					"argument that cannot work and indexing nothing, quietly")
		}
	}
	if err := testutil.RequireFixtureNames("index dispatch", "case", document.RequiredNames, seen); err != nil {
		return document, err
	}
	return document, nil
}

func indexDispatchRuleError(what, where, fix string) error {
	return fmt.Errorf(
		"index dispatch fixture rule failed: %s; a malformed or incomplete corpus invalidates the only evidence that an "+
			"indexer is handed the argument its own contract declares; where=%s %s; when=test fixture loading; "+
			"impact=a session could import, report success, and be stored with no entries at all; %s",
		what, indexDispatchFixturePath, where, fix)
}

// --- loader guards ----------------------------------------------------------

func TestLoadIndexDispatchFixture_RejectsACorpusThatSkipsASourceKind(t *testing.T) {
	t.Parallel()
	_, err := loadIndexDispatchFixture(indexDispatchRejectUncoveredKindData)
	if err == nil || !strings.Contains(err.Error(), `no case declares the "unknown" source kind`) {
		t.Fatalf("error = %v, want rejection of a corpus that leaves the undeclared kind uncovered; that is the arm an "+
			"indexer reaches by forgetting, which is likelier than mis-declaring", err)
	}
}

// Each input-availability case remains required alongside source-kind coverage.
func TestLoadIndexDispatchFixture_RejectsACorpusThatSkipsAnExit(t *testing.T) {
	t.Parallel()
	_, err := loadIndexDispatchFixture(indexDispatchRejectUncoveredExitData)
	if err == nil || !strings.Contains(err.Error(), "directory refuses unavailable native context") {
		t.Fatalf("error = %v, want rejection of a corpus that covers every source KIND while leaving an exit unreached; "+
			"that is the shape this corpus had while two arms could be deleted with everything green", err)
	}
}

func TestLoadIndexDispatchFixture_RejectsARefusalThatPinsNoneOfItsWording(t *testing.T) {
	t.Parallel()
	_, err := loadIndexDispatchFixture(indexDispatchRejectSilentRefusalData)
	if err == nil || !strings.Contains(err.Error(), "pins none of what it says") {
		t.Fatalf("error = %v, want rejection of a refusal row with no errorContains; a refusal nobody can read is barely "+
			"better than the silent empty import it replaced", err)
	}
}

func TestLoadIndexDispatchFixture_RejectsAnUnknownField(t *testing.T) {
	t.Parallel()
	_, err := loadIndexDispatchFixture([]byte("somethingElse: true\n"))
	if err == nil || !strings.Contains(err.Error(), "typed YAML fields must match") {
		t.Fatalf("error = %v, want rejection of an unknown field", err)
	}
}

func TestLoadIndexDispatchFixture_RejectsARenamedRequiredCase(t *testing.T) {
	t.Parallel()
	data := bytes.Replace(indexDispatchFixtureData,
		[]byte("name: a-file-source-with-no-bytes-in-hand-reads-what-was-written-instead"),
		[]byte("name: renamed-retained-input-case"), 1)
	if _, err := loadIndexDispatchFixture(data); err == nil {
		t.Fatal("renaming a required input case did not invalidate the corpus")
	}
}

// --- the corpus -------------------------------------------------------------

// TestPipeline_IndexDispatchFollowsTheIndexersDeclaredSourceKind verifies captured
// file bytes, canonical native-tree parsing and actionable refusal through Store.
func TestPipeline_IndexDispatchFollowsTheIndexersDeclaredSourceKind(t *testing.T) {
	document, err := loadIndexDispatchFixture(indexDispatchFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	required := []string{
		"an-undeclared-source-kind-is-refused-rather-than-assumed-to-be-a-file",
		"a-file-source-is-handed-the-bytes-so-the-second-read-is-really-avoided",
		"a-file-source-with-no-bytes-in-hand-reads-what-was-written-instead",
		"a-directory-source-is-never-handed-bytes-it-would-have-to-discard",
		"a-directory-source-that-lost-its-provider-root-is-refused-not-guessed-at",
	}
	if !slices.Equal(document.RequiredNames, required) {
		t.Fatal("the dispatch required-name manifest changed")
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			mfs := testutil.NewMemFS()
			git := testutil.DefaultGitResolver()
			session := setupOpenCodeFixture(t, mfs, testutil.TestSessionUUID, "dispatchprojecthash")
			addOpenCodeMessage(t, mfs, testutil.TestSessionUUID, "msg_one", string(ingest.RoleUser), 10, 0)
			addOpenCodePart(t, mfs, "msg_one", "prt_one")
			session.ModTime = time.Now().Add(-time.Hour)
			transcript, err := mfs.ReadFile(session.SourcePath.String())
			if err != nil {
				t.Fatal(err)
			}
			sid := session.SessionID
			meta := makeMinimalMeta(t, string(sid))
			meta.Source.FilePath = session.SourcePath.String()
			meta.Source.Format = ingest.SourceFormatJSON
			meta.ModelHarness = ingest.HarnessOpenCode
			if testCase.OriginalRoot == absent {
				session.OriginalRoot = ""
			}
			if testCase.NativeLocator == absent {
				meta.Source.FilePath = ""
			}
			observer := &dispatchRecordingIndexer{recordingIndexer: &recordingIndexer{
				kind:    fixtureSourceKinds[testCase.SourceKind],
				entries: []schema.SessionEntry{{SessionID: sid, Harness: session.Harness, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
			}}
			var indexer ingest.TranscriptIndexer = observer
			if testCase.SourceKind == sourceKindDirectory {
				// The real implementation owns the private captured-tree methods.
				// An arbitrary directory fake cannot satisfy that input contract.
				indexer = ingest.NewOpenCodeIndexer(mfs)
			}
			fixtureStore := newPipelineFixtureStore(t, nil, nil)
			var previousEntries []schema.SessionEntry
			var previousState *ingest.SessionIndexState
			if testCase.IndexOutcome == outcomeError {
				if err := fixtureStore.InsertSessions(t.Context(), []ingest.StoreEntry{{Metadata: meta}}); err != nil {
					t.Fatal(err)
				}
				previousEntries = []schema.SessionEntry{{SessionID: sid, Harness: session.Harness, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText, ContentPreview: &document.LastGoodPreview}}
				if err := fixtureStore.IndexSessionEntries(t.Context(), sid, previousEntries); err != nil {
					t.Fatal(err)
				}
				previousEntries, err = fixtureStore.ListEntries(t.Context(), sid)
				if err != nil {
					t.Fatal(err)
				}
				previousState, err = fixtureStore.ReadIndexState(t.Context(), sid)
				if err != nil {
					t.Fatal(err)
				}
			}
			cfg := makePipelineConfig(testOutputDir)
			cfg.Sources = map[ingest.Harness]ingest.SourceConfig{
				defaults.HarnessOpenCode: {Enabled: true},
			}
			if session.OriginalRoot != "" {
				cfg.Sources[defaults.HarnessOpenCode] = ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{session.OriginalRoot}}
			}
			adapters := map[ingest.Harness]ingest.AdapterFactory{
				defaults.HarnessOpenCode: makeStubAdapter([]ingest.DiscoveredSession{session}, map[ingest.SessionID]*ingest.UnifiedMetadata{sid: meta}),
			}
			if testCase.Bytes == absent {
				// Retained reindex starts without extraction bytes; capture must
				// still deliver the committed file bytes directly to the parser.
				seed, err := ingest.NewPipeline(mfs, git, adapters, cfg)
				if err != nil {
					t.Fatal(err)
				}
				seeded, err := seed.Run(t.Context())
				if err != nil || seeded.Summary.Errors != 0 {
					t.Fatalf("publish retained dispatch input: %+v %v", seeded, err)
				}
				cfg.Reindex, cfg.Force = true, true
			}
			pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg,
				ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{defaults.HarnessOpenCode: indexer}),
				ingest.WithMetricsStore(fixtureStore),
			)
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if testCase.SourceKind != sourceKindDirectory {
				assertDispatchEntryPoint(t, testCase, observer, transcript)
				for _, root := range observer.seenRoots {
					if root != session.OriginalRoot {
						t.Errorf("file parser lost recorded OriginalRoot: got %q, want %q", root, session.OriginalRoot)
					}
				}
				for _, path := range observer.seenPaths {
					if path == session.SourcePath {
						t.Errorf("file parser received the native locator %q instead of the managed transcript locator", path)
					}
				}
			}
			assertDispatchOutcome(t, testCase, result, fixtureStore, sid, previousEntries, previousState)
			if testCase.EntryPoint == entryPointTree {
				entries, err := fixtureStore.ListEntries(t.Context(), sid)
				if err != nil {
					t.Fatal(err)
				}
				found := slices.ContainsFunc(entries, func(entry schema.SessionEntry) bool {
					return entry.EntryID != nil && *entry.EntryID == "msg_one" && entry.Role == ingest.RoleUser &&
						entry.ContentPreview != nil && *entry.ContentPreview == document.DirectoryPreview
				})
				if !found {
					t.Fatalf("canonical directory parser did not persist its captured message: %+v", entries)
				}
			}
		})
	}
}

type dispatchRecordingIndexer struct {
	*recordingIndexer
	captured [][]byte
}

var _ ingest.TranscriptIndexer = (*dispatchRecordingIndexer)(nil)

func (indexer *dispatchRecordingIndexer) IndexTranscriptBytes(ctx context.Context, session ingest.DiscoveredSession, data []byte) ([]schema.SessionEntry, error) {
	indexer.captured = append(indexer.captured, bytes.Clone(data))
	return indexer.recordingIndexer.IndexTranscriptBytes(ctx, session, data)
}

func assertDispatchEntryPoint(t *testing.T, testCase indexDispatchCase, indexer *dispatchRecordingIndexer, transcript []byte) {
	t.Helper()
	if indexer.fileCalls > 0 {
		t.Fatal("file parser reopened a path instead of consuming captured input")
	}
	if testCase.EntryPoint == entryPointNone {
		if len(indexer.captured) != 0 {
			t.Fatal("refused source kind reached the bytes parser")
		}
		return
	}
	if len(indexer.captured) == 0 {
		t.Fatal("file parser received no captured input")
	}
	for _, data := range indexer.captured {
		if !bytes.Equal(data, transcript) {
			t.Fatalf("file parser received different bytes from the retained transcript: got %q, want %q", data, transcript)
		}
	}
}

func assertDispatchOutcome(
	t *testing.T,
	testCase indexDispatchCase,
	result *ingest.PipelineResult,
	database *pipelineFixtureStore,
	sid ingest.SessionID,
	previousEntries []schema.SessionEntry,
	previousState *ingest.SessionIndexState,
) {
	t.Helper()
	entries, err := database.ListEntries(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	state, err := database.ReadIndexState(t.Context(), sid)
	if err != nil || state == nil {
		t.Fatalf("read actual index state: %+v %v", state, err)
	}
	var logged *ingest.IndexLogEntry
	for i := range result.IndexLog {
		if result.IndexLog[i].SessionID != sid {
			t.Fatalf("dispatch indexed an unrelated session: %+v", result.IndexLog[i])
		}
		logged = &result.IndexLog[i]
	}
	if logged == nil {
		t.Fatalf("no index outcome was recorded for %q", sid)
	}
	switch testCase.IndexOutcome {
	case outcomeIndexed:
		wantOutcome := ingest.IndexOutcomeIndexed
		if testCase.Bytes == absent {
			wantOutcome = ingest.IndexOutcomeReindexed
		}
		if logged.Outcome != wantOutcome || len(entries) == 0 || state.IndexedInputHash == nil {
			t.Fatalf("captured input was not committed: outcome=%+v entries=%+v state=%+v", logged, entries, state)
		}
	case outcomeError:
		if logged.Outcome != ingest.IndexOutcomeError || !strings.Contains(derefOrEmpty(logged.ErrorMessage), testCase.ErrorContains) {
			t.Errorf("refusal lacks its actionable source explanation %q: %+v", testCase.ErrorContains, logged)
		}
		state.ArtifactHash = previousState.ArtifactHash // Publication can mirror metadata; refusal cannot replace the index.
		if !reflect.DeepEqual(entries, previousEntries) || !reflect.DeepEqual(state, previousState) {
			t.Fatalf("refused capture changed the last-good index: entries=%+v state=%+v", entries, state)
		}
	}
}

func derefOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
