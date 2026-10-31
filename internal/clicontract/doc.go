// Package clicontract owns the seams every peasant command shares with a
// program that drives it: how session ids come in (--sessions-from), how
// warnings and empty results go out inside a --json document, when a command
// may prompt, and how --yes is spelled.
//
// docs/cli-contract.md describes the contract for people. The code here is
// the one place each rule is implemented, so a command adopts a rule by
// calling the seam rather than restating it.
package clicontract
