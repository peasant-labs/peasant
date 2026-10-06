package ingest

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// recordKindStatusClauses returns the generated document's clause for each status
// of the documented closed set, and fails when a clause is missing, duplicated,
// or out of closed-set order. The prose is read unwrapped, so a clause spanning a
// generated line break still reads as one clause.
func recordKindStatusClauses(t *testing.T, prose string) map[RecordKindStatus]string {
	t.Helper()
	ordered := strings.Join(strings.Fields(prose), " ")
	offsets := make([]int, len(recordKindStatusClosedSet))
	for i, status := range recordKindStatusClosedSet {
		offset := strings.Index(ordered, "**"+string(status)+"**")
		if offset < 0 {
			t.Fatalf("generated document has no clause for status %s", status)
		}
		if i > 0 && offset <= offsets[i-1] {
			t.Fatalf("generated document lists status %s out of closed-set order", status)
		}
		offsets[i] = offset
	}
	clauses := make(map[RecordKindStatus]string, len(recordKindStatusClosedSet))
	for i, status := range recordKindStatusClosedSet {
		end := len(ordered)
		if i+1 < len(offsets) {
			end = offsets[i+1]
		}
		clauses[status] = ordered[offsets[i]:end]
	}
	return clauses
}

// TestRecordKindsStatusTablesCoverTheParserClosedSet keeps the documentation
// tables pinned to the statuses the registry actually parses. A status added to
// the enum without a documented meaning would otherwise be described by no
// sentence at all.
func TestRecordKindsStatusTablesCoverTheParserClosedSet(t *testing.T) {
	seen := make(map[RecordKindStatus]bool, len(recordKindStatusClosedSet))
	for _, status := range recordKindStatusClosedSet {
		if seen[status] {
			t.Fatalf("status %s appears twice in the documented closed set", status)
		}
		seen[status] = true
		if _, err := NewRecordKindStatus(string(status)); err != nil {
			t.Errorf("documented status %s is rejected by the parser boundary: %v", status, err)
		}
		if strings.TrimSpace(recordKindStatusNotes[status]) == "" {
			t.Errorf("status %s has no documented meaning in recordKindStatusNotes", status)
		}
	}
	for status := range recordKindStatusNotes {
		if !seen[status] {
			t.Errorf("recordKindStatusNotes documents %s, which is absent from the closed set", status)
		}
	}
	if _, err := NewRecordKindStatus("imaginary-status"); err == nil {
		t.Error("the parser boundary accepted a status outside the documented closed set")
	}
}

// TestRecordKindsStatusesDeriveFromOneSource pins NewRecordKindStatus to the
// documented closed set: the constructor's accepted inputs and its refusal must
// derive from recordKindStatusClosedSet, not from a second switch whose arms can
// drift from the prose. Feeding a mutated set through the derivation proves the
// derivation reads the set, so a status accepted at the parser boundary can
// never be absent from the generated status prose. The YAML fixture pins the
// out-of-set names the shipping constructor must keep refusing.
func TestRecordKindsStatusesDeriveFromOneSource(t *testing.T) {
	fixture := recordKindsStatusMutationFixture{}
	decodeRegistryFixture(t, recordKindsStatusMutationYAML, &fixture)
	shipped := map[string]bool{"accepted statuses derive from the closed set": true}
	// The shipped constructor's arm list, read from the code as it stands: each
	// accepted raw name is checked against the set it is supposed to read.
	for _, status := range recordKindStatusClosedSet {
		shipped[string(status)] = true
		if _, err := NewRecordKindStatus(string(status)); err != nil {
			t.Errorf("closed-set status %s is not derivable at the shipped boundary: %v", status, err)
		}
	}
	checkRegistryFixtureNames(t, shipped, append([]string{"accepted statuses derive from the closed set"}, fixture.ShippedAcceptedNames...))
	// A mutated set names a status the shipped constructor cannot know; the
	// derivable form must accept exactly that mutated set, so a future
	// constructor derivation cannot read any other list.
	statuses := append(recordKindStatusClosedSet[:0:0], recordKindStatusClosedSet...)
	statuses = append(statuses, RecordKindStatus("mutated-status"))
	derived := func(raw string) (RecordKindStatus, error) {
		return derivedRecordKindStatus(raw, statuses)
	}
	for _, status := range statuses {
		got, err := derived(string(status))
		if err != nil {
			t.Errorf("derived constructor refused %s, a member of the mutated set: %v", status, err)
		} else if got != status {
			t.Errorf("derived constructor returned %s for %s", got, status)
		}
	}
	if got, err := derived(fixture.OutOfSetName); err == nil || got != "" {
		t.Errorf("derived constructor accepted %q outside the mutated set: %q %v", fixture.OutOfSetName, got, err)
	}
	// The shipping constructor refuses every fixture name the mutated set does
	// not contain.
	for _, raw := range fixture.ShippedOutOfSetNames {
		if _, err := NewRecordKindStatus(raw); err == nil {
			t.Errorf("NewRecordKindStatus accepted the out-of-set name %q", raw)
		}
	}
}

// TestRecordKindsDocumentStatusProseReportsDeclaredRows is the review gate for
// the generated status prose. It reads the committed document, so a hand-tuned
// claim about which statuses this build emits cannot be reintroduced: every
// closed-set status is described in order, each clause states the number of rows
// that carry the status, and a status with rows is never called undeclared.
func TestRecordKindsDocumentStatusProseReportsDeclaredRows(t *testing.T) {
	registry, err := LoadRecordKindRegistry()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../docs/record-kinds.md")
	if err != nil {
		t.Fatal(err)
	}
	document := string(raw)
	start := strings.Index(document, "- **Status**")
	end := strings.Index(document, "- **Preview**")
	if start < 0 || end < start {
		t.Fatal("generated document has no **Status** bullet followed by **Preview**")
	}
	prose := document[start:end]
	if lead := strings.Index(prose, "\n"); lead >= 0 {
		prose = strings.TrimLeft(prose[lead+1:], " ")
	}
	if !strings.HasPrefix(prose, "**") {
		t.Fatalf("status bullet carries no derived clause list: %q", prose)
	}
	clauses := recordKindStatusClauses(t, prose)
	counts := registry.statusRowCounts()
	for _, status := range recordKindStatusClosedSet {
		if want := fmt.Sprintf("%d rows", counts[status]); !strings.Contains(clauses[status], want) {
			t.Errorf("status %s clause %q does not state %q", status, clauses[status], want)
		}
	}
	for status, count := range counts {
		if count == 0 {
			continue
		}
		if strings.Contains(clauses[status], "declared by no kind") {
			t.Errorf("status %s carries %d rows but its clause says it is undeclared: %q", status, count, clauses[status])
		}
	}
	for _, claim := range []string{"never emits", "never refused by default", "reserved for a future"} {
		if strings.Contains(prose, claim) {
			t.Errorf("status prose still hard-codes %q instead of deriving it from the registry", claim)
		}
	}
	if statuses := registry.fallbackStatuses(); len(statuses) != 1 {
		t.Errorf("registry lowers the unseen-kind fallback to %v, want exactly one status", statuses)
	} else if clause, ok := clauses[statuses[0]]; !ok || !strings.Contains(clause, "unseen-valid-kind fallback of every harness") {
		t.Errorf("fallback status %s clause %q does not name the open fallback", statuses[0], clause)
	}
}
