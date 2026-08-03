package ingest_test

// Regression guard for Codex subagent/thread-spawn
// sessions were attributed to their own ~/.codex/sessions/YYYY/MM/DD folder
// instead of the session's project cwd. Root cause: codexSessionMeta's
// Source field was typed as a bare Go string, but a subagent session's
// session_meta.payload.source is a nested JSON object
// ({"subagent":{"thread_spawn":{...}}}). That type mismatch made
// json.Unmarshal return a non-nil error, and the caller treated ANY error as
// "the whole session_meta failed to parse" — discarding the otherwise
// correctly-populated cwd and falling back to the rollout file's own
// containing directory. Fixed by typing Source as json.RawMessage (the field
// is never read, so its shape no longer matters). This suite drives
// ExtractMetadata over synthetic representative session_meta byte-shapes (fixture-backed,
// not inline case tables) to prove the fix and guard against a repeat.

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/codex_session_meta_source_field.yaml
var codexSourceFieldFixtureYAML []byte

//go:embed testdata/codex_session_meta_source_field.manifest.yaml
var codexSourceFieldManifestYAML []byte

type codexSourceFieldCase struct {
	Name                    string `yaml:"name"`
	Description             string `yaml:"description"`
	SessionMetaPayloadJSON  string `yaml:"sessionMetaPayloadJSON"`
	ExpectedCWD             string `yaml:"expectedCWD"`
	ExpectedVersion         string `yaml:"expectedVersion"`
	ExpectSessionMetaParsed bool   `yaml:"expectSessionMetaParsed"`
}

type codexSourceFieldFixture struct {
	Cases []codexSourceFieldCase `yaml:"cases"`
}

func decodeCodexSourceFieldFixture(data []byte) (codexSourceFieldFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixture codexSourceFieldFixture
	if err := decoder.Decode(&fixture); err != nil {
		return codexSourceFieldFixture{}, fmt.Errorf("decode codex session_meta source-field fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return codexSourceFieldFixture{}, fmt.Errorf("codex session_meta source-field fixture must contain exactly one YAML document: %v", err)
	}
	names := make(map[string]struct{}, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if _, duplicate := names[c.Name]; duplicate {
			return codexSourceFieldFixture{}, fmt.Errorf("codex session_meta source-field fixture repeats case name %q", c.Name)
		}
		names[c.Name] = struct{}{}
		if c.SessionMetaPayloadJSON == "" || c.ExpectedCWD == "" || c.ExpectedVersion == "" {
			return codexSourceFieldFixture{}, fmt.Errorf("codex session_meta source-field fixture case %q has incomplete input", c.Name)
		}
		// The payload must itself be valid, self-contained JSON — a broken
		// fixture must fail loudly here, not surface as a confusing
		// downstream ExtractMetadata diagnostic mismatch.
		var probe map[string]any
		if err := json.Unmarshal([]byte(c.SessionMetaPayloadJSON), &probe); err != nil {
			return codexSourceFieldFixture{}, fmt.Errorf("codex session_meta source-field fixture case %q sessionMetaPayloadJSON is invalid JSON: %w", c.Name, err)
		}
	}
	return fixture, nil
}

func loadCodexSourceFieldFixture(t *testing.T) codexSourceFieldFixture {
	t.Helper()
	fixture, err := decodeCodexSourceFieldFixture(codexSourceFieldFixtureYAML)
	if err != nil {
		t.Fatalf("load codex session_meta source-field fixture: %v", err)
	}
	manifest, err := testutil.DecodeSemanticManifest(codexSourceFieldManifestYAML, "codex session_meta source-field")
	if err != nil {
		t.Fatal(err)
	}
	actualNames := make([]string, len(fixture.Cases))
	for i, c := range fixture.Cases {
		actualNames[i] = c.Name
	}
	if err := testutil.ValidateSemanticNames(manifest, actualNames, "codex session_meta source-field"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestCodexSourceFieldFixtureGuards is the mutation-proof of non-vacuity: it
// proves the manifest/fixture tooling itself would catch a shrunk case
// count, a dropped required case, and malformed YAML — mutating bytes
// in-memory only, never touching the committed fixture files on disk.
func TestCodexSourceFieldFixtureGuards(t *testing.T) {
	loadCodexSourceFieldFixture(t)
	manifest, err := testutil.DecodeSemanticManifest(codexSourceFieldManifestYAML, "codex session_meta source-field")
	if err != nil {
		t.Fatal(err)
	}

	unknownField := bytes.Replace(codexSourceFieldFixtureYAML, []byte("sessionMetaPayloadJSON:"), []byte("unexpected: true\n    sessionMetaPayloadJSON:"), 1)
	if _, err := decodeCodexSourceFieldFixture(unknownField); err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("unknown-field mutation error = %v, want strict rejection", err)
	}

	trailingDoc := append(append([]byte{}, codexSourceFieldFixtureYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := decodeCodexSourceFieldFixture(trailingDoc); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing-document mutation error = %v, want strict rejection", err)
	}

	for _, required := range manifest.RequiredNames {
		mutated := bytes.Replace(codexSourceFieldFixtureYAML, []byte("name: "+required), []byte("name: replacement_case"), 1)
		fixture, err := decodeCodexSourceFieldFixture(mutated)
		if err != nil {
			t.Fatalf("required case %q replacement unexpectedly failed to decode: %v", required, err)
		}
		names := make([]string, len(fixture.Cases))
		for i, c := range fixture.Cases {
			names[i] = c.Name
		}
		if err := testutil.ValidateSemanticNames(manifest, names, "codex session_meta source-field"); err == nil {
			t.Fatalf("required case %q replacement unexpectedly validated against the manifest", required)
		}
	}

	unknownManifestField := bytes.Replace(codexSourceFieldManifestYAML, []byte("expectedCaseCount:"), []byte("unexpected: true\nexpectedCaseCount:"), 1)
	if _, err := testutil.DecodeSemanticManifest(unknownManifestField, "codex session_meta source-field"); err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("manifest unknown-field mutation error = %v, want strict rejection", err)
	}
}

// TestCodexAdapter_ExtractMetadata_SessionMetaSourceField drives
// ExtractMetadata over synthetic representative session_meta byte-shapes (subagent
// object source, plain-string "cli" source, plain-string "vscode" source)
// and asserts the real project cwd is always extracted — never the rollout
// file's own containing directory, and never dropped behind a spurious
// parse_error/missing_session_meta diagnostic.
func TestCodexAdapter_ExtractMetadata_SessionMetaSourceField(t *testing.T) {
	fixture := loadCodexSourceFieldFixture(t)

	for _, tc := range fixture.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			const base = "/home/test/.codex/sessions"
			sessionID := "00000000-0000-4000-8000-000000000020"
			rollout := fmt.Sprintf("%s/2024/01/02/rollout-2024-01-02T03-12-00-%s.jsonl", base, sessionID)

			sessionMetaLine := fmt.Sprintf(
				`{"timestamp":"2024-01-02T03:12:00Z","type":"session_meta","payload":%s}`,
				tc.SessionMetaPayloadJSON,
			)
			// One trivial response_item line so the rollout is a realistic
			// multi-line file, not a degenerate single-line fixture.
			body := strings.Join([]string{
				sessionMetaLine,
				`{"timestamp":"2024-01-02T03:12:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello from synthetic data"}]}}`,
			}, "\n") + "\n"

			mfs := testutil.NewMemFS()
			if err := mfs.WriteFile(rollout, []byte(body), 0644); err != nil {
				t.Fatalf("seed rollout: %v", err)
			}

			a := ingest.NewCodexAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
			sid, err := ingest.NewSessionID(sessionID)
			if err != nil {
				t.Fatalf("NewSessionID: %v", err)
			}
			ds := ingest.DiscoveredSession{
				SessionID:    sid,
				Harness:      ingest.HarnessCodex,
				SourcePath:   ingest.ResolvedPath(rollout),
				SourceFormat: ingest.SourceFormatJSONL,
			}

			meta, err := a.ExtractMetadata(context.Background(), ds)
			if err != nil {
				t.Fatalf("ExtractMetadata: %v", err)
			}

			if meta.CWD != tc.ExpectedCWD {
				t.Errorf("CWD: got %q, want %q (%s)", meta.CWD, tc.ExpectedCWD, tc.Description)
			}
			// The regression's failure mode specifically: falling back to
			// the rollout's own containing directory instead of the real
			// project cwd. Assert directly against that wrong value too, so
			// a future regression that produces some OTHER wrong cwd still
			// fails the assertion above, and this one call out the
			// regression's exact prior symptom.
			containingDir := "/home/test/.codex/sessions/2024/01/02"
			if meta.CWD == containingDir {
				t.Errorf("CWD regressed to the rollout's containing directory %q instead of the real project cwd %q", containingDir, tc.ExpectedCWD)
			}
			if meta.Version != tc.ExpectedVersion {
				t.Errorf("Version: got %q, want %q", meta.Version, tc.ExpectedVersion)
			}
			if meta.Project.FilePath != tc.ExpectedCWD {
				t.Errorf("Project.FilePath: got %q, want %q", meta.Project.FilePath, tc.ExpectedCWD)
			}

			gotParsed := true
			for _, w := range meta.Diagnostics.Warnings {
				if w.ErrorType == "parse_error" || w.ErrorType == "missing_session_meta" {
					gotParsed = false
					t.Errorf("unexpected diagnostic %+v — session_meta should have parsed cleanly", w)
				}
			}
			if gotParsed != tc.ExpectSessionMetaParsed {
				t.Errorf("session_meta parsed = %v, want %v", gotParsed, tc.ExpectSessionMetaParsed)
			}
		})
	}
}
