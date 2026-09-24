package ingest_test

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

// openCodeBuildTopologyDirectoryPrefix names every copied build-topology
// package. The case runner creates them with it and the startup sweep removes
// only direct child directories that carry it.
const openCodeBuildTopologyDirectoryPrefix = ".sqlite-topology-"

// prepareOpenCodeBuildTopologyProduction takes exclusive ownership of
// sourceDirectory, removes packages a killed earlier run left behind, and lists
// the production files to copy. On success the caller owns release until every
// copied case has been cleaned up. On error no ownership is returned.
func prepareOpenCodeBuildTopologyProduction(sourceDirectory string) (production []string, release func() error, err error) {
	ownerRelease, err := acquireOpenCodeBuildTopologyOwnership(sourceDirectory)
	if err != nil {
		return nil, nil, fmt.Errorf("prepare build-topology packages in %q: %w", sourceDirectory, err)
	}
	defer func() {
		if err == nil {
			return
		}
		if releaseErr := ownerRelease(); releaseErr != nil {
			err = errors.Join(err, releaseErr)
		}
	}()
	if sweepErr := sweepLeftoverTopologyPackages(sourceDirectory); sweepErr != nil {
		return nil, nil, fmt.Errorf("prepare build-topology packages in %q: %w", sourceDirectory, sweepErr)
	}
	production, err = ingestProductionFiles(sourceDirectory)
	if err != nil {
		return nil, nil, fmt.Errorf("prepare build-topology packages in %q: list the production files after the leftover sweep: %w", sourceDirectory, err)
	}
	return production, ownerRelease, nil
}

// sweepLeftoverTopologyPackages removes the direct child directories that carry
// the copied-package prefix. It never selects a nested entry, a regular file or
// a symlink, so it cannot reach outside sourceDirectory.
func sweepLeftoverTopologyPackages(sourceDirectory string) error {
	entries, err := os.ReadDir(sourceDirectory)
	if err != nil {
		return fmt.Errorf("sweep leftover build-topology packages: read the source directory %q before the first case: %w; no leftover was removed and the guard cannot start; make the directory readable, then retry", sourceDirectory, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), openCodeBuildTopologyDirectoryPrefix) {
			continue
		}
		leftover := filepath.Join(sourceDirectory, entry.Name())
		if err := os.RemoveAll(leftover); err != nil {
			return fmt.Errorf("sweep leftover build-topology packages: remove %q before the first case: %w; earlier leftovers may already be removed and this one may be partly removed, so the guard cannot start; fix the permissions on that directory or remove it by hand, then retry", leftover, err)
		}
	}
	return nil
}

// newOpenCodeBuildTopologyRelease closes the ownership descriptor exactly once.
// Every call returns the first result, so a repeated release cannot touch a
// descriptor number that a later owner may have received.
func newOpenCodeBuildTopologyRelease(file *os.File, sourceDirectory string) func() error {
	return sync.OnceValue(func() error {
		if err := file.Close(); err != nil {
			return fmt.Errorf("release build-topology startup ownership of %q: close its directory descriptor: %w; whether the descriptor and its lock were released is unknown, so cleanup cannot be certified; end this isolated test process, then retry from a clean supported test environment", sourceDirectory, err)
		}
		return nil
	})
}

const openCodeTopologyCleanupFixturePath = "internal/ingest/testdata/opencode_topology_cleanup.yaml"

//go:embed testdata/opencode_topology_cleanup.yaml
var openCodeTopologyCleanupFixtureYAML []byte

type openCodeTopologyEntryKind string

const (
	openCodeTopologyEntryFile      openCodeTopologyEntryKind = "file"
	openCodeTopologyEntryDirectory openCodeTopologyEntryKind = "directory"
	openCodeTopologyEntrySymlink   openCodeTopologyEntryKind = "symlink"
)

type openCodeTopologyCleanupAction string

const (
	openCodeTopologyPrepare             openCodeTopologyCleanupAction = "prepare"
	openCodeTopologyPrepareAgain        openCodeTopologyCleanupAction = "prepare-again-after-release"
	openCodeTopologyBusyRoot            openCodeTopologyCleanupAction = "busy-root"
	openCodeTopologyMissingRoot         openCodeTopologyCleanupAction = "missing-root"
	openCodeTopologyReleaseTwice        openCodeTopologyCleanupAction = "release-twice"
	openCodeTopologyInventoryFailure    openCodeTopologyCleanupAction = "inventory-failure"
	openCodeTopologyReleaseCloseFailure openCodeTopologyCleanupAction = "release-close-failure"
	openCodeTopologyUnsupportedPlatform openCodeTopologyCleanupAction = "unsupported-platform"
)

type openCodeTopologyEntry struct {
	Path    string                    `yaml:"path"`
	Kind    openCodeTopologyEntryKind `yaml:"kind"`
	Content string                    `yaml:"content"`
	Target  string                    `yaml:"target"`
}

type openCodeTopologyCleanupCase struct {
	Name              string                        `yaml:"name"`
	Action            openCodeTopologyCleanupAction `yaml:"action"`
	Root              string                        `yaml:"root"`
	Entries           []openCodeTopologyEntry       `yaml:"entries"`
	RepairEntries     []openCodeTopologyEntry       `yaml:"repair_entries"`
	ExpectedAbsent    []string                      `yaml:"expected_absent"`
	ExpectedInventory []string                      `yaml:"expected_inventory"`
	ErrorContains     []string                      `yaml:"error_contains"`
	ReleaseFile       string                        `yaml:"release_file"`
}

type openCodeTopologyCleanupFixture struct {
	RequiredCases []string                      `yaml:"required_cases"`
	Cases         []openCodeTopologyCleanupCase `yaml:"cases"`
}

func loadOpenCodeTopologyCleanupFixture(t testing.TB) openCodeTopologyCleanupFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(openCodeTopologyCleanupFixtureYAML))
	decoder.KnownFields(true)
	var fixture openCodeTopologyCleanupFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode %s: %v", openCodeTopologyCleanupFixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("decode %s: expected exactly one YAML document: %v", openCodeTopologyCleanupFixturePath, err)
	}
	if err := validateOpenCodeTopologyCleanupFixture(fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func validateOpenCodeTopologyCleanupFixture(fixture openCodeTopologyCleanupFixture) error {
	if len(fixture.RequiredCases) == 0 {
		return fmt.Errorf("%s declares no required_cases; restore the named manifest", openCodeTopologyCleanupFixturePath)
	}
	seen := make(map[string]bool, len(fixture.Cases))
	for _, fixtureCase := range fixture.Cases {
		if strings.TrimSpace(fixtureCase.Name) == "" || seen[fixtureCase.Name] {
			return fmt.Errorf("%s has an empty or duplicate case name %q; give every case a unique name", openCodeTopologyCleanupFixturePath, fixtureCase.Name)
		}
		seen[fixtureCase.Name] = true
		if err := validateOpenCodeTopologyCleanupCase(fixtureCase); err != nil {
			return fmt.Errorf("%s case %q: %w", openCodeTopologyCleanupFixturePath, fixtureCase.Name, err)
		}
	}
	required := make(map[string]bool, len(fixture.RequiredCases))
	for _, name := range fixture.RequiredCases {
		if strings.TrimSpace(name) == "" || required[name] {
			return fmt.Errorf("%s required_cases has an empty or duplicate name %q", openCodeTopologyCleanupFixturePath, name)
		}
		required[name] = true
		if !seen[name] {
			return fmt.Errorf("%s is missing required case %q; restore that case so its startup cleanup behavior stays covered", openCodeTopologyCleanupFixturePath, name)
		}
	}
	return nil
}

func validateOpenCodeTopologyCleanupCase(fixtureCase openCodeTopologyCleanupCase) error {
	seeded, err := validateOpenCodeTopologyEntries(fixtureCase.Entries, nil)
	if err != nil {
		return err
	}
	if _, err := validateOpenCodeTopologyEntries(fixtureCase.RepairEntries, seeded); err != nil {
		return fmt.Errorf("repair_entries: %w", err)
	}
	for _, absent := range fixtureCase.ExpectedAbsent {
		if _, ok := seeded[absent]; !ok {
			return fmt.Errorf("expected_absent names %q, which is not a seeded entry", absent)
		}
	}
	succeeds := true
	switch fixtureCase.Action {
	case openCodeTopologyPrepare, openCodeTopologyPrepareAgain:
		if len(fixtureCase.ErrorContains) != 0 {
			return fmt.Errorf("action %q succeeds and must not pin error_contains", fixtureCase.Action)
		}
	case openCodeTopologyBusyRoot, openCodeTopologyReleaseTwice, openCodeTopologyInventoryFailure:
	case openCodeTopologyMissingRoot:
		succeeds = false
		if !filepath.IsLocal(fixtureCase.Root) || len(fixtureCase.Entries) != 0 {
			return fmt.Errorf("action %q needs a local root name and no seeded entries", fixtureCase.Action)
		}
	case openCodeTopologyReleaseCloseFailure:
		succeeds = false
		if seeded[fixtureCase.ReleaseFile] != openCodeTopologyEntryFile {
			return fmt.Errorf("release_file %q must name a seeded file", fixtureCase.ReleaseFile)
		}
	case openCodeTopologyUnsupportedPlatform:
		succeeds = false
		for _, entry := range fixtureCase.Entries {
			if entry.Kind == openCodeTopologyEntrySymlink {
				return fmt.Errorf("action %q must not seed symlinks, found %q", fixtureCase.Action, entry.Path)
			}
		}
	default:
		return fmt.Errorf("unknown action %q; use one of the actions listed at the top of the fixture", fixtureCase.Action)
	}
	if fixtureCase.Action != openCodeTopologyMissingRoot && fixtureCase.Root != "" {
		return fmt.Errorf("root is only valid for action %q", openCodeTopologyMissingRoot)
	}
	if fixtureCase.Action != openCodeTopologyInventoryFailure && len(fixtureCase.RepairEntries) != 0 {
		return fmt.Errorf("repair_entries is only valid for action %q", openCodeTopologyInventoryFailure)
	}
	if fixtureCase.Action != openCodeTopologyReleaseCloseFailure && fixtureCase.ReleaseFile != "" {
		return fmt.Errorf("release_file is only valid for action %q", openCodeTopologyReleaseCloseFailure)
	}
	if fixtureCase.Action != openCodeTopologyPrepare && fixtureCase.Action != openCodeTopologyPrepareAgain && len(fixtureCase.ErrorContains) == 0 {
		return fmt.Errorf("action %q must pin its actionable error clauses in error_contains", fixtureCase.Action)
	}
	if succeeds != (len(fixtureCase.ExpectedInventory) != 0) {
		return fmt.Errorf("action %q expects inventory=%t, but expected_inventory has %d entries", fixtureCase.Action, succeeds, len(fixtureCase.ExpectedInventory))
	}
	if !succeeds && len(fixtureCase.ExpectedAbsent) != 0 {
		return fmt.Errorf("action %q never sweeps, so expected_absent must be empty", fixtureCase.Action)
	}
	return nil
}

// validateOpenCodeTopologyEntries checks that every path stays inside the case
// root and that each entry's parent directory is seeded before it.
func validateOpenCodeTopologyEntries(entries []openCodeTopologyEntry, existing map[string]openCodeTopologyEntryKind) (map[string]openCodeTopologyEntryKind, error) {
	kinds := make(map[string]openCodeTopologyEntryKind, len(existing)+len(entries))
	for location, kind := range existing {
		kinds[location] = kind
	}
	for _, entry := range entries {
		if !filepath.IsLocal(filepath.FromSlash(entry.Path)) || path.Clean(entry.Path) != entry.Path {
			return nil, fmt.Errorf("entry path %q must be a clean path inside the case root", entry.Path)
		}
		if _, duplicate := kinds[entry.Path]; duplicate {
			return nil, fmt.Errorf("entry path %q is seeded twice", entry.Path)
		}
		if parent := path.Dir(entry.Path); parent != "." && kinds[parent] != openCodeTopologyEntryDirectory {
			return nil, fmt.Errorf("entry %q needs its parent directory %q seeded before it", entry.Path, parent)
		}
		switch entry.Kind {
		case openCodeTopologyEntryFile:
			if entry.Target != "" {
				return nil, fmt.Errorf("file entry %q must not declare a target", entry.Path)
			}
		case openCodeTopologyEntryDirectory:
			if entry.Target != "" || entry.Content != "" {
				return nil, fmt.Errorf("directory entry %q must not declare content or a target", entry.Path)
			}
		case openCodeTopologyEntrySymlink:
			resolved := path.Join(path.Dir(entry.Path), entry.Target)
			if entry.Target == "" || entry.Content != "" || !filepath.IsLocal(filepath.FromSlash(resolved)) {
				return nil, fmt.Errorf("symlink entry %q needs a target inside the case root and no content, got %q", entry.Path, entry.Target)
			}
		default:
			return nil, fmt.Errorf("entry %q has unknown kind %q; use file, directory or symlink", entry.Path, entry.Kind)
		}
		kinds[entry.Path] = entry.Kind
	}
	return kinds, nil
}

func TestOpenCodeBuildTopologyStartupFixtureRequiresEveryNamedCase(t *testing.T) {
	fixture := loadOpenCodeTopologyCleanupFixture(t)
	for _, name := range fixture.RequiredCases {
		mutated := fixture
		mutated.Cases = slices.DeleteFunc(slices.Clone(fixture.Cases), func(fixtureCase openCodeTopologyCleanupCase) bool {
			return fixtureCase.Name == name
		})
		err := validateOpenCodeTopologyCleanupFixture(mutated)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("missing required case %q", name)) || !strings.Contains(err.Error(), openCodeTopologyCleanupFixturePath) {
			t.Fatalf("dropping required case %q: validation error = %v, want it to name the case and %s", name, err, openCodeTopologyCleanupFixturePath)
		}
	}
}

func TestOpenCodeBuildTopologyStartupCleanup(t *testing.T) {
	fixture := loadOpenCodeTopologyCleanupFixture(t)
	for _, fixtureCase := range fixture.Cases {
		// Supported platforms prove the lifecycle; unsupported ones prove the refusal.
		if (fixtureCase.Action == openCodeTopologyUnsupportedPlatform) == openCodeBuildTopologyOwnershipSupported {
			continue
		}
		t.Run(fixtureCase.Name, func(t *testing.T) {
			runOpenCodeTopologyCleanupCase(t, fixtureCase)
		})
	}
}

func runOpenCodeTopologyCleanupCase(t *testing.T, fixtureCase openCodeTopologyCleanupCase) {
	parent := t.TempDir()
	root := parent
	if fixtureCase.Action == openCodeTopologyMissingRoot {
		root = filepath.Join(parent, fixtureCase.Root)
	}
	seedOpenCodeTopologyEntries(t, root, fixtureCase.Entries)

	switch fixtureCase.Action {
	case openCodeTopologyPrepare:
		release := requireOpenCodeTopologyPrepared(t, root, fixtureCase)
		requireOpenCodeTopologyReleased(t, release)
	case openCodeTopologyPrepareAgain:
		requireOpenCodeTopologyReleased(t, requireOpenCodeTopologyPrepared(t, root, fixtureCase))
		requireOpenCodeTopologyReleased(t, requireOpenCodeTopologyPrepared(t, root, fixtureCase))
	case openCodeTopologyBusyRoot:
		holder, err := acquireOpenCodeBuildTopologyOwnership(root)
		if err != nil {
			t.Fatalf("live owner could not acquire %q: %v", root, err)
		}
		t.Cleanup(func() { _ = holder() })
		requireOpenCodeTopologyRefused(t, root, fixtureCase)
		assertOpenCodeTopologyEntriesPreserved(t, root, fixtureCase.Entries, nil)
		requireOpenCodeTopologyReleased(t, holder)
		requireOpenCodeTopologyReleased(t, requireOpenCodeTopologyPrepared(t, root, fixtureCase))
	case openCodeTopologyMissingRoot:
		requireOpenCodeTopologyRefused(t, root, fixtureCase)
		if _, err := os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("refused preparation of missing root %q left it in place: Lstat error = %v", root, err)
		}
		if created, err := os.ReadDir(parent); err != nil || len(created) != 0 {
			t.Fatalf("refused preparation of missing root created entries %v in %q (read error %v)", created, parent, err)
		}
	case openCodeTopologyReleaseTwice:
		first := requireOpenCodeTopologyPrepared(t, root, fixtureCase)
		requireOpenCodeTopologyReleased(t, first)
		requireOpenCodeTopologyReleased(t, first)
		second := requireOpenCodeTopologyPrepared(t, root, fixtureCase)
		t.Cleanup(func() { _ = second() })
		requireOpenCodeTopologyReleased(t, first)
		requireOpenCodeTopologyRefused(t, root, fixtureCase)
		requireOpenCodeTopologyReleased(t, second)
		requireOpenCodeTopologyReleased(t, requireOpenCodeTopologyPrepared(t, root, fixtureCase))
	case openCodeTopologyInventoryFailure:
		requireOpenCodeTopologyRefused(t, root, fixtureCase)
		seedOpenCodeTopologyEntries(t, root, fixtureCase.RepairEntries)
		requireOpenCodeTopologyReleased(t, requireOpenCodeTopologyPrepared(t, root, fixtureCase))
	case openCodeTopologyReleaseCloseFailure:
		file, err := os.Open(filepath.Join(root, fixtureCase.ReleaseFile))
		if err != nil {
			t.Fatalf("open the release fixture file: %v", err)
		}
		release := newOpenCodeBuildTopologyRelease(file, root)
		if err := file.Close(); err != nil {
			t.Fatalf("pre-close the release fixture file: %v", err)
		}
		firstErr, secondErr := release(), release()
		if !errors.Is(firstErr, os.ErrClosed) || secondErr != firstErr {
			t.Fatalf("release of a closed descriptor returned %v then %v, want the same cached os.ErrClosed failure", firstErr, secondErr)
		}
		requireOpenCodeTopologyErrorClauses(t, firstErr, fixtureCase.ErrorContains)
	case openCodeTopologyUnsupportedPlatform:
		requireOpenCodeTopologyRefused(t, root, fixtureCase)
		assertOpenCodeTopologyEntriesPreserved(t, root, fixtureCase.Entries, nil)
	}
}

func requireOpenCodeTopologyPrepared(t *testing.T, root string, fixtureCase openCodeTopologyCleanupCase) func() error {
	t.Helper()
	production, release, err := prepareOpenCodeBuildTopologyProduction(root)
	if err != nil || release == nil {
		t.Fatalf("prepare %q: release present=%t, error = %v", root, release != nil, err)
	}
	t.Cleanup(func() { _ = release() })
	basenames := make([]string, 0, len(production))
	for _, filename := range production {
		basenames = append(basenames, filepath.Base(filename))
	}
	if !slices.Equal(basenames, fixtureCase.ExpectedInventory) {
		t.Fatalf("prepare %q returned production inventory %v, want %v", root, basenames, fixtureCase.ExpectedInventory)
	}
	assertOpenCodeTopologyEntriesAbsent(t, root, fixtureCase.ExpectedAbsent)
	assertOpenCodeTopologyEntriesPreserved(t, root, fixtureCase.Entries, fixtureCase.ExpectedAbsent)
	return release
}

func requireOpenCodeTopologyRefused(t *testing.T, root string, fixtureCase openCodeTopologyCleanupCase) {
	t.Helper()
	production, release, err := prepareOpenCodeBuildTopologyProduction(root)
	if release != nil {
		t.Cleanup(func() { _ = release() })
	}
	if err == nil || production != nil || release != nil {
		t.Fatalf("prepare %q returned inventory %v, release present=%t, error %v; want a refusal with no ownership", root, production, release != nil, err)
	}
	requireOpenCodeTopologyErrorClauses(t, err, fixtureCase.ErrorContains)
}

func requireOpenCodeTopologyReleased(t *testing.T, release func() error) {
	t.Helper()
	if err := release(); err != nil {
		t.Fatalf("release build-topology ownership: %v", err)
	}
}

func requireOpenCodeTopologyErrorClauses(t *testing.T, err error, clauses []string) {
	t.Helper()
	for _, clause := range clauses {
		if !strings.Contains(err.Error(), clause) {
			t.Fatalf("error %q omits the fixture-owned clause %q", err, clause)
		}
	}
}

func seedOpenCodeTopologyEntries(t *testing.T, root string, entries []openCodeTopologyEntry) {
	t.Helper()
	for _, entry := range entries {
		location := filepath.Join(root, filepath.FromSlash(entry.Path))
		var err error
		switch entry.Kind {
		case openCodeTopologyEntryFile:
			err = os.WriteFile(location, []byte(entry.Content), 0o600)
		case openCodeTopologyEntryDirectory:
			err = os.Mkdir(location, 0o700)
		case openCodeTopologyEntrySymlink:
			err = os.Symlink(filepath.FromSlash(entry.Target), location)
		}
		if err != nil {
			t.Fatalf("seed %s %q: %v", entry.Kind, entry.Path, err)
		}
	}
}

func assertOpenCodeTopologyEntriesAbsent(t *testing.T, root string, absent []string) {
	t.Helper()
	for _, relative := range absent {
		location := filepath.Join(root, filepath.FromSlash(relative))
		if _, err := os.Lstat(location); !errors.Is(err, fs.ErrNotExist) {
			var remaining []string
			_ = filepath.WalkDir(location, func(walked string, _ fs.DirEntry, _ error) error {
				remaining = append(remaining, walked)
				return nil
			})
			t.Fatalf("leftover build-topology package %q is still present after startup preparation (Lstat error %v); remaining paths: %v", location, err, remaining)
		}
	}
}

func assertOpenCodeTopologyEntriesPreserved(t *testing.T, root string, entries []openCodeTopologyEntry, absent []string) {
	t.Helper()
	for _, entry := range entries {
		if slices.ContainsFunc(absent, func(removed string) bool {
			return entry.Path == removed || strings.HasPrefix(entry.Path, removed+"/")
		}) {
			continue
		}
		location := filepath.Join(root, filepath.FromSlash(entry.Path))
		info, err := os.Lstat(location)
		if err != nil {
			t.Fatalf("entry %q that startup preparation must preserve is gone: %v", entry.Path, err)
		}
		switch entry.Kind {
		case openCodeTopologyEntryFile:
			data, readErr := os.ReadFile(location)
			if !info.Mode().IsRegular() || readErr != nil || string(data) != entry.Content {
				t.Fatalf("preserved file %q changed: mode %v, content %q, read error %v", entry.Path, info.Mode(), data, readErr)
			}
		case openCodeTopologyEntryDirectory:
			if !info.IsDir() {
				t.Fatalf("preserved directory %q is now %v", entry.Path, info.Mode())
			}
		case openCodeTopologyEntrySymlink:
			target, readErr := os.Readlink(location)
			if info.Mode()&fs.ModeSymlink == 0 || readErr != nil || target != filepath.FromSlash(entry.Target) {
				t.Fatalf("preserved symlink %q changed: mode %v, target %q, read error %v", entry.Path, info.Mode(), target, readErr)
			}
		}
	}
}
