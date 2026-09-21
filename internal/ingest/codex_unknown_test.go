package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/codex_unknown.yaml
var codexUnknownFixtures []byte

func TestCodexRetainedUnknown(t *testing.T) {
	var fixture struct {
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name         string `yaml:"name"`
			Record       string `yaml:"record"`
			Namespace    string `yaml:"namespace"`
			Kind         string `yaml:"kind"`
			Pointer      string `yaml:"pointer"`
			Error        bool   `yaml:"error"`
			NativeOnly   bool   `yaml:"native_only"`
			CopyBoundary *int64 `yaml:"copy_boundary"`
			Reverted     bool   `yaml:"reverted"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(codexUnknownFixtures))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, row := range fixture.Cases {
		names = append(names, row.Name)
	}
	slices.Sort(names)
	slices.Sort(fixture.Required)
	if !slices.Equal(names, fixture.Required) {
		t.Fatal("fixture names differ from required manifest")
	}
	for _, row := range fixture.Cases {
		if row.NativeOnly {
			continue
		}
		t.Run(row.Name, func(t *testing.T) {
			session := DiscoveredSession{SessionID: "11111111-1111-4111-8111-111111111111", Harness: HarnessCodex}
			known := `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"known"}]}}`
			data := []byte(known + "\n" + row.Record + "\n" + known + "\n")
			result, err := NewCodexIndexer(nil).IndexTranscriptBytesForCapture(context.Background(), session, data)
			if row.Error {
				if err == nil {
					t.Fatal("corrupt known field accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := retainedUnknownEntries(result.Entries)
			if err != nil {
				t.Fatal(err)
			}
			if len(evidence) != 1 {
				t.Fatalf("evidence = %+v", evidence)
			}
			record := evidence[0]
			if record.Namespace != row.Namespace || record.Kind != row.Kind || record.Position.Line != 2 || record.Position.JSONPointer != row.Pointer {
				t.Fatalf("wrong identity: %+v", record)
			}
			if strings.Contains(string(record.Payload), "ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD") {
				t.Fatal("secret retained")
			}
			if row.Name == "envelope" && !strings.Contains(string(record.Payload), "9007199254740993") {
				t.Fatal("number precision lost")
			}
			if result.Entries[0].ContentPreview == nil || *result.Entries[0].ContentPreview != "known" || result.Entries[len(result.Entries)-1].ContentPreview == nil || *result.Entries[len(result.Entries)-1].ContentPreview != "known" {
				t.Fatal("known siblings lost")
			}
		})
	}
}
