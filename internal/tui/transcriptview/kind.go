package transcriptview

import (
	"github.com/peasant-labs/peasant/internal/ingest"
)

// Kind is the rendering classification of one recorded turn: what the reader
// is looking at, which decides the label, the gutter color, and how the body is
// laid out.
//
// It is derived from a turn rather than stored on one because the two fields it
// derives from answer different questions. ingest.Role says WHO produced the
// turn and ingest.EntryType says WHAT the turn carries, and a renderer needs a
// single answer to "how do I draw this". Deriving keeps that collapse in one
// reviewable place instead of at every draw site.
type Kind string

const (
	// KindUser is a turn the person wrote.
	KindUser Kind = "user"
	// KindAssistant is a turn the coding agent wrote.
	KindAssistant Kind = "assistant"
	// KindThinking is an agent reasoning block.
	KindThinking Kind = "thinking"
	// KindTool is a tool invocation and its result.
	KindTool Kind = "tool"
	// KindSystem is harness chrome: a system message, an error, or a run
	// result the harness recorded rather than either party writing.
	KindSystem Kind = "system"
)

// AllKinds is the closed set of rendering classifications, in the order a
// reader meets them. It is what lets a test assert every kind is covered rather
// than only the ones a fixture happens to carry.
var AllKinds = []Kind{KindUser, KindAssistant, KindThinking, KindTool, KindSystem}

// String implements fmt.Stringer.
func (k Kind) String() string { return string(k) }

// IsValid reports whether k is one of AllKinds.
func (k Kind) IsValid() bool {
	switch k {
	case KindUser, KindAssistant, KindThinking, KindTool, KindSystem:
		return true
	}
	return false
}

// Label is the lowercase chrome word the pane tags a turn of this kind with.
// The user's own turns read "you" rather than "user": the pane is showing a
// person their own recorded conversation, and second person is how they refer
// to themselves in it.
func (k Kind) Label() string {
	switch k {
	case KindUser:
		return "you"
	case KindAssistant:
		return "assistant"
	case KindThinking:
		return "thinking"
	case KindTool:
		return "tool"
	case KindSystem:
		return "system"
	default:
		return unknownKindLabel
	}
}

// unknownKindLabel names a turn whose role and entry type this package does not
// recognize. It fails visible rather than closed: the turn is still shown, but
// it is not silently relabeled as one of the kinds it is not.
const unknownKindLabel = "unknown"

// KindOf classifies one recorded turn.
//
// Entry type is consulted BEFORE role because it is the more specific of the
// two: a thinking block and a tool call are both recorded with the assistant's
// role, and reading role first would flatten all three into one. Role decides
// only what entry type leaves open.
func KindOf(turn ingest.Turn) Kind {
	switch turn.EntryType {
	case ingest.EntryTypeThinking:
		return KindThinking
	case ingest.EntryTypeToolUse, ingest.EntryTypeToolResult:
		return KindTool
	case ingest.EntryTypeSystem, ingest.EntryTypeError, ingest.EntryTypeResult:
		return KindSystem
	}
	switch turn.Role {
	case ingest.RoleUser:
		return KindUser
	case ingest.RoleAssistant:
		return KindAssistant
	case ingest.RoleTool:
		return KindTool
	case ingest.RoleSystem:
		return KindSystem
	}
	// A turn carrying tool calls and nothing else identifying is a tool step
	// whichever way it was recorded.
	if len(turn.ToolCalls) > 0 {
		return KindTool
	}
	return KindSystem
}
