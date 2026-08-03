package push_test

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// publishedPartFloor is the number of byte-bearing parts a captured publish call
// must expose for the sweep below to mean anything.
//
// Measured on this tree: StubPublishCall carries three - the metadata JSON, the
// transcript body, and the filename. A reflection walk that silently found none
// would make every assertion over it pass, which is the shape of failure this
// whole test exists to stop.
const publishedPartFloor = 3

// publishedParts returns EVERY byte-bearing part of a captured publish call,
// discovered by reflection rather than by name.
//
// Naming the parts is what let this defect ship. A publish is multipart, the
// guard below read the part called TranscriptBody, and the identical transcript
// text also leaves in the metadata part - so the assertion was true, and true of
// half the wire. Walking the struct means a part added later is swept the day it
// is added, rather than the day someone remembers it exists.
func publishedParts(call testutil.StubPublishCall) map[string]string {
	parts := map[string]string{}
	value := reflect.ValueOf(call)
	for i := range value.NumField() {
		field := value.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		switch typed := value.Field(i).Interface().(type) {
		case []byte:
			parts[field.Name] = string(typed)
		case string:
			parts[field.Name] = typed
		}
	}
	return parts
}

// requireEveryStringFieldPopulated fails if any string-typed field of a wire
// struct is left empty by the fixture.
//
// This is the OTHER half of "assert the property over everything the consumer
// receives". Sweeping every part by reflection answers "did we read all the
// outputs?"; it cannot answer "did the fixture put anything in them?". The
// metadata part can be swept correctly while quality is absent entirely if the
// store double supplies no metrics, leaving sensitive content outside the
// population under test while the test appears exhaustive.
//
// Requiring every string field to be non-empty means a field added to the wire
// tomorrow fails here, by name, until someone decides what it should carry. That
// forces that decision at the moment of the change.
func requireEveryStringFieldPopulated(t *testing.T, what string, value any) {
	t.Helper()
	reflected := reflect.ValueOf(value)
	var empty []string
	for i := range reflected.NumField() {
		field := reflected.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		item := reflected.Field(i)
		switch {
		case item.Kind() == reflect.String:
			if item.Len() == 0 {
				empty = append(empty, field.Name)
			}
		case item.Kind() == reflect.Ptr && item.Type().Elem().Kind() == reflect.String:
			if item.IsNil() || item.Elem().Len() == 0 {
				empty = append(empty, field.Name)
			}
		}
	}
	if len(empty) > 0 {
		t.Fatalf("%s leaves %v empty in this fixture, so the redaction sweep below cannot say anything about %s. "+
			"Every string this type can put on the wire has to carry a value here - a planted secret if the field is free "+
			"text, a valid one if it is an identifier or an enum. A field nobody populates is a field nobody is testing, "+
			"so sensitive content could reach the wire while this test appeared exhaustive.", what, empty, what)
	}
}

// TestPipeline_PublishedBodyIsRedacted drives the REAL pipeline and reads EVERY
// part the publisher was handed.
//
// It exists because the previous guard was a source regex over the call site. It
// caught a literal `nil` argument and nothing else: assigning a nil-valued
// redactor variable and passing that republishes content unredacted with the
// regex satisfied, because the spelling changed and the behaviour did not. A
// guard on how a call is WRITTEN is not a guard on what it DOES.
//
// It also closes a gap that made every other pipeline test blind here: the shared
// constructor passes nil for the redactor, so no test in this package observed a
// pipeline-produced body being redacted at all.
//
// AND IT ONCE READ ONE PART OF TWO. A publish sends a metadata part beside the
// transcript part, and PublishRequest.Entries inside it carries the recorded
// text: contentPreview, toolInput, toolOutput. Only the transcript part was
// redacted, so this test passed while a planted key left the machine verbatim in
// the part next to the one it read. Nine tests unmarshalled that metadata part
// and three reasoned about entries - none of them named the free-text fields, so
// the part looked covered. The parts are now swept by reflection and each
// free-text field carries its OWN planted secret, so a failure names the field
// that leaked rather than reporting that something somewhere did.
func TestPipeline_PublishedBodyIsRedacted(t *testing.T) {
	ctx := context.Background()
	fs := testutil.NewMemFS()
	const hostSlug = testutil.TestHostSlug
	sessionID := testutil.TestSessionUUID
	seedMemFS(t, fs, hostSlug, sessionID, defaults.HarnessClaudeCode)

	// One planted secret PER free-text field the wire can carry, so a failure says
	// which field leaked rather than that something did.
	//
	// The FIXTURE POPULATION is the axis this test was blind on. Sweeping every
	// part by reflection cannot see a field no fixture puts on the wire, and
	// quality.titleGenerated - the first 80 characters of the user's first
	// message - rode the metadata part verbatim while this test's store supplied
	// no metrics at all. Every string field on both store-sourced types is
	// populated below, and requireEveryStringFieldPopulated fails if a field is
	// added and left empty, so the next one cannot repeat it.
	const secret = "sk-ant-api03-EXAMPLEKEY0000000000000"
	const toolInputSecret = "sk-ant-api03-TOOLINPUTKEY00000000000"
	const toolOutputSecret = "sk-ant-api03-TOOLOUTPUTKEY0000000000"
	const toolNamesSecret = "sk-ant-api03-TOOLNAMESKEY00000000000"
	const extraSecret = "sk-ant-api03-EXTRAFIELDKEY0000000000"
	const partTypeSecret = "sk-ant-api03-PARTTYPEKEY00000000000x"
	const titleSecret = "sk-ant-api03-TITLEKEY0000000000000xx"
	const scopeSecret = "sk-ant-api03-SCOPEKEY0000000000000xx"
	const costModelSecret = "sk-ant-api03-COSTMODELKEY00000000000"
	const modelSecret = "sk-ant-api03-METAMODELKEY00000000000"
	// A planted secret in a META-SOURCED field, which is a third population
	// beside the two store-sourced types below.
	//
	// redact.RedactMetadata walks a HAND-ENUMERATED list of fields, and Model is
	// not on it. So a meta-sourced field the list misses is covered ONLY by the
	// whole-document redaction of the two parts - and the transcript seam's guard
	// was a corpus regex over the literal `nil` at its call site, which a nil
	// arriving through a variable does not match. That is a spelling check, and
	// this file's own header names spelling checks as the reason it exists.
	//
	// requireEveryStringFieldPopulated covers the two STORE-sourced types; the
	// meta-sourced surface has no population guard, so this plant stands in for
	// it deliberately rather than by accident.
	requireMetaFieldOutsideTheHandList(t, modelSecret)
	plantMetaSecret(t, fs, hostSlug, sessionID, modelSecret)
	// A fenced block with text before it: the masking pattern needs a preceding
	// newline, so a fence at byte 0 is unreachable by it and a fixture starting
	// there cannot observe masking at all.
	content := "Here is the fix:\n\n```go\nkey := \"" + secret + "\"\n```\n\nDoes that look right?"
	toolInput := `{"command":"deploy --token=` + toolInputSecret + `"}`
	toolOutput := `{"stdout":"authenticated with ` + toolOutputSecret + `"}`
	toolNames := "Read,Bash," + toolNamesSecret
	extra := `{"providerNote":"` + extraSecret + `"}`
	partType := partTypeSecret
	toolCallID := "tc-001"
	entryID := "entry-001"
	parentEntryID := "entry-000"
	toolKind := schema.ToolCallKindExecute
	stopReason := schema.StopReasonEndTurn
	title := "deploy with " + titleSecret + " please"
	scope := scopeSecret
	costModel := costModelSecret
	outcome := schema.OutcomeResolved
	entry := schema.SessionEntry{
		SessionID:  schema.SessionID(sessionID),
		EntryIndex: 1, Depth: 0, Role: schema.RoleAssistant, ContentPreview: &content,
		Harness:       schema.Harness(defaults.HarnessClaudeCode),
		EntryType:     schema.EntryTypeText,
		ToolInput:     &toolInput,
		ToolOutput:    &toolOutput,
		ToolNamesCSV:  &toolNames,
		Extra:         &extra,
		PartType:      &partType,
		ToolCallID:    &toolCallID,
		EntryID:       &entryID,
		ParentEntryID: &parentEntryID,
		ToolKind:      &toolKind,
		StopReason:    &stopReason,
	}
	// The quality metrics the store returns. titleGenerated is a verbatim copy of
	// the first user message; scope and costModelId are free text beside it.
	//
	// SUPPLIED HERE INDEPENDENTLY OF THE CONCRETE STORE QUERY, and that is the
	// point rather than an accident. The fixture keeps the redaction/title path
	// non-vacuous even if GetQualityMetrics changes how persisted quality data is
	// loaded.
	metrics := &schema.QualityMetrics{
		TitleGenerated: &title,
		Scope:          &scope,
		CostModelID:    &costModel,
		Outcome:        &outcome,
	}
	requireEveryStringFieldPopulated(t, "schema.SessionEntry", entry)
	requireEveryStringFieldPopulated(t, "schema.QualityMetrics", *metrics)
	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{makeSession(sessionID, hostSlug, string(defaults.HarnessClaudeCode), nil)},
		Entries:  map[ingest.SessionID][]schema.SessionEntry{ingest.SessionID(sessionID): {entry}},
		Metrics:  map[ingest.SessionID]*schema.QualityMetrics{ingest.SessionID(sessionID): metrics},
	}
	pub := &testutil.StubPublisher{StatusCode: 201}

	redactor, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
	if err != nil {
		t.Fatalf("build the redactor a production push builds: %v", err)
	}
	var stderr bytes.Buffer
	pipeline := push.NewPipeline(store, pub, baseCreds(), baseTestConfig(), fs,
		push.PipelineConfig{}, redactor, &stderr)

	result, runErr := pipeline.Run(ctx)
	if runErr != nil {
		t.Fatalf("Run: %v (stderr: %s)", runErr, stderr.String())
	}
	if len(pub.Calls) == 0 {
		t.Fatalf("nothing was published, so this test cannot say anything about the published body.\nresult: %+v\nstderr: %s",
			result.Sessions, stderr.String())
	}

	parts := publishedParts(pub.Calls[0])
	if len(parts) < publishedPartFloor {
		t.Fatalf("only %d byte-bearing part(s) were found on the captured publish call, below the floor of %d. The sweep "+
			"below runs over whatever this finds, so finding nothing would make it pass over an unredacted wire.",
			len(parts), publishedPartFloor)
	}
	body := string(pub.Calls[0].TranscriptBody)
	if len(body) == 0 {
		t.Fatal("the publisher was handed an empty transcript body")
	}
	// THE PROPERTY: no planted secret reaches ANY part, asserted on the bytes the
	// publisher received rather than on a function's return, and over every part
	// rather than the one this test used to name.
	for name, part := range parts {
		for label, planted := range map[string]string{
			"contentPreview":         secret,
			"toolInput":              toolInputSecret,
			"toolOutput":             toolOutputSecret,
			"toolNamesCsv":           toolNamesSecret,
			"extra":                  extraSecret,
			"partType":               partTypeSecret,
			"quality.titleGenerated": titleSecret,
			"quality.scope":          scopeSecret,
			"quality.costModelId":    costModelSecret,
			"metadata model":         modelSecret,
		} {
			if strings.Contains(part, planted) {
				t.Errorf("the %s part of the publish carries the %s secret VERBATIM.\n"+
					"A publish is multipart and the transcript text leaves in more than one of them: the metadata part "+
					"carries it through PublishRequest.Entries. Redacting only the part the builder produces leaves the "+
					"other one as recorded, which is what shipped.\n%s part:\n%s", name, label, name, part)
			}
		}
	}
	// And the code block survives, because below Maximum the rules run over code
	// rather than the block being masked away unscanned.
	if strings.Contains(body, "CODE_BLOCK") {
		t.Errorf("the published body has its code block masked wholesale. That removes the artifact and, worse, runs "+
			"before the rules - so the secret inside was never scanned.\nbody:\n%s", body)
	}
}

// stubJSONRedactor is a redactor whose JSON pass returns a shape of the test's
// choosing, so the failure branches of the redaction seams can be reached.
//
// DefaultRedactor cannot produce them - it always returns a document of the same
// shape - which is why those branches had never been exercised. The declared
// interface admits it, so a test can.
type stubJSONRedactor struct {
	json any
	// breaks selects the ONE seam this stub sabotages. Everything else is handed
	// to the real redactor.
	breaks func(any) bool
	// real supplies every part of the interface this stub is not about, so the
	// pipeline behaves normally everywhere except the seam under test.
	real ingest.TextRedactor
}

// RedactJSON breaks exactly ONE seam, selected by marker, and passes everything
// else to the real redactor.
//
// TWO REASONS IT HAS TO BE THIS SELECTIVE. Breaking every seam at once made an
// earlier version of this test pass for the WRONG REASON: the metadata document
// redaction failed too, schema validation rejected the result, and the push
// stopped there while step 3b's branch was never reached. And breaking only the
// entries seam - which runs FIRST, before MapMetadata and marshalTranscriptContent
// - shadows the two seams downstream of it: the session ends at the first
// refusal, so the later two are never exercised and a fail-open in either would
// be invisible while this test read as covering "the" fail-closed property.
//
// The seams are distinguished by what they are handed: the entries seam gets a
// JSON array, and the two document seams get objects carrying different keys.
// Anything not selected goes to the real redactor, so the run reaches the seam
// under test in its normal state.
func (s stubJSONRedactor) RedactJSON(value any) any {
	if s.breaks(value) {
		return s.json
	}
	return s.real.RedactJSON(value)
}

// seamEntries, seamMetadata and seamTranscript select one outward seam each.
//
// The entries seam is the only one handed an array. The two document seams are
// both objects and are told apart by a key only one of them carries: the publish
// request has "identity", and the transcript envelope has "sessionDetail".
var (
	seamEntries = func(value any) bool {
		_, isArray := value.([]any)
		return isArray
	}
	seamMetadata = func(value any) bool {
		document, isObject := value.(map[string]any)
		if !isObject {
			return false
		}
		_, isPublishRequest := document["identity"]
		return isPublishRequest
	}
	seamTranscript = func(value any) bool {
		document, isObject := value.(map[string]any)
		if !isObject {
			return false
		}
		_, isTranscript := document["sessionDetail"]
		return isTranscript
	}
)

func (s stubJSONRedactor) RedactMetadata(meta *ingest.UnifiedMetadata) *ingest.UnifiedMetadata {
	return s.real.RedactMetadata(meta)
}
func (s stubJSONRedactor) Level() string          { return s.real.Level() }
func (s stubJSONRedactor) RuleSetVersion() string { return s.real.RuleSetVersion() }

// TestPipeline_RedactionFailureStopsTheSessionInsteadOfPublishing pins the
// fail-closed property that step 3b and RedactEntries both state in capitals as
// the entire reason they sit where they do.
//
// It was stated and unguarded. Rewriting step 3b in the graceful-degradation
// style of the two reads directly above it - slog.Warn and keep going, which is
// the house style and therefore the likeliest future edit - publishes the
// recorded text with the whole module green.
//
// Both halves are asserted because they are different promises: NOTHING is
// published (the leak), and the session is reported as an error (the user is
// told). A version that swallowed the failure and skipped the session silently
// would satisfy the first and fail the user on the second.
//
// WHAT ACTUALLY FIRES under that mutation, stated so the guard is not read as
// proving more than it does: the count assertion, not the secret assertion. Once
// the metadata part began redacting itself as a document, the entries seam
// failing open no longer LEAKS - the document pass behind it still redacts the
// text. So the property this pins is "a redaction that failed must stop the
// session", independent of whether some later net happens to catch the bytes.
// That is the property worth pinning: relying on the net is how a seam that
// stopped working goes unnoticed.
func TestPipeline_RedactionFailureStopsTheSessionInsteadOfPublishing(t *testing.T) {
	for _, seam := range outwardSeams {
		t.Run(seam.name, func(t *testing.T) {
			ctx := context.Background()
			fs := testutil.NewMemFS()
			const hostSlug = testutil.TestHostSlug
			sessionID := testutil.TestSessionUUID
			seedMemFS(t, fs, hostSlug, sessionID, defaults.HarnessClaudeCode)

			const secret = "sk-ant-api03-FAILCLOSEDKEY0000000000"
			content := "here is the key " + secret
			store := &testutil.StubPushStore{
				Sessions: []ingest.PushSessionRow{makeSession(sessionID, hostSlug, string(defaults.HarnessClaudeCode), nil)},
				Entries: map[ingest.SessionID][]schema.SessionEntry{
					ingest.SessionID(sessionID): {{
						SessionID:  schema.SessionID(sessionID),
						EntryIndex: 1, Depth: 0, Role: schema.RoleAssistant, ContentPreview: &content,
						Harness:   schema.Harness(defaults.HarnessClaudeCode),
						EntryType: schema.EntryTypeText,
					}},
				},
			}
			pub := &testutil.StubPublisher{StatusCode: 201}
			var stderr bytes.Buffer
			// Valid JSON, but not an entry list: the redaction round trip completes and
			// its result cannot be read back, which is the failure RedactEntries exists
			// to convert into a refusal.
			realRedactor, redactorErr := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
			if redactorErr != nil {
				t.Fatal(redactorErr)
			}
			broken := stubJSONRedactor{json: map[string]any{"not": "entries"}, breaks: seam.selects, real: realRedactor}
			pipeline := push.NewPipeline(store, pub, baseCreds(), baseTestConfig(), fs,
				push.PipelineConfig{}, broken, &stderr)

			result, runErr := pipeline.Run(ctx)
			if runErr != nil {
				t.Logf("Run returned %v (a run-level error is an acceptable way to refuse)", runErr)
			}
			for _, call := range pub.Calls {
				for name, part := range publishedParts(call) {
					if strings.Contains(part, secret) {
						t.Errorf("FAIL-OPEN: redaction failed and the session was published anyway, carrying the secret VERBATIM "+
							"in the %s part. The two reads above step 3b degrade gracefully because losing metrics or entries "+
							"only costs completeness; losing the REDACTION costs the user their content.\n%s:\n%s",
							name, name, part)
					}
				}
			}
			if len(pub.Calls) != 0 {
				t.Errorf("%d session(s) were published after redaction failed. Nothing may reach the publisher when the redaction "+
					"that protects it could not be completed.", len(pub.Calls))
			}
			// And the user has to be told, or a silent skip reads as a successful push
			// that simply had nothing to send.
			reported := false
			for _, session := range result.Sessions {
				if session.Status == push.PushStatusError {
					reported = true
				}
			}
			if !reported && runErr == nil {
				t.Errorf("redaction failed, nothing was published, and no session was reported as an error - so the run looks "+
					"like a clean push with nothing to do. The refusal has to be visible.\nsessions: %+v\nstderr: %s",
					result.Sessions, stderr.String())
			}
		})
	}
}

// plantMetaSecret rewrites the seeded metadata.json so a meta-sourced field the
// redactor's hand-enumerated list does NOT cover carries a planted secret.
//
// Model is that field: redact.RedactMetadata names Project.FilePath,
// Project.Name, HostSlug, CWD and the git context, and stops. Whatever it misses
// is covered only by the whole-document redaction of the two published parts, so
// a plant here is what tells those two seams apart from the hand-list.
func plantMetaSecret(t *testing.T, fs *testutil.MemFS, hostSlug, sessionID, secret string) {
	t.Helper()
	path := filepath.Join("/sync", hostSlug, sessionID, sessionID+"--metadata.json")
	raw, err := fs.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded metadata: %v", err)
	}
	var meta ingest.UnifiedMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("parse seeded metadata: %v", err)
	}
	meta.Model = ingest.ModelID(string(meta.Model) + "-" + secret)
	rewritten, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal planted metadata: %v", err)
	}
	if err := fs.WriteFile(path, rewritten, 0o600); err != nil {
		t.Fatalf("write planted metadata: %v", err)
	}
}

// TestPipeline_ARedactionThatCannotBeEncodedStopsTheSession pins the OTHER
// failure branch of the shared round trip: the re-marshal.
//
// It is the branch that makes redactJSONDocument worth having. redact's own
// RedactJSONDocBytes returns its INPUT UNCHANGED when the re-marshal fails, with
// no error, so a caller receives well-formed bytes whether the redaction ran or
// silently did nothing — and the bytes it receives are the unredacted document.
// Reverting the seams to that primitive was measured green; this is what makes
// it red.
//
// The branch is unreachable with DefaultRedactor, which returns only strings,
// numbers, maps and slices — all of which marshal. The declared interface admits
// a value that does not, so a test can reach what production cannot, which is
// the whole reason the fail-closed wrapper is not dead weight.
func TestPipeline_ARedactionThatCannotBeEncodedStopsTheSession(t *testing.T) {
	for _, seam := range outwardSeams {
		t.Run(seam.name, func(t *testing.T) {
			ctx := context.Background()
			fs := testutil.NewMemFS()
			const hostSlug = testutil.TestHostSlug
			sessionID := testutil.TestSessionUUID
			seedMemFS(t, fs, hostSlug, sessionID, defaults.HarnessClaudeCode)

			const secret = "sk-ant-api03-UNENCODABLEKEY000000000"
			content := "here is the key " + secret
			store := &testutil.StubPushStore{
				Sessions: []ingest.PushSessionRow{makeSession(sessionID, hostSlug, string(defaults.HarnessClaudeCode), nil)},
				Entries: map[ingest.SessionID][]schema.SessionEntry{
					ingest.SessionID(sessionID): {{
						SessionID:  schema.SessionID(sessionID),
						EntryIndex: 1, Depth: 0, Role: schema.RoleAssistant, ContentPreview: &content,
						Harness:   schema.Harness(defaults.HarnessClaudeCode),
						EntryType: schema.EntryTypeText,
					}},
				},
			}
			pub := &testutil.StubPublisher{StatusCode: 201}
			realRedactor, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			// NaN has no JSON representation, so encoding the redacted document fails.
			// That is precisely the case the underlying primitive swallows.
			unencodable := stubJSONRedactor{json: []any{map[string]any{"x": math.NaN()}}, breaks: seam.selects, real: realRedactor}
			pipeline := push.NewPipeline(store, pub, baseCreds(), baseTestConfig(), fs,
				push.PipelineConfig{}, unencodable, &stderr)

			if _, runErr := pipeline.Run(ctx); runErr != nil {
				t.Logf("Run returned %v (a run-level error is an acceptable way to refuse)", runErr)
			}
			for _, call := range pub.Calls {
				for name, part := range publishedParts(call) {
					if strings.Contains(part, secret) {
						t.Errorf("FAIL-OPEN: the redaction could not be encoded and the session was published anyway, carrying "+
							"the secret VERBATIM in the %s part. A redaction that silently did nothing must not be "+
							"indistinguishable from one that worked.\n%s:\n%s", name, name, part)
					}
				}
			}
			if len(pub.Calls) != 0 {
				t.Errorf("%d session(s) were published after the redaction failed to encode. The primitive returns the "+
					"UNREDACTED document in this case, so publishing means publishing exactly what redaction was meant to "+
					"remove.", len(pub.Calls))
			}
		})
	}
}

// requireMetaFieldOutsideTheHandList asserts the PRECONDITION that makes the
// planted model value a probe of the transcript seam at all.
//
// The seam is caught through exactly one planted value, and that value only
// works because redact.RedactMetadata's hand-enumerated field list omits Model.
// Nothing asserted that. Adding Model to the redactor's list is the obvious
// hardening — it is the root the last several findings share — and doing it
// would silently retire this guard: the value would be redacted before the seam
// ever saw it, the sweep would pass, and a nil at the seam would go back to being
// invisible. A guard that its own most likely improvement disables is worse than
// no guard, because it reads as coverage.
//
// So this fails LOUDLY on that day, naming what to do, instead of going quiet.
func requireMetaFieldOutsideTheHandList(t *testing.T, secret string) {
	t.Helper()
	redactor, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
	if err != nil {
		t.Fatalf("build the redactor the push path builds: %v", err)
	}
	before := ingest.NewUnifiedMetadata()
	before.Model = ingest.ModelID(string(testutil.TestModel) + "-" + secret)
	after := redactor.RedactMetadata(&before)
	if before.Model != after.Model {
		t.Fatalf("redact.RedactMetadata now rewrites Model, so the planted value below is redacted BEFORE the transcript "+
			"seam sees it and can no longer detect a missing redactor there.\n\n"+
			"That is an improvement to the redactor and a silent retirement of this guard. Pick another metadata field "+
			"the hand-enumerated list does not cover, plant in that instead, and update this precondition - or, better, "+
			"if the list now covers everything, say so here and replace the plant with an assertion of that.\n\n"+
			"got %q -> %q", before.Model, after.Model)
	}
}

// outwardSeams is every seam that redacts something on its way to the wire.
//
// The fail-closed tests below run once PER SEAM. They used to run once, breaking
// the entries seam — which is the FIRST of the three — so the session ended at
// that refusal and the two document seams downstream were never reached. A
// fail-open in either would have been invisible under a test whose name claimed
// the property generally.
var outwardSeams = []struct {
	name    string
	selects func(any) bool
}{
	{"entries at the point they are read", seamEntries},
	{"the assembled metadata document", seamMetadata},
	{"the transcript content document", seamTranscript},
}
