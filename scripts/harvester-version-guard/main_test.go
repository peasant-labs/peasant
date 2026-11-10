package main

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"testing"
)

func TestCompareRequiresAffectedVersion(t *testing.T) {
	data, err := os.ReadFile("../../internal/ingest/testdata/strike_protocol.meta.json")
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.ReplaceAll(data, []byte("canonical sidecar title"), []byte("changed title"))
	if bytes.Equal(data, changed) {
		t.Fatal("existing Strike fixture no longer exercises a title change")
	}
	name := "strike/AdapterVersion/strike_protocol"
	base := snapshot{Versions: map[string]versions{"strike": {AdapterVersion: 1, IndexerVersion: 16}}, Outputs: map[string]json.RawMessage{name: data}}
	if failures := compare(base, base); len(failures) != 0 {
		t.Fatalf("unchanged behavior failed: %v", failures)
	}
	candidate := snapshot{Versions: map[string]versions{"strike": {AdapterVersion: 1, IndexerVersion: 17}}, Outputs: map[string]json.RawMessage{name: changed}}
	if failures := compare(base, candidate); len(failures) != 1 {
		t.Fatalf("missing adapter bump was not refused: %v", failures)
	}
	candidate.Versions["strike"] = versions{AdapterVersion: 2, IndexerVersion: 17}
	if failures := compare(base, candidate); len(failures) != 0 {
		t.Fatalf("affected adapter bump failed: %v", failures)
	}
}

func TestEmbeddedProbeSharesTheObservedNamesContract(t *testing.T) {
	reads := probeGetenvNames(t)
	for _, name := range []string{corpusEnv, fallbackCorpusEnv, observedNativeEnv, outputEnv, nativeRootEnv} {
		if !reads[name] {
			t.Fatalf("the embedded probe no longer reads os.Getenv(%q); the runner and probe disagree about the contract", name)
		}
	}
	if !bytes.Contains(probe, []byte("NativeObserved")) {
		t.Fatal("the embedded probe no longer reports NativeObserved; the runner would hand it names it never sees")
	}
	clear := bytes.Index(probe, []byte("os.RemoveAll(filepath.Dir(nativePath))"))
	write := bytes.Index(probe, []byte("os.WriteFile(nativePath"))
	if clear < 0 || write < 0 || clear > write {
		t.Fatal("the probe no longer clears the case directory before writing the capture's main file")
	}
}

// probeGetenvNames finds the string literal of every os.Getenv call in the
// embedded probe, so a comment or a dead string cannot satisfy the contract.
func probeGetenvNames(t *testing.T) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "probe_test.go", probe, 0)
	if err != nil {
		t.Fatalf("parse embedded probe: %v", err)
	}
	names := make(map[string]bool)
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Getenv" {
			return true
		}
		pkg, ok := selector.X.(*ast.Ident)
		if !ok || pkg.Name != "os" || len(call.Args) != 1 {
			return true
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		if value, err := strconv.Unquote(literal.Value); err == nil {
			names[value] = true
		}
		return true
	})
	return names
}

func TestNewHarnessNeedsNoInventedHistoricalOutput(t *testing.T) {
	base := snapshot{Versions: map[string]versions{}, Outputs: map[string]json.RawMessage{}}
	name := "pi/IndexerVersion/native/header-only"
	candidate := snapshot{
		Versions: map[string]versions{"pi": {AdapterVersion: 1, IndexerVersion: 16}},
		Outputs:  map[string]json.RawMessage{name: json.RawMessage(`{"Failed":false}`)},
	}
	if failures := compare(base, candidate); len(failures) != 0 {
		t.Fatalf("new harness requires invented historical observations: %v", failures)
	}
	if failures := compare(candidate, base); len(failures) == 0 {
		t.Fatal("removing an observed harness was silently accepted")
	}
}

func TestEmbeddedProbeIsValidGo(t *testing.T) {
	if _, err := parser.ParseFile(token.NewFileSet(), "harvester_guard_probe_test.go", probe, parser.AllErrors); err != nil {
		t.Fatalf("archived-revision probe has invalid Go syntax: %v", err)
	}
}

func TestUnobservedNativeUsesProcessedNames(t *testing.T) {
	name := "empty-valid"
	if missing := unobservedNative([]string{name}, nil); len(missing) != 1 {
		t.Fatalf("an unprocessed name was not reported missing: %v", missing)
	}
	// An empty database is a legitimate observation; the probe records the
	// name it processed, not the number of sessions it found.
	if missing := unobservedNative([]string{name}, []string{name}); len(missing) != 0 {
		t.Fatalf("a processed empty case was reported missing: %v", missing)
	}
}
