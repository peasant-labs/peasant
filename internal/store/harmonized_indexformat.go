package store

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
