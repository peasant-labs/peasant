package gates

// KeyMatch is one raw-key-string-comparison occurrence at a specific line
// of a specific file - the key grep gate's counterpart to ColorMatch.
//
// Detection used to be a hand-written regexp scanner in this file
// (KeyPatterns/FindKeyMatches/ScanForKeyViolations), which review found a
// real false negative in (the equality pattern only checked one operand of
// "=="). That regex machinery is REMOVED: detection now runs entirely on
// ast-grep structural rules (internal/tui/gates/astrules/), which make the
// operand-direction bug class structurally impossible - see
// keys_astgrep_test.go (`//go:build astgrep`), which shells out to
// `ast-grep scan --json` and decodes its output directly into KeyMatch
// values.
//
// KeyMatch itself stays as a small, pure, gate-agnostic struct in this
// UNTAGGED file so the count-comparison logic against the shared
// testdata/legacy_allowlist.yaml (checkKeyAllowlistCounts, keys_test.go) -
// and its mutation-proof tests, which build KeyMatch values by hand with no
// scanning involved - remain hermetic and run under plain `go test ./...`
// with no dependency on the ast-grep binary. Only the "go get real matches
// from ast-grep" step is behind the astgrep build tag.
type KeyMatch struct {
	// Path is repository-relative, "/"-separated.
	Path    string
	Line    int
	Pattern string
	Text    string
}
