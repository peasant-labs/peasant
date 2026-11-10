// Package sqliteforkaudit verifies that the vendored SQLite driver copy holds
// only the changes its manifest documents, and that owned code imports the
// audited package rather than the upstream module.
//
// The fork at third_party/zombiezen-sqlite carries a manifest (UPSTREAM.json)
// recording the upstream commit it was taken from, the digest of every
// upstream file, the digest of every file changed for this repository, and the
// exact unsafe.Pointer conversions upstream code contains. The audit compares
// the tree against that manifest and fails closed on any drift. The expected
// upstream identity and the set of files allowed outside upstream source are
// literals here, not manifest fields, so editing the manifest alone can never
// pass: that redundancy is the tripwire.
package sqliteforkaudit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	regexp "github.com/wasilibs/go-re2"
)

// Expected upstream identity of the vendored driver. Compared against the
// manifest tuple, so a manifest that points at another origin fails here.
const (
	// Spelled as a concatenation so this file never holds the quoted module
	// path the owned-import boundary below scans every Go source for, this
	// file included. A joined literal here would match that scan and fail the
	// audit this file implements.
	expectedUpstreamModule  = "zombiezen.com/go/" + "sqlite"
	expectedUpstreamVersion = "v1.4.2"
	expectedUpstreamOrigin  = "https://github.com/zombiezen/go-sqlite.git"
	expectedUpstreamCommit  = "e5fb83745cf1640f27b86f6560ca32f50b2442e9"
)

// expectedMetadataRenames maps each upstream metadata file the fork keeps to
// the name it is stored under. Upstream go.mod/go.sum/go.work/go.work.sum
// would otherwise shadow the module's own files.
func expectedMetadataRenames() map[string]string {
	return map[string]string{
		"go.mod":      "UPSTREAM.go.mod.txt",
		"go.sum":      "UPSTREAM.go.sum.txt",
		"go.work":     "UPSTREAM.go.work.txt",
		"go.work.sum": "UPSTREAM.go.work.sum.txt",
	}
}

// addedForkFiles names every file in the fork that is not upstream source. A
// literal, not a manifest field: a manifest edit alone must never be enough
// to admit a new file.
var addedForkFiles = []string{
	"UPSTREAM.json",
	"PEASANT.md",
	"connection-lifetime.patch",
	"connection_lifetime_test.go",
	"testdata/connection-lifetime.yaml",
}

// unauditedDriverImport matches a Go import of the upstream driver module,
// with or without a subpath. It is the same pattern the previous checker
// used; RE2 accepts the same syntax (escaped dot, non-capturing group,
// negated class), so the matched set is identical, comments included.
var unauditedDriverImport = regexp.MustCompile(`"zombiezen\.com/go/sqlite(?:/[^"\n]*)?"`)

// forkManifest decodes third_party/zombiezen-sqlite/UPSTREAM.json.
type forkManifest struct {
	Module       string              `json:"module"`
	Version      string              `json:"version"`
	Origin       string              `json:"origin"`
	Commit       string              `json:"commit"`
	Files        map[string]string   `json:"files"`
	Modified     map[string]string   `json:"modified_files"`
	Renamed      map[string]string   `json:"renamed_files"`
	PatchSHA256  string              `json:"patch_sha256"`
	PointerLines map[string][]string `json:"native_pointer_lines"`
}

// unauditedImportFound aborts the repository walk with the offending path.
// Its message is the audit failure text.
type unauditedImportFound struct {
	rel string
}

func (e *unauditedImportFound) Error() string {
	return "Peasant still imports the unaudited driver: " + e.rel
}

// audit verifies the fork at forkRoot against its manifest and verifies that
// no Go source under repoRoot imports the upstream driver module. It returns
// nil on success and an error carrying the audit failure text otherwise.
func audit(forkRoot, repoRoot string) error {
	rawManifest, err := os.ReadFile(filepath.Join(forkRoot, "UPSTREAM.json"))
	if err != nil {
		return fmt.Errorf("read the SQLite fork manifest at %s: %w; restore third_party/zombiezen-sqlite/UPSTREAM.json and rerun", filepath.Join(forkRoot, "UPSTREAM.json"), err)
	}
	var manifest forkManifest
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		return fmt.Errorf("decode the SQLite fork manifest at %s: %w; restore third_party/zombiezen-sqlite/UPSTREAM.json and rerun", filepath.Join(forkRoot, "UPSTREAM.json"), err)
	}

	if manifest.Module != expectedUpstreamModule ||
		manifest.Version != expectedUpstreamVersion ||
		manifest.Origin != expectedUpstreamOrigin ||
		manifest.Commit != expectedUpstreamCommit {
		return errors.New("unexpected SQLite upstream origin")
	}

	patch, err := os.ReadFile(filepath.Join(forkRoot, "connection-lifetime.patch"))
	if err != nil {
		return fmt.Errorf("read the SQLite documented patch at %s: %w; restore third_party/zombiezen-sqlite/connection-lifetime.patch and rerun", filepath.Join(forkRoot, "connection-lifetime.patch"), err)
	}
	sum := sha256.Sum256(patch)
	if hex.EncodeToString(sum[:]) != manifest.PatchSHA256 {
		return errors.New("SQLite documented patch changed")
	}

	if _, ok := manifest.Modified["sqlite.go"]; !ok {
		return errors.New("unexpected SQLite modified-file set")
	}
	for name := range manifest.Modified {
		if !strings.HasSuffix(name, ".go") {
			return errors.New("unexpected SQLite modified-file set")
		}
	}

	if !equalStrings(manifest.Renamed, expectedMetadataRenames()) {
		return errors.New("unexpected SQLite metadata renames")
	}

	relFiles, goSources, err := collectForkFiles(forkRoot)
	if err != nil {
		return err
	}
	expectedFiles := make(map[string]struct{}, len(manifest.Files)+len(addedForkFiles))
	for name := range manifest.Files {
		if target, ok := manifest.Renamed[name]; ok {
			name = target
		}
		expectedFiles[name] = struct{}{}
	}
	for _, name := range addedForkFiles {
		expectedFiles[name] = struct{}{}
	}
	actualFiles := make(map[string]struct{}, len(relFiles))
	for _, name := range relFiles {
		actualFiles[name] = struct{}{}
	}
	var missing, extra []string
	for name := range expectedFiles {
		if _, ok := actualFiles[name]; !ok {
			missing = append(missing, name)
		}
	}
	for name := range actualFiles {
		if _, ok := expectedFiles[name]; !ok {
			extra = append(extra, name)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		return fmt.Errorf("SQLite file set changed: missing=%s, extra=%s", formatFileSet(missing), formatFileSet(extra))
	}

	for _, name := range orderedFileNames(rawManifest) {
		upstreamDigest, ok := manifest.Files[name]
		if !ok {
			continue
		}
		expectedDigest := upstreamDigest
		if modifiedDigest, ok := manifest.Modified[name]; ok {
			expectedDigest = modifiedDigest
		}
		diskName := name
		if target, ok := manifest.Renamed[name]; ok {
			diskName = target
		}
		content, err := os.ReadFile(filepath.Join(forkRoot, filepath.FromSlash(diskName)))
		if err != nil {
			// Fail closed with the read error, not a digest mismatch: the
			// file could not be read, so no digest comparison ran. Keep
			// "undocumented SQLite source change" for a computed digest
			// that differs.
			diskPath := filepath.Join(forkRoot, filepath.FromSlash(diskName))
			return fmt.Errorf("read the SQLite fork file at %s: %w; restore third_party/zombiezen-sqlite/%s and rerun", diskPath, err, diskName)
		}
		actualDigest := sha256.Sum256(content)
		if hex.EncodeToString(actualDigest[:]) != expectedDigest {
			return fmt.Errorf("undocumented SQLite source change: %s", name)
		}
	}

	if err := checkPointerLines(goSources, manifest.PointerLines); err != nil {
		return err
	}

	return checkOwnedImports(repoRoot)
}

// equalStrings reports whether two string maps hold the same keys and values.
func equalStrings(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if other, ok := b[key]; !ok || other != value {
			return false
		}
	}
	return true
}

// formatFileSet renders a file-name set the way the previous checker did:
// sorted names in braces with single-quote reprs, and an empty set as set().
func formatFileSet(names []string) string {
	if len(names) == 0 {
		return "set()"
	}
	ordered := append([]string(nil), names...)
	sort.Strings(ordered)
	var out strings.Builder
	out.WriteByte('{')
	for i, name := range ordered {
		if i > 0 {
			out.WriteString(", ")
		}
		out.WriteByte('\'')
		out.WriteString(name)
		out.WriteByte('\'')
	}
	out.WriteByte('}')
	return out.String()
}

// collectForkFiles lists every file under forkRoot as a slash-separated path
// relative to forkRoot, and returns the content of every .go file keyed by
// that same relative path.
func collectForkFiles(forkRoot string) (relFiles []string, goSources map[string][]byte, err error) {
	goSources = map[string][]byte{}
	walkErr := filepath.WalkDir(forkRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(forkRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		relFiles = append(relFiles, rel)
		if strings.HasSuffix(entry.Name(), ".go") {
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			goSources[rel] = content
		}
		return nil
	})
	if walkErr != nil {
		return nil, nil, fmt.Errorf("list the SQLite fork at %s: %w; restore a readable third_party/zombiezen-sqlite tree and rerun", forkRoot, walkErr)
	}
	return relFiles, goSources, nil
}

// orderedFileNames returns the keys of the manifest's files object in the
// order they appear in the document, so the first digest mismatch reported is
// the same one the previous checker reported.
func orderedFileNames(rawManifest []byte) []string {
	decoder := json.NewDecoder(bytes.NewReader(rawManifest))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil
		}
		if key != "files" {
			var skip json.RawMessage
			if err := decoder.Decode(&skip); err != nil {
				return nil
			}
			continue
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
			return nil
		}
		var names []string
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil
			}
			name, ok := key.(string)
			if !ok {
				return nil
			}
			names = append(names, name)
			var skip json.RawMessage
			if err := decoder.Decode(&skip); err != nil {
				return nil
			}
		}
		return names
	}
	return nil
}

// checkPointerLines verifies that the unsafe.Pointer conversions in the
// fork's Go sources are exactly the audited upstream set: for every .go file
// containing such a conversion, the multiset of stripped lines must equal the
// manifest entry. This proves the unsafeptr vet exemption covers exactly the
// upstream lines and nothing added here.
func checkPointerLines(goSources map[string][]byte, expected map[string][]string) error {
	actual := make(map[string]map[string]int, len(goSources))
	for rel, content := range goSources {
		counts := map[string]int{}
		for _, line := range strings.Split(string(content), "\n") {
			if !strings.Contains(line, "unsafe.Pointer(") {
				continue
			}
			counts[strings.TrimSpace(line)]++
		}
		if len(counts) > 0 {
			actual[rel] = counts
		}
	}
	if len(actual) != len(expected) {
		return errors.New("SQLite native pointer expressions differ from audited upstream source")
	}
	for rel, expectedLines := range expected {
		actualCounts, ok := actual[rel]
		expectedCounts := map[string]int{}
		for _, line := range expectedLines {
			expectedCounts[line]++
		}
		if !ok || len(actualCounts) != len(expectedCounts) {
			return errors.New("SQLite native pointer expressions differ from audited upstream source")
		}
		for line, count := range expectedCounts {
			if actualCounts[line] != count {
				return errors.New("SQLite native pointer expressions differ from audited upstream source")
			}
		}
	}
	return nil
}

// symlinkCycleFound aborts the owned-source walk when a directory resolves
// to one of its ancestors: descending into it would recurse forever.
type symlinkCycleFound struct {
	rel string
}

func (e *symlinkCycleFound) Error() string {
	return "directory symlink cycle at " + e.rel
}

// checkOwnedImports fails on the first Go source under repoRoot that imports
// the upstream driver module. Paths with a .git, node_modules, or vendor part
// are not owned source and are skipped: a Nix build vends the module's
// dependencies before it runs the tests, and a vendored dependency's own
// imports are not Peasant's.
func checkOwnedImports(repoRoot string) error {
	rootInfo, err := os.Stat(repoRoot)
	if err != nil {
		return fmt.Errorf("scan owned Go sources under %s: %w; restore a readable tree and rerun", repoRoot, err)
	}
	if !rootInfo.IsDir() {
		return fmt.Errorf("scan owned Go sources under %s: not a directory; restore a readable tree and rerun", repoRoot)
	}
	walkErr := walkOwnedTree(repoRoot, ".", []os.FileInfo{rootInfo})
	if walkErr != nil {
		var found *unauditedImportFound
		if errors.As(walkErr, &found) {
			return found
		}
		var cycle *symlinkCycleFound
		if errors.As(walkErr, &cycle) {
			return fmt.Errorf("scan owned Go sources under %s: %s; remove the symlink loop and rerun", repoRoot, cycle.Error())
		}
		return fmt.Errorf("scan owned Go sources under %s: %w; restore a readable tree and rerun", repoRoot, walkErr)
	}
	return nil
}

// walkOwnedTree scans the directory at fsPath, whose logical repo-relative
// name is rel, with ancestors holding the Stat identity of every directory
// above it. Every entry is resolved with os.Stat, so a symlink to a
// directory is descended into: the Go compiler resolves imports through such
// links, and an audit that never descends passes while the build sees the
// import. The logical (symlink-through) path is what the .git/node_modules
// skip and the failure report use. A directory resolving to an ancestor
// (compared with os.SameFile, which is a device+inode comparison on Unix)
// fails with symlinkCycleFound instead of hanging; an unresolvable entry
// fails with its Stat error.
func walkOwnedTree(fsPath, rel string, ancestors []os.FileInfo) error {
	entries, err := os.ReadDir(fsPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		childRel := entry.Name()
		if rel != "." {
			childRel = rel + "/" + entry.Name()
		}
		skipped := false
		for _, part := range strings.Split(childRel, "/") {
			if part == ".git" || part == "node_modules" || part == "vendor" {
				skipped = true
				break
			}
		}
		if skipped {
			continue
		}
		childFS := filepath.Join(fsPath, entry.Name())
		info, err := os.Stat(childFS)
		if err != nil {
			return err
		}
		if info.IsDir() {
			for _, ancestor := range ancestors {
				if os.SameFile(info, ancestor) {
					return &symlinkCycleFound{rel: childRel}
				}
			}
			if err := walkOwnedTree(childFS, childRel, append(ancestors, info)); err != nil {
				return err
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		content, err := os.ReadFile(childFS)
		if err != nil {
			return err
		}
		if unauditedDriverImport.FindString(string(content)) != "" {
			return &unauditedImportFound{rel: childRel}
		}
	}
	return nil
}
