package kickstart_test

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/source_preview_budgets.yaml
var sourcePreviewBudgetData []byte

type sourcePreviewBudgetCase struct {
	Name              string                   `yaml:"name"`
	Origin            ftue.SessionSourceOrigin `yaml:"origin"`
	InitialBytes      int64                    `yaml:"initial_bytes"`
	BodyBytes         int64                    `yaml:"body_bytes"`
	ContinuationBytes int64                    `yaml:"continuation_bytes"`
	RecordBytes       int                      `yaml:"record_bytes"`
	RecordCount       int                      `yaml:"record_count"`
	RecordTemplate    string                   `yaml:"record_template"`
	SourceFixture     string                   `yaml:"source_fixture"`
	FirstTurns        int                      `yaml:"first_turns"`
	BodyTurns         int                      `yaml:"body_turns"`
	ContinuedTurns    int                      `yaml:"continued_turns"`
}

type sourcePreviewBudgetDocument struct {
	RequiredCases []string                  `yaml:"required_cases"`
	Cases         []sourcePreviewBudgetCase `yaml:"cases"`
}

func loadSourcePreviewBudgetDocument(t *testing.T) sourcePreviewBudgetDocument {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(sourcePreviewBudgetData))
	decoder.KnownFields(true)
	var document sourcePreviewBudgetDocument
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode source preview budget fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("source preview budget fixture must contain exactly one document")
	}
	if len(document.RequiredCases) == 0 {
		t.Fatal("source preview budget fixture declares no required cases")
	}
	seen := make(map[string]bool, len(document.Cases))
	for _, c := range document.Cases {
		if c.Name == "" || c.InitialBytes <= 0 || c.BodyBytes <= 0 || c.ContinuationBytes <= 0 || c.RecordBytes <= 0 || c.RecordCount <= c.ContinuedTurns || c.FirstTurns <= 0 || c.BodyTurns <= c.FirstTurns || c.ContinuedTurns <= c.BodyTurns || !strings.Contains(c.RecordTemplate, "PAYLOAD") {
			t.Fatalf("source preview budget fixture has an incomplete case: %+v", c)
		}
		if err := c.Origin.Validate(); err != nil {
			t.Fatalf("source preview budget case %q has invalid origin %q: %v", c.Name, c.Origin, err)
		}
		if seen[c.Name] {
			t.Fatalf("source preview budget fixture duplicates case %q", c.Name)
		}
		seen[c.Name] = true
	}
	for _, name := range document.RequiredCases {
		if !seen[name] {
			t.Fatalf("source preview budget fixture is missing required case %q", name)
		}
	}
	return document
}

// TestSourceTurns_DefaultBudgetsAreSharedAcrossOrigins reads real synthetic
// JSONL and SQLite sources through the production parser/materializer. Exact
// record sizes make first, body and continuation prefixes observable without
// inspecting SourceTurns fields or overriding any budget. Run cases serially
// because each deliberately exceeds two continuation budgets.
func TestSourceTurns_DefaultBudgetsAreSharedAcrossOrigins(t *testing.T) {
	for _, c := range loadSourcePreviewBudgetDocument(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			if c.InitialBytes != defaults.TranscriptInitialReadBytes || c.BodyBytes != defaults.TranscriptContinuationReadBytes || c.ContinuationBytes != defaults.TranscriptContinuationReadBytes {
				t.Fatal("fixture budgets differ from the shared policy")
			}
			if defaults.OpenCodeManagedProjectionMaxBytes != 64<<20 {
				t.Errorf("managed projection safety ceiling = %d, want independent 64 MiB ceiling", defaults.OpenCodeManagedProjectionMaxBytes)
			}
			listing, payloads := sourceBudgetListing(t, c)
			reader := kickstart.NewSourceTurns(&ingest.OSFileSystem{}, []ftue.SessionListing{listing},
				kickstart.WithSourceTurnsGitResolver(testutil.NoGitResolver()))
			first, more, err := reader.FirstTurns(listing.SessionID)
			if err != nil || !more {
				t.Fatalf("first preview: more=%v err=%v", more, err)
			}
			assertSourceBudgetPrefix(t, first, payloads[:c.FirstTurns])
			if reader.Notice(listing.SessionID) != "" {
				t.Fatal("uncached first prefix must not carry the body notice")
			}
			body, err := reader.Turns(listing.SessionID)
			if err != nil || !reader.HasMore(listing.SessionID) {
				t.Fatalf("automatic body: more=%v err=%v", reader.HasMore(listing.SessionID), err)
			}
			assertSourceBudgetPrefix(t, body, payloads[:c.BodyTurns])
			assertSourceBudgetNotice(t, reader.Notice(listing.SessionID), c.BodyTurns*c.RecordBytes)
			continued, more, err := reader.MoreTurns(listing.SessionID)
			if err != nil || !more {
				t.Fatalf("continuation: more=%v err=%v", more, err)
			}
			assertSourceBudgetPrefix(t, continued, payloads[:c.ContinuedTurns])
			assertSourceBudgetNotice(t, reader.Notice(listing.SessionID), c.ContinuedTurns*c.RecordBytes)
			final, more, err := reader.MoreTurns(listing.SessionID)
			if err != nil || more || reader.HasMore(listing.SessionID) {
				t.Fatalf("final continuation: more=%v err=%v", more, err)
			}
			assertSourceBudgetPrefix(t, final, payloads)
			if reader.Notice(listing.SessionID) != "" {
				t.Fatal("complete preview still offers continuation")
			}
		})
	}
}

func assertSourceBudgetPrefix(t *testing.T, turns []ingest.Turn, payloads []string) {
	t.Helper()
	if len(turns) != len(payloads) {
		t.Fatalf("returned %d turns, want exact budget prefix of %d", len(turns), len(payloads))
	}
	for i, turn := range turns {
		if turn.Content != payloads[i] {
			t.Fatalf("turn %d lost, duplicated, reordered, or truncated source bytes (got %d bytes, want %d)", i, len(turn.Content), len(payloads[i]))
		}
	}
}

func assertSourceBudgetNotice(t *testing.T, notice string, loadedBytes int) {
	t.Helper()
	want := fmt.Sprintf("showing the first %d MiB", loadedBytes/(1<<20))
	if !strings.Contains(notice, want) || !strings.Contains(notice, "scroll to the bottom to load more") {
		t.Fatalf("notice %q does not describe loaded prefix %q and continuation", notice, want)
	}
}

// sourceBudgetListing reuses the synthetic provider schema/discovery helper.
// Padding is fixture-driven and kept inside its test-owned source; no provider
// directory or real transcript is opened. JSONL includes the newline in its
// byte unit; SQLite budgets count the message JSON payload, not its envelope.
func sourceBudgetListing(t *testing.T, c sourcePreviewBudgetCase) (ftue.SessionListing, []string) {
	t.Helper()
	lines := make([]string, c.RecordCount)
	payloads := make([]string, c.RecordCount)
	for i := range lines {
		template := strings.ReplaceAll(c.RecordTemplate, "INDEX", fmt.Sprintf("%06d", i))
		suffix := fmt.Sprintf(" safe-tail-%06d", i)
		padding := c.RecordBytes - len(strings.ReplaceAll(template, "PAYLOAD", "")) - len(suffix)
		if c.Origin == ftue.SessionSourceOriginFile {
			padding-- // JSONL newline is a source byte too.
		}
		if padding <= 0 {
			t.Fatal("record byte budget cannot hold fixture template")
		}
		payloads[i] = strings.Repeat("x", padding) + suffix
		lines[i] = strings.ReplaceAll(template, "PAYLOAD", payloads[i])
	}
	if c.Origin == ftue.SessionSourceOriginFile {
		dir := t.TempDir()
		path := writeTranscript(t, dir, "budget.jsonl", lines)
		return ftue.SessionListing{SessionID: "ses_previewBudget", Harness: string(ingest.HarnessClaudeCode),
			Source: ftue.SessionSource{Path: path, Root: dir, Origin: c.Origin}}, payloads
	}
	if c.Origin != ftue.SessionSourceOriginOpenCodeCurrentSQLite || c.SourceFixture == "" {
		t.Fatal("budget fixture needs a supported synthetic current SQLite source")
	}
	session := discoverOneSQLiteSession(t, c.SourceFixture, c.Origin)
	conn, err := sqlite.OpenConn(session.SourcePath.String(), sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := sqlitex.Execute(conn, "DELETE FROM session_message WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{string(session.SessionID)}}); err != nil {
		t.Fatal(err)
	}
	for i, data := range lines {
		if err := sqlitex.Execute(conn, `INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES (?, ?, 'user', ?, ?, ?, ?)`,
			&sqlitex.ExecOptions{Args: []any{fmt.Sprintf("msg_budget_%06d", i), string(session.SessionID), i + 1, i + 1, data, i}}); err != nil {
			t.Fatal(err)
		}
	}
	return ftue.SessionListing{SessionID: string(session.SessionID), Harness: string(session.Harness), Source: kickstart.ListingSource(session)}, payloads
}
