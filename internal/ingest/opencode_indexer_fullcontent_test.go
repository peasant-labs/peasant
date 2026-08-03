package ingest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// addOpenCodeMessageWithContent adds an OpenCode message whose inline content
// field is a plain string (not an array). This exercises the preview path that
// calls extractOpenCodePreview on the msg.Content field.
func addOpenCodeMessageWithContent(t *testing.T, fs *testutil.MemFS, sessionID, msgID, role, content string) {
	t.Helper()
	root := "/opencode-store"
	msgPath := fmt.Sprintf("%s/storage/message/%s/%s.json", root, sessionID, msgID)
	msgJSON := fmt.Sprintf(`{"id":%q,"sessionID":%q,"role":%q,"time":{"created":1700000001000,"completed":1700000002000},"content":%q}`,
		msgID, sessionID, role, content)
	if err := fs.WriteFile(msgPath, []byte(msgJSON), 0644); err != nil {
		t.Fatalf("write message with content: %v", err)
	}
}

// addOpenCodeTextPart adds an OpenCode text part for a message.
// Text parts exercise the extractPreviewFromParts path.
func addOpenCodeTextPart(t *testing.T, fs *testutil.MemFS, msgID, partID, text string) {
	t.Helper()
	root := "/opencode-store"
	partPath := fmt.Sprintf("%s/storage/part/%s/%s.json", root, msgID, partID)
	// Escape the text for safe JSON embedding.
	escapedText := strings.ReplaceAll(text, `"`, `\"`)
	escapedText = strings.ReplaceAll(escapedText, "\n", `\n`)
	partJSON := fmt.Sprintf(`{"id":%q,"type":"text","text":%q}`, partID, escapedText)
	if err := fs.WriteFile(partPath, []byte(partJSON), 0644); err != nil {
		t.Fatalf("write text part: %v", err)
	}
}

// TestWithOpenCodeFullContent_InlineContent verifies that inline message content
// is not truncated when WithOpenCodeFullContent(true) is set.
func TestWithOpenCodeFullContent_InlineContent(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs, ingest.WithOpenCodeFullContent(true))
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	longContent := strings.Repeat("X", defaults.ContentPreviewLimit+400)
	addOpenCodeMessageWithContent(t, fs, sesID, "msg_001abc", "user", longContent)

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
	if *entries[0].ContentPreview != longContent {
		t.Errorf("WithOpenCodeFullContent(true): inline content not preserved; got length %d, want %d",
			len(*entries[0].ContentPreview), len(longContent))
	}
}

// addOpenCodeMessageNoContent adds an OpenCode message with null/empty content
// so that extractOpenCodePreview returns "" and the preview falls through to
// extractPreviewFromParts. This lets tests exercise the part-preview path.
func addOpenCodeMessageNoContent(t *testing.T, fs *testutil.MemFS, sessionID, msgID, role string) {
	t.Helper()
	root := "/opencode-store"
	msgPath := fmt.Sprintf("%s/storage/message/%s/%s.json", root, sessionID, msgID)
	// content is null — extractOpenCodePreview returns "".
	msgJSON := fmt.Sprintf(`{"id":%q,"sessionID":%q,"role":%q,"time":{"created":1700000001000,"completed":1700000002000},"content":null}`,
		msgID, sessionID, role)
	if err := fs.WriteFile(msgPath, []byte(msgJSON), 0644); err != nil {
		t.Fatalf("write message no content: %v", err)
	}
}

// TestWithOpenCodeFullContent_PartPreview verifies that part-sourced previews
// are not truncated when WithOpenCodeFullContent(true) is set.
func TestWithOpenCodeFullContent_PartPreview(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs, ingest.WithOpenCodeFullContent(true))
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	// Message with null content — preview must come from part file.
	addOpenCodeMessageNoContent(t, fs, sesID, "msg_001abc", "assistant")
	longText := strings.Repeat("Y", defaults.ContentPreviewLimit+300)
	addOpenCodeTextPart(t, fs, "msg_001abc", "part_001", longText)

	entries, err := idx.IndexTranscript(ctx, session)
	if err != nil {
		t.Fatalf("IndexTranscript: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].ContentPreview == nil {
		t.Fatal("content_preview: expected non-nil (sourced from part)")
	}
	if *entries[0].ContentPreview != longText {
		t.Errorf("WithOpenCodeFullContent(true): part preview not preserved; got length %d, want %d",
			len(*entries[0].ContentPreview), len(longText))
	}
}

// TestWithOpenCodeFullContent_FalseStillTruncates verifies that WithOpenCodeFullContent(false)
// (the default) still truncates content from inline message content.
func TestWithOpenCodeFullContent_FalseStillTruncates(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	idx := ingest.NewOpenCodeIndexer(fs, ingest.WithOpenCodeFullContent(false))
	ctx := context.Background()

	sesID := testutil.TestOpenCodeSesID
	session := setupOpenCodeFixture(t, fs, sesID, "proj1")

	longContent := strings.Repeat("Z", defaults.ContentPreviewLimit+500)
	addOpenCodeMessageWithContent(t, fs, sesID, "msg_001abc", "user", longContent)

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
		t.Errorf("WithOpenCodeFullContent(false): expected truncation to %d, got %d",
			defaults.ContentPreviewLimit, len(*entries[0].ContentPreview))
	}
}
