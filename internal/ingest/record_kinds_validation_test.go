package ingest

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/record_kinds_validation.yaml
var recordKindsValidationYAML []byte

type recordKindsValidationFixtures struct {
	RequiredNames []string `yaml:"required_names"`
	Cases         []struct {
		Name      string            `yaml:"name"`
		Operation string            `yaml:"operation"`
		Find      string            `yaml:"find"`
		Replace   string            `yaml:"replace"`
		File      string            `yaml:"file"`
		Error     string            `yaml:"error"`
		Harness   Harness           `yaml:"harness"`
		Context   RecordKindContext `yaml:"context"`
		Namespace string            `yaml:"namespace"`
		Kind      string            `yaml:"kind"`
		Status    RecordKindStatus  `yaml:"status"`
		Preview   RecordKindPreview `yaml:"preview"`
	} `yaml:"cases"`
}

func decodeRegistryFixture(t *testing.T, raw []byte, target any) {
	t.Helper()
	d := yaml.NewDecoder(bytes.NewReader(raw))
	d.KnownFields(true)
	if err := d.Decode(target); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		t.Fatalf("fixture must have exactly one document: %v", err)
	}
}

func checkRegistryFixtureNames(t *testing.T, actual map[string]bool, required []string) {
	t.Helper()
	want := map[string]bool{}
	for _, name := range required {
		if name == "" || want[name] {
			t.Fatalf("empty or duplicate required name %q", name)
		}
		want[name] = true
	}
	for name := range want {
		if !actual[name] {
			t.Errorf("required fixture missing: %s", name)
		}
	}
	for name := range actual {
		if name == "" || !want[name] {
			t.Errorf("fixture not in required-name manifest: %s", name)
		}
	}
}

func verifyControlBehavior(section RecordKindHarness, namespace, name string) error {
	row := section.Lookup(RecordKindRetained, namespace, name)
	probe := name
	if name == "compact_boundary" {
		probe = "compact-boundary"
	}
	line := claudeIndexLine{Type: probe}
	if namespace == "system_subtype" {
		line.Type, line.Subtype = "system", name
	}
	raw := []byte(`{"type":"` + line.Type + `","subtype":"` + line.Subtype + `"}`)
	part, extra, preview := claudeControlRecordFields(raw, line)
	if part == nil || extra == nil {
		return fmt.Errorf("control probe did not produce retained control fields")
	}
	status, previewPolicy := RecordKindTrackedOnly, RecordKindPreviewNo
	if preview != nil && *preview != "" {
		status, previewPolicy = RecordKindRepresented, RecordKindPreviewYes
	}
	if row.Status != status || row.Preview != previewPolicy {
		return fmt.Errorf("control behavior differs: registry %s/%s, production %s/%s", row.Status, row.Preview, status, previewPolicy)
	}
	return nil
}

func TestRecordKindsValidationMutations(t *testing.T) {
	var fixture recordKindsValidationFixtures
	decodeRegistryFixture(t, recordKindsValidationYAML, &fixture)
	names := map[string]bool{}
	for _, row := range fixture.Cases {
		if names[row.Name] {
			t.Fatal("duplicate", row.Name)
		}
		names[row.Name] = true
	}
	checkRegistryFixtureNames(t, names, fixture.RequiredNames)
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			registry, err := LoadRecordKindRegistry()
			if err != nil {
				t.Fatal(err)
			}
			var result error
			switch row.Operation {
			case "yaml":
				if !strings.Contains(string(recordKindsYAML), row.Find) {
					t.Fatal("mutation target not found")
				}
				_, result = decodeRecordKindRegistry([]byte(strings.Replace(string(recordKindsYAML), row.Find, row.Replace, 1)))
			case "trailing":
				_, result = decodeRecordKindRegistry(append(append([]byte{}, recordKindsYAML...), []byte("\n---\nversion: 2\n")...))
			case "source":
				raw, err := os.ReadFile(row.File)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Count(string(raw), row.Find) != 1 {
					t.Fatal("source mutation must target exactly one location")
				}
				changed := []byte(strings.Replace(string(raw), row.Find, row.Replace, 1))
				tree, err := readRecordKindSourceTree(map[string][]byte{row.File: changed})
				if err != nil {
					t.Fatal(err)
				}
				result = verifyRecordKindSources(registry, tree)
			case "delete-row":
				section := registry.Harnesses[row.Harness]
				found := false
				for i := range section.Inventories {
					inv := &section.Inventories[i]
					if inv.Context != row.Context || inv.Namespace != row.Namespace {
						continue
					}
					for j, kind := range inv.Kinds {
						if kind.Kind == row.Kind {
							inv.Kinds = append(inv.Kinds[:j], inv.Kinds[j+1:]...)
							found = true
							break
						}
					}
				}
				if !found {
					t.Fatal("row deletion did not occur")
				}
				registry.Harnesses[row.Harness] = section
				tree, err := readRecordKindSourceTree(nil)
				if err != nil {
					t.Fatal(err)
				}
				result = verifyRecordKindSources(registry, tree)
			case "behavior", "mutate-status", "mutate-preview":
				section := registry.Harnesses[row.Harness]
				for i := range section.Kinds {
					kind := &section.Kinds[i]
					if kind.Context == row.Context && kind.Namespace == row.Namespace && kind.Kind == row.Kind {
						if row.Operation == "mutate-status" {
							kind.Status = row.Status
						}
						if row.Operation == "mutate-preview" {
							kind.Preview = row.Preview
						}
					}
				}
				result = verifyControlBehavior(section, row.Namespace, row.Kind)
			case "lookup":
				kind := registry.Harnesses[row.Harness].Lookup(row.Context, row.Namespace, row.Kind)
				if kind.Status != row.Status || kind.Preview != row.Preview {
					result = fmt.Errorf("lookup %s/%s, want %s/%s", kind.Status, kind.Preview, row.Status, row.Preview)
				}
			case "diagnostic":
				kind := registry.Harnesses[row.Harness].Lookup(row.Context, row.Namespace, row.Kind)
				attribution, ok := codexContentKindRegistry()[row.Kind]
				if !ok || !attribution.mediaDiagnostic || kind.Status != RecordKindRepresented || kind.Preview != RecordKindPreviewYes || !strings.Contains(kind.Payload, "no standalone submitted-input count") {
					result = fmt.Errorf("diagnostic-only attribution differs from production")
				}
			default:
				t.Fatal("unknown fixture operation", row.Operation)
			}
			if row.Error == "" {
				if result != nil {
					t.Fatal(result)
				}
			} else if result == nil || !strings.Contains(result.Error(), row.Error) {
				t.Fatalf("got %v, want error %q", result, row.Error)
			}
		})
	}
}

func TestRecordKindsNamespacesDoNotCollide(t *testing.T) {
	registry, err := LoadRecordKindRegistry()
	if err != nil {
		t.Fatal(err)
	}
	section := registry.Harnesses[HarnessCodex]
	for key, row := range section.KindsByKey() {
		if key != row.Key() {
			t.Fatal("unstable key")
		}
	}
	// Existing distinct message contexts must never be silently overwritten by
	// the legacy name-only API. Qualified reporting is the authoritative API.
	if _, ok := section.KindsByName()["message"]; ok {
		t.Fatal("ambiguous name-only key survived")
	}
	encoded, err := json.Marshal(registry.TrackedNotVisualized())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"namespace"`)) || !bytes.Contains(encoded, []byte(`"context"`)) {
		t.Fatal("report lost qualified identity")
	}
}

func TestRecordKindsEverySupportedHarnessHasOpenFallback(t *testing.T) {
	registry, err := LoadRecordKindRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for harness := range DefaultAdapterRegistry {
		section, ok := registry.Harnesses[harness]
		if !ok {
			t.Fatalf("missing supported harness %s", harness)
		}
		for _, inventory := range section.Inventories {
			name := "valid_kind_not_declared_in_the_registry"
			row := section.Lookup(inventory.Context, inventory.Namespace, name)
			if row.Status != RecordKindRetainedUnknown || row.Visualized != RecordKindHidden || row.Preview != RecordKindPreviewNo || row.Kind != name {
				t.Errorf("%s/%s/%s lacks open hidden fallback", harness, inventory.Context, inventory.Namespace)
			}
		}
	}
}
