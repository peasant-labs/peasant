package ingest_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/opencode_double_encoded_text.yaml
var openCodeDoubleEncodedTextData []byte

// openCodeDoubleEncodedTextCase is one stored text value and the text the
// indexer must produce for it.
type openCodeDoubleEncodedTextCase struct {
	Name   string `yaml:"name"`
	Stored string `yaml:"stored"`
	Unwrap bool   `yaml:"unwrap"`
	Want   string `yaml:"want"`
}

type openCodeDoubleEncodedTextDoc struct {
	RequiredCases []string                        `yaml:"required_cases"`
	Cases         []openCodeDoubleEncodedTextCase `yaml:"cases"`
}

func loadOpenCodeDoubleEncodedTextDoc(t *testing.T) openCodeDoubleEncodedTextDoc {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(openCodeDoubleEncodedTextData))
	decoder.KnownFields(true)
	var doc openCodeDoubleEncodedTextDoc
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("decode double-encoded text fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("double-encoded text fixture must hold exactly one document")
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("double-encoded text fixture declares no required cases")
	}
	present := make(map[string]struct{}, len(doc.Cases))
	for _, testCase := range doc.Cases {
		if testCase.Name == "" || testCase.Stored == "" || testCase.Want == "" {
			t.Fatalf("double-encoded text fixture has an incomplete case: %+v", testCase)
		}
		if testCase.Unwrap == (testCase.Stored == testCase.Want) {
			t.Fatalf("double-encoded text case %q declares unwrap=%v but its stored and wanted text %s; the case would pass whatever the code does",
				testCase.Name, testCase.Unwrap, map[bool]string{true: "are equal", false: "differ"}[testCase.Stored == testCase.Want])
		}
		if _, duplicate := present[testCase.Name]; duplicate {
			t.Fatalf("double-encoded text fixture has a duplicate case name %q", testCase.Name)
		}
		present[testCase.Name] = struct{}{}
	}
	for _, name := range doc.RequiredCases {
		if _, ok := present[name]; !ok {
			t.Fatalf("double-encoded text fixture is missing required case %q", name)
		}
	}
	return doc
}

// managedProjectionWithPartText builds the managed legacy projection bytes for
// one user message carrying one text part with the given text. It is the same
// artifact the materializer writes, so the assertion runs over the production
// indexing path rather than over the helper alone.
func managedProjectionWithPartText(t *testing.T, sessionID, text string) []byte {
	t.Helper()
	partData, err := json.Marshal(map[string]any{"id": "prt_double_encoded", "type": "text", "text": text})
	if err != nil {
		t.Fatalf("encode synthetic part: %v", err)
	}
	messageData, err := json.Marshal(map[string]any{"id": "msg_double_encoded", "role": "user", "time": map[string]any{"created": 1}})
	if err != nil {
		t.Fatalf("encode synthetic message: %v", err)
	}
	projection, err := json.Marshal(map[string]any{
		"format":     "peasant.opencode.legacy-sqlite",
		"version":    2,
		"session_id": sessionID,
		"messages": []any{map[string]any{
			"id": "msg_double_encoded", "session_id": sessionID, "time_created": 1, "time_updated": 1,
			"data": json.RawMessage(messageData),
			"parts": []any{map[string]any{
				"id": "prt_double_encoded", "message_id": "msg_double_encoded", "session_id": sessionID,
				"time_created": 1, "time_updated": 1, "data": json.RawMessage(partData),
			}},
		}},
	})
	if err != nil {
		t.Fatalf("encode synthetic projection: %v", err)
	}
	return projection
}

// TestOpenCodeIndexer_UnwrapsDoubleEncodedPromptText proves the indexer decodes
// a text value that is itself one JSON string literal, and leaves every other
// value alone. Indexing is where the unwrap belongs, so the preview, the stored
// transcript, and a push all carry the same text.
func TestOpenCodeIndexer_UnwrapsDoubleEncodedPromptText(t *testing.T) {
	t.Parallel()
	doc := loadOpenCodeDoubleEncodedTextDoc(t)
	for _, testCase := range doc.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			sessionID, err := ingest.NewSessionID("ses_3cd91f52effeXd3QAJ54jOyzv5")
			if err != nil {
				t.Fatalf("build session identifier: %v", err)
			}
			session := ingest.DiscoveredSession{
				SessionID:        sessionID,
				Harness:          ingest.HarnessOpenCode,
				SourcePath:       ingest.ResolvedPath("/synthetic/opencode.db"),
				SourceFormat:     ingest.SourceFormatJSON,
				TranscriptOrigin: ingest.TranscriptOriginOpenCodeLegacySQLite,
			}
			indexer, ok := ingest.NewIndexerRegistry(&ingest.OSFileSystem{}, ingest.IndexerRegistryOptions{FullContent: true})[ingest.HarnessOpenCode]
			if !ok {
				t.Fatal("the registry holds no OpenCode indexer")
			}
			data := managedProjectionWithPartText(t, string(sessionID), testCase.Stored)
			entries, err := indexer.IndexTranscriptBytes(t.Context(), session, data)
			if err != nil {
				t.Fatalf("index the synthetic managed projection: %v", err)
			}
			preview := firstIndexedContent(t, entries)
			if preview != testCase.Want {
				t.Errorf("indexed content = %q, want %q (stored %q)", preview, testCase.Want, testCase.Stored)
			}
		})
	}
}

func firstIndexedContent(t *testing.T, entries []schema.SessionEntry) string {
	t.Helper()
	for _, entry := range entries {
		if entry.ContentPreview != nil && *entry.ContentPreview != "" {
			return *entry.ContentPreview
		}
	}
	t.Fatal("the indexed projection carried no content")
	return ""
}
