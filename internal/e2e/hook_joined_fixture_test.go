package e2e

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

// This loader is deliberately UNtagged so `make check` exercises every guard
// below on the committed corpus, without podman. The joined evidence test that
// consumes it is tagged `e2e`; the corpus it depends on is validated either way.

//go:embed testdata/hook-joined-evidence.yaml
var hookJoinedEvidenceYAML []byte

//go:embed testdata/hook-joined-evidence-unknown-field.yaml
var hookJoinedEvidenceUnknownFieldYAML []byte

//go:embed testdata/hook-joined-evidence-second-document.yaml
var hookJoinedEvidenceSecondDocumentYAML []byte

//go:embed testdata/hook-joined-evidence-count-mismatch.yaml
var hookJoinedEvidenceCountMismatchYAML []byte

//go:embed testdata/hook-joined-evidence-blank-field.yaml
var hookJoinedEvidenceBlankFieldYAML []byte

//go:embed testdata/hook-joined-evidence-wrong-kind.yaml
var hookJoinedEvidenceWrongKindYAML []byte

const hookJoinedEvidencePath = "internal/e2e/testdata/hook-joined-evidence.yaml"

type hookJoinedEvidenceDocument struct {
	ExpectedCaseCount int                      `yaml:"expectedCaseCount"`
	Cases             []hookJoinedEvidenceCase `yaml:"cases"`
}

// hookJoinedEvidenceOutcome is what a case proves. It is a closed set because the
// two outcomes assert opposite things and carry disjoint fields: one proves the
// chain published, the other proves that when no run can happen Git still
// finishes. Keeping it closed is what lets the loader reject a case that has
// been filled in as the wrong kind.
type hookJoinedEvidenceOutcome string

const (
	// outcomePublishes is the whole chain working: git, the generated hook, the
	// real binary, a real village.
	outcomePublishes hookJoinedEvidenceOutcome = "publishes"
	// outcomeRefuses is a configuration this version cannot run. The upload must
	// refuse, and the commit must still succeed.
	outcomeRefuses hookJoinedEvidenceOutcome = "refuses"
)

var allHookJoinedEvidenceOutcomes = [...]hookJoinedEvidenceOutcome{outcomePublishes, outcomeRefuses}

// hookJoinedEvidenceCase is every value the evidence tests assert on. Nothing a
// case depends on may live in a test file: the point of the corpus is that a
// contract change is one edit in one place.
type hookJoinedEvidenceCase struct {
	Name    string                    `yaml:"name"`
	Outcome hookJoinedEvidenceOutcome `yaml:"outcome"`

	// --- publishing cases -------------------------------------------------

	// ExpectedTranscripts is how many sessions the configured repository
	// publishes; ExcludedTranscripts is how many a second repository records
	// and must never publish.
	ExpectedTranscripts int `yaml:"expected_transcripts,omitempty"`
	ExcludedTranscripts int `yaml:"excluded_transcripts,omitempty"`
	// SecondRootSessionID and SecondSubagentSessionID are the identifiers the
	// second repository's copy of the committed Claude fixture is renamed onto.
	// Without them the two copies would collide on one session identity and the
	// exclusion assertion would have nothing to name.
	SecondRootSessionID     string             `yaml:"second_root_session_id,omitempty"`
	SecondSubagentSessionID string             `yaml:"second_subagent_session_id,omitempty"`
	Visibility              schema.Visibility  `yaml:"visibility,omitempty"`
	IdentityBasis           push.IdentityBasis `yaml:"identity_basis,omitempty"`
	// InstallDisclosureContains is every sentence the install consent surface
	// owes. Install is the last point before automated publishing begins, so a
	// setting the user asked for and will not get has to be stated there, not
	// only after the first commit has already published.
	InstallDisclosureContains []string `yaml:"install_disclosure_contains,omitempty"`
	// DisclosureContains is every sentence the hook-triggered push owes. A hook
	// runs with --quiet, so these are the one-line forms.
	DisclosureContains []string `yaml:"disclosure_contains,omitempty"`
	WarningContains    []string `yaml:"warning_contains,omitempty"`
	SuccessResult      string   `yaml:"success_result,omitempty"`

	// --- refusing cases ---------------------------------------------------

	// InstalledRedactionLevel is the supported level the hook is installed
	// under, before the configuration is edited to RedactionLevel.
	InstalledRedactionLevel redact.RedactionLevel `yaml:"installed_redaction_level,omitempty"`
	// RefusalContains is every part of the refusal that makes it actionable.
	RefusalContains []string `yaml:"refusal_contains,omitempty"`
	// Event is the hook event the case drives. It is case data because only one
	// event can prove the guarantee: Git ignores post-commit's exit status.
	Event githooks.Event `yaml:"event,omitempty"`

	// --- both -------------------------------------------------------------

	// RedactionLevel is the level the configuration carries when the hook fires.
	RedactionLevel redact.RedactionLevel `yaml:"redaction_level"`
	// ForbiddenContains is text the case's surfaces must never emit.
	ForbiddenContains []string `yaml:"forbidden_contains"`
}

// publishingFields and refusingFields are the keys that belong to exactly one
// outcome. A case carrying a field from the other kind is REJECTED rather than
// ignored: a half-filled case is the failure this corpus exists to prevent,
// because it looks complete and asserts nothing.
func (c hookJoinedEvidenceCase) publishingFields() []hookJoinedEvidenceField {
	return []hookJoinedEvidenceField{
		{"second_root_session_id", c.SecondRootSessionID},
		{"second_subagent_session_id", c.SecondSubagentSessionID},
		{"success_result", c.SuccessResult},
		{"visibility", c.Visibility.String()},
		{"identity_basis", string(c.IdentityBasis)},
	}
}

func (c hookJoinedEvidenceCase) refusingFields() []hookJoinedEvidenceField {
	return []hookJoinedEvidenceField{
		{"installed_redaction_level", c.InstalledRedactionLevel.String()},
		{"event", c.Event.String()},
	}
}

// hookJoinedEvidenceField pairs a fixture key with its loaded value so a blank
// value is reported by the exact key a maintainer has to edit, instead of one
// opaque "fixture is incomplete".
type hookJoinedEvidenceField struct {
	Key   string
	Value string
}

// requiredStrings is every string a case of this kind asserts on. A blank value
// is the most expensive failure this corpus has: strings.Contains(x, "") is
// always true, so a blanked or mistyped key turns a live assertion into a
// guaranteed pass wherever the check is negated.
func (c hookJoinedEvidenceCase) requiredStrings() []hookJoinedEvidenceField {
	fields := []hookJoinedEvidenceField{
		{"name", c.Name},
		{"redaction_level", c.RedactionLevel.String()},
	}
	switch c.Outcome {
	case outcomePublishes:
		return append(fields, c.publishingFields()...)
	case outcomeRefuses:
		return append(fields, c.refusingFields()...)
	}
	return fields
}

// hookJoinedEvidenceList pairs a fixture key with the list it loaded. It is a
// slice, not a map, so a fixture with more than one problem reports the same one
// on every run.
type hookJoinedEvidenceList struct {
	Key    string
	Values []string
}

// requiredLists is every list a case of this kind iterates. An empty list makes
// its whole loop vacuous without failing anything.
func (c hookJoinedEvidenceCase) requiredLists() []hookJoinedEvidenceList {
	lists := []hookJoinedEvidenceList{{"forbidden_contains", c.ForbiddenContains}}
	switch c.Outcome {
	case outcomePublishes:
		lists = append(lists,
			hookJoinedEvidenceList{"warning_contains", c.WarningContains},
			hookJoinedEvidenceList{"install_disclosure_contains", c.InstallDisclosureContains},
			hookJoinedEvidenceList{"disclosure_contains", c.DisclosureContains})
	case outcomeRefuses:
		lists = append(lists, hookJoinedEvidenceList{"refusal_contains", c.RefusalContains})
	}
	return lists
}

// forbiddenFields are the keys that must be ABSENT on a case of this kind.
func (c hookJoinedEvidenceCase) fieldsForbiddenByOutcome() ([]hookJoinedEvidenceField, []hookJoinedEvidenceList) {
	switch c.Outcome {
	case outcomePublishes:
		return c.refusingFields(), []hookJoinedEvidenceList{{"refusal_contains", c.RefusalContains}}
	case outcomeRefuses:
		return c.publishingFields(), []hookJoinedEvidenceList{
			{"warning_contains", c.WarningContains},
			{"install_disclosure_contains", c.InstallDisclosureContains},
			{"disclosure_contains", c.DisclosureContains},
		}
	}
	return nil, nil
}

// LoadHookJoinedEvidenceFixtures decodes and fully validates the joined
// post-commit evidence corpus.
func LoadHookJoinedEvidenceFixtures() (hookJoinedEvidenceDocument, error) {
	return loadHookJoinedEvidence(hookJoinedEvidenceYAML)
}

func loadHookJoinedEvidence(data []byte) (hookJoinedEvidenceDocument, error) {
	var document hookJoinedEvidenceDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, hookJoinedEvidenceRuleError(
			"typed YAML fields must match the document schema; an unknown or mistyped key leaves the field it was meant to set at its zero value",
			"loader=first-document decode",
			"fix=correct the key to one the typed schema declares, or add the field to hookJoinedEvidenceCase: "+err.Error())
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		detail := "found a second YAML document"
		if err != nil {
			detail = err.Error()
		}
		return document, hookJoinedEvidenceRuleError(
			"exactly one YAML document is allowed; anything after the first is silently ignored, so cases below it prove nothing",
			"loader=end-of-document check",
			"fix=remove the second document so the next decode returns EOF: "+detail)
	}
	if len(document.Cases) == 0 || document.ExpectedCaseCount != len(document.Cases) {
		return document, hookJoinedEvidenceRuleError(
			fmt.Sprintf("declared and actual case counts must match and be non-zero, got expectedCaseCount=%d cases=%d; a corpus that silently shrinks still passes every assertion it still contains",
				document.ExpectedCaseCount, len(document.Cases)),
			"loader=case-count validation",
			"fix=set expectedCaseCount to the number of cases actually present")
	}
	seen := make(map[string]bool, len(document.Cases))
	for index, evidenceCase := range document.Cases {
		if err := validateHookJoinedEvidenceCase(index, evidenceCase); err != nil {
			return document, err
		}
		if seen[evidenceCase.Name] {
			return document, hookJoinedEvidenceCaseError(index, fmt.Sprintf("duplicate case name %q", evidenceCase.Name),
				"fix=give every case a unique name so a failure names exactly one scenario")
		}
		seen[evidenceCase.Name] = true
	}
	return document, nil
}

func validateHookJoinedEvidenceCase(index int, evidenceCase hookJoinedEvidenceCase) error {
	if !containsValue(allHookJoinedEvidenceOutcomes[:], evidenceCase.Outcome) {
		return hookJoinedEvidenceCaseError(index, fmt.Sprintf("outcome %q is not a kind of evidence this corpus knows", evidenceCase.Outcome),
			fmt.Sprintf("fix=use one of %v; the kind decides which fields the case must and must not carry", allHookJoinedEvidenceOutcomes))
	}
	for _, field := range evidenceCase.requiredStrings() {
		if strings.TrimSpace(field.Value) == "" {
			return hookJoinedEvidenceCaseError(index, fmt.Sprintf("%s is blank on a %q case", field.Key, evidenceCase.Outcome),
				fmt.Sprintf("fix=give %s the exact text or identifier the run must observe", field.Key))
		}
	}
	for _, list := range evidenceCase.requiredLists() {
		if len(list.Values) == 0 {
			return hookJoinedEvidenceCaseError(index, fmt.Sprintf("%s is empty on a %q case", list.Key, evidenceCase.Outcome),
				fmt.Sprintf("fix=list at least one entry under %s, or the loop that asserts on it proves nothing", list.Key))
		}
		for position, value := range list.Values {
			if strings.TrimSpace(value) == "" {
				return hookJoinedEvidenceCaseError(index, fmt.Sprintf("%s[%d] is blank", list.Key, position),
					fmt.Sprintf("fix=state the exact text at %s[%d]", list.Key, position))
			}
		}
	}
	// A field belonging to the OTHER kind is rejected rather than ignored. A case
	// carrying leftovers from the kind it used to be looks complete and asserts
	// nothing, which is the failure this corpus exists to prevent.
	strayFields, strayLists := evidenceCase.fieldsForbiddenByOutcome()
	for _, field := range strayFields {
		if strings.TrimSpace(field.Value) != "" {
			return hookJoinedEvidenceCaseError(index, fmt.Sprintf("a %q case carries %s, which belongs to the other kind", evidenceCase.Outcome, field.Key),
				fmt.Sprintf("fix=remove %s, or change the case's outcome to the kind that uses it", field.Key))
		}
	}
	for _, list := range strayLists {
		if len(list.Values) > 0 {
			return hookJoinedEvidenceCaseError(index, fmt.Sprintf("a %q case carries %s, which belongs to the other kind", evidenceCase.Outcome, list.Key),
				fmt.Sprintf("fix=remove %s, or change the case's outcome to the kind that uses it", list.Key))
		}
	}
	if !evidenceCase.RedactionLevel.IsValid() {
		return hookJoinedEvidenceCaseError(index, fmt.Sprintf("redaction_level %q is not a known level", evidenceCase.RedactionLevel),
			fmt.Sprintf("fix=use one of %q, %q, %q", redact.Minimal, redact.Standard, redact.Maximum))
	}
	if evidenceCase.Outcome == outcomeRefuses {
		return validateHookJoinedEvidenceRefusal(index, evidenceCase)
	}
	return validateHookJoinedEvidencePublish(index, evidenceCase)
}

// validateHookJoinedEvidenceRefusal keeps the case honest about what it proves:
// the level it refuses must be one this version really rejects, and the level it
// installs under must be one it really accepts. Getting either backwards would
// leave the case passing while exercising nothing.
func validateHookJoinedEvidenceRefusal(index int, evidenceCase hookJoinedEvidenceCase) error {
	if config.RedactionLevelSupported(evidenceCase.RedactionLevel) {
		return hookJoinedEvidenceCaseError(index, fmt.Sprintf("redaction_level %q is SUPPORTED, so nothing would refuse", evidenceCase.RedactionLevel),
			"fix=name a level this version rejects, or change the case's outcome to publishes")
	}
	if evidenceCase.Event != githooks.EventPrePush {
		return hookJoinedEvidenceCaseError(index, fmt.Sprintf("event %q cannot prove a refusal leaves Git working", evidenceCase.Event),
			fmt.Sprintf("fix=use %q: Git IGNORES the exit status of %q, so an assertion that Git still succeeded after it can never fail",
				githooks.EventPrePush, githooks.EventPostCommit))
	}
	if !evidenceCase.InstalledRedactionLevel.IsValid() || !config.RedactionLevelSupported(evidenceCase.InstalledRedactionLevel) {
		return hookJoinedEvidenceCaseError(index, fmt.Sprintf("installed_redaction_level %q is not a level a hook can be installed under", evidenceCase.InstalledRedactionLevel),
			fmt.Sprintf("fix=use a supported level (%s); installing under an unsupported one is refused, so the case could never reach a commit", config.RedactionLevelMenu()))
	}
	return nil
}

func validateHookJoinedEvidencePublish(index int, evidenceCase hookJoinedEvidenceCase) error {
	if !config.RedactionLevelSupported(evidenceCase.RedactionLevel) {
		return hookJoinedEvidenceCaseError(index, fmt.Sprintf("redaction_level %q is NOT supported, so this case cannot publish", evidenceCase.RedactionLevel),
			fmt.Sprintf("fix=use a supported level (%s), or change the case's outcome to refuses", config.RedactionLevelMenu()))
	}
	if evidenceCase.ExpectedTranscripts <= 0 || evidenceCase.ExcludedTranscripts <= 0 {
		return hookJoinedEvidenceCaseError(index,
			fmt.Sprintf("expected_transcripts=%d and excluded_transcripts=%d must both be positive", evidenceCase.ExpectedTranscripts, evidenceCase.ExcludedTranscripts),
			"fix=state how many sessions the configured repository publishes and how many a second repository must keep out of scope")
	}
	if !evidenceCase.Visibility.IsValid() {
		return hookJoinedEvidenceCaseError(index, fmt.Sprintf("visibility %q is not in the contract's closed set", evidenceCase.Visibility),
			fmt.Sprintf("fix=use one of %v", schema.AllVisibilities))
	}
	if evidenceCase.IdentityBasis != push.IdentityFromPath && evidenceCase.IdentityBasis != push.IdentityFromRemote {
		return hookJoinedEvidenceCaseError(index, fmt.Sprintf("identity_basis %q is not a derivation Peasant reports", evidenceCase.IdentityBasis),
			fmt.Sprintf("fix=use %q or %q", push.IdentityFromPath, push.IdentityFromRemote))
	}
	return validateHookJoinedEvidenceSessionCounts(index, evidenceCase)
}

func containsValue[T comparable](values []T, want T) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// validateHookJoinedEvidenceSessionCounts keeps every pinned sentence that names
// a session count in step with the case's own count. Without it, bumping
// expected_transcripts silently turns an absence assertion vacuous: a forbidden
// claim naming the old count can never appear in output that names the new one.
func validateHookJoinedEvidenceSessionCounts(index int, evidenceCase hookJoinedEvidenceCase) error {
	const countedPhrase = "session(s)"
	expected := fmt.Sprintf("%d session(s)", evidenceCase.ExpectedTranscripts)
	counted := append([]hookJoinedEvidenceField{{"success_result", evidenceCase.SuccessResult}}, listFields("forbidden_contains", evidenceCase.ForbiddenContains)...)
	for _, field := range counted {
		if !strings.Contains(field.Value, countedPhrase) || strings.Contains(field.Value, expected) {
			continue
		}
		return hookJoinedEvidenceCaseError(index,
			fmt.Sprintf("%s names a session count that is not this case's expected_transcripts=%d: %q", field.Key, evidenceCase.ExpectedTranscripts, field.Value),
			fmt.Sprintf("fix=rewrite it around %q, or the sentence can never match the output it is pinned against", expected))
	}
	return nil
}

func listFields(key string, values []string) []hookJoinedEvidenceField {
	fields := make([]hookJoinedEvidenceField, 0, len(values))
	for position, value := range values {
		fields = append(fields, hookJoinedEvidenceField{Key: fmt.Sprintf("%s[%d]", key, position), Value: value})
	}
	return fields
}

// --- loader guards ----------------------------------------------------------
//
// Each guard has a dedicated rejection fixture rather than an inline string, so
// the corpus that proves the loader strict lives beside the corpus it protects.

func TestLoadHookJoinedEvidence_AcceptsCommittedCorpus(t *testing.T) {
	t.Parallel()
	document, err := LoadHookJoinedEvidenceFixtures()
	if err != nil {
		t.Fatalf("committed joined hook evidence corpus must load: %v", err)
	}
	if document.ExpectedCaseCount != len(document.Cases) {
		t.Fatalf("expectedCaseCount = %d, cases = %d", document.ExpectedCaseCount, len(document.Cases))
	}
}

func TestLoadHookJoinedEvidence_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	_, err := loadHookJoinedEvidence(hookJoinedEvidenceUnknownFieldYAML)
	if err == nil || !strings.Contains(err.Error(), "field success_resul not found") {
		t.Fatalf("error = %v, want a rejection naming the mistyped key", err)
	}
}

func TestLoadHookJoinedEvidence_RejectsSecondDocument(t *testing.T) {
	t.Parallel()
	_, err := loadHookJoinedEvidence(hookJoinedEvidenceSecondDocumentYAML)
	if err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("error = %v, want a second-document rejection", err)
	}
}

func TestLoadHookJoinedEvidence_RejectsCountMismatch(t *testing.T) {
	t.Parallel()
	_, err := loadHookJoinedEvidence(hookJoinedEvidenceCountMismatchYAML)
	if err == nil || !strings.Contains(err.Error(), "expectedCaseCount=4 cases=2") {
		t.Fatalf("error = %v, want a case-count rejection", err)
	}
}

// TestLoadHookJoinedEvidence_RejectsBlankAssertedString is the guard that makes
// every other fixture edit verifiable: a blank asserted string is not a weaker
// assertion, it is a guaranteed pass.
func TestLoadHookJoinedEvidence_RejectsBlankAssertedString(t *testing.T) {
	t.Parallel()
	_, err := loadHookJoinedEvidence(hookJoinedEvidenceBlankFieldYAML)
	if err == nil || !strings.Contains(err.Error(), "warning_contains[0] is blank") {
		t.Fatalf("error = %v, want a blank-value rejection naming warning_contains[0]", err)
	}
}

// TestLoadHookJoinedEvidence_RejectsAFieldFromTheOtherKind is what the case-kind
// schema exists for: a case that still carries the fields of the kind it used to
// be reads as complete and asserts nothing.
func TestLoadHookJoinedEvidence_RejectsAFieldFromTheOtherKind(t *testing.T) {
	t.Parallel()
	_, err := loadHookJoinedEvidence(hookJoinedEvidenceWrongKindYAML)
	if err == nil || !strings.Contains(err.Error(), "carries success_result, which belongs to the other kind") {
		t.Fatalf("error = %v, want a wrong-kind rejection naming the stray field", err)
	}
}

func hookJoinedEvidenceRuleError(what, where, fix string) error {
	return fmt.Errorf(
		"joined hook evidence fixture rule failed: %s; where=%s %s; when=test fixture loading; "+
			"impact=the only end-to-end proof that git -> hook -> peasant -> Village works cannot be trusted; %s",
		what, hookJoinedEvidencePath, where, fix)
}

func hookJoinedEvidenceCaseError(index int, what, fix string) error {
	return hookJoinedEvidenceRuleError(what, fmt.Sprintf("case index %d", index), fix)
}
