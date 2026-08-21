package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/redaction_record.yaml
var redactionRecordFixtureData []byte

// Each loader guard gets its own rejection fixture, so the corpus proving the
// loader strict sits beside the corpus it protects and can explain itself.
var (
	//go:embed testdata/redaction_record-reject-agreeing-stored-level.yaml
	redactionRecordRejectAgreeingData []byte
	//go:embed testdata/redaction_record-reject-unofferable-applied-level.yaml
	redactionRecordRejectUnofferableData []byte
	//go:embed testdata/redaction_record-reject-uncovered-cell.yaml
	redactionRecordRejectUncoveredCellData []byte
	//go:embed testdata/redaction_record-reject-applied-without-a-level.yaml
	redactionRecordRejectAppliedWithoutLevelData []byte
	//go:embed testdata/redaction_record-reject-missing-metadata-mismatch.yaml
	redactionRecordRejectMissingCountData []byte
)

const redactionRecordFixturePath = "cmd/peasant/testdata/redaction_record.yaml"

type redactionRecordDocument struct {
	ExpectedCaseCount int                   `yaml:"expectedCaseCount"`
	Cases             []redactionRecordCase `yaml:"cases"`
}

type redactionRecordCase struct {
	Name                  string                `yaml:"name"`
	SessionCount          int                   `yaml:"sessionCount"`
	MetadataPresent       bool                  `yaml:"metadataPresent"`
	StoredApplied         bool                  `yaml:"storedApplied"`
	StoredLevel           string                `yaml:"storedLevel"`
	StoredRuleSetVersion  string                `yaml:"storedRuleSetVersion"`
	AppliedLevel          redact.RedactionLevel `yaml:"appliedLevel"`
	ExpectMissingMetadata int                   `yaml:"expectMissingMetadata"`
}

// loadRedactionRecordFixture decodes and fully validates the corpus.
//
// The guards beyond the usual strictness are all about one thing: a case can only
// prove the record describes the PUSH rather than the metadata FILE if the two
// disagree. A case where they agree passes under either implementation, so it is
// rejected rather than counted as coverage.
func loadRedactionRecordFixture(data []byte) (redactionRecordDocument, error) {
	var document redactionRecordDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, redactionRecordRuleError(
			"typed YAML fields must match the document schema; an unknown or mistyped key leaves the field it was meant to "+
				"set at its zero value, and every field here has a meaningful zero",
			"loader=first-document decode",
			fmt.Sprintf("fix=correct the key to one the typed schema declares: %v", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		detail := "found a second YAML document"
		if err != nil {
			detail = err.Error()
		}
		return document, redactionRecordRuleError(
			"exactly one YAML document is allowed; anything after the first is silently ignored, so cases below it prove nothing",
			"loader=end-of-document check",
			"fix=remove the second document so the next decode returns EOF: "+detail)
	}
	if len(document.Cases) == 0 || document.ExpectedCaseCount != len(document.Cases) {
		return document, redactionRecordRuleError(
			fmt.Sprintf("declared and actual case counts must match and be non-zero, got expectedCaseCount=%d cases=%d; a "+
				"corpus that silently shrinks still passes every assertion it still contains",
				document.ExpectedCaseCount, len(document.Cases)),
			"loader=case-count validation",
			"fix=set expectedCaseCount to the number of cases actually present")
	}
	seen := map[string]bool{}
	coveredCells := map[recordCase]bool{}
	for index, testCase := range document.Cases {
		if strings.TrimSpace(testCase.Name) == "" || seen[testCase.Name] {
			return document, redactionRecordCaseError(index,
				fmt.Sprintf("case name %q is missing or duplicated", testCase.Name),
				"fix=give every case a unique, behaviour-naming name so a failure names exactly one scenario")
		}
		seen[testCase.Name] = true
		if testCase.SessionCount <= 0 {
			return document, redactionRecordCaseError(index,
				fmt.Sprintf("sessionCount is %d", testCase.SessionCount),
				"fix=publish at least one session; the record is not printed for an empty push, so a zero-session case "+
					"pins output nothing produces")
		}
		// The record must describe a push that can actually happen. Pinning its
		// wording for a level every surface refuses would assert the format of
		// output no user will ever see.
		if !config.RedactionLevelOffered(testCase.AppliedLevel) {
			return document, redactionRecordCaseError(index,
				fmt.Sprintf("appliedLevel %q is not a level this version offers", testCase.AppliedLevel),
				fmt.Sprintf("fix=use one of %s; a push cannot run at a level every surface refuses, so the record for it "+
					"describes nothing", config.RedactionLevelMenu()))
		}
		if testCase.StoredApplied && strings.TrimSpace(testCase.StoredLevel) == "" {
			return document, redactionRecordCaseError(index,
				"storedApplied is true but storedLevel is blank",
				"fix=name the level the metadata file claims; an applied-but-level-less file is not a state the writer "+
					"produces, so the case would test an impossible input")
		}
		// THE LOAD-BEARING GUARD. If the stored level equals the applied level the
		// case passes whether the record is derived from the push policy or from the
		// metadata file, so it cannot detect the defect at all.
		if testCase.StoredLevel != "" && redact.RedactionLevel(testCase.StoredLevel) == testCase.AppliedLevel {
			return document, redactionRecordCaseError(index,
				fmt.Sprintf("storedLevel and appliedLevel are both %q", testCase.AppliedLevel),
				"fix=make them differ; a case where the metadata file already agrees with the push passes under either "+
					"implementation and proves nothing about which one the record reads")
		}
		// The missing count has to follow from the inputs, or the assertion could
		// pass against a record that counted something else entirely.
		wantMissing := 0
		if !testCase.MetadataPresent {
			wantMissing = testCase.SessionCount
		}
		if testCase.ExpectMissingMetadata != wantMissing {
			return document, redactionRecordCaseError(index,
				fmt.Sprintf("expectMissingMetadata is %d but metadataPresent=%v over %d session(s) implies %d",
					testCase.ExpectMissingMetadata, testCase.MetadataPresent, testCase.SessionCount, wantMissing),
				"fix=set it to the count the inputs imply; a free-floating number lets the assertion pass against a "+
					"record that counted something else")
		}
		coveredCells[recordCellOf(testCase)] = true
	}
	// The CROSS-PRODUCT of the three inputs the record's wording actually depends
	// on, walked from the cells rather than counted per axis.
	//
	// It is the ONLY corpus-level rule here, and that is a deletion rather than an
	// omission. Two others stood beside it - "some case stores a disagreeing level"
	// and "some case has unreadable metadata" - and both are strictly implied by
	// this one. MEASURED rather than reasoned: a corpus built to violate each of
	// them was written, and both were rejected by THIS rule naming the missing
	// cell, never by their own. Required cells include metadata-absent ones, which
	// forces the early-warning case; and they include stored-claim ones, which the
	// per-row rules force to carry a level that DISAGREES with the applied one. A
	// rule that cannot fire is not a guard, it is a comment that reads like one.
	//
	// The subsumption rests on exactly two per-row rules, and both now have
	// rejection fixtures of their own so it cannot quietly stop holding: a stored
	// claim must name a level (reject-applied-without-a-level), and that level must
	// DIFFER from the applied one (reject-agreeing-stored-level). An earlier
	// version of this comment claimed every per-row rule had a fixture, which was
	// not true and was more than the argument needs - the name-uniqueness and
	// positive-session-count rules have none, and the subsumption does not depend
	// on them.
	//
	// Per-axis coverage was not enough, and the deletion pass is what showed it:
	// two cases differed only in session count, so either could be removed with
	// the other still supplying every axis, and the corpus stayed green. The cell
	// each one occupies is read from its own fields, so a row cannot be relabelled
	// into covering for another.
	for _, cell := range requiredRecordCells() {
		if !coveredCells[cell] {
			return document, redactionRecordRuleError(
				fmt.Sprintf("no case has metadataPresent=%v with a %s session count and storedApplied=%v",
					cell.metadataPresent, cell.countShape, cell.storedApplied),
				"loader=input cross-product coverage",
				"fix=add one; these three decide what the record says - whether the early warning appears, whether the "+
					"count reads as one session or several, and whether there is a stored claim to be ignored - so a missing "+
					"combination is a shape of push the record was never asked to describe")
		}
	}
	return document, nil
}

// recordCase is one cell of the corpus's cross-product: the three inputs the
// record's wording depends on.
//
// storedApplied is meaningless when the metadata file is absent - there is no
// file to carry a claim - so the required cells below are constrained rather than
// a full 2x2x2, which would demand rows for inputs the writer cannot produce.
type recordCase struct {
	metadataPresent bool
	countShape      string
	storedApplied   bool
}

const (
	// countSingular is a push of exactly one session, where the record must read
	// as one session rather than guess at a plural.
	countSingular = "single"
	// countPlural is a push of several.
	countPlural = "plural"
)

func recordCellOf(testCase redactionRecordCase) recordCase {
	shape := countPlural
	if testCase.SessionCount == 1 {
		shape = countSingular
	}
	// With no metadata file there is no stored claim, whatever the row happens to
	// say, so the cell records the only value that input can meaningfully take.
	stored := testCase.StoredApplied && testCase.MetadataPresent
	return recordCase{metadataPresent: testCase.MetadataPresent, countShape: shape, storedApplied: stored}
}

// requiredRecordCells is every combination of inputs the record must describe.
func requiredRecordCells() []recordCase {
	var cells []recordCase
	for _, shape := range []string{countSingular, countPlural} {
		for _, storedApplied := range []bool{true, false} {
			cells = append(cells, recordCase{metadataPresent: true, countShape: shape, storedApplied: storedApplied})
		}
		cells = append(cells, recordCase{metadataPresent: false, countShape: shape, storedApplied: false})
	}
	return cells
}

func redactionRecordRuleError(what, where, fix string) error {
	return fmt.Errorf(
		"redaction record fixture rule failed: %s; a malformed corpus invalidates the only evidence that the "+
			"published-content record describes the protection actually applied rather than a field nothing sets; "+
			"where=%s %s; when=test fixture loading; impact=the record could go back to reporting zero redaction on every "+
			"push, inviting the conclusion that nothing is redacted; %s",
		what, redactionRecordFixturePath, where, fix)
}

func redactionRecordCaseError(index int, what, fix string) error {
	return redactionRecordRuleError(what, fmt.Sprintf("case index %d", index), fix)
}

// --- loader guards ----------------------------------------------------------

// TestLoadRedactionRecordFixture_RejectsACorpusThatLeavesACellEmpty is what ends
// the regress, and it ends it in a way a count could not.
//
// A size pin on the required set was tried first and defeated by the edit its own
// failure message instructed: drop a cell and decrement the count in one commit,
// which reviews as "I removed an axis and updated the count", and the door is open
// for the next commit to delete the row that cell protected. It also guarded the
// DATA rather than the CHECK - the loop consuming the set was independently
// deletable with the package green, including that test, which went on counting
// six cells nobody looked at.
//
// A rejection fixture is falsified by the ABSENCE of rejection. Delete the loop,
// shrink the required set, or empty it, and the corpus below stops being rejected
// and this goes red. It restates no number, so there is no constant for a later
// edit to decrement alongside it.
func TestLoadRedactionRecordFixture_RejectsACorpusThatLeavesACellEmpty(t *testing.T) {
	t.Parallel()
	_, err := loadRedactionRecordFixture(redactionRecordRejectUncoveredCellData)
	if err == nil || !strings.Contains(err.Error(), "no case has metadataPresent=true with a single session count and storedApplied=true") {
		t.Fatalf("error = %v, want rejection of a corpus that satisfies every per-axis and per-row rule while leaving one "+
			"cross-product cell empty", err)
	}
}

// TestLoadRedactionRecordFixture_RejectsAStoredClaimWithNoLevel covers the rule
// the subsumption argument rests on.
//
// The cross-product guard requires cells carrying a stored claim, and the two
// corpus-level rules deleted as subsumed were subsumed BECAUSE every such claim
// must name a level that disagrees with the applied one. A row claiming a
// redaction with a blank level would fill the cell while distinguishing nothing,
// so this rule is load-bearing for that argument and now has a fixture like the
// rules beside it.
func TestLoadRedactionRecordFixture_RejectsAStoredClaimWithNoLevel(t *testing.T) {
	t.Parallel()
	_, err := loadRedactionRecordFixture(redactionRecordRejectAppliedWithoutLevelData)
	if err == nil || !strings.Contains(err.Error(), "storedApplied is true but storedLevel is blank") {
		t.Fatalf("error = %v, want rejection of a case claiming a redaction with no level to name", err)
	}
}

func TestLoadRedactionRecordFixture_AcceptsCommittedCorpus(t *testing.T) {
	t.Parallel()
	document, err := loadRedactionRecordFixture(redactionRecordFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	if document.ExpectedCaseCount != len(document.Cases) {
		t.Fatalf("expectedCaseCount=%d but %d cases loaded", document.ExpectedCaseCount, len(document.Cases))
	}
}

func TestLoadRedactionRecordFixture_RejectsAStoredLevelThatAgreesWithTheAppliedOne(t *testing.T) {
	t.Parallel()
	_, err := loadRedactionRecordFixture(redactionRecordRejectAgreeingData)
	if err == nil || !strings.Contains(err.Error(), "storedLevel and appliedLevel are both") {
		t.Fatalf("error = %v, want rejection of a case that cannot distinguish the push policy from the metadata file", err)
	}
}

func TestLoadRedactionRecordFixture_RejectsAnUnofferableAppliedLevel(t *testing.T) {
	t.Parallel()
	_, err := loadRedactionRecordFixture(redactionRecordRejectUnofferableData)
	if err == nil || !strings.Contains(err.Error(), "is not a level this version offers") {
		t.Fatalf("error = %v, want rejection of a record pinned for a level no push can run at", err)
	}
}

func TestLoadRedactionRecordFixture_RejectsAMissingCountThatDoesNotFollow(t *testing.T) {
	t.Parallel()
	_, err := loadRedactionRecordFixture(redactionRecordRejectMissingCountData)
	if err == nil || !strings.Contains(err.Error(), "expectMissingMetadata is 2") {
		t.Fatalf("error = %v, want rejection of a missing-metadata count that does not follow from the inputs", err)
	}
}

// assertRedactionRecordEntry requires ONE labelled entry of the rendered record
// to carry every named part together.
//
// The record prints two entries whose bodies open identically:
//
//	metadata:            redacted at standard on upload, all 3 session(s)
//	transcript content:  redacted at standard on upload, matched patterns replaced
//
// so a whole-output strings.Contains for "redacted at standard on upload" is
// satisfied by EITHER, and the transcript-content entry's level was therefore
// unpinned by two assertions that both read as covering it. Splitting on the
// renderer's own separator makes the entry the unit: every part named here has
// to land in the same segment, which no sibling entry can supply.
//
// ITS ACCEPTANCE TEST, stated as the production edit that must turn it red:
// dropping record.Level from the transcript-content body in
// printRedactionReport, leaving the metadata entry untouched.
func assertRedactionRecordEntry(t *testing.T, rendered, label string, parts ...string) {
	t.Helper()
	required := append([]string{label}, parts...)
	for _, part := range required {
		// An empty needle is contained by every segment, so a part derived from a
		// production value that went blank would make this pass on any output at
		// all - the failure mode the assertion exists to prevent.
		if part == "" {
			t.Fatalf("the %q entry was asserted with an EMPTY part, which every segment trivially contains; the parts are "+
				"%q, so one of the production values they are built from renders as nothing", label, required)
		}
	}
	segments := strings.Split(rendered, redactionRecordEntrySeparator)
	if !slices.ContainsFunc(segments, func(segment string) bool {
		for _, part := range required {
			if !strings.Contains(segment, part) {
				return false
			}
		}
		return true
	}) {
		t.Errorf("no single entry of the published-content record carries %q together. Every part of one entry has to land "+
			"on that entry: the record's entries share wording, so a part found elsewhere in the output says nothing about "+
			"the entry it belongs to.\nrecord:\n%s", required, rendered)
	}
}

// --- the corpus -------------------------------------------------------------

// TestRedactionRecord_DescribesTheOutwardPushNotTheMetadataOnDisk is the guard for
// the record that always said "redacted: 0 of N".
//
// It drives the real production pair - buildRedactionRecord over a filesystem
// holding real metadata written through the real path helper, then the real
// printRedactionReport - and requires the rendered record to name the level and
// rule set the push applies. Every case's stored metadata contradicts that, so a
// record re-derived from the file produces a DIFFERENT level and the assertion
// fails on a value it can print, rather than on a missing substring.
func TestRedactionRecord_DescribesTheOutwardPushNotTheMetadataOnDisk(t *testing.T) {
	t.Parallel()
	document, err := loadRedactionRecordFixture(redactionRecordFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			const basePath = "/data/peasant-sync"
			fs := testutil.NewMemFS()
			sessions := make([]ingest.PushSessionRow, 0, testCase.SessionCount)
			for i := range testCase.SessionCount {
				row := ingest.PushSessionRow{
					SessionID:    fmt.Sprintf("session-%d", i),
					HostSlug:     testutil.TestHostSlug,
					ModelHarness: string(defaults.HarnessClaudeCode),
				}
				sessions = append(sessions, row)
				if !testCase.MetadataPresent {
					continue
				}
				// Written through the SAME path helper the push read seam uses, so a
				// change to either side cannot leave the record silently reading
				// nothing and reporting a clean zero.
				meta := schema.UnifiedMetadata{
					SessionID: schema.SessionID(row.SessionID),
					HostSlug:  schema.HostSlug(row.HostSlug),
					Redaction: schema.RedactionInfo{
						Applied:        testCase.StoredApplied,
						Level:          testCase.StoredLevel,
						RuleSetVersion: testCase.StoredRuleSetVersion,
					},
				}
				data, marshalErr := json.Marshal(meta)
				if marshalErr != nil {
					t.Fatalf("marshal metadata: %v", marshalErr)
				}
				path := ingest.SessionMetadataPath(basePath, row.HostSlug, row.SessionID, row.ParentID)
				if writeErr := fs.WriteFile(path, data, 0o600); writeErr != nil {
					t.Fatalf("write metadata at %s: %v", path, writeErr)
				}
			}

			record := buildRedactionRecord(sessions, basePath, fs, testCase.AppliedLevel)

			if record.SessionCount != testCase.SessionCount {
				t.Errorf("SessionCount = %d, want %d", record.SessionCount, testCase.SessionCount)
			}
			// The two equality assertions that carry the whole test. Deriving either
			// value from the metadata file makes them the stored values instead.
			if record.Level != testCase.AppliedLevel {
				t.Errorf("Level = %q, want %q (the level this push applies); stored metadata claimed %q, so this value "+
					"came from the file rather than from the push policy",
					record.Level, testCase.AppliedLevel, testCase.StoredLevel)
			}
			if record.RuleSetVersion != redact.RuleSetVersion {
				t.Errorf("RuleSetVersion = %q, want the compiled %q; stored metadata claimed %q, so this value came from "+
					"the file rather than from the rules the push will actually run",
					record.RuleSetVersion, redact.RuleSetVersion, testCase.StoredRuleSetVersion)
			}
			if record.MissingMetadataCount != testCase.ExpectMissingMetadata {
				t.Errorf("MissingMetadataCount = %d, want %d", record.MissingMetadataCount, testCase.ExpectMissingMetadata)
			}

			var out bytes.Buffer
			printRedactionReport(&out, record)
			rendered := out.String()
			for _, want := range []string{
				fmt.Sprintf("You are about to publish %d session(s).", testCase.SessionCount),
				"Redaction report:",
				// The hedge, shared verbatim with every refusal and the wizard.
				"known patterns",
				"not a guarantee",
			} {
				if !strings.Contains(rendered, want) {
					t.Errorf("the record must state %q; got:\n%s", want, rendered)
				}
			}
			// Each labelled entry is asserted as a UNIT. The metadata and
			// transcript-content bodies share the opening phrase "redacted at
			// <level> on upload", so a whole-output search for the applied level is
			// satisfied by either one - and the content entry's level was unpinned
			// that way while the flat list above read as covering it.
			assertRedactionRecordEntry(t, rendered, "metadata:",
				"redacted at "+testCase.AppliedLevel.String()+" on upload",
				// The SAME count as the header one line above. It was unasserted
				// while the identical number was pinned, so the two could disagree
				// in front of a user - which is exactly what the harvest warning did
				// when it counted log rows instead of sessions and printed
				// "2 session(s)" beneath a line saying 1. A count is worth pinning
				// wherever it is rendered, not once per screen.
				fmt.Sprintf("all %d session(s)", testCase.SessionCount))
			assertRedactionRecordEntry(t, rendered, "rule set version:", "v"+redact.RuleSetVersion)
			// The entry most easily dropped and most important to keep: a record
			// listing only metadata lets a reader infer content was untouched. What
			// it must SAY changed once - content used to be published as recorded
			// and is now redacted on the way out - so this pins the current truth
			// rather than the sentence that was here.
			assertRedactionRecordEntry(t, rendered, "transcript content:",
				"redacted at "+testCase.AppliedLevel.String()+" on upload",
				"matched patterns replaced")
			if testCase.ExpectMissingMetadata > 0 {
				assertRedactionRecordEntry(t, rendered, "note:",
					fmt.Sprintf("%d session(s) missing metadata", testCase.ExpectMissingMetadata))
			}
			// The stored level must not appear anywhere in the rendered record. This
			// needle is not empty, is not derived from production code, and is a
			// value the record WOULD print if it read the file - so it can fail.
			if testCase.StoredLevel != "" && strings.Contains(rendered, testCase.StoredLevel) {
				t.Errorf("the record names the stored level %q, which describes an earlier import rather than this push; got:\n%s",
					testCase.StoredLevel, rendered)
			}
			if testCase.StoredRuleSetVersion != "" && strings.Contains(rendered, testCase.StoredRuleSetVersion) {
				t.Errorf("the record names the stored rule set %q; the push applies %q whatever an older import recorded; got:\n%s",
					testCase.StoredRuleSetVersion, redact.RuleSetVersion, rendered)
			}
			if strings.ContainsRune(rendered, '—') {
				t.Errorf("the record must stay ASCII: it is printed to stderr from a Git hook; got:\n%s", rendered)
			}
		})
	}
}
