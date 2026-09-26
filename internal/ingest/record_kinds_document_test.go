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
