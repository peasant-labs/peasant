package sessionorigin

import (
	"regexp"
	"slices"
	"strings"

	"github.com/peasant-labs/redact"
)

// Evidence is the closed set of facts the rule reads. The content fields come
// from a harness adapter; HasParent comes from Peasant's own discovery and
// linking state, which is why an adapter takes it as a separate argument. The
// rule touches no file, no store, and no harness type. A harness that records
// none of these leaves the zero value, which classifies Unknown.
type Evidence struct {
	// HasParent says this transcript is the child of another session: a subagent
	// transcript, or a root discovery already linked to its spawner. It is
	// supplied by Peasant rather than by the harness, and it outranks every
	// content signal.
	HasParent bool
	// AgentName and TeamName are the structured teammate identity a modern
	// Claude Code client writes into its entries.
	AgentName string
	TeamName  string
	// Entrypoint and PromptSource describe a programmatic launch.
	Entrypoint   string
	PromptSource string
	// FirstUserText is the raw, untrimmed text of the transcript's opening
	// user-role entry, and is empty when the transcript has none.
	//
	// Which entry that is belongs to the adapter, not to this rule. A harness
	// may write its own scaffolding into user-role entries ahead of the real
	// opening one, and an adapter that recognises its own markup is expected to
	// read past a leading run of it and supply the first entry a person or an
	// agent actually produced. The rule is harness-neutral and reads whatever it
	// is handed; it never decides which entry that should have been.
	FirstUserText string
}

// Signal names the evidence that decided an Origin, so a classification can be
// explained and audited instead of re-derived. It is not persisted; the audit
// harness and the fixtures are its consumers.
type Signal string

const (
	// SignalParentLinked: the transcript is the child of another session.
	SignalParentLinked Signal = "parent-linked"
	// SignalStructuredIdentity: the harness recorded a teammate identity.
	SignalStructuredIdentity Signal = "structured-identity"
	// SignalProgrammaticEntry: the harness recorded a programmatic launch.
	SignalProgrammaticEntry Signal = "programmatic-entry"
	// SignalCommandInvocation: the first user text opens with a command wrapper,
	// which is something a person typed.
	SignalCommandInvocation Signal = "command-invocation"
	// SignalBootstrapText: the first user text opens with an agent bootstrap.
	SignalBootstrapText Signal = "bootstrap-text"
	// SignalNoEvidence: nothing in the evidence decides the question.
	SignalNoEvidence Signal = "no-evidence"
)

// AllSignals is the canonical set of deciding signals, in rule order.
var AllSignals = []Signal{
	SignalParentLinked,
	SignalStructuredIdentity,
	SignalProgrammaticEntry,
	SignalCommandInvocation,
	SignalBootstrapText,
	SignalNoEvidence,
}

// Valid reports whether s is one of the deciding signals this build knows.
func (s Signal) Valid() bool {
	return slices.Contains(AllSignals, s)
}

// String returns the reported form of the signal.
func (s Signal) String() string { return string(s) }

// entrypointSDKCLI and promptSourceSDK are the pair a harness records when a
// program, rather than a person, started the session. Both must be present:
// either one alone describes a launch shape that a person can also produce.
const (
	entrypointSDKCLI = "sdk-cli"
	promptSourceSDK  = "sdk"
)

// bootstrapSkillInvocation matches the opening of an agent brief that starts by
// invoking a skill. This is the one marker expressed locally rather than taken
// from the redact wrapper catalog, because it is an invocation form rather than
// harness markup and redact owns no name for it.
var bootstrapSkillInvocation = regexp.MustCompile(`^(\d+\.\s*)?(Use |First invoke )?Skill\(/`)

// Classify is the one rule. It is ordered and first match wins:
//
//  1. HasParent                                   -> Agent   (parent-linked)
//  2. AgentName or TeamName present               -> Agent   (structured-identity)
//  3. Entrypoint sdk-cli AND PromptSource sdk     -> Agent   (programmatic-entry)
//  4. FirstUserText opens with a command wrapper  -> User    (command-invocation)
//  5. FirstUserText opens with a bootstrap marker -> Agent   (bootstrap-text)
//  6. otherwise                                   -> Unknown (no-evidence)
//
// Step 4 precedes step 5 on purpose. A slash command is something a person
// typed, whatever the harness wrapped it in, and the great majority of measured
// command-wrapped roots carry no agent marker at all. Classifying one of them
// Agent would hide a person's own session, so a command-wrapped root reaching
// Agent is a defect and never a tolerance.
//
// Plain prose falls to Unknown rather than User: prose cannot distinguish a
// person's prompt from an agent brief, so declaring User would be a false
// declaration. The honest answer costs nothing, because a declared Unknown
// returns the decision to the consumer's own rule.
//
// Every caller derives its answer from this function. No caller re-implements
// the ordering and no caller hardcodes an Origin and Signal pair, including the
// subagent path, which calls Classify(Evidence{HasParent: true}) so that step 1
// stays the single definition of what a child transcript means.
func Classify(ev Evidence) (Origin, Signal) {
	if ev.HasParent {
		return Agent, SignalParentLinked
	}
	if ev.AgentName != "" || ev.TeamName != "" {
		return Agent, SignalStructuredIdentity
	}
	if ev.Entrypoint == entrypointSDKCLI && ev.PromptSource == promptSourceSDK {
		return Agent, SignalProgrammaticEntry
	}
	opening := strings.TrimLeft(ev.FirstUserText, " \t\r\n")
	if opensWithTag(opening, redact.WrapperCommandMessage) || opensWithTag(opening, redact.WrapperCommandName) {
		return User, SignalCommandInvocation
	}
	if opensWithTag(opening, redact.WrapperTeammateMessage) || bootstrapSkillInvocation.MatchString(opening) {
		return Agent, SignalBootstrapText
	}
	return Unknown, SignalNoEvidence
}

// opensWithTag reports whether text begins with an opening tag of exactly the
// given name. Attributes are tolerated and a self-closing form is accepted, but
// a longer name that merely starts with this one is not a match: the character
// after the name must end it.
//
// The names are never spelled here. All three come from the redact wrapper
// catalog, which is the one place that owns them, so a harness renaming a
// wrapper is a single upstream change rather than a silent divergence between
// this rule and every other reader of the same markup.
func opensWithTag(text, name string) bool {
	if name == "" || !strings.HasPrefix(text, "<"+name) {
		return false
	}
	rest := text[len(name)+1:]
	if rest == "" {
		return false
	}
	switch rest[0] {
	case '>', '/', ' ', '\t', '\r', '\n':
		return true
	default:
		return false
	}
}
