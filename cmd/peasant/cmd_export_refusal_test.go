package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/export_refusal_report.yaml
var exportRefusalReportYAML []byte

type exportRefusalCase struct {
	Name         string         `yaml:"name"`
	Sessions     []string       `yaml:"sessions"`
	WantExported int            `yaml:"wantExported"`
	WantRefused  map[string]int `yaml:"wantRefused"`
	WantSummary  string         `yaml:"wantSummary"`
	WantError    string         `yaml:"wantError"`
}

// TestExportSessionsReportsWhatItRefused drives the real command over stored
// sessions, with and without --json. The run must say how many sessions were
// exported, how many refused and why, and still write the sessions it could.
func TestExportSessionsReportsWhatItRefused(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string            `yaml:"requiredNames"`
		Cases         []exportRefusalCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(exportRefusalReportYAML, &fixture); err != nil {
		t.Fatalf("export_refusal_report.yaml: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	for _, required := range fixture.RequiredNames {
		if !slices.Contains(names, required) {
			t.Fatalf("export_refusal_report.yaml lost required case %q", required)
		}
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			ids := make([]string, 0, len(c.Sessions))
			for index, shape := range c.Sessions {
				id := fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012d", index+1)
				seedUploadableSession(t, dir, id)
				seedEntryCarrying(t, dir, id, "the stored body")
				seedRefusedCapture(t, dir, id, shape)
				ids = append(ids, id)
			}
			list := filepath.Join(dir, "ids.txt")
			if err := os.WriteFile(list, []byte(strings.Join(ids, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			outDir := filepath.Join(dir, "exported")

			output, err := executeWithDataDir(t, BuildExportCommand(), dir, []string{"sessions", "--sessions-from", list, "--output-dir", outDir})
			assertExportOutcome(t, c, err)
			if !strings.Contains(output, c.WantSummary+"\n") {
				t.Fatalf("summary line missing; want %q in:\n%s", c.WantSummary, output)
			}
			if c.WantError != "" && strings.Contains(output, "Usage:") {
				t.Fatalf("a refusal report ended in the flag usage:\n%s", output)
			}
			written, globErr := filepath.Glob(filepath.Join(outDir, "*.json"))
			if globErr != nil {
				t.Fatal(globErr)
			}
			if len(written) != c.WantExported {
				t.Fatalf("wrote %d transcripts, want %d", len(written), c.WantExported)
			}

			root := newTestRoot()
			root.AddCommand(BuildExportCommand())
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"--data-dir", dir, "--config-dir", dir, "--state-dir", dir, "export", "sessions", "--sessions-from", list, "--output-dir", outDir, "--json"})
			assertExportOutcome(t, c, root.Execute())
			var document exportSessionsDocument
			if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
				t.Fatalf("--json stdout is not one document: %v\n%s", err, stdout.String())
			}
			refused := 0
			for _, count := range c.WantRefused {
				refused += count
			}
			if document.Exported != c.WantExported || document.Refused != refused {
				t.Fatalf("document counts exported=%d refused=%d, want %d and %d", document.Exported, document.Refused, c.WantExported, refused)
			}
			if !maps.Equal(document.RefusalReasons, nonNilCounts(c.WantRefused)) {
				t.Fatalf("document reasons = %v, want %v", document.RefusalReasons, c.WantRefused)
			}
			if len(document.Warnings) != refused {
				t.Fatalf("document warnings = %+v, want one per refused session", document.Warnings)
			}
		})
	}
}

func assertExportOutcome(t *testing.T, c exportRefusalCase, err error) {
	t.Helper()
	if c.WantError == "" {
		if err != nil {
			t.Fatalf("export failed with nothing refused: %v", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), c.WantError) {
		t.Fatalf("export error = %v, want %q", err, c.WantError)
	}
}

func nonNilCounts(counts map[string]int) map[string]int {
	if counts == nil {
		return map[string]int{}
	}
	return counts
}

// seedRefusedCapture gives one stored session the capture a refusal reads. The
// entries stay as they are: only the capture says the session is not whole.
func seedRefusedCapture(t *testing.T, dir, sessionID, shape string) {
	t.Helper()
	var format, code string
	switch shape {
	case "ok":
		return
	case "refused-strict":
		format, code = "preview_only", "strict_capture_refused"
	case "refused-legacy":
		format, code = "legacy_preview_only", "legacy_preview_only"
	default:
		t.Fatalf("export_refusal_report.yaml names unknown session shape %q", shape)
	}
	db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	if err := sqlitex.Execute(conn, `UPDATE session_content_captures SET status='incomplete', capture_format=?, failure_code=? WHERE session_id=?`, &sqlitex.ExecOptions{
		Args: []any{format, code, sessionID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.Execute(conn, `UPDATE sessions SET index_version=1 WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{sessionID}}); err != nil {
		t.Fatal(err)
	}
}
