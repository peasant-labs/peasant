package ingest_test

import (
	"testing"
	"time"

	"github.com/dayvidpham/bestiary"
	"github.com/peasant-labs/peasant/internal/ingest"
)

// fixedSyncedAt is a deterministic stamp used to assert the empty-LastSynced path.
var fixedSyncedAt = time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)

func f64(v float64) *float64 { return &v }

// TestModelFromBestiary_AllFields verifies every mapped field for a fully
// populated bestiary record, including the value->pointer conversions.
func TestModelFromBestiary_AllFields(t *testing.T) {
	t.Parallel()

	b := bestiary.ModelInfo{
		ID:                    bestiary.ModelID("claude-opus-4-6"),
		Provider:              bestiary.ProviderAnthropic,
		DisplayName:           "Claude Opus 4.6",
		Family:                bestiary.Family("claude-opus"),
		ContextWindow:         200000,
		MaxOutput:             8192,
		Reasoning:             true,
		ToolCall:              true,
		CostInputPerMTok:      f64(15.0),
		CostOutputPerMTok:     f64(75.0),
		CostReasoningPerMTok:  f64(75.0),
		CostCacheReadPerMTok:  f64(1.5),
		CostCacheWritePerMTok: f64(7.5),
		ReleaseDate:           "2025-05-14",
		LastSynced:            "2026-04-08T17:58:57Z",
		// Dropped extras — must NOT influence the mapping.
		Attachment:       true,
		Temperature:      true,
		StructuredOutput: true,
		OpenWeights:      true,
		Knowledge:        "2025-03",
	}

	got := ingest.ModelFromBestiary(b, fixedSyncedAt)

	if got.ModelID != "claude-opus-4-6" {
		t.Errorf("ModelID: got %q, want %q", got.ModelID, "claude-opus-4-6")
	}
	if got.ProviderKey != bestiary.ProviderAnthropic.String() {
		t.Errorf("ProviderKey: got %q, want %q", got.ProviderKey, bestiary.ProviderAnthropic.String())
	}
	if got.DisplayName != "Claude Opus 4.6" {
		t.Errorf("DisplayName: got %q, want %q", got.DisplayName, "Claude Opus 4.6")
	}
	if got.Family == nil || *got.Family != "claude-opus" {
		t.Errorf("Family: got %v, want claude-opus", got.Family)
	}
	if got.ContextWindow == nil || *got.ContextWindow != 200000 {
		t.Errorf("ContextWindow: got %v, want 200000", got.ContextWindow)
	}
	if got.MaxOutput == nil || *got.MaxOutput != 8192 {
		t.Errorf("MaxOutput: got %v, want 8192", got.MaxOutput)
	}
	if !got.Reasoning {
		t.Error("Reasoning: got false, want true")
	}
	if !got.ToolCall {
		t.Error("ToolCall: got false, want true")
	}
	assertF64(t, "CostInputPerMTok", got.CostInputPerMTok, 15.0)
	assertF64(t, "CostOutputPerMTok", got.CostOutputPerMTok, 75.0)
	assertF64(t, "CostReasoningPerMTok", got.CostReasoningPerMTok, 75.0)
	assertF64(t, "CostCacheReadPerMTok", got.CostCacheReadPerMTok, 1.5)
	assertF64(t, "CostCacheWritePerMTok", got.CostCacheWritePerMTok, 7.5)
	if got.ReleaseDate == nil || *got.ReleaseDate != "2025-05-14" {
		t.Errorf("ReleaseDate: got %v, want 2025-05-14", got.ReleaseDate)
	}
	// Non-empty LastSynced is preserved verbatim (NOT stamped with syncedAt).
	if got.LastSynced != "2026-04-08T17:58:57Z" {
		t.Errorf("LastSynced: got %q, want preserved %q", got.LastSynced, "2026-04-08T17:58:57Z")
	}
}

// TestModelFromBestiary_ZeroAndEmptyToNil verifies that zero/empty value fields
// map to nil pointers and that nil cost pointers stay nil.
func TestModelFromBestiary_ZeroAndEmptyToNil(t *testing.T) {
	t.Parallel()

	b := bestiary.ModelInfo{
		ID:            bestiary.ModelID("bare-model"),
		Provider:      bestiary.ProviderOpenAI,
		DisplayName:   "Bare Model",
		Family:        bestiary.Family(""), // -> nil
		ContextWindow: 0,                   // -> nil
		MaxOutput:     0,                   // -> nil
		ReleaseDate:   "",                  // -> nil
		// All cost pointers nil -> stay nil.
	}

	got := ingest.ModelFromBestiary(b, fixedSyncedAt)

	if got.Family != nil {
		t.Errorf("Family: got %v, want nil", got.Family)
	}
	if got.ContextWindow != nil {
		t.Errorf("ContextWindow: got %v, want nil", got.ContextWindow)
	}
	if got.MaxOutput != nil {
		t.Errorf("MaxOutput: got %v, want nil", got.MaxOutput)
	}
	if got.ReleaseDate != nil {
		t.Errorf("ReleaseDate: got %v, want nil", got.ReleaseDate)
	}
	if got.CostInputPerMTok != nil || got.CostOutputPerMTok != nil ||
		got.CostReasoningPerMTok != nil || got.CostCacheReadPerMTok != nil ||
		got.CostCacheWritePerMTok != nil {
		t.Error("nil cost pointers must stay nil")
	}
}

// TestModelFromBestiary_LastSyncedStampedWhenEmpty verifies the preserve-if-present
// rule's empty branch: an empty LastSynced is stamped with syncedAt (RFC3339 UTC).
func TestModelFromBestiary_LastSyncedStampedWhenEmpty(t *testing.T) {
	t.Parallel()

	b := bestiary.ModelInfo{
		ID:          bestiary.ModelID("live-model"),
		Provider:    bestiary.ProviderAnthropic,
		DisplayName: "Live Model",
		LastSynced:  "", // live fetch leaves this empty
	}

	got := ingest.ModelFromBestiary(b, fixedSyncedAt)

	want := fixedSyncedAt.UTC().Format(time.RFC3339)
	if got.LastSynced != want {
		t.Errorf("LastSynced: got %q, want stamped %q", got.LastSynced, want)
	}
}

// TestModelsFromBestiary_NilAndEmptyInput verifies nil/empty input yields nil output.
func TestModelsFromBestiary_NilAndEmptyInput(t *testing.T) {
	t.Parallel()

	if got := ingest.ModelsFromBestiary(nil, fixedSyncedAt); got != nil {
		t.Errorf("nil input: got %v, want nil", got)
	}
	if got := ingest.ModelsFromBestiary([]bestiary.ModelInfo{}, fixedSyncedAt); got != nil {
		t.Errorf("empty input: got %v, want nil", got)
	}
}

// TestModelsFromBestiary_OrderPreserved verifies the slice mapping preserves order
// and applies the per-element rules consistently.
func TestModelsFromBestiary_OrderPreserved(t *testing.T) {
	t.Parallel()

	in := []bestiary.ModelInfo{
		{ID: bestiary.ModelID("first"), Provider: bestiary.ProviderAnthropic, LastSynced: "2026-04-08T17:58:57Z"},
		{ID: bestiary.ModelID("second"), Provider: bestiary.ProviderOpenAI, LastSynced: ""},
		{ID: bestiary.ModelID("third"), Provider: bestiary.ProviderGoogle, LastSynced: "2026-04-08T17:58:57Z"},
	}

	got := ingest.ModelsFromBestiary(in, fixedSyncedAt)

	if len(got) != 3 {
		t.Fatalf("len: got %d, want 3", len(got))
	}
	if got[0].ModelID != "first" || got[1].ModelID != "second" || got[2].ModelID != "third" {
		t.Errorf("order not preserved: %q, %q, %q", got[0].ModelID, got[1].ModelID, got[2].ModelID)
	}
	// Element-wise preserve-if-present still holds inside the slice mapping.
	if got[0].LastSynced != "2026-04-08T17:58:57Z" {
		t.Errorf("got[0].LastSynced: got %q, want preserved", got[0].LastSynced)
	}
	if got[1].LastSynced != fixedSyncedAt.UTC().Format(time.RFC3339) {
		t.Errorf("got[1].LastSynced: got %q, want stamped", got[1].LastSynced)
	}
}

func assertF64(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || *got != want {
		t.Errorf("%s: got %v, want %v", name, got, want)
	}
}
