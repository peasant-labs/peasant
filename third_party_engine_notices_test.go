package peasant

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEngineNoticeFilesMatchThePinnedBuildRecipe keeps the committed licence
// texts tied to the components the pinned engine module actually compiles into
// its wasm blob. The module records those tags in its own build recipe, so a
// version bump that moves a tag without refreshing the texts fails here instead
// of shipping a notice for a component the binary no longer contains.
func TestEngineNoticeFilesMatchThePinnedBuildRecipe(t *testing.T) {
	t.Parallel()

	moduleDir := engineModuleDir(t)
	re2Tag := strings.TrimSpace(readEngineFile(t, filepath.Join(moduleDir, "buildtools", "wasm", "version.txt")))
	if re2Tag == "" {
		t.Fatal("the engine's build recipe names no RE2 tag; the notice texts cannot be verified")
	}
	abseilTag := dockerTag(t, readEngineFile(t, filepath.Join(moduleDir, "buildtools", "wasm", "Dockerfile")), "abseil-cpp")

	want := map[string]string{"RE2": re2Tag, "ABSEIL": abseilTag}
	entries, err := os.ReadDir(filepath.Join("third_party", "go-re2-engine"))
	if err != nil {
		t.Fatalf("read the engine notice directory: %v", err)
	}
	seen := make(map[string]bool, len(want))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, "-LICENSE") {
			continue
		}
		stem := strings.TrimSuffix(name, "-LICENSE")
		component, version, ok := strings.Cut(stem, "-")
		if !ok {
			t.Fatalf("third_party/go-re2-engine/%s does not name a component and a version", name)
		}
		pinned, tracked := want[component]
		if !tracked {
			t.Fatalf("third_party/go-re2-engine/%s names component %q, which the engine's build recipe does not compile in", name, component)
		}
		if version != pinned {
			t.Fatalf("third_party/go-re2-engine/%s is pinned to %s, but the engine's build recipe pins %s %s; refresh the licence text from the tag the recipe names", name, version, component, pinned)
		}
		seen[component] = true
	}
	for component, version := range want {
		if !seen[component] {
			t.Fatalf("the engine's build recipe compiles in %s %s, but third_party/go-re2-engine has no %s-%s-LICENSE; reproduce its notice", component, version, component, version)
		}
	}
}

func engineModuleDir(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/wasilibs/go-re2").Output()
	if err != nil {
		t.Fatalf("resolve the pinned engine module: %v; the notice provenance cannot be verified", err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		// A vendored, pruned build carries only the module's Go files, so the
		// build recipe this check reads is not in the tree. The check runs where
		// the module source is available (the dev shell and CI), which is where a
		// version bump is reviewed.
		t.Skip("the pinned module's source is not in this build; the notices are verified in the dev shell and CI")
	}
	return dir
}

func readEngineFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v; the engine's build recipe moved, so re-read it and refresh the notice texts", path, err)
	}
	return string(data)
}

// dockerTag returns the tag a Dockerfile fetches a component from, e.g.
// .../abseil-cpp/archive/refs/tags/20240722.0.tar.gz. It parses by hand because
// this package's own guard forbids the stdlib regexp engine.
func dockerTag(t *testing.T, dockerfile, component string) string {
	t.Helper()
	marker := component + "/archive/refs/tags/"
	for _, line := range strings.Split(dockerfile, "\n") {
		_, rest, found := strings.Cut(line, marker)
		if !found {
			continue
		}
		tag, _, found := strings.Cut(rest, ".tar.gz")
		if found && tag != "" {
			return tag
		}
	}
	t.Fatalf("the engine's build recipe fetches no %s tag; re-read buildtools/wasm/Dockerfile and refresh the notice texts", component)
	return ""
}
