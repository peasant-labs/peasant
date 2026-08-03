package ingest_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
)

// Basenames whose derived host slug a maximum redactor really does replace.
//
// A repository with no origin remote gets the slug
// `__peasant-untracked__--<8hex>--<basename>`, where the 8-hex segment is
// HMAC-SHA256(per-install salt, path) - so it is different on every install and
// CANNOT be pinned. Recorded tripping values from one machine do not reproduce on
// another, which is why the incidence looked like "one unlucky path" and was in
// fact 15-37% of untracked repositories.
//
// The BASENAME can be pinned, and these two are measured to trip on every salt
// value: hex-charset entropy over the whole slug stays above the threshold no
// matter what the random segment is. A candidate that tripped 2985 times out of
// 3000 was rejected - a fixture that flakes once every two hundred runs teaches
// people to re-run rather than to read.
const (
	entropyPinnedBasename       = "a3f8d1c9b2e7"
	entropyPinnedLongerBasename = "7f3a9c1e5b8d2604af71"
)

// TestPipeline_RedactionKeepsTheHostSlugResolvable is the regression proof for the
// defect that made a repository permanently unpublishable.
//
// The host slug is written in THREE places that have to agree: the directory
// ingest creates, the database row, and the metadata file. The directory is built
// from the slug BEFORE metadata redaction; redaction then rewrote the slug on the
// metadata, so the row and the file recorded a location no directory used. Push
// resolves the metadata path from the stored row, so it looked for a directory
// that was never written and failed with "metadata file missing or unreadable" on
// every attempt - permanently, because the host-slug insert ignores conflicts and
// re-ingest skips an unchanged session.
//
// This drives the pipeline with an EXPLICITLY constructed maximum redactor. The
// commands refuse that level outright, so the CLI cannot reach this code path any
// more; the repair still has to hold, because it is what makes the level safe to
// re-enable later and because the defect is in the pipeline rather than in the
// policy that currently avoids it.
func TestPipeline_RedactionKeepsTheHostSlugResolvable(t *testing.T) {
	if !redact.MaximumAvailable {
		t.Skip("a maximum redactor cannot be constructed without cgo, and only that level redacts the slug")
	}
	for _, basename := range []string{entropyPinnedBasename, entropyPinnedLongerBasename} {
		t.Run(basename, func(t *testing.T) {
			redactor, err := redact.NewRedactor(redact.Maximum, nil, redact.XDGPaths{})
			if err != nil {
				t.Fatalf("construct the maximum redactor: %v", err)
			}
			// The slug of an untracked repository, with the salt-derived segment
			// standing in for whatever this install would produce.
			slug := "__peasant-untracked__--69ea0eb8--" + basename
			// Non-vacuity: this case only proves something if the redactor really
			// would rewrite this slug. Without the check, the repair could be
			// deleted and the test would still pass.
			if !strings.Contains(redactor.RedactText(slug), "<HIGH_ENTROPY>") {
				t.Fatalf("the basename %q no longer pins: a maximum redactor leaves the slug %q intact, so this case cannot "+
					"detect the defect it exists for", basename, slug)
			}

			recorded := ingestOneSessionAtMaximum(t, slug, redactor)

			if recorded.directory != slug {
				t.Errorf("ingest wrote the directory %q, want the real slug %q", recorded.directory, slug)
			}
			if recorded.metadataFile != slug {
				t.Errorf("the metadata file records the slug %q, want %q; push resolves the metadata path from the stored slug, "+
					"so a redacted value there points at a directory that was never written", recorded.metadataFile, slug)
			}
			if recorded.databaseRow != slug {
				t.Errorf("the database row records the slug %q, want %q", recorded.databaseRow, slug)
			}
			for label, value := range map[string]string{
				"directory": recorded.directory, "metadata file": recorded.metadataFile, "database row": recorded.databaseRow,
			} {
				if strings.Contains(value, "<HIGH_ENTROPY>") {
					t.Errorf("the %s kept a redaction placeholder (%q): the slug is a locator, and redacting it makes the "+
						"session unreachable", label, value)
				}
			}
			// The production resolution: this is the exact path push builds from the
			// stored row, and reading it is what used to fail forever.
			if !recorded.resolvedByPush {
				t.Errorf("push resolves %q from the recorded slug and could not read it; that is the permanent publish failure",
					recorded.resolvedPath)
			}

			// The content redaction the level exists for still happened - the repair
			// restores the LOCATOR only, and must not have turned redaction off.
			if !recorded.contentWasRedacted {
				t.Error("the maximum redactor did not redact the metadata it was given; the repair must restore the slug only, " +
					"not disable redaction")
			}
		})
	}
}

// recordedLocation is the same host slug read from each of the three places that
// have to agree, plus whether push could resolve the metadata from it.
type recordedLocation struct {
	directory          string
	metadataFile       string
	databaseRow        string
	resolvedPath       string
	resolvedByPush     bool
	contentWasRedacted bool
}

// ingestOneSessionAtMaximum runs the real pipeline over one session whose metadata
// carries slug, with a real store and a real filesystem, and reads back all three
// records of the location.
func ingestOneSessionAtMaximum(t *testing.T, slug string, redactor ingest.TextRedactor) recordedLocation {
	t.Helper()
	root := t.TempDir()
	outputDir := filepath.Join(root, "peasant-sync")
	sourceDir := filepath.Join(root, "source")

	filesystem := &ingest.OSFileSystem{}
	if err := filesystem.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatalf("create the source directory: %v", err)
	}
	sessionID := "7c7c7c7c-1234-4123-8123-7c7c7c7c7c7c"
	sourcePath := filepath.Join(sourceDir, sessionID+".jsonl")
	// A fenced code block so the redactor has content to transform: that is what
	// distinguishes this level, and the repair must not suppress it.
	transcript := `{"type":"user","message":{"role":"user","content":"totals please"},` +
		`"sessionId":"` + sessionID + `","timestamp":"2026-02-19T00:01:00Z","uuid":"aaaa0000-1111-4111-8111-111111111111"}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text",` +
		`"text":"here:\n\n` + "```" + `go\nfunc computeWidgetTotal(items []int) int { return 0 }\n` + "```" + `\n"}]},` +
		`"sessionId":"` + sessionID + `","timestamp":"2026-02-19T00:01:30Z","uuid":"aaaa0000-2222-4222-8222-222222222222"}` + "\n"
	if err := filesystem.WriteFile(sourcePath, []byte(transcript), 0o600); err != nil {
		t.Fatalf("write the recorded transcript: %v", err)
	}

	typedSessionID, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	typedSlug, err := ingest.NewHostSlug(slug)
	if err != nil {
		t.Fatalf("NewHostSlug(%q): %v", slug, err)
	}
	resolvedSource, err := ingest.NewResolvedPath(sourcePath)
	if err != nil {
		t.Fatalf("NewResolvedPath: %v", err)
	}

	metaValue := ingest.NewUnifiedMetadata()
	meta := &metaValue
	meta.SessionID = typedSessionID
	meta.ModelHarness = ingest.HarnessClaudeCode
	meta.HostSlug = typedSlug
	meta.Model = "claude-opus-4-6"
	meta.Project.Name = "ledger"
	ingested := time.Now().UnixMilli()
	meta.Timestamp = ingest.TimestampInfo{Start: 1708300800000, End: 1708300860000, Ingested: &ingested}
	meta.Source.FilePath = sourcePath
	meta.Source.Format = ingest.SourceFormatJSONL

	session := ingest.DiscoveredSession{
		SessionID:    typedSessionID,
		Harness:      ingest.HarnessClaudeCode,
		SourcePath:   resolvedSource,
		SourceFormat: ingest.SourceFormatJSONL,
		ModTime:      time.Now().Add(-time.Hour),
	}

	database, err := store.Open(filepath.Join(root, "peasant.db"))
	if err != nil {
		t.Fatalf("open the analytics store: %v", err)
	}
	defer database.Close()

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(
			[]ingest.DiscoveredSession{session},
			map[ingest.SessionID]*ingest.UnifiedMetadata{typedSessionID: meta},
		),
	}
	cfg := ingest.PipelineConfig{
		Sources: map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessClaudeCode: {Paths: []ingest.ResolvedPath{ingest.ResolvedPath(sourceDir)}, Enabled: true},
		},
		OutputDir:          ingest.ResolvedPath(outputDir),
		StalenessThreshold: 5 * time.Minute,
	}
	pipeline, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(), adapters, cfg,
		ingest.WithRedactor(redactor), ingest.WithStore(database))
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.New != 1 {
		t.Fatalf("the session must have been imported, got summary %+v", result.Summary)
	}

	recorded := recordedLocation{}

	entries, err := filesystem.ReadDir(outputDir)
	if err != nil {
		t.Fatalf("read the output tree: %v", err)
	}
	var directories []string
	for _, entry := range entries {
		if entry.IsDir() {
			directories = append(directories, entry.Name())
		}
	}
	if len(directories) != 1 {
		t.Fatalf("one imported session must write exactly one slug directory, got %v", directories)
	}
	recorded.directory = directories[0]

	metadataPath := ingest.SessionMetadataPath(outputDir, recorded.directory, sessionID, "")
	rawMetadata, err := filesystem.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("read the metadata ingest wrote: %v", err)
	}
	var diskMeta ingest.UnifiedMetadata
	if err := json.Unmarshal(rawMetadata, &diskMeta); err != nil {
		t.Fatalf("parse the metadata: %v", err)
	}
	recorded.metadataFile = string(diskMeta.HostSlug)
	// The level's own behaviour: identifiers inside the fenced block are renamed,
	// so the original name must be gone from what was stored.
	storedTranscript, err := filesystem.ReadFile(
		filepath.Join(outputDir, recorded.directory, sessionID, sessionID+"--transcript.jsonl"))
	if err != nil {
		t.Fatalf("read the stored transcript: %v", err)
	}
	recorded.contentWasRedacted = !strings.Contains(string(storedTranscript), "computeWidgetTotal")

	rows, err := database.AllPushableSessions(context.Background())
	if err != nil {
		t.Fatalf("read the sessions push would publish: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("push must see exactly the one imported session, got %d", len(rows))
	}
	recorded.databaseRow = rows[0].HostSlug

	// Resolve exactly the way push does: from the DATABASE ROW, not from the
	// directory listing.
	recorded.resolvedPath = ingest.SessionMetadataPath(outputDir, rows[0].HostSlug, rows[0].SessionID, rows[0].ParentID)
	if _, readErr := filesystem.ReadFile(recorded.resolvedPath); readErr == nil {
		recorded.resolvedByPush = true
	}
	return recorded
}

// TestPipeline_RedactionStillRedactsTheMetadataItShould guards the other direction
// of the repair: restoring the slug must not become "skip metadata redaction".
func TestPipeline_RedactionStillRedactsTheMetadataItShould(t *testing.T) {
	if !redact.MaximumAvailable {
		t.Skip("a maximum redactor cannot be constructed without cgo")
	}
	redactor, err := redact.NewRedactor(redact.Maximum, nil, redact.XDGPaths{})
	if err != nil {
		t.Fatalf("construct the maximum redactor: %v", err)
	}
	slug := "__peasant-untracked__--69ea0eb8--" + entropyPinnedBasename
	recorded := ingestOneSessionAtMaximum(t, slug, redactor)
	if !recorded.contentWasRedacted {
		t.Error("transcript content was not redacted at the maximum level, so the repair has disabled the redaction it was " +
			"only supposed to keep out of the locator")
	}
	if recorded.metadataFile != slug {
		t.Errorf("the metadata slug is %q, want %q", recorded.metadataFile, slug)
	}
}
