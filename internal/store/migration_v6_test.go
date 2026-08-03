package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestMigrationV6_SchemaAppliesCleanly verifies the models table and index exist
// after migration v6 and that user_version is 6.
func TestMigrationV6_SchemaAppliesCleanly(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify models table exists.
	var tableExists int
	sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='models'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { tableExists = stmt.ColumnInt(0); return nil },
	})
	if tableExists != 1 {
		t.Error("models table not found after migration v6")
	}

	// Verify idx_models_family index exists.
	var idxExists int
	sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_models_family'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { idxExists = stmt.ColumnInt(0); return nil },
	})
	if idxExists != 1 {
		t.Error("idx_models_family index not found after migration v6")
	}

	// Verify user_version >= 6 (V6 migration and all subsequent applied).
	var uv int
	sqlitex.ExecuteTransient(conn, `PRAGMA user_version`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { uv = stmt.ColumnInt(0); return nil },
	})
	if uv < 6 {
		t.Errorf("user_version: expected >= 6, got %d", uv)
	}
}

// TestMigrationV6_SyncModelsRoundTrip verifies SyncModels upserts and GetModel reads back.
func TestMigrationV6_SyncModelsRoundTrip(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	family := "claude-family"
	ctxWindow := 200000
	maxOutput := 4096
	costIn := 15.0
	costOut := 75.0
	releaseDate := "2025-03-01"

	models := []ingest.ModelInfo{
		{
			ModelID:           "claude-opus-4-6",
			ProviderKey:       "anthropic",
			DisplayName:       "Claude Opus 4.6",
			Family:            &family,
			ContextWindow:     &ctxWindow,
			MaxOutput:         &maxOutput,
			Reasoning:         true,
			ToolCall:          true,
			CostInputPerMTok:  &costIn,
			CostOutputPerMTok: &costOut,
			ReleaseDate:       &releaseDate,
			LastSynced:        "2026-02-23T00:00:00Z",
		},
	}

	if err := s.SyncModels(ctx, models); err != nil {
		t.Fatalf("SyncModels: %v", err)
	}

	// Read back.
	got, err := s.GetModel(ctx, "claude-opus-4-6", "anthropic")
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if got == nil {
		t.Fatal("GetModel returned nil for known model")
	}
	if got.ModelID != "claude-opus-4-6" {
		t.Errorf("ModelID: expected claude-opus-4-6, got %s", got.ModelID)
	}
	if got.ProviderKey != "anthropic" {
		t.Errorf("ProviderKey: expected anthropic, got %s", got.ProviderKey)
	}
	if got.DisplayName != "Claude Opus 4.6" {
		t.Errorf("DisplayName: expected Claude Opus 4.6, got %s", got.DisplayName)
	}
	if got.Family == nil || *got.Family != "claude-family" {
		t.Errorf("Family: expected claude, got %v", got.Family)
	}
	if got.ContextWindow == nil || *got.ContextWindow != 200000 {
		t.Errorf("ContextWindow: expected 200000, got %v", got.ContextWindow)
	}
	if got.MaxOutput == nil || *got.MaxOutput != 4096 {
		t.Errorf("MaxOutput: expected 4096, got %v", got.MaxOutput)
	}
	if !got.Reasoning {
		t.Error("Reasoning: expected true")
	}
	if !got.ToolCall {
		t.Error("ToolCall: expected true")
	}
	if got.CostInputPerMTok == nil || *got.CostInputPerMTok != 15.0 {
		t.Errorf("CostInputPerMTok: expected 15.0, got %v", got.CostInputPerMTok)
	}
	if got.CostOutputPerMTok == nil || *got.CostOutputPerMTok != 75.0 {
		t.Errorf("CostOutputPerMTok: expected 75.0, got %v", got.CostOutputPerMTok)
	}
	if got.ReleaseDate == nil || *got.ReleaseDate != "2025-03-01" {
		t.Errorf("ReleaseDate: expected 2025-03-01, got %v", got.ReleaseDate)
	}
	if got.LastSynced != "2026-02-23T00:00:00Z" {
		t.Errorf("LastSynced: expected 2026-02-23T00:00:00Z, got %s", got.LastSynced)
	}
}

// TestMigrationV6_SyncModelsUpsert verifies that re-syncing updates existing rows.
func TestMigrationV6_SyncModelsUpsert(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	ctxWindow1 := 100000
	ctxWindow2 := 200000

	// Initial sync.
	if err := s.SyncModels(ctx, []ingest.ModelInfo{
		{
			ModelID:       "gpt-4o",
			ProviderKey:   "openai",
			DisplayName:   "GPT-4o",
			ContextWindow: &ctxWindow1,
			LastSynced:    "2026-02-22T00:00:00Z",
		},
	}); err != nil {
		t.Fatalf("SyncModels (initial): %v", err)
	}

	// Re-sync with updated context window.
	if err := s.SyncModels(ctx, []ingest.ModelInfo{
		{
			ModelID:       "gpt-4o",
			ProviderKey:   "openai",
			DisplayName:   "GPT-4o Updated",
			ContextWindow: &ctxWindow2,
			LastSynced:    "2026-02-23T00:00:00Z",
		},
	}); err != nil {
		t.Fatalf("SyncModels (upsert): %v", err)
	}

	got, err := s.GetModel(ctx, "gpt-4o", "openai")
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if got == nil {
		t.Fatal("GetModel returned nil")
	}
	if got.DisplayName != "GPT-4o Updated" {
		t.Errorf("DisplayName after upsert: expected GPT-4o Updated, got %s", got.DisplayName)
	}
	if got.ContextWindow == nil || *got.ContextWindow != 200000 {
		t.Errorf("ContextWindow after upsert: expected 200000, got %v", got.ContextWindow)
	}
	if got.LastSynced != "2026-02-23T00:00:00Z" {
		t.Errorf("LastSynced after upsert: expected 2026-02-23T00:00:00Z, got %s", got.LastSynced)
	}
}

// TestMigrationV6_GetContextWindow_KnownModel verifies GetContextWindow returns
// the context window for a known model.
func TestMigrationV6_GetContextWindow_KnownModel(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	ctxWindow := 128000
	if err := s.SyncModels(ctx, []ingest.ModelInfo{
		{
			ModelID:       "claude-sonnet-4-6",
			ProviderKey:   "anthropic",
			DisplayName:   "Claude Sonnet 4.6",
			ContextWindow: &ctxWindow,
			LastSynced:    "2026-02-23T00:00:00Z",
		},
	}); err != nil {
		t.Fatalf("SyncModels: %v", err)
	}

	window, found, err := s.GetContextWindow(ctx, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("GetContextWindow: %v", err)
	}
	if !found {
		t.Error("GetContextWindow: expected found=true for known model")
	}
	if window != 128000 {
		t.Errorf("GetContextWindow: expected 128000, got %d", window)
	}
}

// TestMigrationV6_GetContextWindow_UnknownModel verifies GetContextWindow returns
// (0, false, nil) for an unknown model.
func TestMigrationV6_GetContextWindow_UnknownModel(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	window, found, err := s.GetContextWindow(ctx, "nonexistent-model")
	if err != nil {
		t.Fatalf("GetContextWindow: %v", err)
	}
	if found {
		t.Error("GetContextWindow: expected found=false for unknown model")
	}
	if window != 0 {
		t.Errorf("GetContextWindow: expected 0, got %d", window)
	}
}

// TestMigrationV6_GetModel_NotFound verifies GetModel returns nil for unknown model.
func TestMigrationV6_GetModel_NotFound(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	got, err := s.GetModel(ctx, "nonexistent", "unknown")
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if got != nil {
		t.Errorf("GetModel: expected nil for unknown model, got %+v", got)
	}
}

// TestMigrationV6_CompositeKey verifies that the same model_id with different
// provider_keys creates separate rows (composite PK).
func TestMigrationV6_CompositeKey(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	ctxWindow1 := 128000
	ctxWindow2 := 200000

	if err := s.SyncModels(ctx, []ingest.ModelInfo{
		{
			ModelID:       "gpt-4",
			ProviderKey:   "openai",
			DisplayName:   "GPT-4 (OpenAI)",
			ContextWindow: &ctxWindow1,
			LastSynced:    "2026-02-23T00:00:00Z",
		},
		{
			ModelID:       "gpt-4",
			ProviderKey:   "azure",
			DisplayName:   "GPT-4 (Azure)",
			ContextWindow: &ctxWindow2,
			LastSynced:    "2026-02-23T00:00:00Z",
		},
	}); err != nil {
		t.Fatalf("SyncModels: %v", err)
	}

	openai, err := s.GetModel(ctx, "gpt-4", "openai")
	if err != nil {
		t.Fatalf("GetModel openai: %v", err)
	}
	azure, err := s.GetModel(ctx, "gpt-4", "azure")
	if err != nil {
		t.Fatalf("GetModel azure: %v", err)
	}

	if openai == nil || azure == nil {
		t.Fatal("expected both models to exist")
	}
	if openai.DisplayName != "GPT-4 (OpenAI)" {
		t.Errorf("openai DisplayName: expected GPT-4 (OpenAI), got %s", openai.DisplayName)
	}
	if azure.DisplayName != "GPT-4 (Azure)" {
		t.Errorf("azure DisplayName: expected GPT-4 (Azure), got %s", azure.DisplayName)
	}
	if *openai.ContextWindow != 128000 {
		t.Errorf("openai ContextWindow: expected 128000, got %d", *openai.ContextWindow)
	}
	if *azure.ContextWindow != 200000 {
		t.Errorf("azure ContextWindow: expected 200000, got %d", *azure.ContextWindow)
	}

	// GetContextWindow should return one of them (query uses LIMIT 1).
	window, found, err := s.GetContextWindow(ctx, "gpt-4")
	if err != nil {
		t.Fatalf("GetContextWindow: %v", err)
	}
	if !found {
		t.Error("GetContextWindow: expected found=true")
	}
	// Should return one of the two valid windows.
	if window != 128000 && window != 200000 {
		t.Errorf("GetContextWindow: expected 128000 or 200000, got %d", window)
	}
}
