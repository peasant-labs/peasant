package main

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/kickstart_sqlite_preview.yaml
var kickstartSQLitePreviewData []byte

const expectedKickstartSQLitePreviewCaseCount = 2

// kickstartSQLitePreviewCase drives the mounted preview over one synthetic
// OpenCode SQLite database that discovery found but no store imported.
type kickstartSQLitePreviewCase struct {
	Name          string                   `yaml:"name"`
	SourceFixture string                   `yaml:"source_fixture"`
	Origin        ftue.SessionSourceOrigin `yaml:"origin"`
	SessionID     string                   `yaml:"session_id"`
	ExpectError   bool                     `yaml:"expect_error"`
	WantContains  string                   `yaml:"want_contains"`
}

type kickstartSQLitePreviewDoc struct {
	DeclaredCases int                          `yaml:"declared_cases"`
	Cases         []kickstartSQLitePreviewCase `yaml:"cases"`
}

func loadKickstartSQLitePreviewDoc(t *testing.T) kickstartSQLitePreviewDoc {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(kickstartSQLitePreviewData))
	decoder.KnownFields(true)
	var doc kickstartSQLitePreviewDoc
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("decode mounted SQLite preview fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("mounted SQLite preview fixture must hold exactly one document")
	}
	if doc.DeclaredCases != expectedKickstartSQLitePreviewCaseCount || len(doc.Cases) != expectedKickstartSQLitePreviewCaseCount {
		t.Fatalf("mounted SQLite preview fixture count guard failed: declared %d, cases %d, want %d", doc.DeclaredCases, len(doc.Cases), expectedKickstartSQLitePreviewCaseCount)
	}
	seen := make(map[string]struct{}, len(doc.Cases))
	for _, c := range doc.Cases {
		if c.Name == "" || c.SourceFixture == "" || c.Origin == "" || c.SessionID == "" {
			t.Fatalf("mounted SQLite preview fixture has an incomplete case: %+v", c)
		}
		if err := c.Origin.Validate(); err != nil {
			t.Fatalf("mounted SQLite preview fixture case %q has an unsupported origin %q", c.Name, c.Origin)
		}
		if _, dup := seen[c.Name]; dup {
			t.Fatalf("mounted SQLite preview fixture has a duplicate case name %q", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	return doc
}

// TestKickstartPreview_UningestedSQLiteSessionYieldsTurnsOrAnError proves the
// mounted preview wiring for an un-ingested OpenCode SQLite session: there is no
// store, and the pane reads the one selected session through the materializer.
// A readable database renders real turns; an unreadable one surfaces an
// actionable error string rather than aborting the process. This is the crash
// path the maintainer hit, exercised through the same kickstartPreview the
// command mounts.
func TestKickstartPreview_UningestedSQLiteSessionYieldsTurnsOrAnError(t *testing.T) {
	t.Parallel()
	doc := loadKickstartSQLitePreviewDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			materialized := testfixture.MaterializeByName(t, c.SourceFixture)
			listings := []ftue.SessionListing{{
				Harness:   string(ingest.HarnessOpenCode),
				SessionID: c.SessionID,
				Source:    ftue.SessionSource{Path: materialized.Path, Origin: c.Origin},
			}}
			cmd := mountTestCmd(t, t.TempDir())

			db, closeStore := openKickstartStore(cmd)
			if db != nil {
				t.Fatal("openKickstartStore opened a store for a data directory with no database")
			}
			closeStore()

			body, err := previewBodyWithoutPanic(t, kickstartPreview(cmd, db, theme.New(theme.ModeDark), listings), c.SessionID)
			if c.ExpectError {
				if err == nil {
					t.Fatalf("an unreadable SQLite session must surface an error, not empty turns")
				}
				if len(err.Error()) == 0 {
					t.Fatalf("the surfaced failure must carry an actionable message")
				}
				return
			}
			if err != nil {
				t.Fatalf("preview a readable un-ingested SQLite session: %v", err)
			}
			got := flattenPane(body.Render(doc.width(t)))
			if !strings.Contains(got, needle(c.WantContains)) {
				t.Fatalf("preview must render %q; got:\n%s", c.WantContains, got)
			}
			if strings.Contains(got, "not imported yet") {
				t.Fatalf("a session the pane can read must not be described as unreadable; got:\n%s", got)
			}
		})
	}
}

func (kickstartSQLitePreviewDoc) width(t *testing.T) int {
	t.Helper()
	return loadPreviewDoc(t).Width
}

// previewBodyWithoutPanic loads the preview body for id and fails the test on a
// panic, so a materialization failure that reaches the pane is proven to
// surface as an error rather than aborting the process.
func previewBodyWithoutPanic(t *testing.T, source kit.BodySource, id string) (body kit.PreviewBody, err error) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("previewing session %q panicked instead of returning an error: %v", id, recovered)
		}
	}()
	body, err = source.Body(id)
	return body, err
}
