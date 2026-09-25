package ingest

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/peasant-labs/schema"
)

// ErrUnknownPositionUnavailable distinguishes old private captures from corrupt
// evidence. Only a bounded local preview may suppress this export refusal.
var ErrUnknownPositionUnavailable = errors.New("stored capture lacks complete source traversal coordinates")

// ProjectRetainedUnknown validates stored evidence and copies the retained JSON
// text verbatim. Capture owns coordinates: entry indices, line numbers and native
// IDs cannot reconstruct traversal positions after known blocks have been folded.
func ProjectRetainedUnknown(entries []schema.SessionEntry, harness Harness) ([]schema.RetainedUnknownRecord, error) {
	projected, err := CollectRetainedUnknown(entries, harness)
	if err != nil {
		return nil, err
	}
	for _, record := range projected {
		if len(record.Payload) > 8<<20 {
			return nil, fmt.Errorf("export retained evidence: payload exceeds the published 8 MiB transfer limit; complete source data remains stored locally; nothing exported or uploaded; use a receiver and contract supporting larger transfers when available")
		}
	}
	if len(projected) > 0 {
		if err := schema.ValidateRetainedUnknown(schema.SessionDetailPayload{RetainedUnknown: projected, Diagnostics: &schema.InterpretationDiagnostics{Partial: true}}); err != nil {
			return nil, fmt.Errorf("export retained evidence: public syntax, position or byte/depth requirements are not met: %w; local evidence is unchanged; nothing exported or uploaded", err)
		}
	}
	return projected, nil
}

// CollectRetainedUnknown certifies LOCAL evidence integrity and source positions.
// It does not impose a public transfer budget on captured source bytes. Source
// adapters own their source-size policy; export separately validates the public
// contract through ProjectRetainedUnknown.
//
// The entire evidence set validates before any legacy missing-position result
// returns: corruption in any record outranks legacy compatibility, so one old
// record can never mask later corruption.
func CollectRetainedUnknown(entries []schema.SessionEntry, harness Harness) ([]schema.RetainedUnknownRecord, error) {
	var projected []schema.RetainedUnknownRecord
	legacy := false
	for _, entry := range entries {
		records, err := RetainedUnknownOf(entry)
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			if (harness != "" && record.Harness != harness) || (entry.Harness != "" && record.Harness != entry.Harness) {
				return nil, &EvidenceIntegrityError{Field: "envelope.harness"}
			}
			position := record.Position.Public
			if position == nil {
				legacy = true
				continue
			}
			projected = append(projected, schema.RetainedUnknownRecord{
				SourceRef: position.SourceRef, RecordIndex: position.RecordIndex, Position: position.Position,
				Pointer: record.Position.JSONPointer, Namespace: record.Namespace, Kind: record.Kind, Payload: string(record.Payload),
			})
		}
	}
	if len(projected) != 0 {
		// Entry folding and partition layout are not source traversal order.
		// Sort by captured coordinates, never payload identity; duplicates remain
		// present for schema validation to reject rather than silently deduping.
		slices.SortStableFunc(projected, func(a, b schema.RetainedUnknownRecord) int {
			if source := cmp.Compare(a.SourceRef, b.SourceRef); source != 0 {
				return source
			}
			return cmp.Compare(a.Position, b.Position)
		})
		if err := validateLocalRetainedUnknown(projected); err != nil {
			return nil, err
		}
	}
	if legacy {
		return nil, fmt.Errorf("project retained unknown evidence: %w; line numbers and native IDs cannot reconstruct block positions; no detail was emitted; re-index with a position-aware adapter", &LegacyPositionUnavailableError{})
	}
	return projected, nil
}

func validateLocalRetainedUnknown(records []schema.RetainedUnknownRecord) error {
	coordinateFail := func() error {
		return &EvidenceIntegrityError{Field: "position.public"}
	}
	payloadFail := func() error {
		return &EvidenceIntegrityError{Field: "envelope.payload"}
	}
	type cursor struct {
		position, record int64
		pointers         *localUnknownPointer
	}
	sources := map[string]cursor{}
	for _, record := range records {
		if record.SourceRef == "" || strings.TrimSpace(record.Kind) == "" || strings.TrimSpace(record.Namespace) == "" ||
			!utf8.ValidString(record.SourceRef) || !utf8.ValidString(record.Kind) || !utf8.ValidString(record.Namespace) || !utf8.ValidString(record.Pointer) || !utf8.ValidString(record.Payload) ||
			record.RecordIndex < 0 || record.RecordIndex > 9007199254740991 || record.Position < record.RecordIndex || record.Position > 9007199254740991 || !validUnknownPointer(record.Pointer) {
			return coordinateFail()
		}
		prior, exists := sources[record.SourceRef]
		if exists && (record.Position <= prior.position || record.RecordIndex < prior.record) {
			return coordinateFail()
		}
		if !exists || record.RecordIndex != prior.record {
			prior.pointers = &localUnknownPointer{}
		}
		if !prior.pointers.insert(record.Pointer) {
			return coordinateFail()
		}
		// Scan through the shared raw-evidence helper so the local syntax
		// depth and byte-budget policy stay single-sourced with capture.
		if err := ScanRawEvidenceDocument([]byte(record.Payload), "envelope.payload"); err != nil {
			return payloadFail()
		}
		sources[record.SourceRef] = cursor{record.Position, record.RecordIndex, prior.pointers}
	}
	return nil
}

type localUnknownPointer struct {
	terminal bool
	children map[string]*localUnknownPointer
}

func (node *localUnknownPointer) insert(pointer string) bool {
	if pointer != "" {
		for _, component := range strings.Split(pointer[1:], "/") {
			if node.terminal {
				return false
			}
			if node.children == nil {
				node.children = map[string]*localUnknownPointer{}
			}
			if node.children[component] == nil {
				node.children[component] = &localUnknownPointer{}
			}
			node = node.children[component]
		}
	}
	if node.terminal || len(node.children) > 0 {
		return false
	}
	node.terminal = true
	return true
}
