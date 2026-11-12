package harnesslayout

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

type antigravityRootsFixture struct {
	Cases []struct {
		Name  string `yaml:"name"`
		Check string `yaml:"check"`
		Tools []Tool `yaml:"tools"`
		Root  string `yaml:"root"`
		Path  string `yaml:"path"`
		Owner Tool   `yaml:"owner"`
	} `yaml:"cases"`
}

// antigravityTestEnvs resolves root templates for every operating system.
var antigravityTestEnvs = []Env{
	{GOOS: OSLinux, Home: "/home/u", ConfigDir: "/home/u/.config"},
	{GOOS: OSDarwin, Home: "/Users/u", ConfigDir: "/Users/u/Library/Application Support"},
	{GOOS: OSWindows, Home: `C:\Users\u`, ConfigDir: `C:\Users\u\AppData\Roaming`},
}

func resolveTemplate(env Env, template string) string {
	return env.Resolve(Layout{Roots: []Root{{Path: template}}})[0]
}

func lookupLayout(t *testing.T, tool Tool) Layout {
	t.Helper()
	layout, ok := Lookup(tool)
	if !ok {
		t.Fatalf("tool %q is not registered", tool)
	}
	return layout
}

// pathWithin reports whether inner equals outer or lies below it.
func pathWithin(outer, inner string) bool {
	rel, err := filepath.Rel(outer, inner)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func pathsOverlap(a, b string) bool {
	return pathWithin(a, b) || pathWithin(b, a)
}

// TestAntigravityRoots pins the roots of the two Antigravity tools against
// each other, the shared ~/.gemini/config, and the Cline root inside the
// Antigravity desktop application.
func TestAntigravityRoots(t *testing.T) {
	var fixture antigravityRootsFixture
	loadAntigravityFixture(t, "antigravity_roots", &fixture)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			for _, env := range antigravityTestEnvs {
				switch tc.Check {
				case "root-is":
					got := env.Resolve(lookupLayout(t, tc.Tools[0]))
					want := []string{resolveTemplate(env, tc.Root)}
					if !reflect.DeepEqual(got, want) {
						t.Errorf("%s on %s: roots %q, want %q", tc.Tools[0], env.GOOS, got, want)
					}
				case "roots-disjoint":
					for i, a := range tc.Tools {
						for _, b := range tc.Tools[i+1:] {
							for _, ra := range env.Resolve(lookupLayout(t, a)) {
								for _, rb := range env.Resolve(lookupLayout(t, b)) {
									if pathsOverlap(ra, rb) {
										t.Errorf("on %s: root %s of %s and root %s of %s overlap", env.GOOS, ra, a, rb, b)
									}
								}
							}
						}
					}
				case "outside-tools":
					target := resolveTemplate(env, tc.Path)
					if tc.Owner != "" && !slices.Contains(env.Resolve(lookupLayout(t, tc.Owner)), target) {
						t.Errorf("on %s: %s is not a root of %s", env.GOOS, target, tc.Owner)
					}
					for _, tool := range tc.Tools {
						for _, root := range env.Resolve(lookupLayout(t, tool)) {
							if pathsOverlap(root, target) {
								t.Errorf("on %s: %s overlaps root %s of %s", env.GOOS, target, root, tool)
							}
						}
					}
				default:
					t.Fatalf("unknown check %q; use root-is, roots-disjoint, or outside-tools", tc.Check)
				}
			}
		})
	}
}

type antigravityWantField struct {
	Path  string     `yaml:"path"`
	Types []JSONType `yaml:"types"`
}

type antigravityCasesFixture struct {
	Cases []struct {
		Name  string                    `yaml:"name"`
		Roots []antigravityScenarioRoot `yaml:"roots"`
		Check string                    `yaml:"check"`
		Want  struct {
			Sessions  map[Tool][]string                 `yaml:"sessions"`
			Artifacts []string                          `yaml:"artifacts"`
			Present   map[Tool][]string                 `yaml:"present"`
			Absent    map[Tool][]string                 `yaml:"absent"`
			Tables    map[string][]string               `yaml:"tables"`
			Kinds     map[string]int                    `yaml:"kinds"`
			Fields    map[string][]antigravityWantField `yaml:"fields"`
			Sized     map[Tool][]string                 `yaml:"sized"`
		} `yaml:"want"`
	} `yaml:"cases"`
}

// antigravityScenario is the result of capturing every root of one case with
// the tool of that root, as `peasant layout capture --limit 0` does.
type antigravityScenario struct {
	tools     []Tool
	reports   map[Tool]Report
	recorders map[Tool][]*antigravityOpenRecorder
}

func runAntigravityScenario(t *testing.T, store antigravityStore, roots []antigravityScenarioRoot) antigravityScenario {
	t.Helper()
	parent := t.TempDir()
	paths := map[Tool][]string{}
	scenario := antigravityScenario{reports: map[Tool]Report{}, recorders: map[Tool][]*antigravityOpenRecorder{}}
	for i, root := range roots {
		dir := store.build(t, filepath.Join(parent, string(rune('a'+i))), root)
		if _, seen := paths[root.Tool]; !seen {
			scenario.tools = append(scenario.tools, root.Tool)
		}
		paths[root.Tool] = append(paths[root.Tool], dir)
	}
	for _, tool := range scenario.tools {
		open := func(dir string) Source {
			recorder := &antigravityOpenRecorder{fsys: os.DirFS(dir)}
			scenario.recorders[tool] = append(scenario.recorders[tool], recorder)
			return Source{FS: recorder, Dir: dir}
		}
		report, err := Run(context.Background(), lookupLayout(t, tool), paths[tool], open, 0)
		if err != nil {
			t.Fatalf("capture %s: %v", tool, err)
		}
		scenario.reports[tool] = report
	}
	return scenario
}

func (s antigravityScenario) opened(pattern string, tool Tool) []string {
	var out []string
	for _, recorder := range s.recorders[tool] {
		out = append(out, recorder.openedMatching(pattern)...)
	}
	return out
}

func shapeByName(capture Capture, name string) (ArtifactShape, bool) {
	for _, shape := range capture.Shapes {
		if shape.Artifact == name {
			return shape, true
		}
	}
	return ArtifactShape{}, false
}

func assertNoSentinel(t *testing.T, store antigravityStore, scenario antigravityScenario) {
	t.Helper()
	for _, tool := range scenario.tools {
		for label, report := range map[string]Report{"default": scenario.reports[tool].ShapeOnly(), "--include-metadata": scenario.reports[tool]} {
			encoded, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), store.Sentinel) {
				t.Errorf("%s report of %s holds the sentinel %q:\n%s", label, tool, store.Sentinel, encoded)
			}
		}
	}
}

func assertCaptured(t *testing.T, scenario antigravityScenario) {
	t.Helper()
	for _, tool := range scenario.tools {
		report := scenario.reports[tool]
		if len(report.Captures) == 0 || len(report.Failures) > 0 {
			t.Fatalf("%s captured %d sessions with failures %+v, want captures and no failure", tool, len(report.Captures), report.Failures)
		}
	}
}

// TestAntigravityCases runs the two registered probes on synthetic stores.
func TestAntigravityCases(t *testing.T) {
	var fixture antigravityCasesFixture
	loadAntigravityFixture(t, "antigravity_cases", &fixture)
	store := loadAntigravityStore(t)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			scenario := runAntigravityScenario(t, store, tc.Roots)
			want := tc.Want
			switch tc.Check {
			case "sessions":
				for _, tool := range scenario.tools {
					wantIDs, ok := want.Sessions[tool]
					if !ok {
						t.Fatalf("want.sessions names no session list for %s", tool)
					}
					report := scenario.reports[tool]
					var got []string
					for _, capture := range report.Captures {
						got = append(got, capture.Session)
					}
					sort.Strings(got)
					if !slices.Equal(got, wantIDs) {
						t.Errorf("%s captured %q, want %q", tool, got, wantIDs)
					}
					for _, root := range report.Roots {
						if root.Error != "" {
							t.Errorf("%s root %s: error %q, want none", tool, root.Path, root.Error)
						}
					}
					if len(report.Failures) > 0 {
						t.Errorf("%s failures %+v, want none", tool, report.Failures)
					}
				}
			case "capture-tool":
				assertCaptured(t, scenario)
				for _, tool := range scenario.tools {
					report := scenario.reports[tool]
					if report.Tool != tool {
						t.Errorf("report of a %s root carries tool %q", tool, report.Tool)
					}
					for _, capture := range report.Captures {
						if capture.Tool != tool {
							t.Errorf("capture %s of a %s root carries tool %q", capture.Session, tool, capture.Tool)
						}
					}
				}
			case "same-core-shape":
				assertCaptured(t, scenario)
				if len(scenario.tools) != 2 {
					t.Fatalf("same-core-shape needs two tools, got %v", scenario.tools)
				}
				a, b := scenario.reports[scenario.tools[0]].Captures[0], scenario.reports[scenario.tools[1]].Captures[0]
				for _, name := range want.Artifacts {
					shapeA, okA := shapeByName(a, name)
					shapeB, okB := shapeByName(b, name)
					if !okA || !okB {
						t.Errorf("artifact %s: present for %s %v, for %s %v; want both", name, scenario.tools[0], okA, scenario.tools[1], okB)
						continue
					}
					shapeA.Path, shapeB.Path = "", ""
					if !reflect.DeepEqual(shapeA, shapeB) {
						t.Errorf("artifact %s differs between the tools:\n%+v\n%+v", name, shapeA, shapeB)
					}
				}
			case "artifacts":
				assertCaptured(t, scenario)
				for tool, names := range want.Present {
					for _, capture := range scenario.reports[tool].Captures {
						for _, name := range names {
							if _, ok := shapeByName(capture, name); !ok {
								t.Errorf("%s capture %s has no %s artifact", tool, capture.Session, name)
							}
						}
					}
				}
				for tool, names := range want.Absent {
					for _, capture := range scenario.reports[tool].Captures {
						for _, name := range names {
							if _, ok := shapeByName(capture, name); ok {
								t.Errorf("%s capture %s reports %s, which only the other product writes", tool, capture.Session, name)
							}
						}
					}
				}
			case "body-shape":
				assertCaptured(t, scenario)
				shape, ok := shapeByName(scenario.reports[scenario.tools[0]].Captures[0], antigravityBody.Name)
				if !ok {
					t.Fatal("no conversation-body artifact")
				}
				var wantPaths, gotPaths []string
				for table, columns := range want.Tables {
					for _, column := range columns {
						wantPaths = append(wantPaths, "$."+table+"."+column)
					}
				}
				for _, field := range shape.Fields {
					gotPaths = append(gotPaths, field.Path)
				}
				sort.Strings(wantPaths)
				sort.Strings(gotPaths)
				if !slices.Equal(gotPaths, wantPaths) {
					t.Errorf("body fields %q, want %q", gotPaths, wantPaths)
				}
				if !reflect.DeepEqual(shape.Kinds, want.Kinds) {
					t.Errorf("body kinds %v, want %v", shape.Kinds, want.Kinds)
				}
			case "no-sentinel":
				assertCaptured(t, scenario)
				assertNoSentinel(t, store, scenario)
			case "title-empty":
				assertCaptured(t, scenario)
				for _, tool := range scenario.tools {
					for _, capture := range scenario.reports[tool].Captures {
						if capture.Metadata.Title != "" {
							t.Errorf("%s capture %s has title %q, want none", tool, capture.Session, capture.Metadata.Title)
						}
					}
				}
			case "fields":
				assertCaptured(t, scenario)
				assertNoSentinel(t, store, scenario)
				capture := scenario.reports[scenario.tools[0]].Captures[0]
				for name, fields := range want.Fields {
					shape, ok := shapeByName(capture, name)
					if !ok {
						t.Errorf("no %s artifact", name)
						continue
					}
					for _, field := range fields {
						found := false
						for _, got := range shape.Fields {
							if got.Path == field.Path {
								found = true
								if !slices.Equal(got.Types, field.Types) {
									t.Errorf("%s %s: types %v, want %v", name, field.Path, got.Types, field.Types)
								}
							}
						}
						if !found {
							t.Errorf("%s has no field %s", name, field.Path)
						}
					}
				}
			case "sized":
				assertCaptured(t, scenario)
				assertNoSentinel(t, store, scenario)
				for tool, names := range want.Sized {
					layout := lookupLayout(t, tool)
					for _, name := range names {
						for _, capture := range scenario.reports[tool].Captures {
							shape, ok := shapeByName(capture, name)
							if !ok || shape.Records == 0 || shape.Bytes == 0 {
								t.Errorf("%s capture %s: %s = %+v, want a file count and a size", tool, capture.Session, name, shape)
							}
						}
						for _, artifact := range layout.Artifacts {
							if artifact.Name != name {
								continue
							}
							if opened := scenario.opened(artifact.Pattern, tool); len(opened) > 0 {
								t.Errorf("%s opened %q of %s, want sized only", tool, opened, name)
							}
						}
					}
				}
			case "credentials":
				if os.Geteuid() == 0 {
					t.Log("running as root: mode 0000 does not restrict reads, so this case rests on the open recorder and the sentinel")
				}
				assertCaptured(t, scenario)
				assertNoSentinel(t, store, scenario)
				for _, tool := range scenario.tools {
					for _, name := range store.Products[tool].Credentials {
						if opened := scenario.opened(name, tool); len(opened) > 0 {
							t.Errorf("%s opened the credential file %q", tool, opened)
						}
					}
				}
			case "workspace-path":
				assertCaptured(t, scenario)
				for _, tool := range scenario.tools {
					report := scenario.reports[tool]
					for i, capture := range report.Captures {
						if got := capture.Metadata.ProjectPath; got != store.WorkspacePath {
							t.Errorf("%s with metadata: project path %q, want %q", tool, got, store.WorkspacePath)
						}
						if got := report.ShapeOnly().Captures[i].Metadata.ProjectPath; got != "" {
							t.Errorf("%s by default: project path %q, want none", tool, got)
						}
					}
				}
			default:
				t.Fatalf("unknown check %q", tc.Check)
			}
		})
	}
}
