package push

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/forbidden_source_text.yaml
var forbiddenSourceTextFixtureData []byte

const forbiddenSourceTextFixturePath = "internal/push/testdata/forbidden_source_text.yaml"

// expectedForbiddenSourceTextCount anchors the corpus's size OUTSIDE the file
// that holds it.
//
// A forbid-list cannot be anchored the way a case corpus is. There is no closed
// set behind it - it is an open ledger of sentences that were in this repository
// and cost something - and the text it names is absent, so removing a row breaks
// nothing and fails nothing. Measured BEFORE this constant existed: any row
// could be deleted, with the declared count in the same file decremented
// alongside it, and the suite stayed green. Deliberately stated without a row
// number - the corpus has grown twice since, and each time the prose describing
// it did not.
//
// Re-measured WITH it: deleting any row is caught, because the second copy of
// the count does not move with the fixture. That does not make removal hard. It
// makes it VISIBLE: retiring a guard becomes an edit to a Go constant in a
// second file, rather than an edit to a fixture that reviews as a data change.
const expectedForbiddenSourceTextCount = 5

// requiredForbiddenClaims pins WHAT each guard forbids and WHERE, outside the
// fixture that carries them.
//
// It holds needle-and-PACKAGE pairs, not rows: a row scoped to three packages
// contributes three pairs here, so its length and the row count above are
// unrelated numbers and a reader should not try to reconcile them. The two
// anchors measure different things on purpose - this one that a guard still
// reaches every directory it was written for, the count above that no row left
// silently.
//
// The count alone was not enough, and the gap is not theoretical - both of these
// were measured green:
//
//   - NEUTER a needle: change `phrase` and its own `matchesSample` together, so
//     the loader's fire-proof still passes and the declared count still matches
//     the rows. The guard is retired as completely as by deleting it.
//   - NARROW a row's scope: delete `- internal/ingest` from claim 1's packages.
//     The package stays declared and stays scanned, because another claim is
//     still scoped to it, so the package-reach check does not fire either. That
//     leaves "redaction-safe by construction" forbidden in internal/push and
//     permitted in internal/ingest - which is verbatim the state the original
//     finding reported, reopened by a one-line data edit.
//
// Both were re-measured against this list and are now caught: neutering a needle
// fails the pair lookup, and narrowing a row's packages fails it as well.
//
// This corpus is the SOLE enforcement for two properties nothing else can assert:
// that neither sync door names a redaction level again (the API tests delegate to
// it by name, being unable to see it themselves), and that the redaction-safety
// claim stays out of the package the belief started in. Neither survives its
// guard being edited away in silence.
var requiredForbiddenClaims = []struct{ needle, pkg string }{
	{"redaction-safe by construction", "internal/ingest"},
	{"redaction-safe by construction", "internal/push"},
	{"redaction-safe by construction", "internal/api"},
	{"already-redacted", "internal/ingest"},
	{`\w*level\s*:?=\s*redact\.(Minimal|Standard|Maximum)\b`, "internal/api"},
	// The two newest rows were left out of this list when they were added, so
	// each could be narrowed or neutered while only the count anchor watched -
	// and the count is exactly what a narrowing does not change. The nil-redactor
	// shape is the wiring a user asked to be fixed, and the code-block sentence
	// is scoped to docs precisely because the docs copy is the one that lives
	// longest.
	{`marshalTranscriptContent\([^)]*,\s*nil\)`, "internal/push"},
	{"removes whole code blocks", "docs"},
	{"removes whole code blocks", "internal/push"},
}

type forbiddenSourceTextDocument struct {
	ExpectedPackageCount int                   `yaml:"expectedPackageCount"`
	Packages             []string              `yaml:"packages"`
	ExpectedClaimCount   int                   `yaml:"expectedClaimCount"`
	Claims               []forbiddenSourceText `yaml:"claims"`
}

// forbiddenSourceText is one sentence or shape that must not appear, together
// with the evidence that its needle can actually fire.
type forbiddenSourceText struct {
	Name string `yaml:"name"`
	// Phrase is literal text. Exactly one of Phrase and Pattern is set.
	Phrase string `yaml:"phrase,omitempty"`
	// Pattern is a regular expression, for a shape rather than a sentence.
	Pattern string `yaml:"pattern,omitempty"`
	// Packages are the repository-relative directories this row applies to.
	Packages []string `yaml:"packages"`
	// MatchesSample is text the needle MUST match. It is what stops a needle that
	// can never fire from reading as a permanent pass.
	MatchesSample string `yaml:"matchesSample"`
	// Why is what this text cost, printed when the row fires so the reader meets
	// the reason rather than only the rule.
	Why string `yaml:"why"`
	// compiled is the prepared Pattern, or nil for a Phrase row.
	compiled *regexp.Regexp
	// normalisedPhrase is the prepared Phrase, or "" for a Pattern row.
	normalisedPhrase string
}

// needle is the row's own text, for a failure message.
func (c forbiddenSourceText) needle() string {
	if c.Pattern != "" {
		return c.Pattern
	}
	return c.Phrase
}

// matches reports whether already-normalised source text carries this row.
func (c forbiddenSourceText) matches(normalised string) bool {
	if c.compiled != nil {
		return c.compiled.MatchString(normalised)
	}
	return strings.Contains(normalised, c.normalisedPhrase)
}

// scannedFile reports whether a file's text is subject to the forbidden claims.
//
// Non-test Go, and MARKDOWN. Markdown was out of scope and silently so: planting
// a forbidden phrase in docs/NETWORK.md was a GAP while the identical phrase in a
// Go file was caught, because the scan filtered on a .go extension. Documentation
// is where a claim is stated at its most quotable and lives longest - the
// maintainer-facing doc had told the next reader that the old claim was "the
// claim to get right" - so a guard that reads only code protects the copy nobody
// reads and misses the copy everybody does.
func scannedFile(name string) bool {
	if strings.HasSuffix(name, "_test.go") {
		return false
	}
	switch filepath.Ext(name) {
	case ".go", ".md":
		return true
	}
	return false
}

// normaliseSourceText renders source text in the one form every rendering of a
// sentence collapses to.
//
// Comment markers go, hyphens become spaces, runs of whitespace become one space,
// and the result is lower-cased. Each of those is an evasion that costs the author
// nothing: gofmt wrapping a comment across two lines already made one copy of the
// redaction-safety claim invisible to a contiguous needle, and re-wrapping,
// re-hyphenating or re-capitalising a sentence are the same class of accident.
func normaliseSourceText(source string) string {
	replaced := strings.NewReplacer("//", " ", "/*", " ", "*/", " ", "-", " ").Replace(source)
	return strings.ToLower(strings.Join(strings.Fields(replaced), " "))
}

// wrappedRenderings returns the sample as it would read if a comment line broke at
// each of its spaces in turn.
//
// This is what proves the normaliser does its job, rather than the corpus merely
// asserting that it does. The failure it descends from is precise: the needle was
// contiguous, the claim in the file had been wrapped, and the guard read green
// over the exact file it named.
func wrappedRenderings(sample string) []string {
	var out []string
	for index, character := range sample {
		if character != ' ' {
			continue
		}
		out = append(out, sample[:index]+"\n\t// "+sample[index+1:])
	}
	return out
}

// loadForbiddenSourceTextFixture decodes, validates, and PREPARES the corpus.
func loadForbiddenSourceTextFixture(data []byte) (forbiddenSourceTextDocument, error) {
	var document forbiddenSourceTextDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, forbiddenSourceTextRuleError(
			"typed YAML fields must match the document schema",
			"loader=first-document decode",
			fmt.Sprintf("fix=remove unknown fields and match the typed schema: %v", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found another YAML document")
		}
		return document, forbiddenSourceTextRuleError(
			"exactly one YAML document is allowed; rows below a second one prove nothing",
			"loader=end-of-document check",
			fmt.Sprintf("fix=remove the second document so the next decode returns EOF: %v", err))
	}
	if len(document.Packages) == 0 || document.ExpectedPackageCount != len(document.Packages) {
		return document, forbiddenSourceTextRuleError(
			fmt.Sprintf("declared and actual package counts must match and be non-zero, got expectedPackageCount=%d packages=%d",
				document.ExpectedPackageCount, len(document.Packages)),
			"loader=package-count validation",
			"fix=set expectedPackageCount to the number of packages present; a package silently dropped from this list is a "+
				"package the guard stops reading, which is how the claim survived at its origin")
	}
	if len(document.Claims) == 0 || document.ExpectedClaimCount != len(document.Claims) {
		return document, forbiddenSourceTextRuleError(
			fmt.Sprintf("declared and actual claim counts must match and be non-zero, got expectedClaimCount=%d claims=%d",
				document.ExpectedClaimCount, len(document.Claims)),
			"loader=claim-count validation",
			"fix=set expectedClaimCount to the number of claims present")
	}
	seen := map[string]bool{}
	for index, claim := range document.Claims {
		where := fmt.Sprintf("loader=claim index %d", index)
		if strings.TrimSpace(claim.Name) == "" || seen[claim.Name] {
			return document, forbiddenSourceTextRuleError(
				fmt.Sprintf("claim name %q is missing or duplicated", claim.Name), where,
				"fix=give every claim a unique name saying what must stay true")
		}
		seen[claim.Name] = true
		if (claim.Phrase == "") == (claim.Pattern == "") {
			return document, forbiddenSourceTextRuleError(
				fmt.Sprintf("claim %q must set exactly one of phrase and pattern", claim.Name), where,
				"fix=use phrase for a sentence and pattern for a shape; setting neither leaves an EMPTY needle, which every "+
					"file trivially fails to contain, and setting both hides which one is doing the work")
		}
		if strings.TrimSpace(claim.Why) == "" {
			return document, forbiddenSourceTextRuleError(
				fmt.Sprintf("claim %q says nothing about what it cost", claim.Name), where,
				"fix=write why; a rule whose reason is not recorded is deleted by the next person who finds it inconvenient")
		}
		if len(claim.Packages) == 0 {
			return document, forbiddenSourceTextRuleError(
				fmt.Sprintf("claim %q names no packages, so nothing is scanned for it", claim.Name), where,
				"fix=name at least one declared package")
		}
		for _, pkg := range claim.Packages {
			if !slices.Contains(document.Packages, pkg) {
				return document, forbiddenSourceTextRuleError(
					fmt.Sprintf("claim %q names the package %q, which the corpus does not declare", claim.Name, pkg), where,
					"fix=add it to packages, or correct the name; a row scoped to a directory nothing reads never fires")
			}
		}
		if strings.TrimSpace(claim.MatchesSample) == "" {
			return document, forbiddenSourceTextRuleError(
				fmt.Sprintf("claim %q carries no sample, so nothing shows its needle can fire", claim.Name), where,
				"fix=add matchesSample with text the needle must match; a forbid-needle is green whether it works or not, so "+
					"the corpus has to demonstrate it works")
		}
		prepared, err := prepareForbiddenSourceText(claim)
		if err != nil {
			return document, forbiddenSourceTextRuleError(err.Error(), where,
				"fix=correct the needle so it matches its own sample, including every line-wrapped rendering of it")
		}
		document.Claims[index] = prepared
	}
	return document, nil
}

// prepareForbiddenSourceText compiles a row's needle and proves it fires.
//
// The proof is the point. It runs the needle against the row's sample and against
// every rendering of that sample a wrapped comment line would produce, so a needle
// that only works on one arrangement of the same sentence fails to load instead of
// passing forever.
func prepareForbiddenSourceText(claim forbiddenSourceText) (forbiddenSourceText, error) {
	if claim.Pattern != "" {
		compiled, err := regexp.Compile("(?i)" + claim.Pattern)
		if err != nil {
			return claim, fmt.Errorf("claim %q has a pattern that does not compile: %v", claim.Name, err)
		}
		claim.compiled = compiled
	} else {
		claim.normalisedPhrase = normaliseSourceText(claim.Phrase)
		if claim.normalisedPhrase == "" {
			return claim, fmt.Errorf("claim %q normalises to an EMPTY needle, which every file trivially contains", claim.Name)
		}
	}
	renderings := append([]string{claim.MatchesSample}, wrappedRenderings(claim.MatchesSample)...)
	for _, rendering := range renderings {
		if !claim.matches(normaliseSourceText(rendering)) {
			return claim, fmt.Errorf(
				"claim %q does not match its own sample rendered as %q; the needle cannot fire on text it is meant to catch, "+
					"so the guard would stay green through the exact regression it names",
				claim.Name, rendering)
		}
	}
	return claim, nil
}

func forbiddenSourceTextRuleError(what, where, fix string) error {
	return fmt.Errorf(
		"forbidden source text fixture rule failed: %s; a malformed corpus turns a guard that is green by default into one "+
			"that is green by construction; where=%s %s; when=test fixture loading; "+
			"impact=a sentence that already caused a safety net to be deleted could return unnoticed; %s",
		what, forbiddenSourceTextFixturePath, where, fix)
}

// TestPushContent_RedactsContentWithoutBreakingIdentifiers pins what push does to
// transcript content on the way out, and what it must not do to the identifiers
// the village stores a transcript against.
//
// Content used to leave the machine exactly as recorded. It is redacted now,
// through the same function `peasant redact` delegates to, because a user asked
// for the two entrypoints to stop disagreeing. Why the claim that kept them apart
// stood as long as it did is recorded in marshalTranscriptContent's own comment
// and deliberately not restated here - a header that repeats a retracted
// rationale is where the next reader meets the argument for reverting.
//
// Four things are asserted:
//
//   - the planted secrets are gone from what leaves the machine;
//   - the replacements are the redaction module's own, so this is the shared pipeline
//     rather than something similar written here;
//   - the fenced block survives with its structure intact;
//   - every string in the document that carries no planted secret is
//     BYTE-IDENTICAL with and without the redactor.
//
// The last one is the point of the test's name, and it is stated as a property
// over the whole document rather than as a handful of literals. Every string
// value in the outward body passes through the redactor, so identifiers survive
// because no rule matches their shapes, not because they are excluded - and the
// kind discriminator, the harness and role enums, both halves of the version pair
// and the session UUID are all in that same position. Naming two of them looked
// like coverage of a concern that applies to all of them.
//
// The value is the DAY IT FAILS. A rule added later that matched one of those
// shapes would corrupt the key a published transcript is stored against, rather
// than merely over-redact it; a rule that swallowed code structure would take
// back the trade-off this path was rebuilt on. Either forces the decision to be
// made again deliberately instead of absorbed.
func TestPushContent_RedactsContentWithoutBreakingIdentifiers(t *testing.T) {
	t.Parallel()
	// The PLANTS: text put into the body precisely because a rule is expected to
	// match it. They are what separates the two halves of this test - a string
	// carrying one must change, and a string carrying none must not.
	const (
		secret   = "sk-ant-api03-EXAMPLEKEY0000000000000"
		email    = "alice@example.com"
		homePath = "/home/alice"
	)
	plants := []string{secret, email, homePath}
	// A fenced code block with TEXT BEFORE IT, and the leading text is the point.
	//
	// This fixture used to start the fence at byte 0, and that made the assertion
	// pass for the wrong reason: the masking pattern requires a newline before the
	// fence, so a block at the very start of a string was never matched and the
	// test could not have observed masking even while masking was what shipped.
	// Any preceding character - including the blank line a real message has -
	// escapes it. A realistic sample is the fix, not a longer needle.
	recorded := "Here is the fix:\n\n```go\nfunc main() {\n\tkey := \"" + secret + "\"\n\tuser := \"" + email + "\"\n}\n```\n\nDoes that look right?"
	sessionID := schema.SessionID("11111111-2222-3333-4444-555555555555")
	// A body carrying the fields a real push carries, because the identity
	// property below is only worth anything over a document that HAS identities in
	// it. An empty harness, model or working directory would leave the property
	// true over a handful of blank strings.
	meta := &ingest.UnifiedMetadata{
		SessionID:    sessionID,
		ModelHarness: defaults.HarnessClaudeCode,
		Model:        schema.ModelID(testutil.TestModel),
		CWD:          homePath + "/dev/widgets",
	}
	preview := recorded
	entries := []schema.SessionEntry{{
		EntryIndex: 1, Depth: 0, Role: schema.RoleAssistant, ContentPreview: &preview,
	}}
	const emit = schema.PushContractVersion("0.1.1")

	redactor, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
	if err != nil {
		t.Fatalf("build the redactor the push path uses: %v", err)
	}
	fields := config.PushFieldVisibility{ProjectPath: testutil.BoolPtr(true)}
	// A DECLARED origin, so the document actually carries the field the walk
	// below compares. It is agent rather than unknown because unknown is also
	// what a build with no opinion sends, and a fixture that cannot tell the two
	// apart would pass whether or not the declaration was ever set.
	const declared = sessionorigin.Agent
	body, err := marshalTranscriptContent(meta, entries, emit, fields, declared, redactor)
	if err != nil {
		t.Fatalf("marshal the outward body: %v", err)
	}
	// Assert on what the village DECODES, not on the serialized bytes.
	// encoding/json escapes < and > to \u003c/\u003e, so the canonical <EMAIL>
	// placeholder is not literally present in the wire form even though it is
	// exactly what the receiver gets. Checking the raw string would have tested the
	// escaping rather than the redaction - and would have failed while the
	// behaviour was correct.
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("the outward body is not valid JSON: %v", err)
	}
	roundTripped, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	published := string(roundTripped)
	if decodedText, decodeErr := decodeAllStrings(body); decodeErr == nil {
		published = decodedText
	}

	// 1. The secrets are gone from what leaves the machine. This is the behaviour
	// a user asked for after finding the two entrypoints disagreed: `peasant
	// redact` rewrote content and pushing did not.
	for _, leaked := range []string{secret, email} {
		if strings.Contains(published, leaked) {
			t.Errorf("the outward body still carries %q verbatim. Push must apply the same content redaction `peasant "+
				"redact` applies, at the effective level, before anything is published.\nbody:\n%s", leaked, published)
		}
	}
	// 2. And the replacements are the engine's own, so this is the shared pipeline
	// rather than something similar written here.
	for _, want := range []string{"<ANTHROPIC_KEY>", "<EMAIL>"} {
		if !strings.Contains(published, want) {
			t.Errorf("the outward body does not carry the canonical replacement %q, so the redaction that ran was not "+
				"the redaction module's:\nbody:\n%s", want, published)
		}
	}
	// 3. The artifact survives. The whole justification for publishing content
	// unredacted was that redaction would destroy a code block; it does not, and
	// this is what would fail if a rule ever did.
	if strings.Contains(published, "<CODE_BLOCK>") {
		t.Errorf("the published body has a code block replaced wholesale. Below Maximum the rules run over code instead: "+
			"masking removed the artifact AND ran before the rules, so nothing inside the block was ever scanned - a "+
			"config snippet holding a key was published with the key unexamined.\nbody:\n%s", published)
	}
	for _, structure := range []string{"```go", "func main()", "```"} {
		if !strings.Contains(published, structure) {
			t.Errorf("redaction destroyed the code block's %q. If a rule that swallows structure was added, the "+
				"trade-off this path was built on has changed and needs re-deciding rather than absorbing.\nbody:\n%s",
				structure, published)
		}
	}
	// 4. IDENTIFIERS SURVIVE - as a property of the whole document, not as a
	// couple of literals.
	//
	// Every string value in the body passes through the redactor, so the keys the
	// village stores against are in exactly the same position as the content: they
	// are unharmed because no rule matches their shapes, not because they are
	// excluded. The earlier version of this check named the session UUID and the
	// contract version, which is two of the ten-odd strings in that position - the
	// envelope discriminator, the harness and role enums, the status and source
	// values and the other half of the version pair were all unstated while the
	// comment above them described a concern that covers all of them.
	//
	// So the same body is marshalled twice, once with the redactor and once
	// without, and every string is compared path by path. A string carrying a
	// plant must change; a string carrying none must be byte-identical.
	unredactedBody, err := marshalTranscriptContent(meta, entries, emit, fields, declared, nil)
	if err != nil {
		t.Fatalf("marshal the outward body without a redactor: %v", err)
	}
	asRecorded, err := stringsByPath(unredactedBody)
	if err != nil {
		t.Fatalf("walk the unredacted body: %v", err)
	}
	asPublished, err := stringsByPath(body)
	if err != nil {
		t.Fatalf("walk the published body: %v", err)
	}
	if len(asRecorded) != len(asPublished) {
		t.Fatalf("redaction changed the SHAPE of the document: %d string value(s) before, %d after. The two halves below "+
			"compare by path and cannot mean anything if the paths differ.", len(asRecorded), len(asPublished))
	}
	planted, untouched := 0, 0
	for path, recordedValue := range asRecorded {
		publishedValue, present := asPublished[path]
		if !present {
			t.Errorf("the path %s carried a string before redaction and carries none after; redaction must rewrite values, "+
				"not restructure the document the village validates.", path)
			continue
		}
		carriesPlant := false
		for _, plant := range plants {
			if strings.Contains(recordedValue, plant) {
				carriesPlant = true
			}
		}
		if carriesPlant {
			planted++
			if publishedValue == recordedValue {
				t.Errorf("the string at %s carries planted sensitive text and left the machine unchanged: %q", path, recordedValue)
			}
			continue
		}
		untouched++
		if publishedValue != recordedValue {
			t.Errorf("the string at %s was rewritten by redaction: %q became %q. It carries none of the planted secrets, so "+
				"a rule now matches its SHAPE - and every value in this position is an identifier the village stores a "+
				"transcript against, so this corrupts a published transcript's identity rather than over-redacting its "+
				"content. If the new rule is wanted, the identifier it collides with needs a decision, not a passing test.",
				path, recordedValue, publishedValue)
		}
	}
	// Both halves have to be non-empty, or the loop above proves one of them by
	// having nothing to prove it over.
	if planted == 0 {
		t.Error("no string in the document carried a planted secret, so the half of this test that watches redaction RUN " +
			"had nothing to run over")
	}
	if untouched < identityStringFloor {
		t.Errorf("only %d string(s) in the document carry no planted secret, below the floor of %d. The property is "+
			"'identifiers survive'; over a nearly-empty document it is true and worthless.", untouched, identityStringFloor)
	}
	// And the property has to be running over the values the concern is ABOUT. A
	// document that dropped its discriminator or its enums would satisfy
	// everything above by no longer containing them.
	for _, identity := range []string{
		string(sessionID),
		string(emit),
		string(schema.ContentKindSessionDetail),
		string(defaults.HarnessClaudeCode),
		string(schema.RoleAssistant),
		string(testutil.TestModel),
		// The declared origin is covered here BY CONSTRUCTION, not by a rule of
		// its own: it is walked by the two halves above like every other string,
		// so a redaction rule that rewrote it would fail the untouched half, and
		// a build that stopped declaring it fails here. Adding per-field handling
		// for it would reintroduce the per-source form that kept leaving new
		// fields uncovered.
		string(schema.SessionOriginAgent),
	} {
		found := false
		for _, publishedValue := range asPublished {
			if publishedValue == identity {
				found = true
			}
		}
		if !found {
			t.Errorf("the published document carries no string equal to %q. Either redaction rewrote it - which the loop "+
				"above says how - or the body no longer carries it at all, and a value the document does not carry cannot "+
				"be proved to survive.\nbody:\n%s", identity, published)
		}
	}
}

// identityStringFloor is the number of non-planted strings the outward document
// must carry for the survival property to mean anything.
//
// Measured on this tree: the body renders twelve of them - the envelope's kind
// and contract version, the payload's schema version, id, harness, model, status,
// source and two timestamps, and the turn's role and timestamp - beside the two
// that carry plants, the turn's content and the working directory. The floor sits
// below twelve so an added field does not fail the test, and far enough above
// zero that a document collapsing to its envelope cannot satisfy a property about
// the values inside it.
const identityStringFloor = 10

// stringsByPath renders every string value in a JSON document as a map from its
// path to its DECODED value.
//
// Decoded, not raw: encoding/json escapes < and > to </>, so a
// comparison over the serialized bytes would be reading the escaping rather than
// the redaction. Pathed, because the point is comparing the same value across two
// marshals of the same body - an unordered set of strings could not tell a
// rewritten field from a reordered one.
func stringsByPath(body []byte) (map[string]string, error) {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, err
	}
	out := map[string]string{}
	var walk func(string, any)
	walk = func(path string, node any) {
		switch typed := node.(type) {
		case string:
			out[path] = typed
		case []any:
			for index, item := range typed {
				walk(fmt.Sprintf("%s[%d]", path, index), item)
			}
		case map[string]any:
			for key, item := range typed {
				walk(path+"."+key, item)
			}
		}
	}
	walk("", value)
	return out, nil
}

// decodeAllStrings renders every string value in a JSON document as plain text,
// so an assertion reads what the receiver decodes rather than how encoding/json
// escaped it on the way out.
func decodeAllStrings(body []byte) (string, error) {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return "", err
	}
	var out strings.Builder
	var walk func(any)
	walk = func(node any) {
		switch typed := node.(type) {
		case string:
			out.WriteString(typed)
			out.WriteString("\n")
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(value)
	return out.String(), nil
}

// TestPushContent_IsUnredactedOnlyWithNoRedactor pins the one path that still
// publishes as recorded, so the nil case is a decision rather than an accident.
func TestPushContent_IsUnredactedOnlyWithNoRedactor(t *testing.T) {
	t.Parallel()
	const secret = "sk-ant-api03-EXAMPLEKEY0000000000000"
	preview := secret
	body, err := marshalTranscriptContent(
		&ingest.UnifiedMetadata{SessionID: schema.SessionID("11111111-2222-3333-4444-555555555555")},
		[]schema.SessionEntry{{EntryIndex: 1, Role: schema.RoleAssistant, ContentPreview: &preview}},
		schema.PushContractVersion("0.1.1"),
		config.DefaultPushFieldVisibility(),
		sessionorigin.Unknown,
		nil,
	)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), secret) {
		t.Error("a nil redactor redacted anyway. The nil case exists so a caller must choose explicitly; if it silently " +
			"redacted, a caller that forgot to build a redactor would look correct while a caller that meant to skip " +
			"redaction could not.")
	}
}

// --- loader guards ----------------------------------------------------------
//
// Each rejection fixture holds a full corpus with exactly ONE thing wrong, so the
// evidence that the loader is strict sits beside the corpus it protects and its
// own header can say which failure it descends from.

var (
	//go:embed testdata/forbidden_source_text-reject-needle-that-cannot-fire.yaml
	forbiddenSourceTextRejectUnfireableData []byte
	//go:embed testdata/forbidden_source_text-reject-unscanned-package.yaml
	forbiddenSourceTextRejectUnscannedPackageData []byte
	//go:embed testdata/forbidden_source_text-reject-empty-needle.yaml
	forbiddenSourceTextRejectEmptyNeedleData []byte
	//go:embed testdata/forbidden_source_text-reject-unknown-field.yaml
	forbiddenSourceTextRejectUnknownFieldData []byte
)

// TestForbiddenSourceTextCorpus_KeepsEveryClaimItEverAdded is the anchor the
// forbid-list has no closed set to provide, and the guard on the SCAN's reach.
//
// Both halves matter and neither is provable inside the fixture. A deleted claim
// is a sentence that may return unnoticed; a deleted package is a directory the
// scan stops reading, which is precisely how the redaction-safety claim survived
// at its origin while being corrected in the package that inherited it.
func TestForbiddenSourceTextCorpus_KeepsEveryClaimItEverAdded(t *testing.T) {
	t.Parallel()
	document, err := loadForbiddenSourceTextFixture(forbiddenSourceTextFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	// Each required (needle, package) pair must still be carried by some row.
	// Retiring or narrowing a guard is now an edit to this Go slice as well.
	for _, required := range requiredForbiddenClaims {
		found := false
		for _, claim := range document.Claims {
			if claim.needle() == required.needle && slices.Contains(claim.Packages, required.pkg) {
				found = true
			}
		}
		if !found {
			t.Errorf("no claim forbids %q in %s any more.\n"+
				"A guard is retired just as completely by neutering its needle or narrowing its package scope as by "+
				"deleting the row, and both keep the row count at %d. If this guard was deliberately retired, say why in "+
				"the fixture header and remove it here too; if the needle was reworded, update this pair.",
				required.needle, required.pkg, expectedForbiddenSourceTextCount)
		}
	}
	if len(document.Claims) != expectedForbiddenSourceTextCount {
		t.Errorf("the corpus carries %d forbidden claims, want %d. Each one names text that was in this repository while "+
			"being false or unsafe; none stops being worth guarding because the code that carried it was rewritten. If a "+
			"guard was deliberately retired, say why in the fixture header and change expectedForbiddenSourceTextCount as "+
			"well. If one was ADDED, this constant is simply behind.",
			len(document.Claims), expectedForbiddenSourceTextCount)
	}
	// Every declared package must be reached by at least one claim. A package
	// nobody scopes a row to is a directory the scan walks and asks nothing of -
	// it satisfies the non-vacuity check on file counts while proving nothing.
	for _, pkg := range document.Packages {
		reached := false
		for _, claim := range document.Claims {
			if slices.Contains(claim.Packages, pkg) {
				reached = true
			}
		}
		if !reached {
			t.Errorf("no claim is scoped to %s, so the scan reads it and asks nothing of it. Either scope a claim there or "+
				"remove the package; a directory in the list with no rule against it reads as coverage and is none.", pkg)
		}
	}
}

func TestLoadForbiddenSourceText_RejectsANeedleBlindToItsOwnWrappedSample(t *testing.T) {
	t.Parallel()
	_, err := loadForbiddenSourceTextFixture(forbiddenSourceTextRejectUnfireableData)
	if err == nil || !strings.Contains(err.Error(), "does not match its own sample rendered as") {
		t.Fatalf("error = %v, want rejection of a needle that misses the line-wrapped form of its own sample; that is "+
			"precisely how the previous guard read green over the file it named", err)
	}
}

// TestNormaliseSourceText_SeesTheFormTheClaimActuallyTook is the guard on the
// guard.
//
// The text below is the redaction-safety claim VERBATIM as it stood in
// internal/push/pipeline.go before this work, line break and all. A contiguous
// needle could not see it, so the previous version of this test read green over
// the one file its own doc comment named. Everything the corpus does rests on the
// normaliser collapsing that form onto the plain one.
//
// ITS ACCEPTANCE TEST, stated as the production edit that must turn it red:
// removing the whitespace collapse from normaliseSourceText - the single call to
// strings.Fields. That is the realistic simplification, because on unwrapped text
// nothing else in the suite would notice it had gone.
func TestNormaliseSourceText_SeesTheFormTheClaimActuallyTook(t *testing.T) {
	t.Parallel()
	const historical = "// already-redacted indexed entries (DB session_entries are redaction-safe by\n" +
		"\t// construction), so no raw provider JSONL leaves the host"
	for _, needle := range []string{"redaction-safe by construction", "already-redacted"} {
		if !strings.Contains(normaliseSourceText(historical), normaliseSourceText(needle)) {
			t.Errorf("the normaliser does not see %q in the form the claim actually took in this repository:\n%s\n"+
				"A needle blind to a wrapped comment is blind to the file it was written for; gofmt decides where these "+
				"lines break, not the author.", needle, historical)
		}
	}
}

func TestLoadForbiddenSourceText_RejectsARowScopedToAnUnscannedPackage(t *testing.T) {
	t.Parallel()
	_, err := loadForbiddenSourceTextFixture(forbiddenSourceTextRejectUnscannedPackageData)
	if err == nil || !strings.Contains(err.Error(), "which the corpus does not declare") {
		t.Fatalf("error = %v, want rejection of a row scoped to a directory the scan never reads", err)
	}
}

func TestLoadForbiddenSourceText_RejectsARowWithNoNeedleAtAll(t *testing.T) {
	t.Parallel()
	_, err := loadForbiddenSourceTextFixture(forbiddenSourceTextRejectEmptyNeedleData)
	if err == nil || !strings.Contains(err.Error(), "exactly one of phrase and pattern") {
		t.Fatalf("error = %v, want rejection of a row with an EMPTY needle; every file trivially fails to contain one, so "+
			"the row would pass forever while looking like a rule", err)
	}
}

func TestLoadForbiddenSourceText_RejectsAnUnknownField(t *testing.T) {
	t.Parallel()
	_, err := loadForbiddenSourceTextFixture(forbiddenSourceTextRejectUnknownFieldData)
	if err == nil || !strings.Contains(err.Error(), "typed YAML fields must match") {
		t.Fatalf("error = %v, want rejection of an unknown field", err)
	}
}

// --- the corpus -------------------------------------------------------------

// TestSourceText_CarriesNoneOfTheClaimsThatCausedRealDefects stops specific
// sentences and shapes from coming back, in every package they did damage in.
//
// The corpus is the argument; this drives it. Three properties are worth stating
// because each one was a way the previous version of this guard proved less than
// it appeared to:
//
//   - SCOPE. It scanned its own package directory only. The redaction-safety
//     claim's ORIGIN was internal/ingest, which the guard never read, so the
//     sentence was corrected in the package that inherited the belief and left
//     standing in the one that produced it. Packages are now declared in the
//     corpus and resolved from the module root.
//   - FORM. It matched contiguous text. The copy in internal/push/pipeline.go was
//     wrapped across two comment lines by gofmt, so the guard was blind to the
//     very file whose deleted safety net was its whole justification. Matching is
//     now done on normalised text.
//   - VACUITY. A forbid-needle passes for free: the text is gone, so it stays
//     green if the needle is misspelled, if the scan reads nothing, or if the
//     phrase returns in a shape the needle cannot see. Every row now carries a
//     sample it must match, the loader proves each needle fires against that
//     sample and against every line-wrapped rendering of it, and the scan fails
//     if any declared package yields no files.
func TestSourceText_CarriesNoneOfTheClaimsThatCausedRealDefects(t *testing.T) {
	t.Parallel()
	document, err := loadForbiddenSourceTextFixture(forbiddenSourceTextFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	root := testutil.ModuleRoot(t)
	scannedPerPackage := map[string]int{}
	nestedScanned := 0
	for _, pkg := range document.Packages {
		// WalkDir, not ReadDir. The scan read only the TOP LEVEL of each declared
		// package for its whole life, which made its reach roughly half its
		// declared scope - 76 files of 158 - while the corpus said it covered
		// those directories. It cost a real miss: a sentence in docs/research
		// describing the removed code-block masking as current behaviour was
		// invisible to a guard that named docs among its packages, and was found
		// by hand instead.
		walkErr := filepath.WalkDir(filepath.Join(root, pkg), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !scannedFile(entry.Name()) {
				return nil
			}
			data, fileErr := os.ReadFile(path)
			if fileErr != nil {
				return fileErr
			}
			relative, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			scannedPerPackage[pkg]++
			if filepath.Dir(relative) != pkg {
				nestedScanned++
			}
			normalised := normaliseSourceText(string(data))
			for _, claim := range document.Claims {
				if !slices.Contains(claim.Packages, pkg) || !claim.matches(normalised) {
					continue
				}
				t.Errorf("%s carries the forbidden text %q.\n%s", relative, claim.needle(), strings.TrimSpace(claim.Why))
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk the %s package directory: %v", pkg, walkErr)
		}
	}
	// Without this the scan could read nothing - a moved package, a renamed
	// directory, a changed working directory - and report a clean pass over zero
	// files for every row at once.
	for _, pkg := range document.Packages {
		if scannedPerPackage[pkg] == 0 {
			t.Errorf("no non-test Go or Markdown files were scanned in %s, so this guard proved nothing about that "+
				"package; either the directory moved or the corpus names one that no longer exists", pkg)
		}
	}
	// And the reach must stay DEEP. Reverting the walk to a single-level read is a
	// one-line edit that removes half the scan's domain and fails nothing else:
	// the per-package counts above stay non-zero, every row still loads, and the
	// suite is green over a guard that has quietly stopped reading the
	// subdirectories its own corpus declares.
	if nestedScanned == 0 {
		t.Errorf("every scanned file sits at the top level of its declared package, so this guard is reading only one "+
			"directory deep. That was its behaviour until it was found to have missed a falsified sentence in a "+
			"docs subdirectory; if the layout genuinely flattened, say so in %s, and if the walk was reverted, restore it.",
			forbiddenSourceTextFixturePath)
	}
}
