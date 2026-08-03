package ftue

import (
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/redact"
)

// The onboarding privacy screen produces its examples with the real redactor.
// This keeps the displayed placeholders and partial-path behavior synchronized
// with the rules that publication applies instead of duplicating their output in
// UI copy.

// privacyExampleInput is one line of the screen's before/after list.
//
// The inputs are chosen to exercise the three categories Standard actually
// redacts, one apiece, and the SECRET is first because it is the category users
// care most about and the screen previously showed none at all.
type privacyExampleInput struct {
	// text is fed to the real redactor. It must be synthetic: this string is
	// compiled into the binary and printed during onboarding.
	text string
	// why names the category for a reader of this file, not for the screen.
	why redact.Category
}

// privacyClaimedCategories are the categories the screen's description promises.
// The examples must demonstrate each one, so the copy cannot outlive the sample
// that backs it.
var privacyClaimedCategories = []redact.Category{
	redact.CategorySecrets, redact.CategoryPII, redact.CategoryPaths,
}

// privacyExampleInputs are the samples the screen redacts in front of the user.
//
// Every sample must genuinely match its rule. For example, the synthetic API key
// needs at least twenty characters after its key type; a shorter illustration
// would remain visible even though the screen promises known secrets are redacted.
// The samples below carry no real secret, and privacyExamples rejects any sample
// the engine leaves unchanged.
var privacyExampleInputs = []privacyExampleInput{
	{text: "sk-ant-api03-EXAMPLEKEY0000000000000", why: redact.CategorySecrets},
	{text: "user@example.com", why: redact.CategoryPII},
	{text: "/Users/alice/projects/", why: redact.CategoryPaths},
}

// privacyExamples renders the before/after lines for the onboarding screen by
// REDACTING the samples at the level the screen is about to apply.
//
// It returns an error rather than a best-effort string when a sample survives
// unchanged. A screen that shows `user@example.com -> user@example.com` would tell
// the user their address is kept, which is a worse lie than the one this replaced,
// and it would mean the rule for a category the screen CLAIMS to redact had
// stopped firing. That is a fact worth failing on, not papering over - the caller
// turns it into a test failure rather than shipping it.
func privacyExamples(level redact.RedactionLevel) ([]string, error) {
	redactor, err := redact.NewRedactor(level, nil, redact.XDGPaths{})
	if err != nil {
		return nil, fmt.Errorf(
			"ftue: cannot build the onboarding privacy examples: constructing a %s redactor failed.\n"+
				"What went wrong: redact.NewRedactor returned %v.\n"+
				"Where: ftue.privacyExamples, rendering the onboarding privacy screen.\n"+
				"When: at screen construction, before anything was shown to the user.\n"+
				"Means: the screen cannot show what redaction does, so it must not claim to.\n"+
				"Fix: repair the redactor or its sample inputs rather than hard-coding expected output in the UI.",
			level, err)
	}
	// Column width for the arrows, so the before/after pairs line up as a table
	// rather than as ragged prose. Derived from the samples, so adding one cannot
	// leave the list misaligned.
	widest := 0
	for _, sample := range privacyExampleInputs {
		if len(sample.text) > widest {
			widest = len(sample.text)
		}
	}
	// Every category the screen's description CLAIMS must have a sample, walked
	// from the samples' own declared categories.
	//
	// The screen says it redacts secrets, personal data, and username paths, so it
	// must demonstrate every category independently.
	for _, claimed := range privacyClaimedCategories {
		found := false
		for _, sample := range privacyExampleInputs {
			if sample.why == claimed {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf(
				"ftue: cannot build the onboarding privacy examples: the screen claims to redact %s and has no sample for it.\n"+
					"What went wrong: privacyClaimedCategories names a category that privacyExampleInputs does not exercise.\n"+
					"Where: ftue.privacyExamples, rendering the onboarding privacy screen.\n"+
					"When: at screen construction, before anything was shown to the user.\n"+
					"Means: the screen would describe a protection it does not demonstrate, and a rule for that category "+
					"could stop firing with the copy still claiming it.\n"+
					"Fix: add a sample for %s to privacyExampleInputs, or stop claiming it in the description.",
				claimed, claimed)
		}
	}
	lines := make([]string, 0, len(privacyExampleInputs))
	for _, sample := range privacyExampleInputs {
		redacted := redactor.RedactText(sample.text)
		if redacted == sample.text {
			return nil, fmt.Errorf(
				"ftue: cannot build the onboarding privacy examples: the %s sample %q is unchanged at the %s level.\n"+
					"What went wrong: the rule that redacts this category no longer fires on it.\n"+
					"Where: ftue.privacyExamples, rendering the onboarding privacy screen.\n"+
					"When: at screen construction, before anything was shown to the user.\n"+
					"Means: the screen would show this sample surviving redaction while telling the user the category is "+
					"redacted - it would over-claim in the one direction that costs the user something.\n"+
					"Fix: either the sample no longer matches the rule's shape (update the sample in "+
					"privacyExampleInputs), or the rule stopped covering the category (repair github.com/peasant-labs/redact).",
				sample.why, sample.text, level)
		}
		lines = append(lines, fmt.Sprintf("%-*s  ->  %s", widest, sample.text, redacted))
	}
	return lines, nil
}

// privacyScopeNoun is what redaction is applied TO, and it is the word this
// screen turns on.
//
// Push and `peasant redact` use the same content-redaction path, so the scope is
// transcripts rather than metadata alone. The assertion that pins this value
// uses a literal to avoid a tautological test that changes with the constant.
const privacyScopeNoun = "transcripts"

// privacyDisclosure is the sentence the screen leads with when there is exactly
// one offered level: it STATES what will be applied rather than asking the user
// to choose it.
//
// With one level offered, a radio glyph, a "(recommended)" tag and an up/down
// hint are affordances asserting alternatives that do not exist. On a privacy
// screen that can imply hidden options. The multi-option rendering is kept and
// returns automatically when a second level is offered.
func privacyDisclosure(level redact.RedactionLevel) string {
	return fmt.Sprintf("Your %s will be redacted at the %s level\nbefore they leave your machine.",
		privacyScopeNoun, privacyLevelLabel(level))
}

// privacyContentSentence is what happens to the conversation text and how far the
// redaction claim goes.
//
// It returns config.RedactionScopeSentence UNMODIFIED, and the absence of a
// prefix is deliberate. That sentence already carries every element this screen
// owes - the known-patterns hedge, that it is best effort and not a guarantee,
// and that transcript content is covered too rather than only metadata. A local
// lead-in would duplicate policy wording and allow this screen to drift.
func privacyContentSentence() string {
	return config.RedactionScopeSentence()
}

// privacyLevelLabel is the level as the screen writes it, capitalised.
func privacyLevelLabel(level redact.RedactionLevel) string {
	name := level.String()
	if name == "" {
		return ""
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// offeredLevelIsSingular reports whether the screen should state rather than
// offer. It reads the POLICY, not the local option list, so the screen follows
// the product automatically when the menu widens.
func offeredLevelIsSingular() bool { return len(config.OfferedRedactionLevels) == 1 }
