package ingest_test

import (
	"errors"
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

// TestMergeLatestAttemptAliases pins the coexistence prior merge: a shared key
// whose refs agree keeps that ref, a key only the latest attempt introduced is
// added under the authority's identity, and a shared key whose refs disagree
// refuses with the actionable conflict category rather than silently rebinding
// either generation's content. The authority's own state is never mutated.
func TestMergeLatestAttemptAliases(t *testing.T) {
	t.Parallel()
	authority := ingest.NewProjectionPriorState()
	authority.Entries["shared"] = "e_authority"
	authority.Submissions["accept"] = "s_authority"

	attempt := ingest.NewProjectionPriorState()
	attempt.Entries["shared"] = "e_authority"
	attempt.Entries["introduced"] = "e_attempt"
	attempt.Submissions["accept"] = "s_authority"
	attempt.Submissions["accept-introduced"] = "s_attempt"

	merged, err := ingest.MergeLatestAttemptAliases(authority, attempt)
	if err != nil {
		t.Fatalf("MergeLatestAttemptAliases: %v", err)
	}
	if merged.Entries["shared"] != "e_authority" {
		t.Fatalf("shared entry ref = %q, want the authority's agreeing ref", merged.Entries["shared"])
	}
	if merged.Entries["introduced"] != "e_attempt" {
		t.Fatalf("introduced entry ref = %q, want the attempt's own ref", merged.Entries["introduced"])
	}
	if merged.Submissions["accept"] != "s_authority" {
		t.Fatalf("shared submission ref = %q, want the authority's agreeing ref", merged.Submissions["accept"])
	}
	if merged.Submissions["accept-introduced"] != "s_attempt" {
		t.Fatalf("introduced submission ref = %q, want the attempt's own ref", merged.Submissions["accept-introduced"])
	}

	entryConflict := ingest.NewProjectionPriorState()
	entryConflict.Entries["shared"] = "e_other"
	if _, err := ingest.MergeLatestAttemptAliases(authority, entryConflict); !errors.Is(err, ingest.ErrPriorAliasConflict) {
		t.Fatalf("entry conflict error = %v, want ErrPriorAliasConflict", err)
	} else if !strings.Contains(err.Error(), "shared") || !strings.Contains(err.Error(), "e_other") {
		t.Fatalf("entry conflict error %q does not name the key and both refs", err)
	}
	submissionConflict := ingest.NewProjectionPriorState()
	submissionConflict.Submissions["accept"] = "s_other"
	if _, err := ingest.MergeLatestAttemptAliases(authority, submissionConflict); !errors.Is(err, ingest.ErrPriorAliasConflict) {
		t.Fatalf("submission conflict error = %v, want ErrPriorAliasConflict", err)
	}
	if authority.Entries["shared"] != "e_authority" || len(authority.Entries) != 1 {
		t.Fatalf("the authority state was mutated by the merge: %+v", authority.Entries)
	}
}
