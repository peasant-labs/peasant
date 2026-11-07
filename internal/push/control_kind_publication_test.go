package push_test

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/schema"
)

func controlKindPublicationEntries(sessionID schema.SessionID) []schema.SessionEntry {
	text := func(value string) *string { return &value }
	mode := "mode"
	attachment := "attachment"
	return []schema.SessionEntry{
		{
			SessionID: sessionID, EntryIndex: 0, Harness: schema.HarnessClaudeCode,
			EntryType: schema.EntryTypeText, Role: schema.RoleUser,
			ContentPreview: text("run the checks"), SourceEntryRef: "source:0",
		},
		{
			SessionID: sessionID, EntryIndex: 1, Harness: schema.HarnessClaudeCode,
			EntryType: schema.EntryTypeText, Role: schema.RoleSystem,
			PartType: &mode, ContentPreview: text("mode: normal"), SourceEntryRef: "source:1",
		},
		{
			SessionID: sessionID, EntryIndex: 2, Harness: schema.HarnessClaudeCode,
			EntryType: schema.EntryTypeText, Role: schema.RoleSystem,
			PartType: &attachment, SourceEntryRef: "source:2",
		},
		{
			SessionID: sessionID, EntryIndex: 3, Harness: schema.HarnessClaudeCode,
			EntryType: schema.EntryTypeText, Role: schema.RoleAssistant,
			ContentPreview: text("checks are running"), SourceEntryRef: "source:3",
		},
	}
}

// TestBuildPublishTranscriptContent_CarriesControlKind covers the entries/fold
// branch of the publication builder: a legacy publish with no committed detail
// folds the stored entries and must carry the producer's control kind exactly
// where the local viewer does.
func TestBuildPublishTranscriptContent_CarriesControlKind(t *testing.T) {
	t.Parallel()
	sessionID := schema.SessionID("45454545-4545-4545-4545-45454545454f")
	entries := controlKindPublicationEntries(sessionID)
	meta := &ingest.UnifiedMetadata{SessionID: sessionID, ModelHarness: defaults.HarnessClaudeCode}

	content, err := push.BuildPublishTranscriptContent(
		nil, meta, entries,
		defaults.PublishSchemaVersion,
		config.DefaultPushFieldVisibility(),
		sessionorigin.User,
	)
	if err != nil {
		t.Fatalf("BuildPublishTranscriptContent (fold branch): %v", err)
	}
	if content.SessionDetail == nil {
		t.Fatal("publication envelope carries no session detail")
	}
	if len(content.SessionDetail.Turns) != 3 {
		t.Fatalf("published %d turns, want 3 (the empty attachment is suppressed)", len(content.SessionDetail.Turns))
	}
	var kind string
	for _, turn := range content.SessionDetail.Turns {
		if turn.Index == 1 && turn.ControlKind != nil {
			kind = *turn.ControlKind
		}
		if turn.Index != 1 && turn.ControlKind != nil {
			t.Fatalf("published turn %d carries controlKind %q; only the control-record turn may carry one", turn.Index, *turn.ControlKind)
		}
	}
	if kind != "mode" {
		t.Fatalf("published control-record turn kind = %q, want mode", kind)
	}
	if content.SessionDetail.TurnCount != 2 {
		t.Fatalf("published turnCount = %d, want 2 conversation turns", content.SessionDetail.TurnCount)
	}
}

// TestSessionToDetail_ControlKindOnCommittedDetail covers the committed-detail
// branch: an already-hydrated detail whose turns carry controlKind is published
// verbatim (consent overlay aside), never rebuilt from entries.
func TestSessionToDetail_ControlKindOnCommittedDetail(t *testing.T) {
	t.Parallel()
	sessionID := schema.SessionID("45454545-4545-4545-4545-454545454550")
	mode := "mode"
	committed := &schema.SessionDetailPayload{
		ID:        string(sessionID),
		Harness:   schema.HarnessClaudeCode,
		TurnCount: 1,
		Turns: []schema.TurnDetail{
			{Index: 0, Role: schema.RoleUser, Content: "run the checks"},
			{Index: 1, Role: schema.RoleSystem, Content: "mode: normal", ControlKind: &mode},
		},
	}
	meta := &ingest.UnifiedMetadata{SessionID: sessionID, ModelHarness: defaults.HarnessClaudeCode}

	content, err := push.BuildPublishTranscriptContent(
		committed, meta, controlKindPublicationEntries(sessionID),
		defaults.PublishSchemaVersion,
		config.DefaultPushFieldVisibility(),
		sessionorigin.User,
	)
	if err != nil {
		t.Fatalf("BuildPublishTranscriptContent (committed branch): %v", err)
	}
	if content.SessionDetail == nil {
		t.Fatal("publication envelope carries no session detail")
	}
	if len(content.SessionDetail.Turns) != 2 {
		t.Fatalf("published %d turns, want the committed detail's 2 (the committed branch must not re-fold entries)", len(content.SessionDetail.Turns))
	}
	turn := content.SessionDetail.Turns[1]
	if turn.ControlKind == nil || *turn.ControlKind != "mode" {
		t.Fatalf("committed detail controlKind = %v, want mode preserved through publication", turn.ControlKind)
	}
}
