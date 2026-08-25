package sessionorigin

// OpensWithTag reports whether text begins with an opening tag of exactly the
// given wrapper name. It is the exported doorway onto the same boundary test
// the rule itself applies, so a harness adapter deciding WHICH record the rule
// should read matches a wrapper name exactly the way the rule matches one.
//
// It exists because two readers of the same markup that disagree on the tag
// boundary are worse than one: an adapter with its own hand-rolled prefix check
// would accept "<command-name-v2>" where the rule rejects it, and the
// divergence would only show up as a misclassification nobody could locate.
//
// The name itself is never spelled here or by the caller. It comes from the
// redact wrapper catalog, which is the one place that owns harness markup names.
//
// text is expected to be already left-trimmed of whitespace by the caller, in
// the same way the rule trims before asking.
func OpensWithTag(text, name string) bool { return opensWithTag(text, name) }
