package harnesslayout

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/testutil"
)

// loadAntigravityFixture decodes testdata/<name>.yaml strictly into target and
// requires its case names to equal testdata/<name>.manifest.yaml exactly.
func loadAntigravityFixture(t *testing.T, name string, target any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := testutil.DecodeFixtureYAML(data, target); err != nil {
		t.Fatalf("decode %s.yaml: %v", name, err)
	}
	manifestData, err := os.ReadFile(filepath.Join("testdata", name+".manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest testutil.RequiredNamesManifest
	if err := testutil.DecodeFixtureYAML(manifestData, &manifest); err != nil {
		t.Fatalf("decode %s.manifest.yaml: %v", name, err)
	}
	var inventory struct {
		Cases []struct {
			Name string `yaml:"name"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(inventory.Cases))
	for _, c := range inventory.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, name); err != nil {
		t.Fatal(err)
	}
}

type antigravityFixtureFile struct {
	Path    string `yaml:"path"`
	Content string `yaml:"content"`
}

type antigravityFixtureProduct struct {
	RootFiles    []antigravityFixtureFile `yaml:"rootFiles"`
	SessionFiles []antigravityFixtureFile `yaml:"sessionFiles"`
	Credentials  []string                 `yaml:"credentials"`
}

// antigravityStore is the synthetic store of testdata/antigravity_store.yaml.
type antigravityStore struct {
	Sentinel string `yaml:"sentinel"`
	Body     struct {
		Schema        []string `yaml:"schema"`
		Rows          []string `yaml:"rows"`
		BlobExtension []string `yaml:"blobExtension"`
	} `yaml:"body"`
	Index struct {
		Schema []string `yaml:"schema"`
		Row    []string `yaml:"row"`
	} `yaml:"index"`
	WorkspacePath   string                             `yaml:"workspacePath"`
	SessionFiles    []antigravityFixtureFile           `yaml:"sessionFiles"`
	SharedRootFiles []antigravityFixtureFile           `yaml:"sharedRootFiles"`
	Products        map[Tool]antigravityFixtureProduct `yaml:"products"`
}

func loadAntigravityStore(t *testing.T) antigravityStore {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "antigravity_store.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var store antigravityStore
	if err := testutil.DecodeFixtureYAML(data, &store); err != nil {
		t.Fatalf("decode antigravity_store.yaml: %v", err)
	}
	if store.Sentinel == "" {
		t.Fatal("antigravity_store.yaml names no sentinel")
	}
	for _, tool := range []Tool{ToolAntigravityCLI, ToolAntigravityIDE} {
		if _, ok := store.Products[tool]; !ok {
			t.Fatalf("antigravity_store.yaml has no product %q", tool)
		}
	}
	return store
}

func antigravityWithID(text, id string) string {
	return strings.ReplaceAll(text, antigravityIDToken, id)
}

// writeBody builds conversations/<id>.db. With extension the body also has a
// BLOB column that no denial names.
func (s antigravityStore) writeBody(t *testing.T, file string, extension bool) {
	t.Helper()
	statements := append(append([]string{}, s.Body.Schema...), s.Body.Rows...)
	if extension {
		statements = append(statements, s.Body.BlobExtension...)
	}
	writeSQLiteFixture(t, file, statements)
}

// writeIndex builds conversation_summaries.db with one row per id.
func (s antigravityStore) writeIndex(t *testing.T, file string, ids []string) {
	t.Helper()
	statements := append([]string{}, s.Index.Schema...)
	for _, id := range ids {
		for _, row := range s.Index.Row {
			statements = append(statements, antigravityWithID(row, id))
		}
	}
	writeSQLiteFixture(t, file, statements)
}

func writeAntigravityFile(t *testing.T, root string, file antigravityFixtureFile, id string) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(antigravityWithID(file.Path, id)))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(antigravityWithID(file.Content, id)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// antigravityRootState is how a scenario root exists on disk.
type antigravityRootState string

const (
	antigravityRootStore   antigravityRootState = "store"
	antigravityRootMissing antigravityRootState = "missing"
	antigravityRootEmpty   antigravityRootState = "empty"
)

// antigravityScenarioRoot is one store root of a case in antigravity_cases.yaml.
type antigravityScenarioRoot struct {
	Tool         Tool                 `yaml:"tool"`
	State        antigravityRootState `yaml:"state"`
	Bodies       []string             `yaml:"bodies"`
	BrainOnly    []string             `yaml:"brainOnly"`
	Indexed      []string             `yaml:"indexed"`
	ForeignFiles bool                 `yaml:"foreignFiles"`
	WALFiles     bool                 `yaml:"walFiles"`
}

// build writes the root under parent and returns its path.
func (s antigravityStore) build(t *testing.T, parent string, root antigravityScenarioRoot) string {
	t.Helper()
	dir := filepath.Join(parent, string(root.Tool))
	switch root.State {
	case antigravityRootMissing:
		return filepath.Join(parent, "missing")
	case antigravityRootEmpty:
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	case "", antigravityRootStore:
	default:
		t.Fatalf("unknown root state %q; use store, missing, or empty", root.State)
	}
	own, ok := s.Products[root.Tool]
	if !ok {
		t.Fatalf("root tool %q is not an Antigravity product of antigravity_store.yaml", root.Tool)
	}
	products := []antigravityFixtureProduct{own}
	if root.ForeignFiles {
		for tool, product := range s.Products {
			if tool != root.Tool {
				products = append(products, product)
			}
		}
	}
	for _, id := range root.Bodies {
		s.writeBody(t, filepath.Join(dir, "conversations", id+".db"), false)
		for _, file := range s.SessionFiles {
			writeAntigravityFile(t, dir, file, id)
		}
		for _, product := range products {
			for _, file := range product.SessionFiles {
				writeAntigravityFile(t, dir, file, id)
			}
		}
		if root.WALFiles {
			for _, suffix := range []string{".db-wal", ".db-shm"} {
				writeAntigravityFile(t, dir, antigravityFixtureFile{Path: "conversations/" + id + suffix, Content: s.Sentinel + "-wal"}, id)
			}
		}
	}
	if root.WALFiles {
		writeAntigravityFile(t, dir, antigravityFixtureFile{Path: "conversations/orphan.db-wal", Content: s.Sentinel + "-orphan"}, "")
	}
	for _, id := range root.BrainOnly {
		for _, file := range s.SessionFiles {
			if strings.HasPrefix(file.Path, "brain/") {
				writeAntigravityFile(t, dir, file, id)
			}
		}
	}
	for _, file := range s.SharedRootFiles {
		writeAntigravityFile(t, dir, file, "")
	}
	for _, product := range products {
		for _, file := range product.RootFiles {
			writeAntigravityFile(t, dir, file, "")
		}
	}
	s.writeIndex(t, filepath.Join(dir, antigravityIndexDB.Pattern), root.Indexed)
	for _, name := range own.Credentials {
		writeAntigravityCredential(t, dir, name, s.Sentinel)
	}
	return dir
}

// writeAntigravityCredential writes a credential file with sentinel content
// and mode 0000. The mode is restored before the temporary directory is
// removed.
func writeAntigravityCredential(t *testing.T, dir, name, sentinel string) {
	t.Helper()
	target := filepath.Join(dir, name)
	if err := os.WriteFile(target, []byte(sentinel+"-credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(target, 0o600); err != nil {
			t.Errorf("restore mode of %s: %v", name, err)
		}
	})
}

// antigravityOpenRecorder records every path opened through the fs.FS that a
// probe receives. Stat and ReadDir pass through without an open, as they do
// on os.DirFS. SQLite opens go through Source.Dir and are not recorded; the
// read guard covers them.
type antigravityOpenRecorder struct {
	fsys   fs.FS
	mu     sync.Mutex
	opened []string
}

func (r *antigravityOpenRecorder) Open(name string) (fs.File, error) {
	r.mu.Lock()
	r.opened = append(r.opened, name)
	r.mu.Unlock()
	return r.fsys.Open(name)
}

func (r *antigravityOpenRecorder) Stat(name string) (fs.FileInfo, error) {
	return fs.Stat(r.fsys, name)
}

func (r *antigravityOpenRecorder) ReadDir(name string) ([]fs.DirEntry, error) {
	return fs.ReadDir(r.fsys, name)
}

// openedMatching returns the recorded opens that match pattern.
func (r *antigravityOpenRecorder) openedMatching(pattern string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, name := range r.opened {
		if ok, _ := path.Match(pattern, name); ok {
			out = append(out, name)
		}
	}
	return out
}
