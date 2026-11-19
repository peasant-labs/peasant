package store

import (
	_ "embed"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
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
// front: sqlitex.Transaction, sqlitex.Save, the sqlitex script helpers (which
// wrap the script in a savepoint), and a literal BEGIN or SAVEPOINT. Each one
// must be on the reviewed list in testdata/transaction_mode_guard.yaml with
// the reason it is safe: it only reads, it runs nested inside a caller's
// IMMEDIATE transaction (a savepoint inherits the outer mode), or its
// connection is private to it. A new deferred begin, a moved one, or a stale
// entry fails the guard.

//go:embed testdata/transaction_mode_guard.yaml
var transactionModeGuardYAML []byte

// transactionModeGuardFixture names the embedded reviewed list in messages.
const transactionModeGuardFixture = "testdata/transaction_mode_guard.yaml"

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

// deferredBeginCalls are the sqlitex helpers that begin a deferred
// transaction, or a savepoint that is one when it is outermost.
var deferredBeginCalls = map[string]bool{
	"sqlitex.Transaction":     true,
	"sqlitex.Save":            true,
	"sqlitex.ExecuteScript":   true,
	"sqlitex.ExecScript":      true,
	"sqlitex.ExecuteScriptFS": true,
	"sqlitex.ExecScriptFS":    true,
}

// A literal statement that begins a deferred transaction or a savepoint. An
// explicit BEGIN IMMEDIATE or BEGIN EXCLUSIVE takes the writer and is not one.
var (
	deferredBeginLiteral = regexp.MustCompile(`(?i)^BEGIN(\s+DEFERRED)?(\s+TRANSACTION)?\s*;?$`)
	savepointLiteral     = regexp.MustCompile(`(?i)^SAVEPOINT(\s+\w+)?\s*;?$`)
)

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
	allowed := make(map[deferredBegin]deferredBeginEntry, len(fixture.DeferredBegins))
	for _, entry := range fixture.DeferredBegins {
		located := entry.File != "" && entry.Function != ""
		explained := entry.Begin != "" && strings.TrimSpace(entry.Reason) != ""
		if !located || !explained {
			t.Fatalf("%s entry %+v needs file, function, begin and reason", transactionModeGuardFixture, entry)
		}
		if !deferredBeginModes[entry.Mode] {
			t.Fatalf("%s entry %s names mode %q outside read-only, nested and private-connection", transactionModeGuardFixture, entry.deferredBegin, entry.Mode)
		}
		if _, dup := allowed[entry.deferredBegin]; dup {
			t.Fatalf("%s lists %s twice", transactionModeGuardFixture, entry.deferredBegin)
		}
		allowed[entry.deferredBegin] = entry
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

// scanDeferredBegins parses the named Go source and returns every deferred
// begin it holds, with how many times each occurs.
func scanDeferredBegins(fset *token.FileSet, file string, src any) (map[deferredBegin]int, error) {
	parsed, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		return nil, err
	}
	found := make(map[deferredBegin]int)
	for _, decl := range parsed.Decls {
		function, ok := decl.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		name := storeFuncName(function)
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.CallExpr:
				selector, ok := node.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := selector.X.(*ast.Ident)
				if !ok {
					return true
				}
				if symbol := pkg.Name + "." + selector.Sel.Name; deferredBeginCalls[symbol] {
					found[deferredBegin{File: file, Function: name, Begin: symbol}]++
				}
			case *ast.BasicLit:
				if node.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(node.Value)
				if err != nil {
					return true
				}
				value = strings.TrimSpace(value)
				switch {
				case deferredBeginLiteral.MatchString(value):
					found[deferredBegin{File: file, Function: name, Begin: "BEGIN"}]++
				case savepointLiteral.MatchString(value):
					found[deferredBegin{File: file, Function: name, Begin: "SAVEPOINT"}]++
				}
			}
			return true
		})
	}
	return found, nil
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

// unreviewedDeferredBegins compares what the scan found with the reviewed
// list and returns one finding per difference: a begin missing from the
// list, a function holding a listed begin more than once, and a listed begin
// the code no longer has.
func unreviewedDeferredBegins(found map[deferredBegin]int, allowed map[deferredBegin]deferredBeginEntry) []string {
	var findings []string
	for site, count := range found {
		if _, ok := allowed[site]; !ok {
			findings = append(findings, fmt.Sprintf("%s, which is not on the reviewed list: begin a transaction that may write with beginWrite (IMMEDIATE); if it truly only reads, or only runs nested inside an IMMEDIATE transaction, add it to %s with the reason", site, transactionModeGuardFixture))
			continue
		}
		if count != 1 {
			findings = append(findings, fmt.Sprintf("%s %d times; the reviewed list covers one begin per function, so split the function or review each begin", site, count))
		}
	}
	for site := range allowed {
		if found[site] == 0 {
			findings = append(findings, fmt.Sprintf("the reviewed list names %s, which the code no longer has: remove the stale entry", site))
		}
	}
	sort.Strings(findings)
	return findings
}

// TestStoreWritesBeginImmediate proves every deferred begin in the store's
// production code is a reviewed read-only, nested or private-connection one.
func TestStoreWritesBeginImmediate(t *testing.T) {
	allowed := loadDeferredBeginAllowList(t)
	fset := token.NewFileSet()
	found := make(map[deferredBegin]int)
	for _, file := range storeProductionFiles(t) {
		sites, err := scanDeferredBegins(fset, file, nil)
		if err != nil {
			t.Fatal(err)
		}
		for site, count := range sites {
			found[site] += count
		}
	}
	if findings := unreviewedDeferredBegins(found, allowed); len(findings) > 0 {
		t.Fatalf("store transaction modes differ from the reviewed list:\n%s", strings.Join(findings, "\n"))
	}
}

// TestTransactionModeGuardRefusesDeferredWrite feeds the guard a deferred
// transaction that reads and then writes, in each spelling the scan knows,
// and proves the guard reports it while it accepts the IMMEDIATE forms.
func TestTransactionModeGuardRefusesDeferredWrite(t *testing.T) {
	const probe = `package store

func (s *Store) probeDeferredHelper(conn *sqlite.Conn) (err error) {
	defer sqlitex.Transaction(conn)(&err)
	return readThenWrite(conn)
}

func probeDeferredLiteral(conn *sqlite.Conn) error {
	if err := sqlitex.ExecuteTransient(conn, "BEGIN", nil); err != nil {
		return err
	}
	return readThenWrite(conn)
}

func probeOutermostSavepoint(conn *sqlite.Conn) (err error) {
	defer sqlitex.Save(conn)(&err)
	return readThenWrite(conn)
}

func probeImmediate(conn *sqlite.Conn) (err error) {
	end, err := beginWrite(conn, "write the probe")
	if err != nil {
		return err
	}
	defer end(&err)
	if err := sqlitex.ExecuteTransient(conn, "BEGIN IMMEDIATE", nil); err != nil {
		return err
	}
	return readThenWrite(conn)
}
`
	found, err := scanDeferredBegins(token.NewFileSet(), "probe.go", probe)
	if err != nil {
		t.Fatal(err)
	}
	findings := unreviewedDeferredBegins(found, map[deferredBegin]deferredBeginEntry{})
	want := []string{
		"probe.go Store.probeDeferredHelper begins with sqlitex.Transaction",
		"probe.go probeDeferredLiteral begins with BEGIN",
		"probe.go probeOutermostSavepoint begins with sqlitex.Save",
	}
	if len(findings) != len(want) {
		t.Fatalf("guard findings for the probe = %q, want one for each of %q", findings, want)
	}
	for i, prefix := range want {
		if !strings.HasPrefix(findings[i], prefix+", which is not on the reviewed list") {
			t.Fatalf("guard finding %d = %q, want it to report %q", i, findings[i], prefix)
		}
	}
}
