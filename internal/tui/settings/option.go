package settings

// Option is one static choice offered by a [Radio] or [MultiSelect] field: the
// Label the user sees and the typed Value written back through the field's
// [Accessor] when it is chosen. Options are STATIC - passed as a variadic list
// to the field constructor at authoring time - because the settings vocabulary
// has exactly one asynchronous data source (the selection [Tree], fed by a
// kit.TreeSource); every other field's choices are known up front, so there is
// deliberately no asynchronous OptionSource to load them.
type Option[T comparable] struct {
	// Label is the human-readable text drawn for the choice.
	Label string
	// Value is the typed value written through the field's Accessor when this
	// option is selected.
	Value T
	// Description is an optional plain-language line explaining what the choice
	// means. A [Radio] draws it under the option list for the highlighted
	// choice, so the user sees the meaning of the value they are about to pick.
	// An empty string draws no help.
	Description string
}
