package ingest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestWithFullContent_ClaudeUserMessage verifies that when WithClaudeFullContent(true) is
// set, ContentPreview is not truncated — even for content exceeding ContentPreviewLimit.
func TestWithFullContent_ClaudeUserMessage(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullContent(true))
	ctx := context.Background()

	longContent := strings.Repeat("A", defaults.ContentPreviewLimit+500)
	line := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q}}`, longContent)
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID, line)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].ContentPreview == nil {
		t.Fatal("content_preview: expected non-nil")
	}
	if len(*entries[0].ContentPreview) <= defaults.ContentPreviewLimit {
		t.Errorf("WithClaudeFullContent(true): expected preview length > %d, got %d",
			defaults.ContentPreviewLimit, len(*entries[0].ContentPreview))
	}
	if *entries[0].ContentPreview != longContent {
		t.Errorf("WithClaudeFullContent(true): full content not preserved; got length %d, want %d",
			len(*entries[0].ContentPreview), len(longContent))
	}
}

// TestWithFullContent_ClaudeAssistantTextBlock verifies that WithClaudeFullContent(true)
// preserves the full content of a text block in an assistant message.
func TestWithFullContent_ClaudeAssistantTextBlock(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullContent(true))
	ctx := context.Background()

	longText := strings.Repeat("B", defaults.ContentPreviewLimit+300)
	line := fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`, longText)
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID, line)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].ContentPreview == nil {
		t.Fatal("content_preview: expected non-nil")
	}
	if *entries[0].ContentPreview != longText {
		t.Errorf("WithClaudeFullContent(true): text block content not preserved; got length %d, want %d",
			len(*entries[0].ContentPreview), len(longText))
	}
}

// TestWithFullContent_ClaudeThinkingBlock verifies that WithClaudeFullContent(true)
// preserves the full thinking block content.
func TestWithFullContent_ClaudeThinkingBlock(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	ctx := context.Background()

	// Use full-depth so the thinking block becomes a depth=1 entry.
	idxFull := ingest.NewClaudeIndexer(fs,
		ingest.WithClaudeFullContent(true),
		ingest.WithClaudeFullDepth(true),
	)

	longThinking := strings.Repeat("C", defaults.ContentPreviewLimit+200)
	line := fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","text":%q}]}}`, longThinking)
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID, line)

	entries, err := idxFull.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	// Expect at least one depth=1 thinking entry.
	var thinkingEntry *schema.SessionEntry
	for i := range entries {
		if entries[i].HasThinking && entries[i].Depth == 1 {
			thinkingEntry = &entries[i]
			break
		}
	}
	if thinkingEntry == nil {
		t.Fatal("no depth=1 thinking entry found")
	}
	if thinkingEntry.ContentPreview == nil {
		t.Fatal("thinking entry content_preview: expected non-nil")
	}
	if *thinkingEntry.ContentPreview != longThinking {
		t.Errorf("WithClaudeFullContent(true): thinking content not preserved; got length %d, want %d",
			len(*thinkingEntry.ContentPreview), len(longThinking))
	}
}

// TestWithFullContent_ClaudeFalseStillTruncates verifies that WithClaudeFullContent(false)
// (the default) still truncates content at ContentPreviewLimit.
func TestWithFullContent_ClaudeFalseStillTruncates(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullContent(false))
	ctx := context.Background()

	longContent := strings.Repeat("D", defaults.ContentPreviewLimit+100)
	line := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q}}`, longContent)
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID, line)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].ContentPreview == nil {
		t.Fatal("content_preview: expected non-nil")
	}
	if len(*entries[0].ContentPreview) != defaults.ContentPreviewLimit {
		t.Errorf("WithClaudeFullContent(false): expected truncation to %d, got %d",
			defaults.ContentPreviewLimit, len(*entries[0].ContentPreview))
	}
}

// TestWithFullContent_ClaudeDefaultStillTruncates verifies that the default
// ClaudeIndexer (no option) still truncates.
func TestWithFullContent_ClaudeDefaultStillTruncates(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewClaudeIndexer(fs) // no WithFullContent
	ctx := context.Background()

	longContent := strings.Repeat("E", defaults.ContentPreviewLimit+200)
	line := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q}}`, longContent)
	session := makeClaudeSession(t, fs, testutil.TestSessionUUID, line)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].ContentPreview == nil {
		t.Fatal("content_preview: expected non-nil")
	}
	if len(*entries[0].ContentPreview) != defaults.ContentPreviewLimit {
		t.Errorf("default indexer: expected truncation to %d, got %d",
			defaults.ContentPreviewLimit, len(*entries[0].ContentPreview))
	}
}
