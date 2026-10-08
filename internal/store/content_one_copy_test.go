package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

type contentOneCopyCase struct {
	Name       string            `yaml:"name"`
	Mode       string            `yaml:"mode"`
	Texts      []string          `yaml:"texts"`
	NextTexts  []string          `yaml:"nextTexts"`
	AppendText string            `yaml:"appendText"`
	ExtraBlobs map[string]string `yaml:"extraBlobs"`
	Bodies     int               `yaml:"bodies"`
	Blobs      int               `yaml:"blobs"`
}

func loadContentOneCopyFixture(t *testing.T) []contentOneCopyCase {
	t.Helper()
	var fixture struct {
		Cases []contentOneCopyCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(contentOneCopyYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	manifest, err := decodeRecoveryRequiredNames(contentOneCopyManifestYAML)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		if c.Mode == "" || len(c.Texts) == 0 || c.Bodies < 1 {
			t.Fatalf("incomplete one-copy fixture %+v", c)
		}
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, names, "content one copy"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

func ownedContentTree(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == "locks" {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, rel)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return paths
}

func contentBodyIDs(t *testing.T, s *Store, sid schema.SessionID) map[string]int64 {
	t.Helper()
	conn, err := s.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	ids := map[string]int64{}
	if err := sqlitex.Execute(conn, `SELECT body_digest,body_id FROM session_entry_bodies WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error {
		ids[stmt.ColumnText(0)] = stmt.ColumnInt64(1)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestContentOneCopy(t *testing.T) {
	for _, c := range loadContentOneCopyFixture(t) {
		t.Run(c.Name, func(t *testing.T) {
			s, root := openGenerationStore(t)
			sid := gcSession(t, s, "a2a2a2a2-a2a2-42a2-82a2-a2a2a2a2a2a2")
			before := ownedContentTree(t, root)
			first, blobs := gcBuildCandidate(t, sid, "copy_first", c.Texts, c.ExtraBlobs)
			if err := activateTestGeneration(t, s, first, blobs); err != nil {
				t.Fatal(err)
			}
			original := contentBodyIDs(t, s, sid)
			active := first
			switch c.Mode {
			case "activate", "columns", "collision":
			case "refresh", "sweep":
				active, blobs = gcBuildCandidate(t, sid, "copy_next", c.NextTexts, nil)
				if err := activateTestGeneration(t, s, active, blobs); err != nil {
					t.Fatal(err)
				}
			case "append":
				if c.AppendText == "" {
					t.Fatal("append fixture has no appended content")
				}
				active.Generation.ID = "copy_append"
				text := c.AppendText
				index := len(active.Generation.Main.Entries)
				entry := active.Generation.Main.Entries[0]
				entry.EntryIndex, entry.SourceEntryRef, entry.ContentPreview = index, schema.SourceEntryRef(fmt.Sprintf("e_append%d", index)), &text
				active.Generation.Main.Entries = append(active.Generation.Main.Entries, entry)
				active.Generation.Content = append(active.Generation.Content, indexformat.ContentRecord{Ref: entry.SourceEntryRef})
				blobs[entry.SourceEntryRef] = []byte(text)
				active = filledCandidateForValidation(t, active, blobs)
				if err := activateTestGeneration(t, s, active, blobs); err != nil {
					t.Fatal(err)
				}
				current := contentBodyIDs(t, s, sid)
				for digest, id := range original {
					if current[digest] != id {
						t.Fatalf("append replaced unchanged body %s: %d -> %d", digest, id, current[digest])
					}
				}
			default:
				t.Fatalf("unknown one-copy mode %q", c.Mode)
			}
			if c.Mode == "sweep" {
				if _, err := s.SweepSession(t.Context(), sid); err != nil {
					t.Fatal(err)
				}
				for _, table := range reclaimTableNames {
					if countGenerationRows(t, s, table, sid, "copy_first") != 0 {
						t.Fatalf("superseded rows remain in %s", table)
					}
				}
				assertGCDigestsEqual(t, s, sid, c.Name, gcBodyDigests(t, sid, active, blobs))
			}
			if after := ownedContentTree(t, root); !reflect.DeepEqual(before, after) {
				t.Fatalf("activation changed owned tree: before %v; after %v", before, after)
			}
			if got := len(contentBodyIDs(t, s, sid)); got != c.Bodies {
				t.Fatalf("body rows = %d; want %d", got, c.Bodies)
			}
			conn, err := s.pool.Take(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer s.pool.Put(conn)
			actual := map[string]int64{}
			if err := sqlitex.Execute(conn, `SELECT digest,byte_length FROM session_content WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error {
				actual[stmt.ColumnText(0)] = stmt.ColumnInt64(1)
				return nil
			}}); err != nil {
				t.Fatal(err)
			}
			expected := map[string]int64{}
			for _, text := range c.ExtraBlobs {
				digest := sha256.Sum256([]byte(text))
				expected[hex.EncodeToString(digest[:])] = int64(len(text))
			}
			if len(actual) != c.Blobs || !reflect.DeepEqual(actual, expected) {
				t.Fatalf("blob set = %v; want %v (%d rows)", actual, expected, c.Blobs)
			}
			if c.Mode == "columns" || c.Mode == "collision" {
				var texts []string
				if err := sqlitex.Execute(conn, `SELECT content_preview,tool_input,tool_output FROM session_entry_bodies WHERE session_id=? ORDER BY body_id`, &sqlitex.ExecOptions{Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error {
					texts = append(texts, stmt.ColumnText(0))
					if stmt.ColumnType(1) != sqlite.TypeNull || stmt.ColumnType(2) != sqlite.TypeNull {
						return fmt.Errorf("text body duplicates emitted content in tool columns")
					}
					return nil
				}}); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(texts, c.Texts) {
					t.Fatalf("stored content = %v; want %v", texts, c.Texts)
				}
			}
		})
	}
}
