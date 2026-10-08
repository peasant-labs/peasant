package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runGCVerify(t *testing.T, c contentGCCase) {
	s, root := openGenerationStore(t)
	sid := gcSession(t, s, "a1a1a1a1-a1a1-41a1-81a1-a1a1a1a1a1a1")
	if c.Verify == "rowid" {
		recallExec(t, s, `INSERT INTO session_entries(rowid, session_id, entry_index, provider, entry_type, role, depth) VALUES (?, ?, 0, 'claude-code', 'text', 'user', 0)`, int64(BodyRowIDBase), string(sid))
		if _, err := s.VerifyContent(t.Context(), false); err == nil || !strings.Contains(err.Error(), "rowid") {
			t.Fatalf("verify rowid ceiling: %v", err)
		}
		return
	}
	texts := []string{"orphan zero", "orphan one", "orphan two"}
	blob := "unmapped blob"
	v2, blobs := gcBuildCandidate(t, sid, "g_verify_orphan", texts, map[string]string{"e_gcblobVerify": blob})
	if _, err := s.StageGeneration(t.Context(), GenerationActivation{Generation: v2, Blobs: blobs}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, string(sid), "generations", ".tmp-gen-verify")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "payload"), []byte(blob), 0600); err != nil {
		t.Fatal(err)
	}
	if c.Unflagged {
		recallExec(t, s, `UPDATE sessions SET content_sweep_pending=0 WHERE session_id=?`, string(sid))
	}
	report, err := s.VerifyContent(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes := int64(len(blob))
	for _, text := range texts {
		wantBytes += int64(len(text))
	}
	if report.OrphanBodies != 3 || report.OrphanBlobs != 1 || report.OrphanBytes != wantBytes || report.OrphanOwnedDirs != 1 || report.OrphanOwnedBytes != int64(len(blob)) {
		t.Fatalf("orphan report = %+v, want database payload %d bytes", report, wantBytes)
	}
	if c.Unflagged {
		if len(report.UnflaggedOrphanSessions) != 1 || report.UnflaggedOrphanSessions[0] != sid {
			t.Fatalf("unflagged owners = %v", report.UnflaggedOrphanSessions)
		}
	} else if len(report.UnflaggedOrphanSessions) != 0 {
		t.Fatalf("flagged owner reported unflagged: %v", report.UnflaggedOrphanSessions)
	}
	if gcCount(t, s, "session_entry_bodies", sid) != 3 {
		t.Fatal("verification deleted orphan content")
	}
}
