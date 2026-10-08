package store

import "errors"

// ErrHarmonizedNotImplemented is the refusal every harmonized-model contract
// stub returns (or panics with, for value-returning helpers) until the
// behavior lands. It carries the six actionable parts: what seam is
// missing, why (the behavior has not landed yet), where (the calling
// seam), when (at call time, before any read or write), what it means (no
// state was read or changed), and the fix (run a build with the harmonized
// behavior implemented).
var ErrHarmonizedNotImplemented = errors.New("store: the harmonized session content model seam was called before its implementation landed; no read or write was performed and no state changed; use a build with the harmonized behavior implemented")

// The harmonized representation reuses index format version 2
// (session_generations.index_format_version CHECKs = 2; design §3.3 table 3),
// so no new format version is registered here. The existing IndexFormat
// interface (Version, Validate, Write, Delete, plus the WithIndexFormats
// registration in index_format.go) is the seam the harmonized writer and the
// migration extend; the harmonized handler itself lands with the writer.
// These guards pin that the registered handlers still satisfy the seam the
// harmonized consumers compile against.
var (
	_ IndexFormat = relationalIndexFormat{}
	_ IndexFormat = generationIndexFormat{}
)
