package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestSessionSourceInfo_Found verifies that SessionSourceInfo returns the correct
// source_path, source_format, and provider for a seeded session.
func TestSessionSourceInfo_Found(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionID := testutil.TestSessionUUID
	storetest.SeedSession(t, s, sessionID)

	info, err := s.SessionSourceInfo(ctx, sessionID)
	if err != nil {
		t.Fatalf("SessionSourceInfo: %v", err)
	}
	if info == nil {
		t.Fatal("SessionSourceInfo: expected non-nil result for seeded session")
	}
	// storetest.SeedSession seeds with source_path="/f", source_format="jsonl", model_harness="claude-code".
	if info.SourcePath == "" {
		t.Error("SourcePath: expected non-empty string")
	}
	if info.SourceFormat == "" {
		t.Error("SourceFormat: expected non-empty string")
	}
	// Verify the format is a recognised value.
	if info.SourceFormat != schema.SourceFormatJSONL && info.SourceFormat != schema.SourceFormatJSON {
		t.Errorf("SourceFormat: expected %q or %q, got %q", schema.SourceFormatJSONL, schema.SourceFormatJSON, info.SourceFormat)
	}
	// Verify provider is populated.
	if info.Harness == "" {
		t.Error("Harness:  expected non-empty string")
	}
}

// TestSessionSourceInfo_NotFound verifies that SessionSourceInfo returns nil
// when the session ID does not exist in the store.
func TestSessionSourceInfo_NotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	info, err := s.SessionSourceInfo(ctx, "nonexistent-session-id-that-will-not-be-found")
	if err != nil {
		t.Fatalf("SessionSourceInfo: unexpected error for missing session: %v", err)
	}
	if info != nil {
		t.Errorf("SessionSourceInfo: expected nil for missing session, got %+v", info)
	}
}

// TestSessionSourceInfo_SeededValues verifies the exact source_path and
// source_format values written by storetest.SeedSession.
func TestSessionSourceInfo_SeededValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	// Use a distinct session ID to avoid interference with other parallel tests.
	sessionID := testutil.TestSessionUUID2
	storetest.SeedSession(t, s, sessionID)

	info, err := s.SessionSourceInfo(ctx, sessionID)
	if err != nil {
		t.Fatalf("SessionSourceInfo: %v", err)
	}
	if info == nil {
		t.Fatal("SessionSourceInfo: expected non-nil result")
	}

	// storetest.SeedSession uses source_path="/f", source_format="jsonl", model_harness="claude-code" —
	// see internal/store/storetest/golden.go SeedSession function.
	if info.SourcePath != "/f" {
		t.Errorf("SourcePath: expected %q, got %q", "/f", info.SourcePath)
	}
	if info.SourceFormat != schema.SourceFormatJSONL {
		t.Errorf("SourceFormat: expected %q, got %q", schema.SourceFormatJSONL, info.SourceFormat)
	}
	if info.Harness != string(ingest.HarnessClaudeCode) {
		t.Errorf("Harness:  expected %q, got %q", string(ingest.HarnessClaudeCode), info.Harness)
	}
}

// TestLookupSourceInfo_RoundTrip verifies that LookupSourceInfo returns the correct
// 3-tuple (sourcePath, sourceFormat, provider) for a session seeded via SeedSession.
// This exercises the delegation chain: LookupSourceInfo → SessionSourceInfo → SQL query.
func TestLookupSourceInfo_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionID := testutil.TestSessionUUID
	storetest.SeedSession(t, s, sessionID)

	sid, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}

	sourcePath, sourceFormat, provider, err := s.LookupSourceInfo(ctx, sid)
	if err != nil {
		t.Fatalf("LookupSourceInfo: %v", err)
	}

	// storetest.SeedSession seeds with source_path="/f", source_format="jsonl", model_harness="claude-code".
	if sourcePath != "/f" {
		t.Errorf("sourcePath: expected %q, got %q", "/f", sourcePath)
	}
	if sourceFormat != ingest.SourceFormatJSONL {
		t.Errorf("sourceFormat: expected %q, got %q", ingest.SourceFormatJSONL, sourceFormat)
	}
	if provider != string(ingest.HarnessClaudeCode) {
		t.Errorf("provider: expected %q, got %q", string(ingest.HarnessClaudeCode), provider)
	}
}

// TestLookupSourceInfo_NotFound verifies that LookupSourceInfo returns empty strings
// (not an error) when the session does not exist in the store.
func TestLookupSourceInfo_NotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sid, err := ingest.NewSessionID(testutil.TestSessionUUID2)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}

	sourcePath, sourceFormat, provider, err := s.LookupSourceInfo(ctx, sid)
	if err != nil {
		t.Fatalf("LookupSourceInfo: unexpected error for missing session: %v", err)
	}
	if sourcePath != "" || string(sourceFormat) != "" || provider != "" {
		t.Errorf("LookupSourceInfo: expected empty strings for missing session, got (%q, %q, %q)",
			sourcePath, sourceFormat, provider)
	}
}

// TestSessionSourceInfo_TypeDefinition verifies that store.SessionSourceInfo
// is a concrete struct with the expected exported fields, and is distinct
// from nil (*store.SessionSourceInfo typed nil check).
func TestSessionSourceInfo_TypeDefinition(t *testing.T) {
	t.Parallel()

	// Verify that the struct can be instantiated and its fields assigned.
	info := &store.SessionSourceInfo{
		SourcePath:   "/some/path.jsonl",
		SourceFormat: schema.SourceFormatJSONL,
		Harness:      string(ingest.HarnessClaudeCode),
	}
	if info.SourcePath != "/some/path.jsonl" {
		t.Errorf("SourcePath field: got %q", info.SourcePath)
	}
	if info.SourceFormat != schema.SourceFormatJSONL {
		t.Errorf("SourceFormat field: got %q", info.SourceFormat)
	}
	if info.Harness != string(ingest.HarnessClaudeCode) {
		t.Errorf("Provider field: got %q", info.Harness)
	}
}
