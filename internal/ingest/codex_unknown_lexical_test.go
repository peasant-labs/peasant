package ingest

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/codex_unknown_lexical.yaml
var codexUnknownLexicalFixtures []byte

type codexLexicalCase struct {
	Name             string `yaml:"name"`
	Kind             string `yaml:"kind"`
	Namespace        string `yaml:"namespace"`
	Pointer          string `yaml:"pointer"`
	Record           string `yaml:"record"`
	ExpectedPayload  string `yaml:"expected_payload"`
	ExpectedSiblings int    `yaml:"expected_siblings"`
	WideSiblings     int    `yaml:"wide_siblings"`
	LeafSize         int    `yaml:"leaf_size"`
	UnknownEvery     int    `yaml:"unknown_every"`
}

func loadCodexLexicalFixtures(t *testing.T) []codexLexicalCase {
	t.Helper()
	var fixture struct {
		Required []string           `yaml:"required_names"`
		Cases    []codexLexicalCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(codexUnknownLexicalFixtures))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("trailing fixture document", trailing)
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
		t.Fatalf("fixture names %v differ from required manifest %v", names, fixture.Required)
	}
	return fixture.Cases
}

// buildWideCarriedRecord deterministically expands a wide recipe into a native
// item_completed source record plus the exact source block bytes per sibling.
// It is a fixture-construction transform only: expectations come from these
// constructed strings, never from the production capture path. Every leaf is
// exactly leafSize JSON bytes so sibling count scales linearly with bytes.
func buildWideCarriedRecord(siblings, leafSize, unknownEvery int) (record string, blocks []string, unknownIndices []int) {
	blocks = make([]string, siblings)
	for i := 0; i < siblings; i++ {
		unknown := unknownEvery > 0 && i%unknownEvery == 0
		kind := "input_text"
		if unknown {
			kind = "future"
		}
		prefix := `{"type":"` + kind + `","text":"`
		suffix := `"}`
		fill := leafSize - len(prefix) - len(suffix)
		if fill < 16 {
			panic("leaf_size too small for fixed-leaf construction")
		}
		tag := fmt.Sprintf("leaf-%06d-", i)
		text := tag + strings.Repeat("x", fill-len(tag))
		blocks[i] = prefix + text + suffix
		if unknown {
			unknownIndices = append(unknownIndices, i)
		}
	}
	var sb strings.Builder
	sb.WriteString(`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","id":"wide-item","content":[`)
	for i, b := range blocks {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(b)
	}
	sb.WriteString(`]}}}`)
	return sb.String(), blocks, unknownIndices
}

func TestCodexLexicalFidelity(t *testing.T) {
	cases := loadCodexLexicalFixtures(t)
	for _, row := range cases {
		if row.WideSiblings > 0 {
			continue
		}
		t.Run(row.Name, func(t *testing.T) {
			native := strings.HasPrefix(row.Pointer, "/payload/item/")
			position := UnknownSourcePosition{Line: 7, SourceID: "lexical-stream", Public: codexPublicPosition("lexical-thread\x00lexical-stream", 7, 0)}
			prepared, unknown, err := prepareCodexRecord([]byte(row.Record), position, native)
			if err != nil {
				t.Fatal(err)
			}
			if len(unknown) != 1 {
				t.Fatalf("retained leaves = %d, want 1", len(unknown))
			}
			got := unknown[0]
			if string(got.Payload) != row.ExpectedPayload {
				t.Fatalf("payload mismatch:\ngot  %q\nwant %q", string(got.Payload), row.ExpectedPayload)
			}
			if got.Namespace != row.Namespace || got.Kind != row.Kind || got.Position.JSONPointer != row.Pointer {
				t.Fatalf("identity mismatch: %+v", got)
			}
			if got.Position.Public == nil || got.Position.Line != 7 || !strings.HasPrefix(got.Position.Public.SourceRef, "src_") {
				t.Fatalf("coordinates lost: %+v", got.Position)
			}
			// Known siblings survive in the interpretation copy with vector
			// alignment preserved; the unknown leaf is replaced, never kept.
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal(prepared, &envelope); err != nil {
				t.Fatal(err)
			}
			target := envelope["payload"]
			if !native {
				var payloadObj map[string]json.RawMessage
				if err := json.Unmarshal(target, &payloadObj); err != nil {
					t.Fatal(err)
				}
				target = payloadObj["content"]
				if target == nil {
					target = payloadObj["summary"]
				}
			}
			if native {
				var outer map[string]json.RawMessage
				if err := json.Unmarshal(target, &outer); err != nil {
					t.Fatal(err)
				}
				target = outer["item"]
				var item map[string]json.RawMessage
				if err := json.Unmarshal(target, &item); err != nil {
					t.Fatal(err)
				}
				// Canonical type/role are restored to the original after the
				// normalized interpretation pass.
				var header struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(item["type"], &header.Type); err != nil {
					// type is a JSON string; decode directly.
					var typ string
					if err2 := json.Unmarshal(item["type"], &typ); err2 != nil {
						t.Fatal(err2)
					}
					header.Type = typ
				}
				if header.Type != "UserMessage" && header.Type != "Reasoning" {
					t.Fatalf("canonical type not restored: %s", header.Type)
				}
				target = item["content"]
				if target == nil {
					target = item["summary"]
				}
			}
			var blocks []json.RawMessage
			if err := json.Unmarshal(target, &blocks); err != nil {
				t.Fatal(err)
			}
			if len(blocks) != row.ExpectedSiblings+1 {
				t.Fatalf("sibling alignment lost: %d blocks, want %d", len(blocks), row.ExpectedSiblings+1)
			}
			for _, b := range blocks {
				if bytes.Contains(b, []byte("lexical-leaf")) || bytes.Contains(b, []byte("reason-leaf")) || bytes.Contains(b, []byte("summary-leaf")) {
					t.Fatalf("unknown source text survived in interpretation copy: %s", string(b))
				}
			}
			// At least one known sibling keeps its exact text.
			known := 0
			for _, b := range blocks {
				if bytes.Contains(b, []byte("before")) || bytes.Contains(b, []byte("after")) {
					known++
				}
			}
			if known != row.ExpectedSiblings {
				t.Fatalf("known siblings = %d, want %d", known, row.ExpectedSiblings)
			}
		})
	}
}

func TestCodexWideLexicalFidelity(t *testing.T) {
	cases := loadCodexLexicalFixtures(t)
	for _, row := range cases {
		if row.WideSiblings == 0 {
			continue
		}
		t.Run(row.Name, func(t *testing.T) {
			record, blocks, unknownIndices := buildWideCarriedRecord(row.WideSiblings, row.LeafSize, row.UnknownEvery)
			if len(record) == 0 || len(blocks) != row.WideSiblings {
				t.Fatal("wide recipe construction failed")
			}
			// Fixed per-leaf size: total source scales linearly with siblings.
			for i, b := range blocks {
				if len(b) != row.LeafSize {
					t.Fatalf("leaf %d size = %d, want %d", i, len(b), row.LeafSize)
				}
			}
			position := UnknownSourcePosition{Line: 11, SourceID: "wide-stream", Public: codexPublicPosition("wide-thread\x00wide-stream", 11, 0)}
			prepared, unknown, err := prepareCodexRecord([]byte(record), position, true)
			if err != nil {
				t.Fatal(err)
			}
			if len(unknown) != len(unknownIndices) {
				t.Fatalf("retained leaves = %d, want %d", len(unknown), len(unknownIndices))
			}
			byPointer := map[string]string{}
			for _, rec := range unknown {
				byPointer[rec.Position.JSONPointer] = string(rec.Payload)
				if rec.Position.Public == nil || rec.Position.Line != 11 {
					t.Fatalf("coordinates lost: %+v", rec.Position)
				}
			}
			for _, idx := range unknownIndices {
				pointer := fmt.Sprintf("/payload/item/content/%d", idx)
				got, ok := byPointer[pointer]
				if !ok {
					t.Fatalf("missing retained leaf %s", pointer)
				}
				if got != blocks[idx] {
					t.Fatalf("leaf %d payload mismatch:\ngot  %q\nwant %q", idx, got, blocks[idx])
				}
			}
			// Interpretation copy keeps vector alignment and known siblings.
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal(prepared, &envelope); err != nil {
				t.Fatal(err)
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(envelope["payload"], &payload); err != nil {
				t.Fatal(err)
			}
			var item map[string]json.RawMessage
			if err := json.Unmarshal(payload["item"], &item); err != nil {
				t.Fatal(err)
			}
			var out []json.RawMessage
			if err := json.Unmarshal(item["content"], &out); err != nil {
				t.Fatal(err)
			}
			if len(out) != row.WideSiblings {
				t.Fatalf("alignment lost: %d blocks, want %d", len(out), row.WideSiblings)
			}
		})
	}
}
