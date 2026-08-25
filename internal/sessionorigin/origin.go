// Package sessionorigin decides who drove a recorded session and carries the
// closed menu of that decision across ingest, storage, discovery, sharing, and
// the push wire.
//
// The producer knows and the consumer guesses. At ingest Peasant still holds the
// raw harness entry, where a modern Claude Code client writes a structured
// teammate identity and a programmatic entrypoint. This package turns those
// facts into one closed answer, once, so that every later surface reads a
// decision instead of re-deriving one.
package sessionorigin

import "fmt"

// Origin is the closed menu of session origins. Its values are the exact tokens
// the sessions.session_origin CHECK accepts, so a value that round-trips through
// the database is always one of them.
type Origin string

const (
	// User: a person drove it, by typing a prompt or running a slash command.
	// Always visible.
	User Origin = "user"
	// Agent: a program started it. The only value any surface hides, and only
	// ever reached from positive evidence.
	Agent Origin = "agent"
	// Unknown: the origin could not be established. The fail-safe value; it
	// behaves exactly like User on every Peasant surface. It IS sent on the
	// wire, where it instructs the consumer to apply its own rule.
	Unknown Origin = "unknown"
)

// All is the canonical menu, in the order the database CHECK lists it. The
// migration test derives its accept set from this slice, so widening the menu is
// one edit here plus one migration.
var All = []Origin{User, Agent, Unknown}

// Valid reports whether o is one of the three menu values.
//
// The empty string is NOT valid. It is the storage-layer marker for "this cache
// record predates the origin field", which is the absence of an origin rather
// than an origin, and it must be resolved to a menu value before anything treats
// it as one.
func (o Origin) Valid() bool {
	switch o {
	case User, Agent, Unknown:
		return true
	default:
		return false
	}
}

// String returns the stored and wire form of the origin.
func (o Origin) String() string { return string(o) }

// Validate is the fail-closed trust boundary for an origin that arrived from
// outside this package: a database row, a configuration file, a wire payload.
//
// It never substitutes a default, because both substitutions are wrong. Choosing
// user would expose an agent-driven session in every list; choosing agent would
// hide a person's own session. Neither failure is recoverable by the caller
// afterwards, so an unrecognised value is refused instead.
func (o Origin) Validate() error {
	if o.Valid() {
		return nil
	}
	if o == "" {
		return fmt.Errorf(
			"sessionorigin: empty session origin rejected while validating a value from outside the package "+
				"(sessionorigin.Origin.Validate): the empty string marks a cache record written before the origin "+
				"field existed, which is the absence of an origin rather than one of %s; the caller cannot treat it "+
				"as a decision, and no default may be substituted because user would expose an agent-driven session "+
				"in every list while agent would hide a person's own; resolve the record first, by re-mining its "+
				"transcript or by classifying it, then validate the resulting menu value",
			Menu(),
		)
	}
	return fmt.Errorf(
		"sessionorigin: session origin %q is outside the closed menu, rejected while validating a value from "+
			"outside the package (sessionorigin.Origin.Validate): the accepted menu is %s, so the value came from a "+
			"newer build, a hand-edited record, or a corrupt row; the caller must not store or act on it, and no "+
			"default may be substituted because user would expose an agent-driven session in every list while agent "+
			"would hide a person's own; correct the source of the value, or widen the menu in sessionorigin.All and "+
			"the matching database CHECK together",
		string(o), Menu(),
	)
}

// Parse converts a stored or transmitted token into an Origin, refusing anything
// outside the menu. The empty string is refused with the rest: a caller holding a
// record that may predate the origin field resolves that case before it parses.
func Parse(value string) (Origin, error) {
	candidate := Origin(value)
	if err := candidate.Validate(); err != nil {
		return "", err
	}
	return candidate, nil
}

// Menu renders the accepted values for an error message, in canonical order.
func Menu() string {
	rendered := make([]byte, 0, 32)
	for i, origin := range All {
		if i > 0 {
			rendered = append(rendered, ',', ' ')
		}
		rendered = append(rendered, origin.String()...)
	}
	return string(rendered)
}
