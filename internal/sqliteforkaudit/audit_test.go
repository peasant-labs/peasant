package sqliteforkaudit

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/mutations.yaml
var mutationsYAML []byte

//go:embed testdata/mutations.manifest.yaml
var mutationsManifestYAML []byte

type mutationStep struct {
	Kind   string `yaml:"kind"`
	Path   string `yaml:"path"`
	Key    string `yaml:"key"`
	Field  string `yaml:"field"`
	Value  string `yaml:"value"`
	Text   string `yaml:"text"`
	Import string `yaml:"import"`
}

type mutationCase struct {
	Name  string         `yaml:"name"`
	Steps []mutationStep `yaml:"steps"`
	Want  string         `yaml:"want"`
}

func loadMutationsFixture(t *testing.T) []mutationCase {
	t.Helper()
	var document struct {
		Cases []mutationCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(mutationsYAML, &document); err != nil {
		t.Fatalf("decode testdata/mutations.yaml: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(mutationsManifestYAML, "SQLite fork audit mutations")
	if err != nil {
		t.Fatalf("decode testdata/mutations.manifest.yaml: %v", err)
	}
	names := make([]string, 0, len(document.Cases))
	for _, c := range document.Cases {
		if c.Name == "" {
			t.Fatal("testdata/mutations.yaml has a case with no name; name every case")
		}
		names = append(names, c.Name)
		for _, step := range c.Steps {
			checkMutationStep(t, c.Name, step)
		}
		if c.Want == "" && len(c.Steps) > 0 {
			t.Fatalf("case %q mutates the tree but expects success; state the failure text it must report", c.Name)
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "SQLite fork audit mutations"); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

// checkMutationStep rejects a step that names no known kind or omits the
// parameters its kind needs, so a misspelled fixture step fails here instead
// of silently mutating nothing.
func checkMutationStep(t *testing.T, caseName string, step mutationStep) {
	t.Helper()
	need := func(what string, ok bool) {
		t.Helper()
		if !ok {
			t.Fatalf("case %q has a %q step missing %s; declare it so the step mutates what it claims", caseName, step.Kind, what)
		}
	}
	switch step.Kind {
	case "manifest-field":
		need("field and value", step.Field != "" && step.Value != "")
	case "manifest-drop-modified", "manifest-drop-renamed":
		need("key", step.Key != "")
	case "manifest-digest-match", "file-remove":
		need("path", step.Path != "")
	case "patch-append":
		need("text", step.Text != "")
	case "file-add", "file-append":
		need("path and text", step.Path != "" && step.Text != "")
	case "repo-import":
		need("path and import", step.Path != "" && step.Import != "")
	default:
		t.Fatalf("case %q names unknown mutation kind %q; use one the audit test applies", caseName, step.Kind)
	}
}

// TestAuditCleanTree runs the audit over the real fork and the real module.
// It proves the checked-in tree passes, including the owned-import boundary.
func TestAuditCleanTree(t *testing.T) {
	t.Parallel()
	root := testutil.ModuleRoot(t)
	if err := audit(filepath.Join(root, "third_party", "zombiezen-sqlite"), root); err != nil {
		t.Fatalf("audit of the clean tree failed: %v", err)
	}
}

// TestAuditMutations copies the fork per case, applies the case's steps, and
// checks the audit reports the case's failure text. The copy keeps mutations
// off the real tree; the empty repository root keeps fork mutations from
// tripping the owned-import boundary, except in the repo-import cases, which
// point the audit at a root holding their probe file.
func TestAuditMutations(t *testing.T) {
	t.Parallel()
	root := testutil.ModuleRoot(t)
	for _, c := range loadMutationsFixture(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			forkCopy := copyFork(t, filepath.Join(root, "third_party", "zombiezen-sqlite"))
			repoDir := t.TempDir()
			for _, step := range c.Steps {
				applyMutationStep(t, forkCopy, repoDir, step)
			}
			err := audit(forkCopy, repoDir)
			if c.Want == "" {
				if err != nil {
					t.Fatalf("audit of the unmutated copy failed: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("audit passed a %q mutation; want failure containing %q", c.Name, c.Want)
			}
			if !strings.Contains(err.Error(), c.Want) {
				t.Fatalf("audit error = %q, want substring %q", err.Error(), c.Want)
			}
		})
	}
}

// copyFork duplicates the fork into a temporary directory, preserving file
// modes, so a case can mutate the copy while the real tree stays clean.
func copyFork(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	walkErr := filepath.WalkDir(src, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(target, content, info.Mode())
	})
	if walkErr != nil {
		t.Fatalf("copy the SQLite fork for a mutation case: %v", walkErr)
	}
	return dst
}

// applyMutationStep applies one fixture step to the fork copy, or to the
// temporary repository root for a repo-import step.
func applyMutationStep(t *testing.T, forkCopy, repoDir string, step mutationStep) {
	t.Helper()
	switch step.Kind {
	case "manifest-field", "manifest-drop-modified", "manifest-drop-renamed", "manifest-digest-match":
		editManifest(t, forkCopy, step)
	case "patch-append":
		appendFile(t, filepath.Join(forkCopy, "connection-lifetime.patch"), step.Text)
	case "file-remove":
		if err := os.Remove(filepath.Join(forkCopy, filepath.FromSlash(step.Path))); err != nil {
			t.Fatalf("remove %s from the fork copy: %v", step.Path, err)
		}
	case "file-add":
		target := filepath.Join(forkCopy, filepath.FromSlash(step.Path))
		if _, err := os.Stat(target); err == nil {
			t.Fatalf("case adds %s, which the fork already holds; add a file outside the audited set", step.Path)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("create the parent of %s in the fork copy: %v", step.Path, err)
		}
		if err := os.WriteFile(target, []byte(step.Text), 0o644); err != nil {
			t.Fatalf("add %s to the fork copy: %v", step.Path, err)
		}
	case "file-append":
		appendFile(t, filepath.Join(forkCopy, filepath.FromSlash(step.Path)), step.Text)
	case "repo-import":
		content := "package probe\n\nimport _ \"" + step.Import + "\"\n"
		if err := os.WriteFile(filepath.Join(repoDir, filepath.FromSlash(step.Path)), []byte(content), 0o644); err != nil {
			t.Fatalf("write the %s import probe: %v", step.Path, err)
		}
	}
}

// editManifest applies a manifest fixture step to the copy's UPSTREAM.json.
func editManifest(t *testing.T, forkCopy string, step mutationStep) {
	t.Helper()
	manifestPath := filepath.Join(forkCopy, "UPSTREAM.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read the fork copy manifest: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode the fork copy manifest: %v", err)
	}
	nested := func(field string) map[string]any {
		t.Helper()
		entries, ok := manifest[field].(map[string]any)
		if !ok {
			t.Fatalf("fork copy manifest has no %s object; the fixture assumes the checked-in shape", field)
		}
		return entries
	}
	switch step.Kind {
	case "manifest-field":
		manifest[step.Field] = step.Value
	case "manifest-drop-modified":
		entries := nested("modified_files")
		if _, ok := entries[step.Key]; !ok {
			t.Fatalf("fixture drops modified_files[%s], which the manifest does not hold", step.Key)
		}
		delete(entries, step.Key)
	case "manifest-drop-renamed":
		entries := nested("renamed_files")
		if _, ok := entries[step.Key]; !ok {
			t.Fatalf("fixture drops renamed_files[%s], which the manifest does not hold", step.Key)
		}
		delete(entries, step.Key)
	case "manifest-digest-match":
		content, err := os.ReadFile(filepath.Join(forkCopy, filepath.FromSlash(step.Path)))
		if err != nil {
			t.Fatalf("read %s from the fork copy for its digest: %v", step.Path, err)
		}
		sum := sha256.Sum256(content)
		nested("files")[step.Path] = hex.EncodeToString(sum[:])
	}
	rewritten, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("encode the mutated fork copy manifest: %v", err)
	}
	if err := os.WriteFile(manifestPath, append(rewritten, '\n'), 0o644); err != nil {
		t.Fatalf("write the mutated fork copy manifest: %v", err)
	}
}

// appendFile appends text to an existing file in the fork copy.
func appendFile(t *testing.T, path, text string) {
	t.Helper()
	handle, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s in the fork copy for appending: %v", path, err)
	}
	defer handle.Close()
	if _, err := handle.WriteString(text); err != nil {
		t.Fatalf("append to %s in the fork copy: %v", path, err)
	}
}
