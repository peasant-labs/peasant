package ingest_test

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

var _ ingest.RefAllocator = ingest.RandomRefAllocator{}

// TestRandomRefAllocatorMintsOpaqueRefs pins the production allocator to the
// public-reference contract: a one-letter kind prefix, valid UTF-8 within the
// 96-byte bound, and a fresh random value per call.
func TestRandomRefAllocatorMintsOpaqueRefs(t *testing.T) {
	t.Parallel()
	allocator := ingest.RandomRefAllocator{}
	seen := make(map[string]bool)
	for i := 0; i < 64; i++ {
		entry, err := allocator.NewEntryRef()
		if err != nil {
			t.Fatalf("NewEntryRef() = %v", err)
		}
		if !strings.HasPrefix(string(entry), "e_") {
			t.Fatalf("entry ref %q does not carry the e_ prefix", entry)
		}
		if err := entry.Validate(); err != nil {
			t.Fatalf("entry ref %q is not a valid public reference: %v", entry, err)
		}
		if seen[string(entry)] {
			t.Fatalf("entry ref %q repeats", entry)
		}
		seen[string(entry)] = true

		submission, err := allocator.NewSubmissionRef()
		if err != nil {
			t.Fatalf("NewSubmissionRef() = %v", err)
		}
		if !strings.HasPrefix(string(submission), "s_") {
			t.Fatalf("submission ref %q does not carry the s_ prefix", submission)
		}
		if err := submission.Validate(); err != nil {
			t.Fatalf("submission ref %q is not a valid public reference: %v", submission, err)
		}
		if seen[string(submission)] {
			t.Fatalf("submission ref %q repeats", submission)
		}
		seen[string(submission)] = true
	}
}
