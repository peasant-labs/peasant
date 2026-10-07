package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/pi_cwd_forms.yaml
var piCWDFormsFixtureYAML []byte

type piCWDFormCase struct {
	Name     string `yaml:"name"`
	CWD      string `yaml:"cwd"`
	Absolute bool   `yaml:"absolute"`
}

type piCWDFormsFixture struct {
	DeclaredRows  int             `yaml:"declared_rows"`
	RequiredCases []string        `yaml:"required_cases"`
	Cases         []piCWDFormCase `yaml:"cases"`
}

func loadPiCWDFormsFixture(t *testing.T) piCWDFormsFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(piCWDFormsFixtureYAML))
	decoder.KnownFields(true)
	var fixture piCWDFormsFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode testdata/pi_cwd_forms.yaml with known fields: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("testdata/pi_cwd_forms.yaml must hold exactly one YAML document: %v", err)
	}
	if fixture.DeclaredRows != len(fixture.Cases) {
		t.Fatalf("fixture declares %d rows but carries %d cases", fixture.DeclaredRows, len(fixture.Cases))
	}
	if len(fixture.RequiredCases) == 0 {
		t.Fatal("testdata/pi_cwd_forms.yaml required_cases is empty, so no case is protected from deletion")
	}
	for _, required := range fixture.RequiredCases {
		if !slices.ContainsFunc(fixture.Cases, func(c piCWDFormCase) bool { return c.Name == required }) {
			t.Fatalf("fixture is missing required case %q", required)
		}
	}
	return fixture
}

// TestHasAbsolutePathFormFixture pins which recorded working directories count
// as absolute. See testdata/pi_cwd_forms.yaml for why the decision cannot come
// from filepath.IsAbs.
func TestHasAbsolutePathFormFixture(t *testing.T) {
	t.Parallel()
	fixture := loadPiCWDFormsFixture(t)
	for _, tc := range fixture.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			if got := hasAbsolutePathForm(tc.CWD); got != tc.Absolute {
				t.Fatalf("hasAbsolutePathForm(%q) = %v, want %v", tc.CWD, got, tc.Absolute)
			}
		})
	}
}

// TestParsePiDocumentAcceptsWindowsRecordedCWD proves the wiring, not just the
// helper: a pi recording made on Windows carries a drive-letter cwd, and the
// whole document was previously refused at its header because the check demanded
// a leading "/". The same document with a unix cwd is parsed alongside it so the
// two are held to the identical standard.
func TestParsePiDocumentAcceptsWindowsRecordedCWD(t *testing.T) {
	t.Parallel()
	const sessionID = "11111111-2222-4333-8444-555555555555"
	for _, tc := range []struct{ name, cwd string }{
		{"unix-recorded", "/workspace/project"},
		{"windows-recorded", `C:\Users\alice\work`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The cwd is JSON-encoded, so a backslash is escaped on the wire
			// exactly as pi would write it.
			header := fmt.Sprintf(
				`{"type":"session","version":3,"id":%q,"cwd":%q,"timestamp":"2026-01-01T00:00:00Z"}`,
				sessionID, tc.cwd,
			)
			doc, err := parsePiDocument(context.Background(), []byte(header+"\n"))
			if err != nil {
				t.Fatalf("parsePiDocument rejected a %s header: %v", tc.name, err)
			}
			if doc.header.CWD != tc.cwd {
				t.Fatalf("header cwd = %q, want %q", doc.header.CWD, tc.cwd)
			}
			if strings.TrimSpace(doc.header.ID) != sessionID {
				t.Fatalf("header id = %q, want %q", doc.header.ID, sessionID)
			}
		})
	}
}
