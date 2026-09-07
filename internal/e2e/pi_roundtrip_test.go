package e2e

import (
	"bytes"
	_ "embed"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/pi_roundtrip.yaml
var piRoundTripYAML []byte

//go:embed testdata/pi_roundtrip.manifest.yaml
var piRoundTripManifest []byte

type piRoundTripCase struct {
	Name            string                               `yaml:"name"`
	NativeCase      string                               `yaml:"nativeCase"`
	SessionID       string                               `yaml:"sessionID"`
	Capabilities    []schema.ContentCapability           `yaml:"capabilities"`
	Contents        []string                             `yaml:"contents"`
	Forbidden       []string                             `yaml:"forbidden"`
	MetadataOnly    string                               `yaml:"metadataOnly"`
	Placeholders    int                                  `yaml:"placeholders"`
	Metadata        int                                  `yaml:"metadata"`
	MetadataSources map[schema.NativeMetadataKind]string `yaml:"metadataSources"`
	Tool            struct {
		NativeID  string `yaml:"nativeID"`
		Name      string `yaml:"name"`
		Arguments string `yaml:"arguments"`
		Result    string `yaml:"result"`
		IsError   bool   `yaml:"isError"`
	} `yaml:"tool"`
	AssistantCost      *schema.RecordedCostAmount `yaml:"assistantCost"`
	AssistantTokens    map[string]int64           `yaml:"assistantTokens"`
	SourceReplacements []struct {
		From string `yaml:"from"`
		To   string `yaml:"to"`
	} `yaml:"sourceReplacements"`
	Owners []struct {
		NativeID     string                   `yaml:"nativeID"`
		Scope        schema.UsageScope        `yaml:"scope"`
		Completeness schema.UsageCompleteness `yaml:"completeness"`
	} `yaml:"owners"`
	InvalidContent []struct {
		Name        string `yaml:"name"`
		Find        string `yaml:"find"`
		Replacement string `yaml:"replacement"`
		Repeat      int    `yaml:"repeat"`
		Status      int    `yaml:"status"`
	} `yaml:"invalidContent"`
}

func loadPiRoundTripCases(t *testing.T) []piRoundTripCase {
	t.Helper()
	var fixture struct {
		Cases []piRoundTripCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(piRoundTripYAML))
	decoder.KnownFields(true)
	piNoError(t, decoder.Decode(&fixture))
	var trailing any
	piEqual(t, io.EOF, decoder.Decode(&trailing))
	manifest, err := testutil.DecodeRequiredNamesManifest(piRoundTripManifest, "Pi roundtrip")
	piNoError(t, err)
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
		piCheck(t, c.NativeCase != "" && c.SessionID != "" && c.MetadataOnly != "", "fixture string expectations must be populated")
		if c.AssistantCost != nil {
			piNoError(t, schema.ValidateUsageDetail(schema.UsageDetail{
				OwnerID: "fixture-cost", SourceEntryRef: "fixture-cost", Scope: schema.UsageScopeAssistant,
				Completeness: schema.UsageUnknown,
				Cost:         &schema.RecordedCostDetail{Source: schema.RecordedCostSourceHarnessEstimate, Total: c.AssistantCost},
			}))
		}
		piCheck(t, len(c.Capabilities) > 0 && len(c.Contents) > 0 && len(c.Forbidden) > 0 && len(c.Owners) > 0, "fixture evidence expectations must be populated")
		piCheck(t, c.Placeholders > 0 && c.Metadata > 0, "fixture must require placeholders and metadata")
		piCheck(t, len(c.MetadataSources) == c.Metadata && c.Tool.NativeID != "" && c.Tool.Name != "" && c.Tool.Arguments != "" && c.Tool.Result != "", "fixture must name exact metadata sources and paired tool evidence")
		for _, replacement := range c.SourceReplacements {
			piCheck(t, replacement.From != "" && replacement.To != "", "native fixture substitutions require nonempty source and replacement")
		}
		for _, invalid := range c.InvalidContent {
			names = append(names, invalid.Name)
			piCheck(t, invalid.Find != "" && invalid.Replacement != "" && invalid.Status >= 400 && invalid.Status < 500 && invalid.Repeat >= 0 && invalid.Repeat <= 100000, "invalid content fixture needs bounded substitution and exact client-error status")
		}
	}
	piNoError(t, testutil.ValidateRequiredNames(manifest, names, "Pi roundtrip"))
	return fixture.Cases
}

// Reuse the native ingestion corpus, not a hand-authored public payload. That
// corpus owns its strict field/required-name validation; this reader selects one
// source by name while leaving its other native-parser assertions there.
func piRoundTripNativeSource(t *testing.T, name string) string {
	t.Helper()
	wd, err := os.Getwd()
	piNoError(t, err)
	raw, err := os.ReadFile(filepath.Join(peasantRepoRoot(wd), "internal", "ingest", "testdata", "pi_source_v3_structure.yaml"))
	piNoError(t, err)
	var corpus struct {
		Cases []struct{ Name, Source string }
	}
	piNoError(t, yaml.Unmarshal(raw, &corpus))
	var selected []string
	for _, c := range corpus.Cases {
		if c.Name == name {
			selected = append(selected, c.Source)
		}
	}
	piEqual(t, 1, len(selected), "native source selector must resolve exactly one named fixture")
	piCheck(t, selected[0] != "", "native source must be nonempty")
	return selected[0]
}

func TestPiRoundTripFixtureSources(t *testing.T) {
	for _, c := range loadPiRoundTripCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			piCheck(t, piRoundTripNativeSource(t, c.NativeCase) != "", "native fixture source required")
		})
	}
}

func piNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func piCheck(t *testing.T, ok bool, reason string) {
	t.Helper()
	if !ok {
		t.Fatal(reason)
	}
}

func piEqual(t *testing.T, want, got any, reason ...string) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("%v: got %#v, want %#v", reason, got, want)
	}
}
