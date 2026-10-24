package pull_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/install-safety.yaml
var installSafetyYAML []byte

type installSafetyCase struct {
	Name, Kind, Target string
	Force              bool `yaml:"force"`
}

func loadInstallSafetyFixtures(t *testing.T) []installSafetyCase {
	t.Helper()
	var corpus struct {
		RequiredNames []string            `yaml:"requiredNames"`
		Cases         []installSafetyCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(installSafetyYAML, &corpus); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range corpus.Cases {
		if c.Target == "" || filepath.Base(c.Target) != c.Target || !filepath.IsLocal(c.Target) {
			t.Fatalf("fixture target must be a bounded name: %q", c.Target)
		}
		if c.Name == "" || seen[c.Name] {
			t.Fatalf("empty/duplicate safety fixture: %q", c.Name)
		}
		seen[c.Name] = true
	}
	for _, name := range corpus.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing required safety fixture: %s", name)
		}
	}
	return corpus.Cases
}

func newSafetySandbox(t *testing.T) (string, *store.Store, ingest.FileSystem) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "index.db")
	storetest.CopyGoldenTo(t, path)
	db, err := store.Open(path, store.WithSkipMigrations())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return filepath.Join(root, "pulls"), db, pull.NewFileSystem(filepath.Join(root, "pulls"))
}

func replacementReader() *testutil.StubVillageReader {
	r := happyReader()
	r.Meta.Title = "Validated replacement"
	r.ContentBody = []byte(`{"replacement":true}`)
	r.Meta.ContentHash = schema.ComputeTranscriptHash(r.ContentBody)
	r.ContentETag = quotedETag(r.Meta.ContentHash)
	return r
}

func assertInstalledContent(t *testing.T, db *store.Store, final string, reader *testutil.StubVillageReader) {
	t.Helper()
	bytes, err := os.ReadFile(filepath.Join(final, pull.TranscriptFilename))
	if err != nil || string(bytes) != string(reader.ContentBody) {
		t.Fatalf("installed blob differs: %q %v", bytes, err)
	}
	data, err := os.ReadFile(filepath.Join(final, pull.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	var manifest pull.PullManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.TranscriptID != testutil.TestTranscriptID ||
		manifest.ServedBlobHash != schema.ComputeTranscriptHash(bytes) ||
		manifest.OwnerUserID != reader.Meta.OwnerUserID || manifest.LocalSessionID != reader.Meta.LocalID {
		t.Fatalf("manifest identity/hash differs: %+v", manifest)
	}
	if manifest.VillageURL != testVillageURL || manifest.VillageHost != testutil.TestVillageHost ||
		manifest.ManifestVersion != pull.PullManifestVersion || manifest.BlobContractVersion != reader.Meta.ContractVersion ||
		manifest.PulledAt != testutil.NewFixedClock().NowUnixMilli() {
		t.Fatalf("manifest provenance/version/time differs: %+v", manifest)
	}
	expectedProvenance := make([]pull.ManifestAnnotation, 0, len(reader.Annotations))
	expectedHashes := map[string]bool{}
	for _, a := range reader.Annotations {
		if a.ContentHash == nil {
			t.Fatal("fixture must provide annotation hash")
		}
		expectedHashes[*a.ContentHash] = true
		expectedProvenance = append(expectedProvenance, pull.ManifestAnnotation{ContentHash: *a.ContentHash, AuthorUserID: a.AuthorUserID, AuthorUsername: a.AuthorUsername})
	}
	if !reflect.DeepEqual(manifest.Annotations, expectedProvenance) {
		t.Fatalf("annotation provenance differs: %+v", manifest.Annotations)
	}
	row, err := db.GetPulledTranscript(context.Background(), testutil.TestVillageHost, testutil.TestTranscriptID)
	if err != nil || row == nil {
		t.Fatalf("missing index: %+v %v", row, err)
	}
	if row.ContentHash != manifest.ServedBlobHash || row.OwnerUserID != manifest.OwnerUserID ||
		row.LocalSessionID != manifest.LocalSessionID || row.Title != reader.Meta.Title ||
		row.AnnotationCount != len(expectedProvenance) {
		t.Fatalf("index differs from files/provenance: %+v", row)
	}
	canonical := final
	if filepath.Base(final) == "previous" {
		canonical = filepath.Join(filepath.Dir(filepath.Dir(final)), manifest.TranscriptID.String())
	}
	if row.PullDir != canonical {
		t.Fatalf("index references wrong content path: %q != %q", row.PullDir, canonical)
	}
	hashes, err := db.ListPulledAnnotationHashesByTranscript(context.Background(), testutil.TestVillageHost)
	if err != nil {
		t.Fatal(err)
	}
	actual := map[string]bool{}
	for _, hash := range hashes[testutil.TestTranscriptID] {
		actual[hash] = true
	}
	if !reflect.DeepEqual(actual, expectedHashes) {
		t.Fatalf("annotation index differs: %v != %v", actual, expectedHashes)
	}
	projection := readAnnotationProjection(t, filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(canonical))), "index.db"))
	for _, a := range reader.Annotations {
		payload, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		actual, ok := projection[manifest.VillageHost+"|"+*a.ContentHash]
		expected := store.PulledAnnotationRow{VillageHost: manifest.VillageHost, ContentHash: *a.ContentHash, TranscriptID: manifest.TranscriptID, LocalSessionID: manifest.LocalSessionID, AuthorUserID: a.AuthorUserID, AuthorUsername: a.AuthorUsername, Payload: string(payload), PulledAt: manifest.PulledAt}
		if !ok || !reflect.DeepEqual(actual, expected) {
			t.Fatalf("stored annotation payload/authorship differs for %s: actual=%+v expected=%+v", *a.ContentHash, actual, expected)
		}
	}
}

// Read the real persisted projection without extending production Store APIs
// merely for a test. The caller supplies only its golden-copy sandbox database.
func readAnnotationProjection(t *testing.T, path string) map[string]store.PulledAnnotationRow {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rows := map[string]store.PulledAnnotationRow{}
	err = sqlitex.Execute(conn, `SELECT village_host,content_hash,transcript_id,COALESCE(local_session_id,''),author_user_id,author_username,payload,pulled_at,retracted_at FROM pulled_annotations ORDER BY village_host,content_hash`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		id, err := schema.NewTranscriptID(stmt.ColumnText(2))
		if err != nil {
			return err
		}
		row := store.PulledAnnotationRow{VillageHost: stmt.ColumnText(0), ContentHash: stmt.ColumnText(1), TranscriptID: id, LocalSessionID: stmt.ColumnText(3), AuthorUserID: stmt.ColumnText(4), AuthorUsername: stmt.ColumnText(5), Payload: stmt.ColumnText(6), PulledAt: stmt.ColumnInt64(7)}
		if stmt.ColumnType(8) != sqlite.TypeNull {
			value := stmt.ColumnInt64(8)
			row.RetractedAt = &value
		}
		rows[row.VillageHost+"|"+row.ContentHash] = row
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Snapshot no-follow file/link evidence, not targets outside the test namespace.
func snapshotInstallTree(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			result[rel] = "link:" + target
			return err
		}
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			result[rel] = string(data)
			return err
		}
		result[rel] = "directory"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestContentInstallSafety(t *testing.T) {
	for _, c := range loadInstallSafetyFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			pulls, db, filesystem := newSafetySandbox(t)
			ctx := context.Background()
			ref := newRef(t)
			run := func(reader *testutil.StubVillageReader, fs ingest.FileSystem, persistence pull.PullStore, force bool) error {
				_, err := pull.NewPipeline(reader, fs, persistence, testutil.NewFixedClock(), testCreds(), pulls).PullTranscript(ctx, ref, pull.PullOptions{Force: force})
				return err
			}
			if c.Kind == "ancestor" {
				final := filepath.Join(pulls, testutil.TestVillageHost, ref.ID.String())
				fault := &installFaultFS{FileSystem: filesystem, final: final, fixture: installCase{Operation: "sync", Target: "ancestor"}}
				audit := &installAuditStore{PullStore: db}
				_, err := pull.NewPipeline(happyReader(), fault, audit, testutil.NewFixedClock(), testCreds(), pulls, pull.WithInstallSync(fault.sync)).PullTranscript(ctx, ref, pull.PullOptions{})
				if !errors.Is(err, errInstallInjected) || !fault.fired || audit.commits != 0 {
					t.Fatalf("first-pull ancestor barrier did not guard index: %v", err)
				}
				if _, err := os.Lstat(final); !os.IsNotExist(err) {
					t.Fatalf("ancestor failure published content: %v", err)
				}
				if err := run(happyReader(), filesystem, db, false); err != nil {
					t.Fatal(err)
				}
				assertInstalledContent(t, db, final, happyReader())
				return
			}
			if err := run(happyReader(), filesystem, db, false); err != nil {
				t.Fatal(err)
			}
			final := filepath.Join(pulls, testutil.TestVillageHost, ref.ID.String())
			root := filepath.Join(filepath.Dir(final), ".install-"+ref.ID.String())
			if c.Kind == "force" {
				if err := os.WriteFile(filepath.Join(final, c.Target), []byte("damaged"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := run(replacementReader(), filesystem, db, false); err == nil {
					t.Fatal("normal replacement of damaged previous files must refuse")
				}
				checkedPrevious := false
				audit := &installAuditStore{PullStore: db, before: func() {
					if checkedPrevious {
						return
					}
					checkedPrevious = true
					previous, err := os.ReadFile(filepath.Join(root, "previous", c.Target))
					if err != nil || string(previous) != "damaged" {
						t.Fatalf("force deleted previous bytes before index commit: %q %v", previous, err)
					}
				}}
				if err := run(replacementReader(), filesystem, audit, true); err != nil {
					t.Fatalf("fresh explicit force cannot repair valid pending journal: %v", err)
				}
				if !checkedPrevious {
					t.Fatal("force repair did not commit the verified journal")
				}
				assertInstalledContent(t, db, final, replacementReader())
				entries, err := os.ReadDir(root)
				if err != nil || len(entries) != 0 {
					t.Fatalf("owned resources leaked: %v %v", entries, err)
				}
				return
			}
			if c.Kind == "cleanup" {
				stage := filepath.Join(root, uuid.NewString())
				if err := os.MkdirAll(stage, 0o700); err != nil {
					t.Fatal(err)
				}
				if c.Target != "missing" {
					owner := ref.ID.String()
					if c.Target == "wrong" {
						owner = "not-this-transcript"
					}
					if err := os.WriteFile(filepath.Join(stage, ".pull-stage-owner"), []byte(owner), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := run(happyReader(), filesystem, db, false); err != nil {
					t.Fatal(err)
				}
				_, err := os.Lstat(stage)
				if (c.Target == "correct") != os.IsNotExist(err) {
					t.Fatalf("cleanup did not honor exact ownership: %v", err)
				}
				return
			}
			if c.Kind == "root" {
				external := filepath.Join(t.TempDir(), "retained")
				if err := os.Rename(root, external); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, root); err != nil {
					t.Fatal(err)
				}
				before := snapshotInstallTree(t, external)
				if err := run(happyReader(), filesystem, db, false); err == nil {
					t.Fatal("symlink recovery root accepted")
				}
				if !reflect.DeepEqual(before, snapshotInstallTree(t, external)) {
					t.Fatal("external recovery-root target changed")
				}
				return
			}
			fault := &installFaultFS{FileSystem: filesystem, final: final, fixture: installCase{Operation: "rename", Target: "backup"}}
			if err := run(replacementReader(), fault, db, true); err == nil || !fault.fired {
				t.Fatalf("did not leave pending real installation: %v", err)
			}
			journalPath := filepath.Join(root, "journal.json")
			data, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			var journal map[string]json.RawMessage
			if err := json.Unmarshal(data, &journal); err != nil {
				t.Fatal(err)
			}
			var stageName string
			if err := json.Unmarshal(journal["stage"], &stageName); err != nil {
				t.Fatal(err)
			}
			stage := filepath.Join(root, stageName)
			if c.Kind == "swap" || c.Kind == "rename-swap" {
				target := stage
				skip := 1
				if c.Target != "stage-swap" {
					target = filepath.Join(stage, c.Target)
					skip = 0
				}
				swap := &installSwapFS{FileSystem: filesystem, path: target, skip: skip, rename: c.Kind == "rename-swap"}
				var expected map[string]string
				external := filepath.Join(t.TempDir(), "swapped-object")
				swap.swap = func() {
					if err := os.Rename(target, external); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(external, target); err != nil {
						t.Fatal(err)
					}
					expected = snapshotInstallTree(t, pulls)
				}
				oldRow, err := db.GetPulledTranscript(ctx, testutil.TestVillageHost, ref.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := run(replacementReader(), swap, db, false); err == nil || !swap.fired {
					t.Fatalf("uncoordinated path swap was not refused: %v", err)
				}
				if !reflect.DeepEqual(expected, snapshotInstallTree(t, pulls)) {
					t.Fatal("path swap caused additional publication/deletion")
				}
				previous := final
				if c.Kind == "rename-swap" {
					previous = filepath.Join(root, "previous")
				}
				assertInstalledContent(t, db, previous, happyReader())
				row, err := db.GetPulledTranscript(ctx, testutil.TestVillageHost, ref.ID)
				if err != nil || !reflect.DeepEqual(row, oldRow) {
					t.Fatalf("path swap mutated index: %v", err)
				}
				return
			}
			switch c.Target {
			case "version":
				journal["version"] = json.RawMessage(`99`)
			case "corrupt":
				data = []byte("{broken")
			case "digest":
				journal["digest"] = json.RawMessage(`"wrong-digest"`)
			case "identity", "projection", "annotation-projection":
				var commit store.PullCommit
				if err := json.Unmarshal(journal["commit"], &commit); err != nil {
					t.Fatal(err)
				}
				if c.Target == "identity" {
					commit.Transcript.VillageHost = "wrong-host"
				} else if c.Target == "annotation-projection" {
					commit.Annotations[0].AuthorUsername = "not-the-payload-author"
				} else {
					commit.Transcript.Title = "not-the-files"
				}
				journal["commit"], err = json.Marshal(commit)
				if err != nil {
					t.Fatal(err)
				}
			case "blob-corrupt":
				if err := os.WriteFile(filepath.Join(stage, pull.TranscriptFilename), []byte("not-the-served-bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "manifest-identity":
				path := filepath.Join(stage, pull.ManifestFilename)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var manifest pull.PullManifest
				if err := json.Unmarshal(data, &manifest); err != nil {
					t.Fatal(err)
				}
				manifest.VillageHost = "wrong-host"
				data, err = json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "metadata-identity":
				path := filepath.Join(stage, pull.MetadataFilename)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var meta schema.PullTranscriptInfo
				if err := json.Unmarshal(data, &meta); err != nil {
					t.Fatal(err)
				}
				meta.OwnerUserID = "wrong-owner"
				data, err = json.Marshal(meta)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "owner-wrong":
				if err := os.WriteFile(filepath.Join(stage, ".pull-stage-owner"), []byte("other"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "owner-missing":
				if err := os.Remove(filepath.Join(stage, ".pull-stage-owner")); err != nil {
					t.Fatal(err)
				}
			case "stage-link":
				external := filepath.Join(t.TempDir(), "external-stage")
				if err := os.Rename(stage, external); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, stage); err != nil {
					t.Fatal(err)
				}
			default:
				file := filepath.Join(stage, c.Target)
				bytes, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				external := filepath.Join(t.TempDir(), "external-component")
				if err := os.WriteFile(external, bytes, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, file); err != nil {
					t.Fatal(err)
				}
			}
			if c.Target != "corrupt" {
				data, err = json.Marshal(journal)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(journalPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			before := snapshotInstallTree(t, pulls)
			beforeAnnotations := readAnnotationProjection(t, filepath.Join(filepath.Dir(pulls), "index.db"))
			oldRow, err := db.GetPulledTranscript(ctx, testutil.TestVillageHost, ref.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := run(replacementReader(), filesystem, db, c.Force); err == nil {
				t.Fatalf("unsafe pending installation accepted: %s", c.Target)
			}
			if !reflect.DeepEqual(before, snapshotInstallTree(t, pulls)) {
				t.Fatal("refused pending installation changed/deleted files")
			}
			if !reflect.DeepEqual(beforeAnnotations, readAnnotationProjection(t, filepath.Join(filepath.Dir(pulls), "index.db"))) {
				t.Fatal("refused journal mutated annotation projection")
			}
			row, err := db.GetPulledTranscript(ctx, testutil.TestVillageHost, ref.ID)
			if err != nil || !reflect.DeepEqual(oldRow, row) {
				t.Fatalf("refusal mutated index: %v %+v", err, row)
			}
		})
	}
}

// Swap the actual namespace between a validation and its next bounded operation.
// This is a filesystem adversary, not a replacement implementation of Pipeline.
type installSwapFS struct {
	ingest.FileSystem
	path          string
	skip          int
	rename, fired bool
	swap          func()
}

var _ ingest.FileSystem = (*installSwapFS)(nil)

func (f *installSwapFS) Lstat(path string) (os.FileInfo, error) {
	info, err := f.FileSystem.Lstat(path)
	if err == nil && !f.rename &&
		path == f.path && !f.fired {
		if f.skip > 0 {
			f.skip--
		} else {
			f.fired = true
			f.swap()
		}
	}
	return info, err
}
func (f *installSwapFS) Rename(src, dst string) error {
	if f.rename && src == f.path && !f.fired {
		f.fired = true
		f.swap()
	}
	return f.FileSystem.Rename(src, dst)
}
