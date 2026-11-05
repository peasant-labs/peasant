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
	// Match selects how Want is compared: "exact" (the default) requires the
	// audit error to equal Want byte for byte; "contains" requires only a
	// substring match.
	Match string `yaml:"match"`
	// Because states why a case cannot compare exactly (the variable part, a
	// temporary path or operating-system error text) or, for a case that
	// mutates the tree yet expects success, why the documented change passes.
	// Required whenever Match is "contains" and whenever Want is empty but
	// Steps is not.
	Because string `yaml:"because"`
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
		switch c.Match {
		case "", "exact":
		case "contains":
			if c.Because == "" {
				t.Fatalf("case %q matches by substring but states no because; name the variable part that makes an exact comparison impossible", c.Name)
			}
			if c.Want == "" {
				t.Fatalf("case %q matches by substring but expects success; substring matching is only for failure text", c.Name)
			}
		default:
			t.Fatalf("case %q names unknown match %q; use \"exact\" or \"contains\"", c.Name, c.Match)
		}
		if c.Want == "" && len(c.Steps) > 0 && c.Because == "" {
			t.Fatalf("case %q mutates the tree but expects success; state in because why the documented change passes", c.Name)
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
	case "manifest-digest-match", "manifest-modified-match", "file-remove", "file-unreadable":
		need("path", step.Path != "")
	case "manifest-set-modified":
		need("key and value", step.Key != "" && step.Value != "")
	case "patch-append":
		need("text", step.Text != "")
	case "file-add", "file-append":
		need("path and text", step.Path != "" && step.Text != "")
	case "repo-import":
		need("path and import", step.Path != "" && step.Import != "")
	case "repo-symlink":
		need("path and value", step.Path != "" && step.Value != "")
	case "fork-delete":
	default:
		t.Fatalf("case %q names unknown mutation kind %q; use one the audit test applies", caseName, step.Kind)
	}
}

// TestAuditCleanTree runs the audit over the real fork and the real module.
// It proves the checked-in tree passes, including the owned-import boundary.
// This audit is meaningful only under -count=1 (the repository gate passes
// it): a bare `go test` may report a cached pass, which is not evidence.
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
				t.Fatalf("audit passed a %q mutation; want failure %q", c.Name, c.Want)
			}
			if c.Match == "contains" {
				// Substring only: the case's because names the variable
				// part (a temporary path, operating-system error text)
				// that makes a full-string comparison impossible.
				if !strings.Contains(err.Error(), c.Want) {
					t.Fatalf("audit error = %q, want substring %q", err.Error(), c.Want)
				}
				return
			}
			// Exact by default: Want holds the full failure text, so a
			// weakened check that reports a different string fails here.
			if err.Error() != c.Want {
				t.Fatalf("audit error = %q, want exact %q", err.Error(), c.Want)
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
	case "manifest-field", "manifest-drop-modified", "manifest-drop-renamed", "manifest-digest-match", "manifest-modified-match", "manifest-set-modified":
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
		target := filepath.Join(repoDir, filepath.FromSlash(step.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("create the parent of %s in the repository root: %v", step.Path, err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatalf("write the %s import probe: %v", step.Path, err)
		}
	case "repo-symlink":
		link := filepath.Join(repoDir, filepath.FromSlash(step.Path))
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatalf("create the parent of %s in the repository root: %v", step.Path, err)
		}
		if err := os.Symlink(step.Value, link); err != nil {
			t.Fatalf("symlink %s to %s in the repository root: %v", step.Path, step.Value, err)
		}
	case "fork-delete":
		if err := os.RemoveAll(forkCopy); err != nil {
			t.Fatalf("delete the fork copy: %v", err)
		}
	case "file-unreadable":
		// Replace the file with a symlink to a missing target, rather than
		// stripping mode bits: a test run as root reads through chmod 0, so
		// only a dangling link fails the read in every environment.
		target := filepath.Join(forkCopy, filepath.FromSlash(step.Path))
		if err := os.Remove(target); err != nil {
			t.Fatalf("remove %s from the fork copy: %v", step.Path, err)
		}
		if err := os.Symlink("missing-probe-target", target); err != nil {
			t.Fatalf("replace %s with a dangling symlink: %v", step.Path, err)
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
	case "manifest-modified-match":
		// Re-record a modified file's digest after tampering with it, so the
		// digest check passes and a later check must catch the mutation.
		modified := nested("modified_files")
		if _, ok := modified[step.Path]; !ok {
			t.Fatalf("fixture re-records modified_files[%s], which the manifest does not hold", step.Path)
		}
		content, err := os.ReadFile(filepath.Join(forkCopy, filepath.FromSlash(step.Path)))
		if err != nil {
			t.Fatalf("read %s from the fork copy for its digest: %v", step.Path, err)
		}
		sum := sha256.Sum256(content)
		modified[step.Path] = hex.EncodeToString(sum[:])
	case "manifest-set-modified":
		nested("modified_files")[step.Key] = step.Value
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
