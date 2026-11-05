package sessionreview_test

import (
	"bytes"
	_ "embed"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/sessionreview"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/detectors.yaml
var detectorsYAML []byte

//go:embed testdata/detectors.manifest.yaml
var detectorsManifestYAML []byte

type detectorEntry struct {
	Role       string `yaml:"role"`
	Text       string `yaml:"text"`
	Output     string `yaml:"output"`
	ToolCallID string `yaml:"toolCallId"`
	ToolInput  string `yaml:"toolInput"`
	ToolKind   string `yaml:"toolKind"`
	IsError    bool   `yaml:"isError"`
}

type detectorWant struct {
	Kind    string `yaml:"kind"`
	Entry   int    `yaml:"entry"`
	Role    string `yaml:"role"`
	Rule    string `yaml:"rule"`
	Preview string `yaml:"preview"`
}

type detectorCase struct {
	Name        string          `yaml:"name"`
	OwnerEmails []string        `yaml:"ownerEmails"`
	Entries     []detectorEntry `yaml:"entries"`
	Want        []detectorWant  `yaml:"want"`
}

type detectorsFixture struct {
	ClosedLists struct {
		FindingKinds        []string `yaml:"findingKinds"`
		CredentialFileNames []string `yaml:"credentialFileNames"`
		ShareableHosts      []string `yaml:"shareableHosts"`
	} `yaml:"closedLists"`
	Cases []detectorCase `yaml:"cases"`
}

func loadDetectorsFixture(t *testing.T) detectorsFixture {
	t.Helper()
	var fixture detectorsFixture
	decoder := yaml.NewDecoder(bytes.NewReader(detectorsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode detectors.yaml: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(detectorsManifestYAML, "detectors")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "detectors"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func standardRedactor(t *testing.T) redact.Redactor {
	t.Helper()
	r, err := redact.NewRedactor(redact.RedactionLevel("standard"), nil, redact.XDGPaths{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func entriesOf(c detectorCase) []schema.SessionEntry {
	entries := make([]schema.SessionEntry, len(c.Entries))
	for i, e := range c.Entries {
		entry := schema.SessionEntry{EntryIndex: i, Role: schema.Role(e.Role), IsError: e.IsError}
		if e.Text != "" {
			entry.ContentPreview = &c.Entries[i].Text
		}
		if e.Output != "" {
			entry.ToolOutput = &c.Entries[i].Output
		}
		if e.ToolCallID != "" {
			entry.ToolCallID = &c.Entries[i].ToolCallID
		}
		if e.ToolInput != "" {
			entry.ToolInput = &c.Entries[i].ToolInput
		}
		if e.ToolKind != "" {
			kind := schema.ToolCallKind(e.ToolKind)
			entry.ToolKind = &kind
		}
		entries[i] = entry
	}
	return entries
}

func TestReviewDetectors(t *testing.T) {
	t.Parallel()
	for _, tc := range loadDetectorsFixture(t).Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			got := sessionreview.Review(entriesOf(tc), standardRedactor(t), sessionreview.Options{OwnerEmails: tc.OwnerEmails})
			if tc.Name == "preview_max_length" {
				if len(got) != len(tc.Entries) {
					t.Fatalf("findings = %+v, want one per entry", got)
				}
				for _, finding := range got {
					if len(finding.Preview) > redact.PreviewMaxLength {
						t.Errorf("preview %q is %d bytes, over %d", finding.Preview, len(finding.Preview), redact.PreviewMaxLength)
					}
				}
				return
			}
			if len(got) != len(tc.Want) {
				t.Fatalf("findings = %+v, want %+v", got, tc.Want)
			}
			for i, want := range tc.Want {
				finding := got[i]
				wanted := sessionreview.Finding{
					Kind: sessionreview.FindingKind(want.Kind), EntryIndex: want.Entry,
					Role: schema.Role(want.Role), Rule: want.Rule, Preview: want.Preview,
				}
				if finding != wanted {
					t.Errorf("finding %d = %+v, want %+v", i, finding, wanted)
				}
			}
		})
	}
}

// TestFindingKindClosedSet pins the kinds and the constructor (case
// finding_kind_closed_set).
func TestFindingKindClosedSet(t *testing.T) {
	t.Parallel()
	fixture := loadDetectorsFixture(t)
	got := make([]string, 0, len(sessionreview.AllFindingKinds))
	for _, kind := range sessionreview.AllFindingKinds {
		got = append(got, string(kind))
		if _, err := sessionreview.NewFindingKind(string(kind)); err != nil {
			t.Errorf("NewFindingKind(%q): %v", kind, err)
		}
	}
	if !slices.Equal(got, fixture.ClosedLists.FindingKinds) {
		t.Errorf("AllFindingKinds = %v, want %v", got, fixture.ClosedLists.FindingKinds)
	}
	if _, err := sessionreview.NewFindingKind("x"); err == nil {
		t.Error(`NewFindingKind("x") accepted an unknown kind`)
	}
}

func TestDetectorClosedLists(t *testing.T) {
	t.Parallel()
	fixture := loadDetectorsFixture(t)
	if !slices.Equal(sessionreview.CredentialFileNames, fixture.ClosedLists.CredentialFileNames) {
		t.Errorf("CredentialFileNames = %v", sessionreview.CredentialFileNames)
	}
	hosts := make([]string, 0, len(sessionreview.ShareableHosts))
	for _, host := range sessionreview.ShareableHosts {
		hosts = append(hosts, host.Host)
	}
	if !slices.Equal(hosts, fixture.ClosedLists.ShareableHosts) {
		t.Errorf("ShareableHosts = %v", hosts)
	}
	// Each host of the closed list is exercised by the per-host case.
	var perHost detectorCase
	for _, c := range fixture.Cases {
		if c.Name == "shareable_link_each_host" {
			perHost = c
		}
	}
	for _, host := range hosts {
		if !slices.ContainsFunc(perHost.Entries, func(e detectorEntry) bool { return strings.Contains(e.Text, host) }) {
			t.Errorf("no shareable_link_each_host entry for host %q", host)
		}
	}
}
