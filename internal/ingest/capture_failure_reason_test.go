package ingest_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

const (
	repairRecordRemedy = "repair this record and retry ingest"
	readabilityRemedy  = "restore source readability and retry"
)

// unreadableSourceFS fails the capture read of every JSONL source while
// discovery's ordinary reads still succeed, so the session reaches capture.
type unreadableSourceFS struct {
	ingest.OSFileSystem
}

func (*unreadableSourceFS) ReadSourcePrefix(path string) ([]byte, error) {
	return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}
}

// captureFailureReason runs one harvest over a single Claude Code source and
// returns the reason the failed session carries.
func captureFailureReason(t *testing.T, filesystem ingest.FileSystem, body string) string {
	t.Helper()
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(sourceRoot, "-workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	sid := "ffffffff-0000-4000-8000-000000000745"
	body = strings.ReplaceAll(body, "{sid}", sid)
	if err := os.WriteFile(filepath.Join(sourceRoot, "-workspace", sid+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := ingest.PipelineConfig{Sources: map[ingest.Harness]ingest.SourceConfig{ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(sourceRoot)}}}, OutputDir: ingest.ResolvedPath(filepath.Join(root, "output")), Parallelism: 1}
	pipeline, err := newTestPipeline(filesystem, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg)
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range result.Sessions {
		if string(session.SessionID) == sid && session.Error != nil {
			return session.Error.Error()
		}
	}
	t.Fatalf("session %s did not fail: %+v", sid, result.Sessions)
	return ""
}

func claudeRecord(uuid string) string {
	return `{"type":"user","sessionId":"{sid}","uuid":"` + uuid + `","cwd":"/workspace","timestamp":"2026-10-01T10:00:00Z","message":{"role":"user","content":"hello"}}` + "\n"
}

// TestMalformedRecordReasonGivesOneRemedy proves a malformed JSONL record
// reports only its own remedy, repair the record, and no readability advice:
// the source was read, so that advice would contradict it.
func TestMalformedRecordReasonGivesOneRemedy(t *testing.T) {
	t.Parallel()
	reason := captureFailureReason(t, &ingest.OSFileSystem{}, claudeRecord("u1")+"{\"type\":\"user\",\"broken\n"+claudeRecord("u3"))
	if strings.Count(reason, repairRecordRemedy) != 1 || strings.Contains(reason, readabilityRemedy) {
		t.Fatalf("malformed-record reason must hold exactly one remedy %q and not %q:\n%s", repairRecordRemedy, readabilityRemedy, reason)
	}
	if !strings.HasSuffix(reason, repairRecordRemedy) {
		t.Fatalf("malformed-record reason must end with its remedy %q:\n%s", repairRecordRemedy, reason)
	}
	if line, ok := ingest.MalformedJSONLRecordLine(reason); !ok || line != 2 {
		t.Fatalf("MalformedJSONLRecordLine(%q) = %d, %v; want 2, true", reason, line, ok)
	}
}

// TestUnreadableSourceReasonGivesReadabilityRemedy proves a source capture
// could not read keeps the readability remedy and no record-repair advice.
func TestUnreadableSourceReasonGivesReadabilityRemedy(t *testing.T) {
	t.Parallel()
	reason := captureFailureReason(t, &unreadableSourceFS{}, claudeRecord("u1"))
	if strings.Count(reason, readabilityRemedy) != 1 || strings.Contains(reason, repairRecordRemedy) {
		t.Fatalf("unreadable-source reason must hold exactly one remedy %q and not %q:\n%s", readabilityRemedy, repairRecordRemedy, reason)
	}
	if !strings.Contains(reason, fs.ErrPermission.Error()) {
		t.Fatalf("unreadable-source reason must name the read failure:\n%s", reason)
	}
}
