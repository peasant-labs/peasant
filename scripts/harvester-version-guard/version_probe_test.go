package main

import (
	"bytes"
	_ "embed"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/version-probe.yaml
var versionProbeYAML []byte

type versionProbeTree string

const (
	versionProbeRegistered  versionProbeTree = "registered"
	versionProbePreRegistry versionProbeTree = "pre-registry"
	versionProbeUnreadable  versionProbeTree = "unreadable"
)

type versionProbeCase struct {
	Name       string           `yaml:"name"`
	Tree       versionProbeTree `yaml:"tree"`
	Reads      []string         `yaml:"reads"`
	NeverReads []string         `yaml:"neverReads"`
	Error      string           `yaml:"error"`
}

func loadVersionProbeCases(t *testing.T) []versionProbeCase {
	t.Helper()
	var fixture struct {
		RequiredNames []string           `yaml:"requiredNames"`
		Cases         []versionProbeCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(versionProbeYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("version probe fixture must have exactly one document: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: fixture.RequiredNames}, names, "version probe"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

// TestVersionProbeMatchesTheArchivedRevision proves the guard writes the
// parserVersions source the archived revision can compile: the registry form
// where harvester_versions.go exists, the baseline form where it does not, and
// no form at all when it cannot tell.
func TestVersionProbeMatchesTheArchivedRevision(t *testing.T) {
	for _, c := range loadVersionProbeCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			tree := t.TempDir()
			ingest := filepath.Join(tree, "internal", "ingest")
			if err := os.MkdirAll(ingest, 0o700); err != nil {
				t.Fatal(err)
			}
			switch c.Tree {
			case versionProbeRegistered:
				if err := os.WriteFile(filepath.Join(ingest, "harvester_versions.go"), []byte("package ingest\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case versionProbePreRegistry:
			case versionProbeUnreadable:
				if err := os.Chmod(ingest, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(ingest, 0o700) })
			default:
				t.Fatalf("case %s has tree %q; use registered, pre-registry or unreadable", c.Name, c.Tree)
			}
			source, err := versionProbe(tree)
			if c.Error != "" {
				if err == nil || !strings.Contains(err.Error(), c.Error) {
					t.Fatalf("versionProbe error = %v, want one containing %q", err, c.Error)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			file, err := parser.ParseFile(token.NewFileSet(), "harvester_guard_versions_test.go", source, 0)
			if err != nil {
				t.Fatalf("the chosen parserVersions source does not parse: %v", err)
			}
			if file.Name.Name != "ingest_test" || !declaresFunc(file, "parserVersions") {
				t.Fatalf("the chosen source is package %s and must declare parserVersions in package ingest_test", file.Name.Name)
			}
			selectors := selectorTexts(file)
			for _, want := range c.Reads {
				if !slices.Contains(selectors, want) {
					t.Errorf("the chosen source never reads %s; it reads %v", want, selectors)
				}
			}
			for _, never := range c.NeverReads {
				if slices.Contains(selectors, never) {
					t.Errorf("the chosen source reads %s, which this revision does not have", never)
				}
			}
		})
	}
}

func declaresFunc(file *ast.File, name string) bool {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
			return true
		}
	}
	return false
}

// selectorTexts lists every X.Sel expression whose X is an identifier.
func selectorTexts(file *ast.File) []string {
	var texts []string
	ast.Inspect(file, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			if x, ok := selector.X.(*ast.Ident); ok {
				texts = append(texts, x.Name+"."+selector.Sel.Name)
			}
		}
		return true
	})
	return texts
}
