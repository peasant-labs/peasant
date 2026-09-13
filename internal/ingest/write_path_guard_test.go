package ingest_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/write_path_guard.yaml
var writePathGuardYAML []byte

type writePathGuardFixture struct {
	Forbidden []struct {
		Name    string `yaml:"name"`
		Pattern string `yaml:"pattern"`
	} `yaml:"forbidden"`
	RequiredForbiddenNames []string `yaml:"requiredForbiddenNames"`
	CallSites              struct {
		Symbols []string `yaml:"symbols"`
		Allowed []struct {
			Name     string `yaml:"name"`
			File     string `yaml:"file"`
			Function string `yaml:"function"`
			Symbol   string `yaml:"symbol"`
		} `yaml:"allowed"`
		RequiredNames []string `yaml:"requiredNames"`
	} `yaml:"callSites"`
}

func loadWritePathGuardFixture(t *testing.T) writePathGuardFixture {
	t.Helper()
	var fixture writePathGuardFixture
	decoder := yaml.NewDecoder(bytes.NewReader(writePathGuardYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("write path guard fixture requires one YAML document")
	}
	forbidden := make(map[string]bool)
	for _, row := range fixture.Forbidden {
		if row.Name == "" || row.Pattern == "" || forbidden[row.Name] {
			t.Fatalf("invalid forbidden pattern %q", row.Name)
		}
		forbidden[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("write path guard fixture testdata/write_path_guard.yaml", "forbidden pattern", fixture.RequiredForbiddenNames, forbidden); err != nil {
		t.Fatal(err)
	}
	allowed := make(map[string]bool)
	for _, row := range fixture.CallSites.Allowed {
		if row.Name == "" || row.File == "" || row.Function == "" || row.Symbol == "" || allowed[row.Name] {
			t.Fatalf("invalid allowed call site %q", row.Name)
		}
		allowed[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("write path guard fixture testdata/write_path_guard.yaml", "allowed call site", fixture.CallSites.RequiredNames, allowed); err != nil {
		t.Fatal(err)
	}
	if len(fixture.CallSites.Symbols) == 0 {
		t.Fatal("write path guard fixture names no guarded symbols")
	}
	return fixture
}

// productionGoFiles lists the non-test Go files of this package directory.
func productionGoFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, name)
	}
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatal("no production Go files found; the guard is scanning the wrong directory")
	}
	return files
}

// TestWritePathHasNoSyncOrLock proves by source that no production file of
// the ingest package syncs a file or takes a file lock. The session write path
// relies on the database transaction for durability and on rename for
// atomicity; a sync or lock that came back would silently restore the
// per-session cost this guard exists to keep out.
func TestWritePathHasNoSyncOrLock(t *testing.T) {
	fixture := loadWritePathGuardFixture(t)
	var findings []string
	for _, file := range productionGoFiles(t) {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for number, line := range strings.Split(string(data), "\n") {
			for _, forbidden := range fixture.Forbidden {
				if strings.Contains(line, forbidden.Pattern) {
					findings = append(findings, fmt.Sprintf("%s:%d: %s (%q)", file, number+1, forbidden.Name, strings.TrimSpace(line)))
				}
			}
		}
	}
	if len(findings) > 0 {
		t.Fatalf("production code of the ingest package syncs or locks a file; the write path must do neither:\n%s", strings.Join(findings, "\n"))
	}
}

type guardedCallSite struct {
	File     string
	Function string
	Symbol   string
}

func (site guardedCallSite) String() string {
	return fmt.Sprintf("%s %s calls %s", site.File, site.Function, site.Symbol)
}

// enclosingFunctionName names a declaration the way the fixture does: a plain
// function by its name, a method by "Receiver.Name" with the pointer dropped.
func enclosingFunctionName(decl *ast.FuncDecl) string {
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return decl.Name.Name
	}
	receiver := decl.Recv.List[0].Type
	if star, ok := receiver.(*ast.StarExpr); ok {
		receiver = star.X
	}
	if index, ok := receiver.(*ast.IndexExpr); ok {
		receiver = index.X
	}
	if ident, ok := receiver.(*ast.Ident); ok {
		return ident.Name + "." + decl.Name.Name
	}
	return decl.Name.Name
}

// guardedCallSites parses every production file and returns each call of a
// guarded symbol with the file and function it sits in.
func guardedCallSites(t *testing.T, symbols []string) map[guardedCallSite]int {
	t.Helper()
	guarded := make(map[string]bool, len(symbols))
	for _, symbol := range symbols {
		guarded[symbol] = true
	}
	sites := make(map[guardedCallSite]int)
	fset := token.NewFileSet()
	for _, file := range productionGoFiles(t) {
		parsed, err := parser.ParseFile(fset, filepath.Join(".", file), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			name := enclosingFunctionName(function)
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := selector.X.(*ast.Ident)
				if !ok {
					return true
				}
				symbol := pkg.Name + "." + selector.Sel.Name
				if guarded[symbol] {
					sites[guardedCallSite{File: file, Function: name, Symbol: symbol}]++
				}
				return true
			})
		}
	}
	return sites
}

// TestTranscriptHashAndCloneCallSitesAreTheAllowedOnes proves that the
// transcript is hashed and cloned only where the design allows: one hash on
// the worker, one on a pair read back from disk, and nothing on the index
// path. Both directions are checked: a call outside the allowed set is a
// finding, and an allowed site that no longer exists means the allowance is
// stale and must be removed with the code it described.
func TestTranscriptHashAndCloneCallSitesAreTheAllowedOnes(t *testing.T) {
	fixture := loadWritePathGuardFixture(t)
	allowed := make(map[guardedCallSite]string, len(fixture.CallSites.Allowed))
	for _, row := range fixture.CallSites.Allowed {
		allowed[guardedCallSite{File: row.File, Function: row.Function, Symbol: row.Symbol}] = row.Name
	}
	found := guardedCallSites(t, fixture.CallSites.Symbols)
	var unexpected, stale []string
	for site := range found {
		if _, ok := allowed[site]; !ok {
			unexpected = append(unexpected, site.String())
		}
	}
	for site, name := range allowed {
		if found[site] == 0 {
			stale = append(stale, fmt.Sprintf("%s (%s)", site, name))
		}
	}
	sort.Strings(unexpected)
	sort.Strings(stale)
	if len(unexpected) > 0 {
		t.Errorf("guarded symbol called outside the allowed sites; a transcript is hashed once on the worker and cloned nowhere on the index path:\n%s", strings.Join(unexpected, "\n"))
	}
	if len(stale) > 0 {
		t.Errorf("allowed call sites no longer exist; remove them from testdata/write_path_guard.yaml with the code they described:\n%s", strings.Join(stale, "\n"))
	}
}
