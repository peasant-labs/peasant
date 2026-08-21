package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// This file is the single place that knows the difference between what a user
// configured and what this version of Peasant can actually apply.
//
// Two settings are currently affected, and they are handled DIFFERENTLY on
// purpose. The difference is the direction the gap points in, and it is stated
// here so that nobody harmonises the two later:
//
//   - push.visibility: private and public are implemented by authoritative
//     publication plus owner visibility convergence. Group is not yet a
//     project/session publication state, so it resolves to private. That is a move
//     TOWARDS privacy, making a disclosed fallback safe.
//   - redaction.level: a configured maximum cannot be applied, and every weaker
//     level protects LESS. Proceeding would publish content at a protection level
//     the user did not choose, which is a move AWAY from privacy. There is no safe
//     substitute, so the run REFUSES and names the change the user has to make.
//
// A downgrade is disclosable only when it cannot harm the user. When it can, the
// only honest answer is to stop.
//
// The visibility setting is never rewritten in the loaded configuration: the
// disclosure has to be able to name what the user asked for. The redaction level
// is rewritten only along the one direction that adds protection, and only when
// the user has accepted it - see ConfigRedactionTransition.
//
// Both resolutions derive from the closed enum sets rather than from a comparison
// against one named member, so a value added to either contract is handled
// automatically instead of silently becoming a false claim.

// RedactionLevelDisposition is what this version does with a redaction level.
//
// The standalone redaction engine knows three levels and keeps knowing them: its rule
// activation thresholds are expressed in them and its conformance corpus pins
// their outputs. What CHANGED is that a user may no longer choose among them.
// This type is the whole difference, and it is a closed set over a closed set:
// every level the engine defines has exactly one disposition here.
//
// Splitting "can the engine apply it" from "may a user choose it" is what lets
// standard become the single offered level without deleting anything from the
// engine, and it is why the three axes stay separate: what a rule DETECTS
// (redact.Category), the level at which it ACTIVATES (redact.RedactionLevel),
// and whether a user may SELECT that level (this type).
type RedactionLevelDisposition int

const (
	// RedactionLevelDispositionUnknown is the zero value, and the disposition of
	// any level this file has not been taught about. It fails closed: an unknown
	// level is refused rather than guessed at.
	RedactionLevelDispositionUnknown RedactionLevelDisposition = iota
	// RedactionLevelDispositionOffered is a level a user may choose, and which
	// runs exactly as chosen.
	RedactionLevelDispositionOffered
	// RedactionLevelDispositionRaised is a level this version still ACCEPTS in an
	// existing configuration but no longer OFFERS. A configuration carrying it
	// runs at RecommendedRedactionLevel instead, which redacts strictly more, and
	// the change is disclosed. Asking for it directly - by flag, by request field,
	// or from the wizard - is refused instead, because an explicit choice quietly
	// turned into a different one is the dishonesty this whole file exists to
	// avoid.
	RedactionLevelDispositionRaised
	// RedactionLevelDispositionRefused is a level this version cannot apply at
	// all. The run stops, wherever the level came from.
	RedactionLevelDispositionRefused
)

// AllRedactionLevelDispositions is every answer this type can give, INCLUDING
// the fail-closed unknown one.
//
// The unknown value is a member because it is a real answer with real behaviour -
// every surface refuses on it - not a hole in the enum. A corpus walking this set
// therefore has to exercise the arm that is unreachable through today's inputs,
// which is the arm that can otherwise be deleted without anything going red.
//
// Its only consumer today is a test corpus, and that is a considered stopping
// point rather than an oversight. It is not a shim: a shim exists to give a test
// an entry point it otherwise lacks, whereas this states a contract about the
// type, and the corpus walks it precisely because it is the type's own statement
// of its members. The stronger form is an enumeration some production predicate
// DERIVES from, because then it cannot drift from the type without breaking the
// build - redact.AllRedactionLevels became that when IsValid and Ord were made to
// read it. If a predicate here can be expressed that way, prefer it.
var AllRedactionLevelDispositions = []RedactionLevelDisposition{
	RedactionLevelDispositionUnknown,
	RedactionLevelDispositionOffered,
	RedactionLevelDispositionRaised,
	RedactionLevelDispositionRefused,
}

func (d RedactionLevelDisposition) String() string {
	switch d {
	case RedactionLevelDispositionOffered:
		return "offered"
	case RedactionLevelDispositionRaised:
		return "raised"
	case RedactionLevelDispositionRefused:
		return "refused"
	}
	return "unknown"
}

// redactionLevelDispositions is the static table every surface resolves through.
//
// It is a map rather than a switch so the test corpus can require a row for
// every level the redaction module defines: a level added to the engine with no entry here
// resolves to Unknown and is refused, and the closed-set guard in the corpus
// turns red rather than letting it ship unhandled.
//
//   - minimal is Raised, not Refused. The engine applies it correctly; it simply
//     protects less than standard, and raising it adds protection, so an existing
//     configuration carrying it keeps working instead of dead-ending its owner.
//
//   - maximum is Refused, and this entry is the whole of that decision: a static
//     row, no build tag, no capability probe. Its distinguishing behaviour is
//     anonymising code identifiers in transcript content, and the parser that
//     needs is linked in only when Peasant is built with cgo - so OFFERING it
//     would make the same stored configuration run on a locally-built Peasant and
//     dead-end on a released one. Refusing everywhere is what keeps redaction
//     independent of how the binary was compiled. Every weaker level protects
//     less, so there is nothing safe to substitute.
//
//     THE REASON GIVEN TO THE USER HAS NOW BEEN WRONG TWICE, in opposite
//     directions, and both are recorded because the reason is GENERATED into the
//     web wizard - a stale one is machine-copied to a second surface rather than
//     merely sitting in a comment.
//
//     It first ended "and nothing on the outward path does that". True when
//     written; this repository falsified it, because push now redacts transcript
//     content through the same engine and at maximum the outward path anonymises
//     identifiers too (measured: a fenced Go block leaves as
//     "func id2() { id1.Println(...) }").
//
//     Its replacement gave the parser's ABSENCE as the cause - "the released
//     binaries are built without it". That is false in any build made with cgo,
//     where redact.MaximumAvailable is true and the parser is linked in, while
//     this static row refuses the level anyway: a user was told their binary
//     lacked something it had. The lesson both times is the same one. A reason
//     that names a CAPABILITY has to be re-checked whenever the capability moves;
//     a reason that names the POLICY is true wherever the policy is, and this
//     policy is the row directly below.
var redactionLevelDispositions = map[redact.RedactionLevel]RedactionLevelDisposition{
	redact.Minimal:  RedactionLevelDispositionRaised,
	redact.Standard: RedactionLevelDispositionOffered,
	redact.Maximum:  RedactionLevelDispositionRefused,
}

// RedactionLevelDispositionOf returns what this version does with a level.
//
// An unset level is Offered: it resolves to the default, which is offered. Any
// level with no entry in the table - including one added to the engine later -
// is Unknown, which every caller treats as a refusal.
func RedactionLevelDispositionOf(level redact.RedactionLevel) RedactionLevelDisposition {
	if level == "" {
		return RedactionLevelDispositionOffered
	}
	return redactionLevelDispositions[level]
}

// OfferedRedactionLevels are the levels a user may CHOOSE: the wizard's options,
// the values `peasant redact --level` accepts, and the values the two sync
// request fields accept. Derived from the disposition table so a menu can never
// offer a level the product would then refuse or silently change.
//
// It is ordered by the engine's own ordinal so the wizard's option order and the
// menu string are stable rather than map-iteration order.
var OfferedRedactionLevels = levelsWithDisposition(RedactionLevelDispositionOffered)

// SupportedRedactionLevels are the levels a stored configuration may carry
// without refusing the run: the offered levels plus the ones that are raised to
// an offered level.
//
// NOTHING IN PRODUCTION READS IT. An earlier version of this comment ended "a
// level absent from this set stops every surface", which describes consumers that
// no longer exist: what stops a surface is RedactionLevelSupported, and that
// predicate asks the disposition table directly rather than searching this slice.
// The set had three production readers before the disposition table replaced
// them, and it kept the sentence describing them.
//
// It is retained deliberately, as a DERIVED cross-check rather than dead code.
// policy_test.go asserts it against the disposition table for every level the
// engine defines, so a hardcoded list that DISAGREES with the table fails there -
// which is what makes it a cross-check. Test corpora also walk it to enumerate
// the levels a surface must not refuse.
//
// What it does NOT catch, measured rather than assumed: a hardcoded list whose
// value happens to equal the derived one is green. Comparing values cannot see a
// literal that is currently correct; it can only see a literal that is wrong. An
// earlier version of this comment claimed hardcoding fails "even though
// production behaviour would be unchanged", which generalised from a mutation
// that used a DIFFERENT value - the check is narrower than that sentence, and the
// sentence was the kind that gets believed instead of re-run.
//
// If it ever gains a production reader, say so here.
var SupportedRedactionLevels = append(
	levelsWithDisposition(RedactionLevelDispositionOffered),
	levelsWithDisposition(RedactionLevelDispositionRaised)...,
)

// levelsWithDisposition collects the engine's levels carrying one disposition,
// in the engine's own ordinal order.
func levelsWithDisposition(want RedactionLevelDisposition) []redact.RedactionLevel {
	var levels []redact.RedactionLevel
	for level, disposition := range redactionLevelDispositions {
		if disposition == want {
			levels = append(levels, level)
		}
	}
	slices.SortFunc(levels, func(a, b redact.RedactionLevel) int { return a.Ord() - b.Ord() })
	return levels
}

// RecommendedRedactionLevel is the level a refusal tells the user to choose, the
// level a no-longer-offered configuration is raised to, and the level BaseConfig
// writes.
const RecommendedRedactionLevel = redact.Standard

// defaultRedactionLevel is the level assumed when none was configured, matching
// the level BaseConfig writes.
const defaultRedactionLevel = redact.Standard

// ImplementedVisibilities are the values this Peasant version can apply through
// authoritative content publication followed, when needed, by owner visibility convergence.
// Group remains outside project/session publication receipts and safely falls
// back to private with disclosure.
var ImplementedVisibilities = []Visibility{VisibilityPrivate, VisibilityPublic}

// FallbackVisibility is the visibility a configured-but-unimplemented value
// resolves to. It is the most restrictive implemented value, so a downgrade can
// never widen access.
const FallbackVisibility = VisibilityPrivate

// ErrUnsupportedRedactionLevel marks a run refused because the configured
// redaction level is one this version cannot apply. Callers classify with
// errors.Is - the web handlers map it to a client error rather than a 500.
var ErrUnsupportedRedactionLevel = errors.New("redaction level not supported in this version")

// UnsupportedRedactionLevelError is the refusal itself, carrying the context that
// makes it actionable.
//
// Operation, Step, and Impact are genuinely per-surface and are supplied by the
// caller. Everything else - the what, the why, the remedy, the menu - is rendered
// here from RedactionRefusalReason and the offered set, so the six sites that
// construct this error state one answer rather than six.
//
// The reason is SHARED CODE, not a shared intention. It used to be written out
// again in internal/api, where nothing compared the two, and each copy was pinned
// by its own fixtures - so both sides looked thoroughly tested while a maintainer
// improving one of them would ship two different answers to the same question.
// The remaining copy is in the web wizard, across a language boundary; it is
// GENERATED from this function rather than typed again, and its freshness gate
// fails if this text changes without a regeneration.
type UnsupportedRedactionLevelError struct {
	// Level is the configured level that was refused, quoted back to the user.
	Level redact.RedactionLevel
	// Source names where the level came from, so the user knows what to edit -
	// a configuration file path, or the flag or request field that carried it.
	Source string
	// Operation is what refused: the command or handler.
	Operation string
	// Step is the point it refused at.
	Step string
	// Impact is what was and was not done, in the caller's own terms.
	Impact string
}

func (e *UnsupportedRedactionLevelError) Error() string {
	return fmt.Sprintf(
		"%s refused to run: the %s redaction level is not supported in this version.\n"+
			"Why: %s.\n"+
			"Where: %s, from %s.\n"+
			"When: %s.\n"+
			"Means: %s Peasant refuses rather than quietly applying weaker redaction than you chose.\n"+
			"Fix: set redaction.level to %s, then run the command again. %s "+
			"Levels this version offers: %s.",
		e.Operation, e.Level,
		RedactionRefusalReason(e.Level, "continuing would protect less of your content than you asked for"),
		e.Operation, e.Source, e.Step, e.Impact,
		RedactionLevelChoicePhrase(), redactionScopeSentence, RedactionLevelMenu())
}

// Unwrap ties the typed error to the sentinel so errors.Is classifies it.
func (e *UnsupportedRedactionLevelError) Unwrap() error { return ErrUnsupportedRedactionLevel }

// ErrUnofferedRedactionLevel marks a run refused because it was ASKED for a level
// this version no longer offers, by flag or by request field. It is a separate
// sentinel from ErrUnsupportedRedactionLevel because the two conditions are
// genuinely different: the level here works, it is simply not a choice any more,
// and a stored configuration carrying it keeps running. Only a direct request is
// refused.
var ErrUnofferedRedactionLevel = errors.New("redaction level no longer offered")

// UnofferedRedactionLevelError refuses a level a caller asked for directly.
//
// The alternative would be to silently substitute the recommended level. That is
// safe for the DATA - the substitute redacts strictly more - but it is not safe
// for the caller, who may be telling their own user what they selected. An
// explicit choice answered with different behaviour is exactly the dishonesty this
// package exists to prevent, so a direct request stops and says so. A level found
// in a stored configuration is raised instead, because there is nobody at the
// keyboard to tell.
type UnofferedRedactionLevelError struct {
	// Level is the level that was asked for, quoted back to the caller.
	Level redact.RedactionLevel
	// Source names where the level came from: the flag or the request field.
	Source string
	// Operation is what refused: the command or handler.
	Operation string
	// Step is the point it refused at.
	Step string
	// Impact is what was and was not done, in the caller's own terms.
	Impact string
}

func (e *UnofferedRedactionLevelError) Error() string {
	return fmt.Sprintf(
		"%s refused to run: the %s redaction level is no longer offered.\n"+
			"Why: %s.\n"+
			"Where: %s, from %s.\n"+
			"When: %s.\n"+
			"Means: %s Peasant refuses a level you asked for by name rather than quietly running a different one. An "+
			"existing configuration still set to %s keeps working: it runs at %s and says so.\n"+
			"Fix: ask for %s instead, or drop the setting and let the default apply. %s "+
			"Levels this version offers: %s.",
		e.Operation, e.Level,
		RedactionRefusalReason(e.Level, "there is no longer a weaker choice to make"),
		e.Operation, e.Source, e.Step, e.Impact, e.Level, RecommendedRedactionLevel,
		RedactionLevelChoicePhrase(), redactionScopeSentence, RedactionLevelMenu())
}

// Unwrap ties the typed error to the sentinel so errors.Is classifies it.
func (e *UnofferedRedactionLevelError) Unwrap() error { return ErrUnofferedRedactionLevel }

// redactionScopeSentence is what redaction actually does, in one sentence, shared
// by every surface that has to describe it.
//
// It used to say transcript content was published as recorded. That stopped being
// true when push began applying the same content redaction `peasant redact`
// applies, through the same function - one pipeline, two entrypoints. The hedge
// is the part that must survive any rewording: matching finds KNOWN patterns and
// cannot promise it found every one.
//
// It is deliberately bounded and deliberately hedged. Pattern matching is best
// effort: it finds KNOWN patterns and cannot promise it found every one. A
// completeness claim would be a defect, so the single sentence that makes the
// claim lives here once instead of being re-phrased at each surface, where one
// copy would eventually lose the hedge.
//
// IT USED TO END "a publish carrying secrets is separately rejected by the
// village's own scan", and that promised a backstop wider than the one that
// exists: the village scans the TRANSCRIPT part and not the metadata part beside
// it, so the check does not cover everything a publish carries. The clause began
// as one inline CLI remedy and was hoisted into this shared sentence, which
// carried it to the onboarding screen, every push, both sync refusals and the
// generated web policy at once - the same leverage that makes one hedge in one
// place worth having makes one over-claim expensive. Nothing replaces it: what
// this sentence can say honestly is what Peasant itself does.
const redactionScopeSentence = "It redacts known patterns of secrets, personal data and identifying paths in both " +
	"metadata and transcript content, which is best effort and not a guarantee that every one was found."

// RedactionScopeSentence is redactionScopeSentence for surfaces outside this
// package - the wizard copy and the CLI notices - so the hedge cannot be dropped
// by a re-wording somewhere else.
func RedactionScopeSentence() string { return redactionScopeSentence }

// RedactionLevelSupported reports whether a level found in a stored configuration
// lets the run proceed. An unset level is supported: it resolves to the default,
// which is.
//
// This is the check for a CONFIGURED level. A level a caller asked for directly
// must go through RedactionLevelOffered instead, which is stricter.
func RedactionLevelSupported(level redact.RedactionLevel) bool {
	switch RedactionLevelDispositionOf(level) {
	case RedactionLevelDispositionOffered, RedactionLevelDispositionRaised:
		return true
	}
	return false
}

// RedactionLevelOffered reports whether a caller may ASK for this level by name.
// An unset level is offered: it resolves to the default, which is.
//
// This is the check for a level arriving from a flag, a request field, or the
// wizard. It is strictly narrower than RedactionLevelSupported: a level that is
// merely raised is accepted in a configuration and refused as a request.
func RedactionLevelOffered(level redact.RedactionLevel) bool {
	return RedactionLevelDispositionOf(level) == RedactionLevelDispositionOffered
}

// RedactionLevelMenu returns the OFFERED levels as a comma-separated string,
// derived from the set so no remediation text can name a level that is not a
// choice. It is a LABEL - "Levels this version offers: standard" - and reads
// correctly at any size. Prose needs one of the three renderers below instead.
func RedactionLevelMenu() string {
	values := make([]string, 0, len(OfferedRedactionLevels))
	for _, level := range OfferedRedactionLevels {
		values = append(values, level.String())
	}
	return strings.Join(values, ", ")
}

// The three renderers below exist because collapsing the menu to one member left
// user-facing prose that only parsed for a plural set: "fix: set redactionLevel
// to one of standard" is not English, and the fix line is the one line in an
// actionable error whose whole job is to be read and followed. Half the number
// agreement was hand-corrected for the singular at the time, which set up the
// mirror of the same defect: hand-tuned singular prose reads wrong the day a
// second level is offered - the widening this file goes to some length to
// support. The SET is derived from the disposition table, so the grammar around
// it is derived too.

// RedactionLevelChoicePhrase renders the offered levels as the object of an
// instruction: "standard", or "one of minimal, standard".
func RedactionLevelChoicePhrase() string { return levelChoicePhrase(OfferedRedactionLevels) }

// OfferedRedactionLevelsSentence states what is offered, in agreement with how
// many there are: "the level this version offers is standard", or "the levels
// this version offers are minimal and standard".
func OfferedRedactionLevelsSentence() string { return offeredLevelsSentence(OfferedRedactionLevels) }

// offeredLevelsClause is the same fact as the subject of a clause: "standard is
// now the single level this version offers", or "minimal and standard are now the
// levels this version offers".
func offeredLevelsClause() string { return levelsClause(OfferedRedactionLevels) }

// The three take the set as an argument rather than reading the package variable,
// so a corpus can render each of them over a set of every size. A helper that can
// only be exercised at the one size the product currently ships is a helper whose
// widening behaviour nobody has ever seen.

func levelChoicePhrase(levels []redact.RedactionLevel) string {
	if len(levels) == 1 {
		return levels[0].String()
	}
	return "one of " + joinLevels(levels, ", ")
}

func offeredLevelsSentence(levels []redact.RedactionLevel) string {
	if len(levels) == 1 {
		return "the level this version offers is " + levels[0].String()
	}
	return "the levels this version offers are " + joinLevels(levels, " and ")
}

func levelsClause(levels []redact.RedactionLevel) string {
	if len(levels) == 1 {
		return levels[0].String() + " is now the single level this version offers"
	}
	return joinLevels(levels, " and ") + " are now the levels this version offers"
}

// joinLevels renders a list with the final separator spelled out, so a set of
// three reads "minimal, standard and maximum" rather than as a bare CSV.
func joinLevels(levels []redact.RedactionLevel, finalSeparator string) string {
	values := make([]string, 0, len(levels))
	for _, level := range levels {
		values = append(values, level.String())
	}
	if len(values) < 2 {
		return strings.Join(values, "")
	}
	return strings.Join(values[:len(values)-1], ", ") + finalSeparator + values[len(values)-1]
}

// RedactionRefusalReason states WHY a level is not something a caller may ask for,
// derived from the disposition table.
//
// It is the single rendering of that reason for the whole product: the two typed
// refusal errors above, both web request surfaces, and - through generation - the
// wizard. consequence is the caller-specific tail, because what follows "so" is
// genuinely different at each surface: a CLI run stops, a preview would describe
// nothing, a publish would not apply what was named.
//
// The arms are separate sentences on purpose. A level this version cannot apply
// and a level it simply no longer offers are different facts, and one blended
// sentence covering both would be false for one of them.
func RedactionRefusalReason(level redact.RedactionLevel, consequence string) string {
	switch RedactionLevelDispositionOf(level) {
	case RedactionLevelDispositionRaised:
		return fmt.Sprintf("%s redacts less than %s, and %s, so %s",
			level, RecommendedRedactionLevel, offeredLevelsClause(), consequence)
	case RedactionLevelDispositionRefused:
		// The reason has to be true of the binary the reader is running, and this
		// refusal is UNCONDITIONAL - no build tag, no capability probe, a static
		// disposition. An earlier version of this sentence gave the code parser's
		// absence as the cause, which is false in any build made with cgo: there
		// the parser IS linked in and the product refuses anyway. What is true in
		// every build is the policy, so the policy is what it says.
		return fmt.Sprintf("%s redaction anonymises code identifiers in transcript content, and this version applies the "+
			"same redaction on every build of Peasant rather than one that depends on how the binary was compiled, so %s",
			level, consequence)
	}
	// Unknown disposition: fail closed and say so plainly rather than inventing a
	// reason for a level nobody taught this version about.
	return fmt.Sprintf("this version has no defined behaviour for the %s redaction level, so %s", level, consequence)
}

// ConfigRedactionTransition names a redaction level found in a stored
// configuration that this version no longer offers, together with the level it
// now runs as. The zero value means no transition happened.
//
// It exists as its own type rather than a pair of levels because the transition
// has to be DISCLOSED and, separately, PERSISTED, and the two happen at different
// times: disclosure at every run, persistence only once the user has accepted it.
// Nothing here writes to disk.
type ConfigRedactionTransition struct {
	// From is the level the configuration carries.
	From redact.RedactionLevel
	// To is the level the run applies instead. It is always the stricter of the
	// two: the transition only ever adds protection.
	To redact.RedactionLevel
}

// Occurred reports whether the configuration named a level that is being changed
// FOR MORE PROTECTION.
//
// The direction is part of the test, not a separate documented hope. To is
// documented above as always the stricter of the two, and Disclosure below tells
// the user in as many words that the substitute "redacts MORE than you had
// configured; nothing is protected less". A pair pointing the other way would make
// that statement false in the one direction this whole file exists to prevent, so
// the pair reports no transition and nothing is disclosed about it. The resolver
// cannot build such a pair - it refuses instead of substituting downward - and the
// policy corpus rejects a row asserting one, so this is the third fence rather
// than the only one.
func (t ConfigRedactionTransition) Occurred() bool {
	return t.From != "" && t.To != "" && t.To.Ord() > t.From.Ord()
}

// Disclosure is the full what/why/means/fix statement for a transition, or "" when
// there is nothing to disclose. Shared verbatim by every surface so the wizard,
// the push output, and the hook notices cannot disagree about what happened.
func (t ConfigRedactionTransition) Disclosure() string {
	if !t.Occurred() {
		return ""
	}
	return fmt.Sprintf(
		"notice: redaction.level is %s in your configuration, which this version no longer offers.\n"+
			"Why: %s redacted less than %s, and %s is now the single level, so there is no weaker choice to keep.\n"+
			"Means: this run redacts at %s instead, which redacts MORE than you had configured; nothing is protected less. %s\n"+
			"Fix: nothing to do - the change applies automatically. Set redaction.level to %s to stop seeing this notice.",
		t.From, t.From, t.To, t.To, t.To, redactionScopeSentence, t.To)
}

// BriefDisclosure is the one-line form of Disclosure, for --quiet output.
func (t ConfigRedactionTransition) BriefDisclosure() string {
	if !t.Occurred() {
		return ""
	}
	return fmt.Sprintf(
		"notice: redaction.level %s is no longer offered; this run redacts at %s, which redacts more, nothing to do",
		t.From, t.To)
}

// EffectiveRedactionPolicy is the outcome of resolving a configured redaction
// level against what this version offers. Configured is what the configuration
// carries; Effective is what the run actually applies.
//
// It mirrors VisibilityPolicy deliberately: same shape, same Disclosure contract,
// opposite direction of travel. Visibility resolves DOWN towards less sharing;
// redaction resolves UP towards more redaction. Both are safe directions, and both
// have to be said out loud.
type EffectiveRedactionPolicy struct {
	// Configured is the level the configuration carries, defaulted when unset.
	Configured redact.RedactionLevel
	// Effective is the level the run applies, or "" when there is none: a level
	// this version refuses has no honest substitute, and an unset level is not
	// valid anywhere, so using it fails rather than quietly protecting less.
	//
	// The empty string IS the signal. A Refused() predicate was added beside this
	// and removed again: no production caller ever asked it, and its only consumer
	// restated the same fact on the next line. A second way to spell a condition,
	// with nothing depending on either spelling, is one more thing to keep in step
	// for no reader's benefit - unlike Raised(), which has real callers and hides
	// a comparison worth naming.
	Effective redact.RedactionLevel
	// Disposition is what this version does with Configured. It is carried so a
	// caller can tell a refusal from a resolution without asking the table a
	// second question and risking a different answer.
	Disposition RedactionLevelDisposition
	// Transition is the change from Configured to Effective, or the zero value
	// when the configured level is applied as it stands.
	Transition ConfigRedactionTransition
}

// Raised reports whether the run redacts more than the configuration asked for.
func (p EffectiveRedactionPolicy) Raised() bool { return p.Transition.Occurred() }

// Disclosure is the transition's full statement, or "" when there is nothing to
// disclose.
func (p EffectiveRedactionPolicy) Disclosure() string { return p.Transition.Disclosure() }

// BriefDisclosure is the transition's one-line statement, for --quiet output.
func (p EffectiveRedactionPolicy) BriefDisclosure() string { return p.Transition.BriefDisclosure() }

// ResolveRedactionPolicy resolves the level a run applies from the level a
// configuration carries.
//
// There is no per-operation floor parameter, and its absence is the point. There
// used to be one, because publishing ran at Standard while a local command could
// run at whatever was configured - which meant the SAME configuration produced
// different protection depending on which command read it, and the web push path
// simply forgot to pass the floor and honoured Minimal. With one offered level
// there is one answer, resolved in one place, and no surface can forget it.
//
// The result derives from OfferedRedactionLevels rather than naming Standard, so a
// second offered level added later resolves correctly instead of being silently
// raised past.
//
// Callers still check RedactionLevelSupported first, because a refusal is a
// different user-facing event with its own wording and its own remedy. This no
// longer DEPENDS on their doing so: a level it cannot resolve upward is refused
// here too, with Effective left unset, rather than substituted downward.
//
// That arm used to fall back to the recommended level and build a transition from
// it. Reaching it means nothing offered is at least as strict as what was
// configured - so the substitute redacts LESS - and the disclosure would then have
// told the user, in as many words, that the run redacted MORE and nothing was
// protected less. Both claims backwards, in the direction that matters, and
// convincing enough not to be questioned. A refusal a caller ignores fails on an
// unset level; a false reassurance does not fail at all.
//
// This does not mutate any configuration - see ConfigRedactionTransition.Apply.
func ResolveRedactionPolicy(configured redact.RedactionLevel) EffectiveRedactionPolicy {
	if !configured.IsValid() {
		configured = defaultRedactionLevel
	}
	policy := EffectiveRedactionPolicy{
		Configured:  configured,
		Effective:   configured,
		Disposition: RedactionLevelDispositionOf(configured),
	}
	if policy.Disposition == RedactionLevelDispositionOffered {
		return policy
	}
	if policy.Disposition == RedactionLevelDispositionRaised {
		// Raise to the weakest level that IS offered but is at least as strict as
		// what was configured. With a single offered level that is simply that
		// level; the loop is what keeps this honest if the menu widens.
		for _, offered := range OfferedRedactionLevels {
			if offered.Ord() >= configured.Ord() {
				policy.Effective = offered
				policy.Transition = ConfigRedactionTransition{From: configured, To: offered}
				return policy
			}
		}
	}
	// Refused, unknown, or raised with nothing strict enough to raise to: there is
	// no honest substitute, so none is invented. Effective is unset, which is not a
	// valid level anywhere, so a caller that skipped its own check fails loudly at
	// the point of use instead of publishing under a level nobody chose.
	policy.Effective = ""
	return policy
}

// VisibilityPolicy is the outcome of resolving a requested visibility against the
// visibilities this version can apply. Configured is what the user asked for;
// Effective is what the village will actually be asked to store.
type VisibilityPolicy struct {
	// Configured is the visibility the user asked for, by flag or configuration,
	// defaulted when neither is set.
	Configured Visibility
	// Effective is the visibility the run will actually publish at.
	Effective Visibility
}

// Downgraded reports whether this version publishes at a narrower visibility than
// the user configured. Narrower is the safe direction, but it is still a claim
// the user has to be told about: they asked for something that will not happen.
func (p VisibilityPolicy) Downgraded() bool {
	return p.Configured != p.Effective
}

// Disclosure is the full what/why/means/fix statement for a downgrade, or "" when
// there is nothing to disclose. Shared verbatim by the push output and the hook
// install/status notices so the two can never disagree.
func (p VisibilityPolicy) Disclosure() string {
	if !p.Downgraded() {
		return ""
	}
	return fmt.Sprintf(
		"warning: %s visibility is not yet implemented in this version.\n"+
			"Why: the current village publish contract cannot apply the configured %s visibility.\n"+
			"Means: your sessions are downgraded safely to %s visibility instead; nothing is over-shared.\n"+
			"Fix: nothing to do - your configuration is already correct and takes effect automatically "+
			"once a version implements it; installing the update is the only step.",
		p.Configured, p.Configured, p.Effective)
}

// BriefDisclosure is the one-line form of Disclosure, for --quiet output.
func (p VisibilityPolicy) BriefDisclosure() string {
	if !p.Downgraded() {
		return ""
	}
	return fmt.Sprintf(
		"warning: %s visibility is not implemented in this version; published %s, nothing to do",
		p.Configured, p.Effective)
}

// EffectiveVisibility resolves the visibility a push will actually publish at.
//
// requested is an explicit per-run override ("" when the caller has none), which
// takes precedence over the configured value, which falls back to the default.
// Membership is tested against ImplementedVisibilities rather than against one
// named member, so every visibility this version cannot apply is disclosed -
// including one added to the contract later.
//
// It never mutates cfg. (It used to say "like EffectiveRedactionLevel", which
// was renamed to ResolveRedactionPolicy - a comparison to a symbol that no
// longer exists sends the next reader looking for it.)
func EffectiveVisibility(requested Visibility, cfg *Config) VisibilityPolicy {
	configured := FallbackVisibility
	switch {
	case requested != "":
		configured = requested
	case cfg != nil && cfg.Push.Visibility != "":
		configured = cfg.Push.Visibility
	}
	effective := FallbackVisibility
	if slices.Contains(ImplementedVisibilities, configured) {
		effective = configured
	}
	return VisibilityPolicy{Configured: configured, Effective: effective}
}

// VisibilityMenu returns the accepted visibility values as a comma-separated
// string, derived from the contract's closed set so a flag's validation message
// can never drift from what the contract actually accepts. It mirrors
// schema.LicenseMenu, which the --license flag validates against.
func VisibilityMenu() string {
	values := make([]string, 0, len(schema.AllVisibilities))
	for _, visibility := range schema.AllVisibilities {
		values = append(values, visibility.String())
	}
	return strings.Join(values, ", ")
}
