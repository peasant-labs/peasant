package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
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

// indexDispatchFixtureFloor is the row count the committed corpus must not fall below.
// It is a floor EQUAL to the current count rather than a hand-picked minimum:
// any slack between the two is rows that can be deleted in silence.
const indexDispatchFixtureFloor = 5

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

// entryPointName is the index method the pipeline is expected to call.
type entryPointName string

const (
	// entryPointBytes is IndexTranscriptBytes: the in-memory path that exists to
	// avoid a second disk read.
	entryPointBytes entryPointName = "bytes"
	// entryPointFile is IndexTranscript: the indexer resolves what it needs itself.
	entryPointFile entryPointName = "file"
	// entryPointNone is neither, which is only correct for a refusal.
	entryPointNone entryPointName = "none"
)

var allEntryPoints = []entryPointName{entryPointBytes, entryPointFile, entryPointNone}

// indexOutcomeName is what the run records for the session.
type indexOutcomeName string

const (
	outcomeIndexed indexOutcomeName = "indexed"
	outcomeError   indexOutcomeName = "error"
)

type indexDispatchDocument struct {
	ExpectedCaseCount int                 `yaml:"expectedCaseCount"`
	Cases             []indexDispatchCase `yaml:"cases"`
}

type indexDispatchCase struct {
	Name          string           `yaml:"name"`
	SourceKind    sourceKindName   `yaml:"sourceKind"`
	OriginalRoot  presence         `yaml:"originalRoot"`
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

// dispatchExit names one decision indexWithSourceKind can reach.
//
// The corpus's coverage is anchored HERE rather than on the source kinds. There
// are three kinds and six exits, and the two the kind-anchored corpus could not
// reach were both deletable with the package green: the missing-root refusal, and
// the file arm's read-from-disk path when no bytes are in hand. Both are edits a
// maintainer makes while simplifying an arm they believe is redundant, and both
// reinstate the failure this function exists to prevent.
type dispatchExit string

const (
	exitDirectoryMissingRoot dispatchExit = "directory refuses a lost provider root"
	exitDirectoryIndexes     dispatchExit = "directory indexes from its provider tree"
	exitFileFromBytes        dispatchExit = "file indexes from the bytes in hand"
	exitFileFromDisk         dispatchExit = "file reads what was written"
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
		if testCase.OriginalRoot == absent {
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
	if len(document.Cases) == 0 || document.ExpectedCaseCount != len(document.Cases) {
		return document, indexDispatchRuleError(
			fmt.Sprintf("declared and actual case counts must match and be non-zero, got expectedCaseCount=%d cases=%d",
				document.ExpectedCaseCount, len(document.Cases)),
			"loader=case-count validation",
			"fix=set expectedCaseCount to the number of cases present")
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
				fmt.Sprintf("fix=use one of %s, %s, %s", entryPointBytes, entryPointFile, entryPointNone))
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
		for label, value := range map[string]presence{"originalRoot": testCase.OriginalRoot, "transcriptBytes": testCase.Bytes} {
			if value != present && value != absent {
				return document, indexDispatchRuleError(
					fmt.Sprintf("case %q gives %s the value %q", testCase.Name, label, value),
					fmt.Sprintf("loader=case index %d", index),
					"fix=use present or absent; a blank value would decode as neither and silently pick an exit")
			}
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

// TestLoadIndexDispatchFixture_RejectsACorpusThatSkipsAnExit is the guard on the
// axis this corpus was anchored to wrongly.
//
// Coverage used to be asserted per source KIND - three kinds, three rows - while
// the dispatch makes more decisions than it has kinds. Two arms were therefore
// unreachable by any row, and both were deletable with the package green.
func TestLoadIndexDispatchFixture_RejectsACorpusThatSkipsAnExit(t *testing.T) {
	t.Parallel()
	_, err := loadIndexDispatchFixture(indexDispatchRejectUncoveredExitData)
	if err == nil || !strings.Contains(err.Error(), "directory refuses a lost provider root") {
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
	_, err := loadIndexDispatchFixture([]byte("expectedCaseCount: 1\nsomethingElse: true\n"))
	if err == nil || !strings.Contains(err.Error(), "typed YAML fields must match") {
		t.Fatalf("error = %v, want rejection of an unknown field", err)
	}
}

// --- the corpus -------------------------------------------------------------

// TestPipeline_IndexDispatchFollowsTheIndexersDeclaredSourceKind pins the dispatch
// contract itself, for every kind an indexer can declare.
//
// A directory-source indexer must never be handed transcript bytes. Not because
// passing them breaks anything today - it does not, the indexer just drops them -
// but because "the caller passes bytes, the callee discards them" is what made the
// lost provider root invisible for as long as it was. An argument that is always
// ignored cannot signal that the thing it was meant to replace has gone missing.
//
// It also asserts the converse: a file-source indexer IS handed the bytes, so the
// second disk read the in-memory path exists to avoid is genuinely avoided; and
// the refusal: an indexer that declares NOTHING reaches neither method and the run
// records why. That last arm used to be folded into the file case, where a
// directory-source harness that writes one JSON session file - which is exactly
// the shape that broke - would have been handed bytes and stored empty.
func TestPipeline_IndexDispatchFollowsTheIndexersDeclaredSourceKind(t *testing.T) {
	document, err := loadIndexDispatchFixture(indexDispatchFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	// The FLOOR, asserted here rather than in the loader because the rejection
	// fixtures share that loader and are deliberately smaller. The declared count
	// alone is satisfied by deleting a row and decrementing it in the same edit;
	// the floor does not move with the corpus, so the count dropping is caught
	// even when the pair stays self-consistent. Coverage catches a row swapped for
	// a junk one, a floor catches the count dropping - different failures.
	if len(document.Cases) < indexDispatchFixtureFloor {
		t.Fatalf("the index dispatch corpus holds %d cases, below the floor of %d. Restore the case, or lower the floor "+
			"deliberately and say in the fixture header which behaviour stopped being covered.",
			len(document.Cases), indexDispatchFixtureFloor)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			mfs := testutil.NewMemFS()
			git := testutil.DefaultGitResolver()

			const projectHash = "dispatchprojecthash"
			session := setupOpenCodeFixture(t, mfs, testutil.TestSessionUUID, projectHash)
			addOpenCodeMessage(t, mfs, testutil.TestSessionUUID, "msg_one", string(ingest.RoleUser), 10, 0)
			addOpenCodePart(t, mfs, "msg_one", "prt_one")
			session.ModTime = time.Now().Add(-1 * time.Hour)

			sid := session.SessionID
			meta := makeMinimalMeta(t, string(sid))
			meta.Source.FilePath = session.SourcePath.String()
			meta.Source.Format = schema.SourceFormat(ingest.SourceFormatJSON)
			meta.ModelHarness = schema.Harness(defaults.HarnessOpenCode)

			// The row's inputs are DRIVEN, not described. Without this the two arms
			// the kind-anchored corpus could not reach would still be unreached, and
			// the new columns would be decoration.
			providerRoot := session.OriginalRoot
			if testCase.OriginalRoot == absent {
				// What the defect looked like: the provider root lost between
				// discovery and indexing, which is what the directory arm refuses.
				session.OriginalRoot = ""
			}
			if testCase.Bytes == absent {
				// The pipeline keeps in-memory bytes only for the single-file source
				// formats; anything else reaches the indexer with none, which is the
				// state the reindex path is always in.
				meta.Source.Format = ""
				session.SourceFormat = ""
			}

			indexer := &recordingIndexer{
				kind:    fixtureSourceKinds[testCase.SourceKind],
				entries: []schema.SessionEntry{{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText}},
			}
			metricsStore := testutil.NewStubMetricsStore()
			cfg := makePipelineConfig(testOutputDir)
			cfg.Sources = map[ingest.Harness]ingest.SourceConfig{
				defaults.HarnessOpenCode: {Enabled: true, Paths: []ingest.ResolvedPath{providerRoot}},
			}
			pipeline, err := ingest.NewPipeline(mfs, git,
				map[ingest.Harness]ingest.AdapterFactory{
					defaults.HarnessOpenCode: makeStubAdapter(
						[]ingest.DiscoveredSession{session},
						map[ingest.SessionID]*ingest.UnifiedMetadata{sid: meta},
					),
				},
				cfg,
				ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
					defaults.HarnessOpenCode: indexer,
				}),
				ingest.WithMetricsStore(metricsStore),
			)
			if err != nil {
				t.Fatalf("NewPipeline: %v", err)
			}
			result, err := pipeline.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.Summary.New == 0 {
				t.Fatalf("nothing was imported, so this case cannot say anything about the dispatch; summary=%+v", result.Summary)
			}

			assertDispatchEntryPoint(t, testCase, indexer)
			assertDispatchOutcome(t, testCase, result, metricsStore, sid)

			// Whichever path ran, the provider root has to have survived, and the
			// source path has to be the written copy rather than the provider's.
			for index, root := range indexer.seenRoots {
				if root != session.OriginalRoot {
					t.Errorf("call %d saw OriginalRoot %q, want %q; a directory source resolves its tree from this and a "+
						"root derived from the source path points into the output tree",
						index, root, session.OriginalRoot)
				}
			}
			for index, path := range indexer.seenPaths {
				if path == session.SourcePath {
					t.Errorf("call %d was pointed at the PROVIDER source path %q rather than the copy Peasant wrote; the "+
						"indexer must read what was stored", index, path)
				}
			}
		})
	}
}

// assertDispatchEntryPoint holds the pipeline to calling exactly the method the
// indexer's declared kind selects, and no other.
func assertDispatchEntryPoint(t *testing.T, testCase indexDispatchCase, indexer *recordingIndexer) {
	t.Helper()
	wantBytes, wantFile := 0, 0
	switch testCase.EntryPoint {
	case entryPointBytes:
		wantBytes = 1
	case entryPointFile:
		wantFile = 1
	}
	if indexer.bytesCalls != wantBytes {
		t.Errorf("IndexTranscriptBytes called %d time(s), want %d. The pipeline chose its entry point from what it "+
			"happened to have loaded rather than from the indexer's declared source kind (%s); an indexer handed an "+
			"argument it must discard cannot report that what it actually needs has gone missing.",
			indexer.bytesCalls, wantBytes, testCase.SourceKind)
	}
	if indexer.fileCalls != wantFile {
		t.Errorf("IndexTranscript called %d time(s), want %d (source kind %s)", indexer.fileCalls, wantFile, testCase.SourceKind)
	}
	if wantBytes > 0 && indexer.bytesNonNil != wantBytes {
		t.Errorf("a file source was called via the bytes path %d time(s) but received EMPTY bytes %d time(s); the "+
			"in-memory path exists to avoid a second read, so empty bytes make it pointless",
			indexer.bytesCalls, indexer.bytesCalls-indexer.bytesNonNil)
	}
}

// assertDispatchOutcome holds the RUN's record of the session to the corpus.
//
// The entry-point counts above prove which method was called; they cannot prove
// that a refusal was reported rather than swallowed. The refusal arm is the one
// that matters most here, because its predecessor stored an empty session and
// reported success, so this reads the index log the run actually produced.
func assertDispatchOutcome(
	t *testing.T,
	testCase indexDispatchCase,
	result *ingest.PipelineResult,
	metricsStore *testutil.StubMetricsStore,
	sid ingest.SessionID,
) {
	t.Helper()
	entries, listErr := metricsStore.ListEntries(context.Background(), sid)
	if listErr != nil {
		t.Fatalf("read the indexed entries back: %v", listErr)
	}
	var logged *ingest.IndexLogEntry
	for i := range result.IndexLog {
		if result.IndexLog[i].SessionID == sid {
			logged = &result.IndexLog[i]
		}
	}
	if logged == nil {
		t.Fatalf("the run recorded no index-log entry for %q, so nothing says what happened to it; index log: %+v",
			sid, result.IndexLog)
	}
	switch testCase.IndexOutcome {
	case outcomeIndexed:
		if logged.Outcome != ingest.IndexOutcomeIndexed {
			t.Errorf("the run recorded the outcome %q, want %q; error=%v reason=%v",
				logged.Outcome, ingest.IndexOutcomeIndexed, derefOrEmpty(logged.ErrorMessage), derefOrEmpty(logged.Reason))
		}
		if len(entries) == 0 {
			t.Errorf("no entries were stored for a case the corpus says indexes successfully")
		}
	case outcomeError:
		if logged.Outcome != ingest.IndexOutcomeError {
			t.Errorf("an indexer declaring no source kind produced the outcome %q, want %q. Anything else means the "+
				"dispatch guessed a source instead of refusing, which is how a session gets stored with zero entries "+
				"while the import reports success.", logged.Outcome, ingest.IndexOutcomeError)
		}
		message := derefOrEmpty(logged.ErrorMessage)
		if !strings.Contains(message, testCase.ErrorContains) {
			t.Errorf("the recorded refusal does not say %q, so a reader cannot tell what was wrong or how to fix it; got: %s",
				testCase.ErrorContains, message)
		}
		if len(entries) != 0 {
			t.Errorf("a refused dispatch still stored %d entries; the refusal is meant to stop indexing, not to annotate it",
				len(entries))
		}
	}
}

func derefOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
