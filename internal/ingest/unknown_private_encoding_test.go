package ingest_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/unknown_private_encoding.yaml
var unknownPrivateEncodingYAML []byte

func TestUnknownPrivateEncoding(t *testing.T) {
	t.Parallel()
	var doc struct {
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name          string `yaml:"name"`
			Extra         string `yaml:"extra"`
			Payload       string `yaml:"payload"`
			MissingPublic bool   `yaml:"missing_public"`
			Error         bool   `yaml:"error"`
		} `yaml:"cases"`
	}
	d := yaml.NewDecoder(bytes.NewReader(unknownPrivateEncodingYAML))
	d.KnownFields(true)
	if err := d.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := d.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("trailing fixture document: %v", err)
	}
	names := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatal("duplicate or empty fixture name")
		}
		names[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("unknown private encoding", "case", doc.Required, names); err != nil {
		t.Fatal(err)
	}
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			entry := schema.SessionEntry{Harness: schema.HarnessClaudeCode, Extra: &c.Extra}
			records, err := ingest.RetainedUnknownOf(entry)
			if c.Error {
				if err == nil {
					t.Fatal("malformed private encoding accepted")
				}
				return
			}
			if err != nil || len(records) != 1 || string(records[0].Payload) != c.Payload {
				t.Fatalf("decoded payload changed: %+v %v", records, err)
			}
			entry.Extra = nil
			if err := ingest.AttachRetainedUnknown(&entry, records); err != nil {
				t.Fatal(err)
			}
			roundtrip, err := ingest.RetainedUnknownOf(entry)
			if err != nil || len(roundtrip) != 1 || string(roundtrip[0].Payload) != c.Payload {
				t.Fatalf("rewriting normalized source JSON: %+v %v", roundtrip, err)
			}
			projected, err := ingest.ProjectRetainedUnknown([]schema.SessionEntry{entry}, schema.HarnessClaudeCode)
			if c.MissingPublic {
				if !errors.Is(err, ingest.ErrUnknownPositionUnavailable) {
					t.Fatalf("missing coordinates fabricated: %v", err)
				}
			} else if err != nil || len(projected) != 1 || projected[0].RecordIndex != 0 || projected[0].Position != 0 || projected[0].Payload != c.Payload {
				t.Fatalf("zero source coordinates/payload lost: %+v %v", projected, err)
			}
		})
	}
}
