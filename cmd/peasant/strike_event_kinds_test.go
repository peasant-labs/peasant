package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/schema"
)

const (
	strikeKindsLeadID  = "CCCCCCCCCCCCCCCCCCCCCCCCCC"
	strikeKindsChildID = "DDDDDDDDDDDDDDDDDDDDDDDDDD"
	strikeKindsForkID  = "EEEEEEEEEEEEEEEEEEEEEEEEEE"
)

// TestStrikeLifecycleSessionExportsAndPublishes ingests a synthetic Strike
// session holding every lifecycle, control and metadata event kind (#449)
// through the real harvest command, then reads it back through the export and
// publication-capture paths. The capture must be complete: export carries no
// retained-unknown evidence and publication input is ready. The child link
// comes only from the lead's child.* events; leadSessionId, forkedFrom and the
// usage cache counts surface in the stored metadata.
func TestStrikeLifecycleSessionExportsAndPublishes(t *testing.T) {
	t.Parallel()

	testRoot := t.TempDir()
	sourceDir := filepath.Join(testRoot, "strike-source")
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, id := range []string{strikeKindsLeadID, strikeKindsChildID, strikeKindsForkID} {
		names = append(names, id+".jsonl", id+".meta.json")
	}
	copyStrikeFixtureFiles(t, filepath.Join("testdata", "strike_event_kinds"), sourceDir, names...)

	out, err := executeHarvestCmd(t, testRoot, []string{
		"--source-harness", schema.HarnessStrike.String(),
		"--source-path", sourceDir,
		"--output", filepath.Join(testRoot, "sync"),
		"--include-active",
		"--json",
	})
	if err != nil {
		t.Fatalf("harvest Strike lifecycle fixture: %v\n%s", err, out)
	}

	db, err := openPreparedStore(t, defaults.ResolveDBFilePathWith(testRoot).String())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	ctx := t.Context()

	inputs := make(map[string]ingest.PublicationInputBundle)
	for _, id := range []string{strikeKindsLeadID, strikeKindsChildID, strikeKindsForkID} {
		exported, err := export.ExportSession(ctx, db, &ingest.OSFileSystem{}, id)
		if err != nil {
			t.Fatalf("export %s: %v", id, err)
		}
		if len(exported.RetainedUnknown) != 0 {
			t.Errorf("export %s carries %d retained-unknown records; the capture is incomplete", id, len(exported.RetainedUnknown))
		}
		if exported.Diagnostics != nil && exported.Diagnostics.Partial {
			t.Errorf("export %s is marked partial", id)
		}
		input, _, err := push.LoadPublicationInput(ctx, db, id)
		if err != nil {
			t.Fatalf("load publication input %s: %v", id, err)
		}
		if err := push.ValidatePublicationInput(input); err != nil {
			t.Errorf("publication input %s refused: %v", id, err)
		}
		inputs[id] = input
	}

	lead := inputs[strikeKindsLeadID].Metadata
	if len(lead.Subagents) != 1 || lead.Subagents[0].SessionID != strikeKindsChildID {
		t.Errorf("lead subagents = %+v, want child %s from child.* events", lead.Subagents, strikeKindsChildID)
	}
	if lead.Stats.CachedReadTokens == nil || *lead.Stats.CachedReadTokens != 80 {
		t.Errorf("lead cached read tokens = %v, want 80", lead.Stats.CachedReadTokens)
	}
	if lead.Stats.CachedWriteTokens == nil || *lead.Stats.CachedWriteTokens != 15 {
		t.Errorf("lead cached write tokens = %v, want 15", lead.Stats.CachedWriteTokens)
	}

	child := inputs[strikeKindsChildID].Metadata
	if child.ParentUUID == nil || *child.ParentUUID != strikeKindsLeadID {
		t.Errorf("child parent = %v, want %s from child.started", child.ParentUUID, strikeKindsLeadID)
	}
	if child.RootSessionID == nil || *child.RootSessionID != strikeKindsLeadID {
		t.Errorf("child root = %v, want leadSessionId %s", child.RootSessionID, strikeKindsLeadID)
	}

	fork := inputs[strikeKindsForkID].Metadata
	if fork.ParentUUID != nil {
		t.Errorf("fork parent = %v, want none (a fork stays a root)", *fork.ParentUUID)
	}
	found := false
	for _, relationship := range fork.Relationships {
		if relationship.Kind == schema.SessionRelationshipContextFrom && relationship.TargetLocalID != nil && *relationship.TargetLocalID == strikeKindsLeadID {
			found = true
		}
	}
	if !found {
		t.Errorf("fork relationships = %+v, want context_from %s from forkedFrom", fork.Relationships, strikeKindsLeadID)
	}
}
