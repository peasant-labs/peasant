package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
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
			Name           string `yaml:"name"`
			Record         string `yaml:"record"`
			Namespace      string `yaml:"namespace"`
			Kind           string `yaml:"kind"`
			Pointer        string `yaml:"pointer"`
			Error          bool   `yaml:"error"`
			NativeOnly     bool   `yaml:"native_only"`
			CopyBoundary   *int64 `yaml:"copy_boundary"`
			Reverted       bool   `yaml:"reverted"`
			Position       int64  `yaml:"position"`
			NativePosition int64  `yaml:"native_position"`
			PrefixBlank    bool   `yaml:"prefix_blank"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(codexUnknownFixtures))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("trailing fixture document", err)
	}
	if len(fixture.Required) == 0 {
		t.Fatal("missing required-name manifest")
	}
	names := []string{}
	seen := map[string]bool{}
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatal("duplicate or empty fixture name")
		}
		seen[row.Name] = true
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
			middle := row.Record
			line := 2
			if row.PrefixBlank {
				middle = "\n" + middle
				line++
			}
			data := []byte(known + "\n" + middle + "\n" + known + "\n")
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
			for _, entry := range result.Entries {
				if entry.Extra != nil && entry.ContentPreview == nil && !IsRetainedUnknownCarrier(entry) {
					t.Fatal("opaque-only message fabricated a visible empty turn")
				}
			}
			if public := record.Position.Public; public == nil || public.RecordIndex != int64(line-1) || public.Position != row.Position || !strings.HasPrefix(public.SourceRef, "src_") {
				t.Fatalf("wrong source traversal: %+v", public)
			}
			if record.Namespace != row.Namespace || record.Kind != row.Kind || record.Position.Line != line || record.Position.JSONPointer != row.Pointer {
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
