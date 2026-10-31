package clicontract

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
)

// WarningCode names the kind of a warning a command reports. It is a closed
// set, so a program reading a --json document can branch on every value.
type WarningCode string

const (
	// WarningSessionRefused is a session the command was asked for and did
	// not process.
	WarningSessionRefused WarningCode = "session_refused"
	// WarningIndexStale is a result read from an index known to be behind its
	// sources.
	WarningIndexStale WarningCode = "index_stale"
	// WarningIndexMaintenance is index maintenance that is due and has not
	// run yet.
	WarningIndexMaintenance WarningCode = "index_maintenance_pending"
	// WarningFilterIgnored is a filter the command accepted and did not apply.
	WarningFilterIgnored WarningCode = "filter_ignored"
)

// AllWarningCodes is the closed set of warning codes.
var AllWarningCodes = []WarningCode{
	WarningSessionRefused,
	WarningIndexStale,
	WarningIndexMaintenance,
	WarningFilterIgnored,
}

// NewWarningCode validates raw against the closed set.
func NewWarningCode(raw string) (WarningCode, error) {
	code := WarningCode(raw)
	if !slices.Contains(AllWarningCodes, code) {
		return "", fmt.Errorf("unknown warning code %q (known: %s)", raw, joinCodes(AllWarningCodes))
	}
	return code, nil
}

// EmptyCode names why a command produced an empty result. It is a closed set.
type EmptyCode string

const (
	// EmptyStoreHasNoSessions is a store that holds no session at all.
	EmptyStoreHasNoSessions EmptyCode = "store_has_no_sessions"
	// EmptyFiltersExcludedAll is a store whose sessions the filters all
	// excluded.
	EmptyFiltersExcludedAll EmptyCode = "filters_excluded_all"
	// EmptyInputListWasEmpty is an input list that named nothing to act on.
	EmptyInputListWasEmpty EmptyCode = "input_list_was_empty"
	// EmptyNothingChanged is a run with nothing new to do.
	EmptyNothingChanged EmptyCode = "nothing_changed"
	// EmptyNotFetchedYet is a local copy of remote state that has not been
	// fetched yet.
	EmptyNotFetchedYet EmptyCode = "not_fetched_yet"
)

// AllEmptyCodes is the closed set of empty-result codes.
var AllEmptyCodes = []EmptyCode{
	EmptyStoreHasNoSessions,
	EmptyFiltersExcludedAll,
	EmptyInputListWasEmpty,
	EmptyNothingChanged,
	EmptyNotFetchedYet,
}

// NewEmptyCode validates raw against the closed set.
func NewEmptyCode(raw string) (EmptyCode, error) {
	code := EmptyCode(raw)
	if !slices.Contains(AllEmptyCodes, code) {
		return "", fmt.Errorf("unknown empty code %q (known: %s)", raw, joinCodes(AllEmptyCodes))
	}
	return code, nil
}

func joinCodes[T ~string](codes []T) string {
	parts := make([]string, len(codes))
	for i, code := range codes {
		parts[i] = string(code)
	}
	return strings.Join(parts, ", ")
}

// Warning is one warning inside a --json document.
type Warning struct {
	Code      WarningCode `json:"code"`
	Message   string      `json:"message"`
	SessionID string      `json:"session_id,omitempty"`
}

// Meta is embedded (flat, not nested) in every command's JSON document.
type Meta struct {
	// Warnings is never null; it is [] when there are none.
	Warnings    []Warning `json:"warnings"`
	EmptyCode   EmptyCode `json:"empty_code,omitempty"`
	EmptyReason string    `json:"empty_reason,omitempty"`
}

// NewMeta returns a Meta with no warnings and no empty reason. Its Warnings
// is [] rather than nil, so the document never carries "warnings": null.
func NewMeta() Meta {
	return Meta{Warnings: []Warning{}}
}

// Collector gathers warnings once and writes them to both places: each
// warning is printed to stderr as it happens, and in --json mode it is also
// kept for the document's warnings array.
type Collector struct {
	mu       sync.Mutex
	stderr   io.Writer
	jsonMode bool
	warnings []Warning
	count    int
}

// NewCollector returns a collector that prints to stderr. jsonMode keeps every
// warning for Meta as well.
func NewCollector(stderr io.Writer, jsonMode bool) *Collector {
	return &Collector{stderr: stderr, jsonMode: jsonMode}
}

// Warn records w. The stderr line is "warning: session <id>: <message>" for a
// warning about one session and "warning: <message>" otherwise.
func (c *Collector) Warn(w Warning) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count++
	if c.jsonMode {
		c.warnings = append(c.warnings, w)
	}
	if c.stderr == nil {
		return
	}
	if w.SessionID != "" {
		fmt.Fprintf(c.stderr, "warning: session %s: %s\n", w.SessionID, w.Message)
		return
	}
	fmt.Fprintf(c.stderr, "warning: %s\n", w.Message)
}

// Count is the number of warnings recorded, in either mode.
func (c *Collector) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

// Meta returns the document fields for this run. empty and reason are left
// out of the document when empty is "".
func (c *Collector) Meta(empty EmptyCode, reason string) Meta {
	c.mu.Lock()
	defer c.mu.Unlock()
	meta := NewMeta()
	meta.Warnings = append(meta.Warnings, c.warnings...)
	meta.EmptyCode = empty
	meta.EmptyReason = reason
	return meta
}

// WriteJSON encodes v (which embeds Meta) to w with two-space indent.
func WriteJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
