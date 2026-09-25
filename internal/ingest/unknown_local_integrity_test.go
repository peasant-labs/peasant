package ingest_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/unknown_local_integrity.yaml
var unknownLocalIntegrityYAML []byte

func TestUnknownLocalIntegrity(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name        string `yaml:"name"`
			LocalError  bool   `yaml:"localError"`
			PublicError bool   `yaml:"publicError"`
			Missing     bool   `yaml:"missing"`
			Depth       int    `yaml:"depth"`
			Records     []struct {
				Record   int64  `yaml:"record"`
				Position int64  `yaml:"position"`
				Pointer  string `yaml:"pointer"`
				Payload  string `yaml:"payload"`
			} `yaml:"records"`
		} `yaml:"cases"`
	}
	d := yaml.NewDecoder(bytes.NewReader(unknownLocalIntegrityYAML))
	d.KnownFields(true)
	if err := d.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatal("expected one YAML document")
	}
	var names []string
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: fixture.RequiredNames}, names, "local integrity"); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var entries []schema.SessionEntry
			for i, r := range c.Records {
				payload := strings.Repeat("[", c.Depth) + r.Payload + strings.Repeat("]", c.Depth)
				position := ingest.UnknownSourcePosition{Line: int(r.Record) + 1, JSONPointer: r.Pointer}
				if !c.Missing {
					position.Public = &ingest.UnknownPublicPosition{SourceRef: "source-0", RecordIndex: r.Record, Position: r.Position}
				}
				record, err := ingest.NewRetainedUnknown(schema.HarnessCodex, "record", "future", position, json.RawMessage(payload))
				if err != nil {
					if !c.LocalError {
						t.Fatal(err)
					}
					// The capture constructor refused corrupt evidence before
					// persistence. The local boundary still holds, but the
					// read-side defense needs a stored row to judge, so
					// persist the corrupt bytes through the strict read path:
					// RetainedUnknownOf must refuse them as well.
					rawExtra, marshalErr := json.Marshal(map[string]any{
						"retainedUnknown": []any{
							map[string]any{
								"harness":   "codex",
								"namespace": "record",
								"kind":      "future",
								"position": map[string]any{
									"line":      int(r.Record) + 1,
									"jsonPointer": r.Pointer,
									"public": func() any {
										if c.Missing {
											return nil
										}
										return map[string]any{"sourceRef": "source-0", "recordIndex": r.Record, "position": r.Position}
									}(),
								},
								"payloadText": payload,
							},
						},
					})
					if marshalErr != nil {
						t.Fatal(marshalErr)
					}
					// A nil public member marshals as explicit null, which is
					// malformed rather than legacy absence; drop it so the
					// fixture keeps testing wholly-absent coordinates.
					extra := strings.ReplaceAll(string(rawExtra), `,"public":null`, "")
					entry := schema.SessionEntry{Harness: schema.HarnessCodex, EntryIndex: i, Extra: &extra}
					if _, readErr := ingest.RetainedUnknownOf(entry); readErr == nil {
						t.Fatal("read boundary accepted evidence the constructor refused")
					}
					return
				}
				entry, err := ingest.RetainedUnknownEntry(testutil.TestSessionUUID, i, record)
				if err != nil {
					t.Fatal(err)
				}
				entries = append(entries, entry)
			}
			_, err := ingest.CollectRetainedUnknown(entries, schema.HarnessCodex)
			if (err != nil) != c.LocalError {
				t.Fatalf("local integrity acceptance changed: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "private-key") {
				t.Fatal("local validator leaked native key")
			}
			_, err = ingest.ProjectRetainedUnknown(entries, schema.HarnessCodex)
			if (err != nil) != (c.LocalError || c.PublicError) {
				t.Fatalf("public boundary acceptance changed: %v", err)
			}
		})
	}
}
