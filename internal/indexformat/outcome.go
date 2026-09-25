package indexformat

// Outcome is the interpretation a parser assigns to one source node. Adapters
// declare this input-side result; schema.SessionEntry remains the output IR.
type Outcome uint8

const (
	OutcomeText       Outcome = iota // conversation or textual content
	OutcomeToolCall                  // tool invocation
	OutcomeToolResult                // tool output
	OutcomeControl                   // represented non-conversation state
	OutcomeIgnored                   // recognized and intentionally entryless
	OutcomeOpaque                    // unfamiliar; retain evidence at its source position
)

var allOutcomes = [...]Outcome{
	OutcomeText,
	OutcomeToolCall,
	OutcomeToolResult,
	OutcomeControl,
	OutcomeIgnored,
	OutcomeOpaque,
}

// AllOutcomes returns the exact closed outcome set in declaration order.
func AllOutcomes() []Outcome {
	outcomes := allOutcomes
	return append([]Outcome(nil), outcomes[:]...)
}

// IsValid reports whether the outcome belongs to the closed interpretation set.
func (o Outcome) IsValid() bool {
	return o < Outcome(len(allOutcomes))
}

func (o Outcome) String() string {
	switch o {
	case OutcomeText:
		return "text"
	case OutcomeToolCall:
		return "tool-call"
	case OutcomeToolResult:
		return "tool-result"
	case OutcomeControl:
		return "control"
	case OutcomeIgnored:
		return "ignored"
	case OutcomeOpaque:
		return "opaque"
	default:
		return "invalid"
	}
}
