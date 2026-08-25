package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type projectionFormatCase struct {
	Name          string `yaml:"name"`
	Data          string `yaml:"data"`
	ExpectOK      bool   `yaml:"expect_ok"`
	ErrorContains string `yaml:"error_contains"`
}

type projectionFormatDocument struct {
	RequiredCases []string               `yaml:"required_cases"`
	Cases         []projectionFormatCase `yaml:"cases"`
}

//go:embed testdata/opencode_current_projection_format.yaml
var projectionFormatYAML []byte

func loadProjectionFormatDocument(t *testing.T) projectionFormatDocument {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(projectionFormatYAML))
	decoder.KnownFields(true)
	var document projectionFormatDocument
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode projection format fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("projection format fixture must be exactly one YAML document")
	}
	presentFormat := make(map[string]struct{}, len(document.Cases))
	for _, testCase := range document.Cases {
		presentFormat[testCase.Name] = struct{}{}
	}
	if len(document.RequiredCases) == 0 {
		t.Fatal("projection format fixture declares no required cases")
	}
	for _, name := range document.RequiredCases {
		if _, ok := presentFormat[name]; !ok {
			t.Fatalf("projection format fixture is missing required case %q", name)
		}
	}
	seen := make(map[string]struct{}, len(document.Cases))
	for _, testCase := range document.Cases {
		if testCase.Name == "" || testCase.Data == "" {
			t.Fatalf("projection format fixture case %q is incomplete", testCase.Name)
		}
		if !testCase.ExpectOK && testCase.ErrorContains == "" {
			t.Fatalf("projection format failure case %q must name the expected error", testCase.Name)
		}
		if _, duplicate := seen[testCase.Name]; duplicate {
			t.Fatalf("projection format fixture repeats case name %q", testCase.Name)
		}
		seen[testCase.Name] = struct{}{}
	}
	return document
}

// TestOpenCodeManagedProjectionFormatVersionDiscipline documents and enforces
// the version discipline for the managed projections. Tolerating current
// control rows added the typed control field, so the current write version is 2
// while the shared minimum readable version stays 1: a version 1 projection
// that predates the control field still decodes, a version 2 projection that
// carries a control field decodes, and a version above the write version is
// refused. The legacy projection shape is unchanged, so its write version stays
// 2 and it shares the same minimum readable floor.
func TestOpenCodeManagedProjectionFormatVersionDiscipline(t *testing.T) {
	if openCodeCurrentProjectionVersion != 2 {
		t.Fatalf("current projection write version = %d, want 2 after the control field was added", openCodeCurrentProjectionVersion)
	}
	if openCodeLegacyProjectionVersion != 2 {
		t.Fatalf("legacy projection write version = %d, want 2; the legacy persisted shape is unchanged", openCodeLegacyProjectionVersion)
	}
	if openCodeLegacyProjectionMinReadableVersion != 1 {
		t.Fatalf("minimum readable version = %d, want 1 so a previously persisted version 1 projection still decodes", openCodeLegacyProjectionMinReadableVersion)
	}

	document := loadProjectionFormatDocument(t)
	sessionID, err := NewSessionID("ses_3cd91f52effeXd3QAJ54jFmt01")
	if err != nil {
		t.Fatalf("construct fixture session id: %v", err)
	}
	session := DiscoveredSession{SessionID: sessionID, TranscriptOrigin: TranscriptOriginOpenCodeCurrentSQLite}
	indexer := NewOpenCodeIndexer(&OSFileSystem{}, WithOpenCodeFullDepth(true), WithOpenCodeFullContent(true))
	for _, testCase := range document.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			entries, err := indexer.IndexTranscriptBytes(context.Background(), session, []byte(testCase.Data))
			if testCase.ExpectOK {
				if err != nil {
					t.Fatalf("projection %q should decode under the version 2 reader: %v", testCase.Name, err)
				}
				if len(entries) == 0 {
					t.Fatalf("projection %q decoded but produced no entries", testCase.Name)
				}
				return
			}
			if err == nil {
				t.Fatalf("projection %q should be refused but decoded", testCase.Name)
			}
			if !strings.Contains(err.Error(), testCase.ErrorContains) {
				t.Fatalf("projection %q error = %v, want it to contain %q", testCase.Name, err, testCase.ErrorContains)
			}
		})
	}
}
