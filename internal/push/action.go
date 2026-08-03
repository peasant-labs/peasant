package push

// PushAction determines how a session is handled during push.
type PushAction int

const (
	// PushWithRedaction redacts if needed, then pushes.
	PushWithRedaction PushAction = iota
	// PushExclude excludes the session from push.
	PushExclude
)

// String implements fmt.Stringer.
func (a PushAction) String() string {
	switch a {
	case PushWithRedaction:
		return "push (redacted)"
	case PushExclude:
		return "exclude"
	default:
		return "unknown"
	}
}
