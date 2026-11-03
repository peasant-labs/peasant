package peasant

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestOwnedCodeDoesNotImportStdlibRegexp(t *testing.T) {
	t.Parallel()

	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel := filepath.ToSlash(path)
		if entry.IsDir() {
			// Only this hash-audited upstream fork is exempt: its SQL regexp
			// UDF and example retain stdlib imports. Do not edit vendor hashes
			// for an engine substitution in code owned by Peasant.
			if rel == "third_party/zombiezen-sqlite" {
				return filepath.SkipDir
			}
			// Generated assets, dependencies, linked worktrees and hidden
			// metadata are not module source. All other directories, including
			// scripts and testdata, are scanned irrespective of build tags.
			if rel != "." && (strings.HasPrefix(entry.Name(), ".") ||
				entry.Name() == "node_modules" || entry.Name() == "vendor" ||
				rel == "web/out" || rel == "dist" || rel == "bin" ||
				rel == "result" || rel == "worktree" || rel == "review-capture") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Errorf("regex import audit cannot parse %s: %v; fix its import declarations so the module engine can be verified", rel, err)
			return nil
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Errorf("regex import audit cannot decode %s import %s: %v; use a valid Go import path", rel, spec.Path.Value, err)
				continue
			}
			switch importPath {
			case "regexp":
				t.Errorf("%s imports stdlib %q: owned code must use the go-re2 engine; replace this import with regexp %q without changing the call sites", rel, importPath, "github.com/wasilibs/go-re2")
			case "regexp/syntax":
				// Config validates patterns for the pinned redact executor,
				// which still uses stdlib syntax. go-re2 accepts \\C but that
				// executor cannot compile it. Remove this exception when the
				// executor is re-pinned to go-re2, not before.
				if rel != "internal/config/config.go" {
					t.Errorf("%s imports %q: only internal/config/config.go needs the stdlib parser for executor parity; use go-re2 elsewhere", rel, importPath)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("regex import audit could not walk the module: %v; restore readable source directories and rerun the test", err)
	}
}
