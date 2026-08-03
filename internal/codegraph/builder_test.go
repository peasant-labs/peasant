//go:build cgo

// cgo-only: the shared fixture tree (fixtures_test.go) contains TypeScript
// sources, whose import extraction needs the tree-sitter C bindings. The
// build-mode availability gate that runs in BOTH legs is availability_test.go.

package codegraph_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/codegraph"
)

func TestBuild_Fixture_Edges(t *testing.T) {
	g := mustBuild(t, fixtureFiles())
	if !reflect.DeepEqual(g.Edges, fixtureEdges()) {
		t.Errorf("edges mismatch:\n got: %#v\nwant: %#v", g.Edges, fixtureEdges())
	}
	if len(g.Violations) != 0 {
		t.Errorf("expected no violations, got %#v", g.Violations)
	}
}

func TestBuild_Fixture_NodeTree(t *testing.T) {
	g := mustBuild(t, fixtureFiles())

	cases := []struct {
		id        string
		parent    string
		kind      codegraph.NodeKind
		name      string
		language  codegraph.Language
		loc       int
		fileCount int
	}{
		{"cmd", "", codegraph.NodeKindModule, "cmd", codegraph.LanguageGo, 10, 1},
		{"cmd/app", "cmd", codegraph.NodeKindPackage, "app", codegraph.LanguageGo, 10, 1},
		{"cmd/app/main.go", "cmd/app", codegraph.NodeKindFile, "main.go", codegraph.LanguageGo, 10, 1},
		{"internal", "", codegraph.NodeKindModule, "internal", codegraph.LanguageGo, 19, 4},
		{"internal/util", "internal", codegraph.NodeKindPackage, "util", codegraph.LanguageGo, 6, 2},
		{"internal/util/util.go", "internal/util", codegraph.NodeKindFile, "util.go", codegraph.LanguageGo, 3, 1},
		{"web", "", codegraph.NodeKindModule, "web", codegraph.LanguageTypeScript, 26, 7},
		{"web/src", "web", codegraph.NodeKindPackage, "src", codegraph.LanguageTypeScript, 16, 6},
		{"web/src/lib/util/format.ts", "web/src/lib/util", codegraph.NodeKindFile, "format.ts", codegraph.LanguageTypeScript, 1, 1},
		{"docs", "", codegraph.NodeKindModule, "docs", codegraph.Language(""), 1, 1},
		{"README.md", "", codegraph.NodeKindFile, "README.md", codegraph.Language(""), 1, 1},
		{"go.mod", "", codegraph.NodeKindFile, "go.mod", codegraph.Language(""), 3, 1},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			n := nodeByID(t, g, tc.id)
			if n.Parent != tc.parent {
				t.Errorf("Parent = %q, want %q", n.Parent, tc.parent)
			}
			if n.Kind != tc.kind {
				t.Errorf("Kind = %q, want %q", n.Kind, tc.kind)
			}
			if n.Name != tc.name {
				t.Errorf("Name = %q, want %q", n.Name, tc.name)
			}
			if n.Language != tc.language {
				t.Errorf("Language = %q, want %q", n.Language, tc.language)
			}
			if n.Loc != tc.loc {
				t.Errorf("Loc = %d, want %d", n.Loc, tc.loc)
			}
			if n.FileCount != tc.fileCount {
				t.Errorf("FileCount = %d, want %d", n.FileCount, tc.fileCount)
			}
		})
	}
}

func TestGraph_Languages(t *testing.T) {
	g := mustBuild(t, fixtureFiles())
	want := []string{codegraph.LanguageGo.String(), codegraph.LanguageTypeScript.String()}
	if got := g.Languages(); !reflect.DeepEqual(got, want) {
		t.Errorf("Languages() = %v, want %v", got, want)
	}
}

// TestBuild_GoResolution covers Go-specific import handling beyond the main
// fixture: external imports, module-root imports, unknown packages,
// per-package count aggregation, nested modules, malformed files, and go.mod
// arriving only via the FileReader.
func TestBuild_GoResolution(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		extra     map[string]string // readable via FileReader but not listed
		wantEdges []codegraph.Edge
	}{
		{
			name: "external imports skipped",
			files: map[string]string{
				"go.mod":          "module example.com/m\n",
				"internal/a/a.go": "package a\n\nimport \"fmt\"\n\nvar _ = fmt.Sprint\n",
			},
			wantEdges: []codegraph.Edge{},
		},
		{
			name: "module root import has no node and is skipped",
			files: map[string]string{
				"go.mod":          "module example.com/m\n",
				"root.go":         "package m\n\nconst R = 1\n",
				"internal/a/a.go": "package a\n\nimport \"example.com/m\"\n",
			},
			wantEdges: []codegraph.Edge{},
		},
		{
			name: "unknown target package skipped",
			files: map[string]string{
				"go.mod":          "module example.com/m\n",
				"internal/a/a.go": "package a\n\nimport \"example.com/m/internal/missing\"\n",
			},
			wantEdges: []codegraph.Edge{},
		},
		{
			name: "counts aggregate per package",
			files: map[string]string{
				"go.mod":          "module example.com/m\n",
				"internal/a/a.go": "package a\n\nimport \"example.com/m/internal/b\"\n",
				"internal/a/c.go": "package a\n\nimport \"example.com/m/internal/b\"\n",
				"internal/b/b.go": "package b\n\nconst B = 1\n",
			},
			wantEdges: []codegraph.Edge{{From: "internal/a", To: "internal/b", Count: 2}},
		},
		{
			name: "nested module resolves by its own module path",
			files: map[string]string{
				"go.mod":          "module example.com/root\n",
				"lib/sub/go.mod":  "module example.com/other\n",
				"lib/sub/x/x.go":  "package x\n\nconst X = 1\n",
				"internal/a/a.go": "package a\n\nimport \"example.com/other/x\"\n",
			},
			wantEdges: []codegraph.Edge{{From: "internal/a", To: "lib/sub/x", Count: 1}},
		},
		{
			name: "malformed go file contributes no edges",
			files: map[string]string{
				"go.mod":          "module example.com/m\n",
				"internal/a/a.go": "this is not valid go source {{{\n",
				"internal/b/b.go": "package b\n\nimport \"example.com/m/internal/a\"\n",
			},
			wantEdges: []codegraph.Edge{{From: "internal/b", To: "internal/a", Count: 1}},
		},
		{
			name: "go.mod supplied only via FileReader",
			files: map[string]string{
				"internal/a/a.go": "package a\n\nimport \"example.com/m/internal/b\"\n",
				"internal/b/b.go": "package b\n\nconst B = 1\n",
			},
			extra: map[string]string{
				"go.mod": "module example.com/m\n",
			},
			wantEdges: []codegraph.Edge{{From: "internal/a", To: "internal/b", Count: 1}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			readable := make(map[string]string, len(tc.files)+len(tc.extra))
			for k, v := range tc.files {
				readable[k] = v
			}
			for k, v := range tc.extra {
				readable[k] = v
			}
			g, err := codegraph.NewGraphBuilder().Build(
				context.Background(), pathsOf(tc.files), mapReader(readable))
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if !reflect.DeepEqual(g.Edges, tc.wantEdges) {
				t.Errorf("edges mismatch:\n got: %#v\nwant: %#v", g.Edges, tc.wantEdges)
			}
		})
	}
}

// TestBuild_TSResolution covers TypeScript import resolution: relative paths,
// tsconfig aliases (star and exact), index files, compiled-output suffixes,
// TSX parsing, and silent skipping of external/unresolvable specifiers.
func TestBuild_TSResolution(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		wantEdges []codegraph.Edge
	}{
		{
			name: "relative parent dir",
			files: map[string]string{
				"a/x.ts": "import { y } from \"../b/y\";\n",
				"b/y.ts": "export const y = 1;\n",
			},
			wantEdges: []codegraph.Edge{{From: "a", To: "b", Count: 1}},
		},
		{
			name: "same dir import drops self edge",
			files: map[string]string{
				"a/x.ts": "import { y } from \"./y\";\n",
				"a/y.ts": "export const y = 1;\n",
			},
			wantEdges: []codegraph.Edge{},
		},
		{
			name: "alias star pattern",
			files: map[string]string{
				"tsconfig.json": `{"compilerOptions": {"paths": {"~/*": ["./src/*"]}}}`,
				"app/x.ts":      "import { y } from \"~/lib/y\";\n",
				"src/lib/y.ts":  "export const y = 1;\n",
			},
			wantEdges: []codegraph.Edge{{From: "app", To: "src/lib", Count: 1}},
		},
		{
			name: "alias exact pattern",
			files: map[string]string{
				"tsconfig.json":   `{"compilerOptions": {"paths": {"api": ["./src/api/main.ts"]}}}`,
				"app/x.ts":        "import { y } from \"api\";\n",
				"src/api/main.ts": "export const y = 1;\n",
			},
			wantEdges: []codegraph.Edge{{From: "app", To: "src/api", Count: 1}},
		},
		{
			name: "directory index resolution",
			files: map[string]string{
				"a/x.ts":         "import { y } from \"./sub\";\n",
				"a/sub/index.ts": "export const y = 1;\n",
			},
			wantEdges: []codegraph.Edge{{From: "a", To: "a/sub", Count: 1}},
		},
		{
			name: "compiled js suffix maps back to ts source",
			files: map[string]string{
				"a/x.ts":     "import { y } from \"./sub/y.js\";\n",
				"a/sub/y.ts": "export const y = 1;\n",
			},
			wantEdges: []codegraph.Edge{{From: "a", To: "a/sub", Count: 1}},
		},
		{
			name: "tsx parses with the tsx grammar",
			files: map[string]string{
				"a/x.tsx": "import Y from \"../b/y\";\nexport default function X() { return <div>{Y}</div>; }\n",
				"b/y.ts":  "const y = 1;\nexport default y;\n",
			},
			wantEdges: []codegraph.Edge{{From: "a", To: "b", Count: 1}},
		},
		{
			name: "external package skipped",
			files: map[string]string{
				"a/x.ts": "import React from \"react\";\n",
			},
			wantEdges: []codegraph.Edge{},
		},
		{
			name: "unresolvable relative skipped",
			files: map[string]string{
				"a/x.ts": "import { y } from \"./missing\";\n",
			},
			wantEdges: []codegraph.Edge{},
		},
		{
			name: "nested tsconfig with baseUrl governs its subtree",
			files: map[string]string{
				"web/tsconfig.json": `{"compilerOptions": {"baseUrl": ".", "paths": {"@/*": ["./src/*"]}}}`,
				"web/src/a/x.ts":    "import { y } from \"@/b/y\";\n",
				"web/src/b/y.ts":    "export const y = 1;\n",
			},
			wantEdges: []codegraph.Edge{{From: "web/src/a", To: "web/src/b", Count: 1}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := mustBuild(t, tc.files)
			if !reflect.DeepEqual(g.Edges, tc.wantEdges) {
				t.Errorf("edges mismatch:\n got: %#v\nwant: %#v", g.Edges, tc.wantEdges)
			}
		})
	}
}

func TestBuild_UnreadableFileStillHasNode(t *testing.T) {
	files := fixtureFiles()
	paths := append(pathsOf(files), "assets/blob.bin")
	read := func(p string) ([]byte, error) {
		if p == "assets/blob.bin" {
			return nil, errors.New("injected read failure")
		}
		return mapReader(files)(p)
	}
	g, err := codegraph.NewGraphBuilder().Build(context.Background(), paths, read)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	n := nodeByID(t, g, "assets/blob.bin")
	if n.Loc != 0 || n.FileCount != 1 {
		t.Errorf("unreadable file node = %+v, want Loc 0, FileCount 1", n)
	}
}

func TestBuild_EmptyInput(t *testing.T) {
	g, err := codegraph.NewGraphBuilder().Build(context.Background(), nil, mapReader(nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if g.Nodes == nil || g.Edges == nil || g.Violations == nil {
		t.Errorf("slices must be non-nil: %+v", g)
	}
	if len(g.Nodes)+len(g.Edges)+len(g.Violations) != 0 {
		t.Errorf("expected empty graph, got %+v", g)
	}
}

func TestBuild_NilReader(t *testing.T) {
	if _, err := codegraph.NewGraphBuilder().Build(context.Background(), nil, nil); err == nil {
		t.Fatal("expected error for nil FileReader")
	}
}

func TestBuild_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	files := fixtureFiles()
	_, err := codegraph.NewGraphBuilder().Build(ctx, pathsOf(files), mapReader(files))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestNodeKind_Validity(t *testing.T) {
	for _, k := range []codegraph.NodeKind{
		codegraph.NodeKindModule, codegraph.NodeKindPackage, codegraph.NodeKindFile,
	} {
		if !k.IsValid() {
			t.Errorf("%q should be valid", k)
		}
		if k.String() != string(k) {
			t.Errorf("String() = %q, want %q", k.String(), string(k))
		}
	}
	if codegraph.NodeKind("bogus").IsValid() {
		t.Error("bogus NodeKind should be invalid")
	}
}

func TestViolationKind_Validity(t *testing.T) {
	for _, k := range []codegraph.ViolationKind{
		codegraph.ViolationCycle, codegraph.ViolationWrongWay,
	} {
		if !k.IsValid() {
			t.Errorf("%q should be valid", k)
		}
		if k.String() != string(k) {
			t.Errorf("String() = %q, want %q", k.String(), string(k))
		}
	}
	if codegraph.ViolationKind("bogus").IsValid() {
		t.Error("bogus ViolationKind should be invalid")
	}
}
