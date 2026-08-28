package push_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/project_wire.yaml
var projectWireYAML []byte

//go:embed testdata/project_wire.manifest.yaml
var projectWireManifestYAML []byte

// projectWireCase is one row of testdata/project_wire.yaml: a combination of
// git remote and field visibility, and the wire outcome it must produce
// (mapper.go's projectWire / projectWireLabel), or a pipeline-construction
// refusal for the nil_redactor_refused row.
type projectWireCase struct {
	Name   string `yaml:"name"`
	Why    string `yaml:"why"`
	Remote string `yaml:"remote"`

	GitRemote   *bool `yaml:"gitRemote"`
	ProjectName *bool `yaml:"projectName"`
	ProjectPath *bool `yaml:"projectPath"`

	WantName              string `yaml:"wantName"`
	WantFilePath          string `yaml:"wantFilePath"`
	WantGitRemoteAbsent   bool   `yaml:"wantGitRemoteAbsent"`
	WantHashAlwaysSent    bool   `yaml:"wantHashAlwaysSent"`
	UseRealRedactor       bool   `yaml:"useRealRedactor"`
	WantWireContains      string `yaml:"wantWireContains"`
	ExpectPipelineRefusal bool   `yaml:"expectPipelineRefusal"`
}

type projectWireFixture struct {
	Cases []projectWireCase `yaml:"cases"`
}

func loadProjectWireFixture(t *testing.T) projectWireFixture {
	t.Helper()
	var fixture projectWireFixture
	decoder := yaml.NewDecoder(bytes.NewReader(projectWireYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode project_wire.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("project_wire.yaml must contain exactly one YAML document: %v", err)
	}
	for i, c := range fixture.Cases {
		if c.Name == "" {
			t.Fatalf("project_wire.yaml case %d has no name", i)
		}
		if c.Why == "" {
			t.Fatalf("project_wire.yaml case %q has no why", c.Name)
		}
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(projectWireManifestYAML, "project_wire")
	if err != nil {
		t.Fatalf("decode project_wire.manifest.yaml: %v", err)
	}
	names := make([]string, len(fixture.Cases))
	for i, c := range fixture.Cases {
		names[i] = c.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "project_wire"); err != nil {
		t.Fatalf("project_wire.yaml fixture/manifest mismatch: %v", err)
	}
	return fixture
}

// TestProjectWire drives every combinatorial row of testdata/project_wire.yaml
// through the real production path: push.MapMetadata (mapper.go's projectWire)
// for the field-combination rows, and push.NewPipeline for the
// nil_redactor_refused row.
func TestProjectWire(t *testing.T) {
	fixture := loadProjectWireFixture(t)
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if c.ExpectPipelineRefusal {
				assertNilRedactorRefused(t)
				return
			}

			meta := fixtureMetadata()
			if c.Remote == "" {
				meta.Git.Remote = nil
			} else {
				meta.Git.Remote = &c.Remote
			}
			meta.Project.FilePath = "/home/alice/dev/app"

			fields := config.PushFieldVisibility{
				GitRemote:   c.GitRemote,
				ProjectPath: c.ProjectPath,
				ProjectName: c.ProjectName,
			}

			var redactor redact.JSONRedactor
			if c.UseRealRedactor {
				real, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
				if err != nil {
					t.Fatalf("build the redactor a production push builds: %v", err)
				}
				// Mirror the production order (pipeline.go step 1b, before
				// MapMetadata): RedactMetadata sees the WHOLE session's
				// recorded locations and collapses meta.Project.FilePath to
				// the canonical /<PATH>/<project> form. The generic
				// document-wide RedactJSON pass MapMetadata applies below
				// only strips a bare username from transcript-text-shaped
				// strings; it has no notion of "this field is the project
				// root", so skipping this step would leave the pre-rc1
				// two-placeholder form on the wire instead.
				meta = real.RedactMetadata(meta)
				redactor = real
			}

			payload, err := push.MapMetadata(push.MapOptions{
				Meta:     meta,
				Fields:   fields.Resolve(),
				Redactor: redactor,
			})
			if err != nil {
				t.Fatalf("MapMetadata: %v (why: %s)", err, c.Why)
			}

			if c.WantWireContains != "" {
				// json.Marshal HTML-escapes "<"/">" by default, which would
				// make a literal "/<PATH>/app" substring search fail on an
				// artifact of Go's encoder rather than a real absence. Decode
				// and re-encode with HTML escaping off so the check reads the
				// same bytes any other JSON consumer (the village) would see
				// after decoding, not encoding/json's default escaping.
				var generic any
				if err := json.Unmarshal(payload, &generic); err != nil {
					t.Fatalf("unmarshal published wire: %v", err)
				}
				var unescaped bytes.Buffer
				encoder := json.NewEncoder(&unescaped)
				encoder.SetEscapeHTML(false)
				if err := encoder.Encode(generic); err != nil {
					t.Fatalf("re-encode published wire without HTML escaping: %v", err)
				}
				if !strings.Contains(unescaped.String(), c.WantWireContains) {
					t.Fatalf("published wire does not contain %q; body=%s (why: %s)", c.WantWireContains, unescaped.String(), c.Why)
				}
				return
			}

			var m map[string]any
			if err := json.Unmarshal(payload, &m); err != nil {
				t.Fatalf("unmarshal publish request: %v", err)
			}
			project, ok := m["project"].(map[string]any)
			if !ok {
				t.Fatalf("project is not a map, got %T", m["project"])
			}
			if name, _ := project["name"].(string); name != c.WantName {
				t.Errorf("project.name = %q, want %q (why: %s)", name, c.WantName, c.Why)
			}
			if path, _ := project["filePath"].(string); path != c.WantFilePath {
				t.Errorf("project.filePath = %q, want %q (why: %s)", path, c.WantFilePath, c.Why)
			}
			if c.WantGitRemoteAbsent {
				git, _ := m["git"].(map[string]any)
				if _, present := git["remote"]; present {
					t.Errorf("git.remote is present, want absent (why: %s)", c.Why)
				}
			}
			if c.WantHashAlwaysSent {
				hash, _ := project["hash"].(string)
				if hash != string(testutil.TestProjectHash) {
					t.Errorf("project.hash = %q, want %q (always sent; why: %s)", hash, testutil.TestProjectHash, c.Why)
				}
			}
		})
	}
}

// assertNilRedactorRefused proves the nil_redactor_refused row: NewPipeline
// refuses to construct a Pipeline when handed a nil redactor.
func assertNilRedactorRefused(t *testing.T) {
	t.Helper()
	pipeline, err := push.NewPipeline(
		&testutil.StubPushStore{},
		&testutil.StubPublisher{},
		baseCreds(),
		baseTestConfig(),
		testutil.NewMemFS(),
		push.PipelineConfig{},
		nil,
		&strings.Builder{},
	)
	if err == nil {
		t.Fatal("NewPipeline(nil redactor) returned no error; want a refusal")
	}
	if pipeline != nil {
		t.Fatal("NewPipeline(nil redactor) returned a non-nil Pipeline alongside an error")
	}
}

// --- mutation-proof helpers (documented in the completion report, not run in CI) ---
//
// The four mutation proofs named in the slice ("make the three fields plain
// bools", "drop the FromRemote call", "send FilePath alongside the label",
// "remove the nil-redactor refusal") are proved by hand against this file:
// reverting mapper.go's fields to plain bool, deleting the projectlabel.FromRemote
// call, unconditionally setting both Name and FilePath, or deleting the
// `redactor == nil` guard in pipeline.go each fail a specific row above
// (remote_present_sends_label_not_path, remote_present_sends_label_not_path,
// remote_present_sends_label_not_path, nil_redactor_refused respectively).
