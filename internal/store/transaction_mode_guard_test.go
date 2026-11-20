package store

import (
	"embed"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The store's transaction-mode guard. A transaction that begins DEFERRED and
// writes after it reads fails with BUSY at once, without waiting on
// busy_timeout, when another connection writes meanwhile, so every store
// write begins IMMEDIATE (beginWrite). This guard parses the package's
// production files and finds every begin that does NOT take the writer up
// front: a reference to sqlitex.Transaction, sqlitex.Save or the sqlitex
// script helpers (which wrap the script in a savepoint), called or passed as
// a value, matched by import path so a renamed import is still seen; and a
// literal BEGIN or SAVEPOINT, in a function or a package-level declaration.
// Each one must be on the reviewed list in testdata/transaction_mode_guard.yaml
// with the reason it is safe: it only reads, it runs nested inside a caller's
// IMMEDIATE transaction (a savepoint inherits the outer mode), or its
// connection is private to it. A read-only entry whose function holds
// write-shaped SQL, directly or through a package constant, fails too. A new
// deferred begin, a moved one, or a stale entry fails the guard.
//
// What it cannot see: a write reached through a helper the read-only function
// calls, and a dot-import of sqlitex. Both stay review decisions.

//go:embed testdata/transaction_mode_guard.yaml
var transactionModeGuardYAML []byte

//go:embed testdata/transaction_mode_guard_probes.yaml
var transactionModeGuardProbesYAML []byte

//go:embed testdata/transaction_mode_guard_probes
var transactionModeGuardProbeSources embed.FS

// Fixture names, for messages and for reading the probe sources.
const (
	transactionModeGuardFixture   = "testdata/transaction_mode_guard.yaml"
	transactionModeGuardProbesDir = "testdata/transaction_mode_guard_probes"
)

// sqlitexImportPath is the package whose helpers begin deferred transactions.
const sqlitexImportPath = "github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"

// deferredBeginMode is the closed set of reasons a deferred begin is safe.
type deferredBeginMode string

const (
	// deferredBeginReadOnly: the transaction only reads, so it never
	// upgrades its read snapshot to a write.
	deferredBeginReadOnly deferredBeginMode = "read-only"
	// deferredBeginNested: the savepoint only ever runs inside a caller's
	// IMMEDIATE transaction, whose writer it inherits.
	deferredBeginNested deferredBeginMode = "nested"
	// deferredBeginPrivate: the connection is private to the function (an
	// in-memory database), so no other connection can write beside it.
	deferredBeginPrivate deferredBeginMode = "private-connection"
)

var deferredBeginModes = map[deferredBeginMode]bool{
	deferredBeginReadOnly: true,
	deferredBeginNested:   true,
	deferredBeginPrivate:  true,
}

// deferredBeginHelpers are the sqlitex functions that begin a deferred
// transaction, or a savepoint that is one when it is outermost.
var deferredBeginHelpers = map[string]bool{
	"Transaction":     true,
	"Save":            true,
	"ExecuteScript":   true,
	"ExecScript":      true,
	"ExecuteScriptFS": true,
	"ExecScriptFS":    true,
}

// sqlKeyword is the closed set of SQL keywords the guard reads statements by.
type sqlKeyword string

const (
	sqlKeywordBegin       sqlKeyword = "BEGIN"
	sqlKeywordDeferred    sqlKeyword = "DEFERRED"
	sqlKeywordTransaction sqlKeyword = "TRANSACTION"
	sqlKeywordSavepoint   sqlKeyword = "SAVEPOINT"
	sqlKeywordWith        sqlKeyword = "WITH"
	sqlKeywordInsert      sqlKeyword = "INSERT"
	sqlKeywordUpdate      sqlKeyword = "UPDATE"
	sqlKeywordDelete      sqlKeyword = "DELETE"
	sqlKeywordReplace     sqlKeyword = "REPLACE"
	sqlKeywordCreate      sqlKeyword = "CREATE"
	sqlKeywordDrop        sqlKeyword = "DROP"
	sqlKeywordAlter       sqlKeyword = "ALTER"
)

// sqlWriteKeywords lead a statement that changes the database.
var sqlWriteKeywords = map[sqlKeyword]bool{
	sqlKeywordInsert: true, sqlKeywordUpdate: true, sqlKeywordDelete: true, sqlKeywordReplace: true,
	sqlKeywordCreate: true, sqlKeywordDrop: true, sqlKeywordAlter: true,
}

// sqlCTEWriteKeywords are the statements a WITH clause can lead into that
// change the database.
var sqlCTEWriteKeywords = map[sqlKeyword]bool{
	sqlKeywordInsert: true, sqlKeywordUpdate: true, sqlKeywordDelete: true,
}

// isDeferredBeginLiteral reports whether a literal statement begins a
// deferred transaction: BEGIN, optionally DEFERRED, optionally TRANSACTION.
// An explicit BEGIN IMMEDIATE or BEGIN EXCLUSIVE takes the writer and is not
// one.
func isDeferredBeginLiteral(value string) bool {
	words := sqlWords(value)
	if len(words) == 0 || words[0] != sqlKeywordBegin {
		return false
	}
	i := 1
	if i < len(words) && words[i] == sqlKeywordDeferred {
		i++
	}
	if i < len(words) && words[i] == sqlKeywordTransaction {
		i++
	}
	return i == len(words)
}

// isSavepointLiteral reports whether a literal statement opens a savepoint,
// which begins a deferred transaction when it is outermost.
func isSavepointLiteral(value string) bool {
	words := sqlWords(value)
	return len(words) >= 1 && len(words) <= 2 && words[0] == sqlKeywordSavepoint
}

// isWriteSQLLiteral reports whether a literal is write-shaped SQL: after any
// leading comments, it starts with a write keyword, or it starts with a WITH
// clause whose text holds an INSERT, UPDATE or DELETE. It is a shape check,
// not a parser: a CTE that only names one of those words in a string or an
// identifier counts as a write, which errs toward review.
func isWriteSQLLiteral(value string) bool {
	words := sqlWords(stripLeadingSQLComments(value))
	if len(words) == 0 {
		return false
	}
	if sqlWriteKeywords[words[0]] {
		return true
	}
	if words[0] != sqlKeywordWith {
		return false
	}
	for _, word := range words[1:] {
		if sqlCTEWriteKeywords[word] {
			return true
		}
	}
	return false
}

// stripLeadingSQLComments drops the -- line comments and /* */ block
// comments before a statement's first keyword.
func stripLeadingSQLComments(value string) string {
	for {
		value = strings.TrimSpace(value)
		switch {
		case strings.HasPrefix(value, "--"):
			end := strings.IndexByte(value, '\n')
			if end < 0 {
				return ""
			}
			value = value[end+1:]
		case strings.HasPrefix(value, "/*"):
			end := strings.Index(value, "*/")
			if end < 0 {
				return ""
			}
			value = value[end+2:]
		default:
			return value
		}
	}
}

// sqlWords splits SQL text into upper-cased identifier words, dropping
// punctuation, so keywords compare without case and without a trailing ";".
func sqlWords(value string) []sqlKeyword {
	fields := strings.FieldsFunc(strings.ToUpper(value), func(r rune) bool { return !isSQLWordRune(r) })
	words := make([]sqlKeyword, len(fields))
	for i, field := range fields {
		words[i] = sqlKeyword(field)
	}
	return words
}

// isSQLWordRune reports whether r can be part of an upper-cased SQL word.
func isSQLWordRune(r rune) bool {
	letter := r >= 'A' && r <= 'Z'
	digit := r >= '0' && r <= '9'
	return letter || digit || r == '_'
}

type deferredBegin struct {
	File     string `yaml:"file"`
	Function string `yaml:"function"`
	Begin    string `yaml:"begin"`
}

func (b deferredBegin) String() string {
	return fmt.Sprintf("%s %s begins with %s", b.File, b.Function, b.Begin)
}

type deferredBeginEntry struct {
	deferredBegin `yaml:",inline"`
	Mode          deferredBeginMode `yaml:"mode"`
	Reason        string            `yaml:"reason"`
}

// guardScope names a function or a package-level declaration in a file.
type guardScope struct {
	File     string
	Function string
}

// guardScan is what the scan found: every deferred begin with how often it
// occurs, and the write-shaped SQL each function body holds.
type guardScan struct {
	begins map[deferredBegin]int
	writes map[guardScope][]string
}

func decodeDeferredBeginAllowList(entries []deferredBeginEntry, source string) (map[deferredBegin]deferredBeginEntry, error) {
	allowed := make(map[deferredBegin]deferredBeginEntry, len(entries))
	for _, entry := range entries {
		located := entry.File != "" && entry.Function != ""
		explained := entry.Begin != "" && strings.TrimSpace(entry.Reason) != ""
		if !located || !explained {
			return nil, fmt.Errorf("%s entry %+v needs file, function, begin and reason", source, entry)
		}
		if !deferredBeginModes[entry.Mode] {
			return nil, fmt.Errorf("%s entry %s names mode %q outside read-only, nested and private-connection", source, entry.deferredBegin, entry.Mode)
		}
		if _, dup := allowed[entry.deferredBegin]; dup {
			return nil, fmt.Errorf("%s lists %s twice", source, entry.deferredBegin)
		}
		allowed[entry.deferredBegin] = entry
	}
	return allowed, nil
}

func loadDeferredBeginAllowList(t *testing.T) map[deferredBegin]deferredBeginEntry {
	t.Helper()
	var fixture struct {
		DeferredBegins []deferredBeginEntry `yaml:"deferredBegins"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(transactionModeGuardYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode %s: %v", transactionModeGuardFixture, err)
	}
	allowed, err := decodeDeferredBeginAllowList(fixture.DeferredBegins, transactionModeGuardFixture)
	if err != nil {
		t.Fatal(err)
	}
	return allowed
}

// storeFuncName names a declaration the way the fixture does: a plain
// function by its name, a method by "Receiver.Name" with the pointer dropped.
func storeFuncName(decl *ast.FuncDecl) string {
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return decl.Name.Name
	}
	receiver := decl.Recv.List[0].Type
	if star, ok := receiver.(*ast.StarExpr); ok {
		receiver = star.X
	}
	if ident, ok := receiver.(*ast.Ident); ok {
		return ident.Name + "." + decl.Name.Name
	}
	return decl.Name.Name
}

// sqlitexNames returns the names a file refers to the sqlitex package by.
func sqlitexNames(file *ast.File) map[string]bool {
	names := make(map[string]bool)
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil || importPath != sqlitexImportPath {
			continue
		}
		name := path.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		names[name] = true
	}
	return names
}

func stringLiteral(node ast.Node) (string, bool) {
	lit, ok := node.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(value), true
}

// packageStrings returns the package-level string constants and variables
// declared with a single literal, by name.
func packageStrings(files []*ast.File) map[string]string {
	values := make(map[string]string)
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range value.Names {
					if i < len(value.Values) {
						if text, ok := stringLiteral(value.Values[i]); ok {
							values[name.Name] = text
						}
					}
				}
			}
		}
	}
	return values
}

// scanGuardSources parses the named sources as one package and returns every
// deferred begin and every function's write-shaped SQL.
func scanGuardSources(names []string, read func(string) ([]byte, error)) (guardScan, error) {
	fset := token.NewFileSet()
	parsed := make([]*ast.File, 0, len(names))
	for _, name := range names {
		src, err := read(name)
		if err != nil {
			return guardScan{}, err
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			return guardScan{}, err
		}
		parsed = append(parsed, file)
	}
	strs := packageStrings(parsed)
	scan := guardScan{begins: make(map[deferredBegin]int), writes: make(map[guardScope][]string)}
	for i, file := range parsed {
		name := names[i]
		aliases := sqlitexNames(file)
		inspect := func(scope guardScope, root ast.Node, collectWrites bool) {
			ast.Inspect(root, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.SelectorExpr:
					// A selector in or out of call position: a call, a method
					// value, or an argument. An identifier the parser resolved
					// to a local object is not the import.
					pkg, ok := node.X.(*ast.Ident)
					imported := ok && pkg.Obj == nil && aliases[pkg.Name]
					if imported && deferredBeginHelpers[node.Sel.Name] {
						scan.begins[deferredBegin{File: scope.File, Function: scope.Function, Begin: "sqlitex." + node.Sel.Name}]++
					}
				case *ast.BasicLit:
					value, ok := stringLiteral(node)
					if !ok {
						return true
					}
					switch {
					case isDeferredBeginLiteral(value):
						scan.begins[deferredBegin{File: scope.File, Function: scope.Function, Begin: "BEGIN"}]++
					case isSavepointLiteral(value):
						scan.begins[deferredBegin{File: scope.File, Function: scope.Function, Begin: "SAVEPOINT"}]++
					case collectWrites && isWriteSQLLiteral(value):
						scan.writes[scope] = append(scan.writes[scope], value)
					}
				case *ast.Ident:
					if value, ok := strs[node.Name]; collectWrites && ok && isWriteSQLLiteral(value) {
						scan.writes[scope] = append(scan.writes[scope], node.Name)
					}
				}
				return true
			})
		}
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Body != nil {
					inspect(guardScope{File: name, Function: storeFuncName(decl)}, decl.Body, true)
				}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok || len(value.Names) == 0 {
						continue
					}
					inspect(guardScope{File: name, Function: "package-level " + value.Names[0].Name}, value, false)
				}
			}
		}
	}
	return scan, nil
}

func storeProductionFiles(t *testing.T) []string {
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
		t.Fatal("no production Go files found; the transaction-mode guard is scanning the wrong directory")
	}
	return files
}

// unreviewedDeferredBegins compares the scan with the reviewed list and
// returns one finding per difference: a begin missing from the list, a scope
// holding a listed begin more than once, a listed begin the code no longer
// has, and a read-only entry whose function holds write-shaped SQL.
func unreviewedDeferredBegins(scan guardScan, allowed map[deferredBegin]deferredBeginEntry) []string {
	var findings []string
	for site, count := range scan.begins {
		if _, ok := allowed[site]; !ok {
			findings = append(findings, fmt.Sprintf("%s, which is not on the reviewed list: begin a transaction that may write with beginWrite (IMMEDIATE); if it truly only reads, or only runs nested inside an IMMEDIATE transaction, add it to %s with the reason", site, transactionModeGuardFixture))
			continue
		}
		if count != 1 {
			findings = append(findings, fmt.Sprintf("%s %d times; the reviewed list covers one begin per function, so split the function or review each begin", site, count))
		}
	}
	for site, entry := range allowed {
		if scan.begins[site] == 0 {
			findings = append(findings, fmt.Sprintf("the reviewed list names %s, which the code no longer has: remove the stale entry", site))
			continue
		}
		if writes := scan.writes[guardScope{File: site.File, Function: site.Function}]; entry.Mode == deferredBeginReadOnly && len(writes) > 0 {
			findings = append(findings, fmt.Sprintf("%s %s is listed read-only but its body holds write SQL %q: begin it with beginWrite and remove the entry", site.File, site.Function, writes))
		}
	}
	sort.Strings(findings)
	return findings
}

// TestStoreWritesBeginImmediate proves every deferred begin in the store's
// production code is a reviewed read-only, nested or private-connection one.
func TestStoreWritesBeginImmediate(t *testing.T) {
	allowed := loadDeferredBeginAllowList(t)
	scan, err := scanGuardSources(storeProductionFiles(t), os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	if findings := unreviewedDeferredBegins(scan, allowed); len(findings) > 0 {
		t.Fatalf("store transaction modes differ from the reviewed list:\n%s", strings.Join(findings, "\n"))
	}
}

// TestTransactionModeGuardProbes runs the guard over each probe source with
// the probe's own reviewed list and requires exactly the probe's findings:
// deferred begins in every spelling the scan knows, a renamed import matched
// by its path, package-level literals, and read-only entries holding write
// SQL, while the IMMEDIATE forms and a local lookalike pass.
func TestTransactionModeGuardProbes(t *testing.T) {
	var fixture struct {
		Probes []struct {
			Name    string               `yaml:"name"`
			Source  string               `yaml:"source"`
			Allowed []deferredBeginEntry `yaml:"allowed"`
			Want    []string             `yaml:"want"`
		} `yaml:"probes"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(transactionModeGuardProbesYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode the transaction-mode guard probes: %v", err)
	}
	if len(fixture.Probes) == 0 {
		t.Fatal("the transaction-mode guard probe fixture names no probes")
	}
	for _, probe := range fixture.Probes {
		t.Run(probe.Name, func(t *testing.T) {
			allowed, err := decodeDeferredBeginAllowList(probe.Allowed, probe.Name)
			if err != nil {
				t.Fatal(err)
			}
			scan, err := scanGuardSources([]string{probe.Source}, func(name string) ([]byte, error) {
				return transactionModeGuardProbeSources.ReadFile(transactionModeGuardProbesDir + "/" + name)
			})
			if err != nil {
				t.Fatal(err)
			}
			findings := unreviewedDeferredBegins(scan, allowed)
			if len(findings) != len(probe.Want) {
				t.Fatalf("guard findings for %s = %q, want %d starting %q", probe.Source, findings, len(probe.Want), probe.Want)
			}
			for i, want := range probe.Want {
				if !strings.HasPrefix(findings[i], want) {
					t.Fatalf("guard finding %d for %s = %q, want it to start %q", i, probe.Source, findings[i], want)
				}
			}
		})
	}
}
