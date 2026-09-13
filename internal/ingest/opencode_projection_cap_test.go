package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
)

//go:embed testdata/opencode_projection_cap.yaml
var openCodeProjectionCapData []byte

const capProjectionPath = "/synthetic/store/opencode-managed-projection.json"

// openCodeSQLiteFileHeader is the first bytes of every SQLite database. It is
// how the provider's own file announces itself, and the only thing a reader
// needs in order to refuse it.
const openCodeSQLiteFileHeader = "SQLite format 3\x00"

// projectionOutcome is what a reader owes one file at the managed-projection
// path. There are two answers: read it, or refuse it for being the provider's
// database. Size is not one of them, which is the whole point of this corpus.
type projectionOutcome string

const (
	// projectionRead: the reader proceeds to read the file, whatever its size.
	projectionRead projectionOutcome = "read"
	// projectionRefusedAsDatabase: the file holds the provider's database and
	// is refused without being read into memory.
	projectionRefusedAsDatabase projectionOutcome = "refused-as-provider-database"
)

// openCodeProjectionCapCase sizes one synthetic projection file relative to the
// preview bound and states what the readers owe it.
type openCodeProjectionCapCase struct {
	Name            string            `yaml:"name"`
	Origin          string            `yaml:"origin"`
	OffsetFromBound int64             `yaml:"offset_from_bound"`
	SQLiteHeader    bool              `yaml:"sqlite_header"`
	Outcome         projectionOutcome `yaml:"outcome"`
}

func (c openCodeProjectionCapCase) size() int64 {
	return int64(defaults.OpenCodeManagedProjectionMaxBytes) + c.OffsetFromBound
}

func (c openCodeProjectionCapCase) transcriptOrigin(t *testing.T) ingest.TranscriptOrigin {
	t.Helper()
	switch c.Origin {
	case "opencode-legacy-sqlite":
		return ingest.TranscriptOriginOpenCodeLegacySQLite
	case "opencode-current-sqlite":
		return ingest.TranscriptOriginOpenCodeCurrentSQLite
	default:
		t.Fatalf("cap fixture case %q has an unsupported origin %q", c.Name, c.Origin)
		return ingest.TranscriptOriginFile
	}
}

type openCodeProjectionCapDoc struct {
	RequiredCases []string                    `yaml:"required_cases"`
	Cases         []openCodeProjectionCapCase `yaml:"cases"`
}

func loadOpenCodeProjectionCapDoc(t *testing.T) []openCodeProjectionCapCase {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(openCodeProjectionCapData))
	decoder.KnownFields(true)
	var doc openCodeProjectionCapDoc
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("decode projection cap fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("projection cap fixture must hold exactly one document")
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("projection cap fixture declares no required cases")
	}
	seen := make(map[string]struct{}, len(doc.Cases))
	readsPastTheBound, refuses := false, false
	for _, c := range doc.Cases {
		if c.Name == "" || c.Origin == "" {
			t.Fatalf("projection cap fixture has an incomplete case: %+v", c)
		}
		if _, dup := seen[c.Name]; dup {
			t.Fatalf("projection cap fixture has a duplicate case name %q", c.Name)
		}
		seen[c.Name] = struct{}{}
		switch c.Outcome {
		case projectionRead:
			if c.SQLiteHeader {
				t.Fatalf("case %q holds a database header but expects to be read; a database must never be read into memory", c.Name)
			}
			if c.OffsetFromBound > 0 {
				readsPastTheBound = true
			}
		case projectionRefusedAsDatabase:
			if !c.SQLiteHeader {
				t.Fatalf("case %q expects the provider-database refusal without holding a database header, so it would be refused for some other reason", c.Name)
			}
			refuses = true
		default:
			t.Fatalf("case %q states the unknown outcome %q; a reader either reads the file or refuses it as the provider's database", c.Name, c.Outcome)
		}
	}
	if !readsPastTheBound {
		t.Fatal("no case sizes a projection past the preview bound and requires it to be read; without one, a reinstated size gate would keep this corpus green while long sessions failed")
	}
	if !refuses {
		t.Fatal("no case presents the provider's database; without one, deleting the defence entirely would keep this corpus green")
	}
	for _, name := range doc.RequiredCases {
		if _, ok := seen[name]; !ok {
			t.Fatalf("projection cap fixture is missing required case %q", name)
		}
	}
	return doc.Cases
}

// sizedFileInfo reports a chosen size for one synthetic path, so this corpus
// can present a very large file without writing one.
type sizedFileInfo struct{ size int64 }

func (i sizedFileInfo) Name() string       { return "opencode-managed-projection.json" }
func (i sizedFileInfo) Size() int64        { return i.size }
func (i sizedFileInfo) Mode() os.FileMode  { return 0o600 }
func (i sizedFileInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (i sizedFileInfo) IsDir() bool        { return false }
func (i sizedFileInfo) Sys() any           { return nil }

// countingCapFileSystem presents one synthetic projection: a chosen size, a
// chosen first-bytes header, and a counter for every WHOLE read of it. Reads
// return a sentinel error, so no real projection bytes are needed; a case
// asserts on whether the whole file was read, not on decode.
type countingCapFileSystem struct {
	*ingest.OSFileSystem
	size   int64
	header string
	reads  int
}

var _ ingest.FileSystem = (*countingCapFileSystem)(nil)

var errCapReadAttempted = errors.New("synthetic projection read attempted")

func (fsys *countingCapFileSystem) Stat(path string) (os.FileInfo, error) {
	if path == capProjectionPath {
		return sizedFileInfo{size: fsys.size}, nil
	}
	return fsys.OSFileSystem.Stat(path)
}

func (fsys *countingCapFileSystem) ReadFile(path string) ([]byte, error) {
	if path == capProjectionPath {
		fsys.reads++
		return nil, errCapReadAttempted
	}
	return fsys.OSFileSystem.ReadFile(path)
}

// ReadFileHeader serves the synthetic first bytes WITHOUT counting a read: the
// point of the capability is that identifying the file never loads it.
func (fsys *countingCapFileSystem) ReadFileHeader(path string, limit int) ([]byte, error) {
	if path != capProjectionPath {
		return fsys.OSFileSystem.ReadFileHeader(path, limit)
	}
	header := fsys.header
	if len(header) > limit {
		header = header[:limit]
	}
	return []byte(header), nil
}

// TestOpenCodeProjectionReadersRefuseTheProviderDatabaseNotLargeSessions pins
// what decides whether a managed projection is read. A long session's
// projection is large and must be read, so no size may refuse it; the
// provider's own database must be refused, and identified from its first bytes
// so that it is never loaded. Both readers, the indexer and the capture, follow
// the one rule.
func TestOpenCodeProjectionReadersRefuseTheProviderDatabaseNotLargeSessions(t *testing.T) {
	t.Parallel()
	for _, c := range loadOpenCodeProjectionCapDoc(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			session := ingest.DiscoveredSession{
				SessionID:        ingest.SessionID("ses_3cd91f52effeXd3QAJ54jOyzv5"),
				Harness:          ingest.HarnessOpenCode,
				SourcePath:       ingest.ResolvedPath(capProjectionPath),
				TranscriptOrigin: c.transcriptOrigin(t),
			}
			for _, reader := range []struct {
				name string
				run  func(ingest.FileSystem) error
			}{
				{"index", func(filesystem ingest.FileSystem) error {
					_, err := ingest.NewOpenCodeIndexer(filesystem).IndexTranscript(context.Background(), session)
					return err
				}},
				{"capture", func(filesystem ingest.FileSystem) error {
					_, err := ingest.NewOpenCodeIndexer(filesystem).IndexTranscriptForCapture(context.Background(), session)
					return err
				}},
			} {
				t.Run(reader.name, func(t *testing.T) {
					header := ""
					if c.SQLiteHeader {
						header = openCodeSQLiteFileHeader
					}
					fsys := &countingCapFileSystem{OSFileSystem: &ingest.OSFileSystem{}, size: c.size(), header: header}
					err := reader.run(fsys)
					if err == nil {
						t.Fatal("the reader returned no error; either the sentinel read error or the provider-database refusal was expected")
					}
					if c.Outcome == projectionRead {
						if !errors.Is(err, errCapReadAttempted) {
							t.Fatalf("a projection of %d bytes was not read: %v; a long session's projection is legitimately large and refusing it loses the whole session", c.size(), err)
						}
						if fsys.reads != 1 {
							t.Fatalf("the projection was read %d times, want exactly 1", fsys.reads)
						}
						return
					}
					if errors.Is(err, errCapReadAttempted) {
						t.Fatal("the provider's database was read into memory; it must be refused from its first bytes alone")
					}
					if fsys.reads != 0 {
						t.Fatalf("the provider's database was read %d times; identifying it must never load it", fsys.reads)
					}
					for _, want := range []string{capProjectionPath, string(session.SessionID), "rerun harvest"} {
						if !strings.Contains(err.Error(), want) {
							t.Fatalf("the refusal must name %q so the user can act on it; got %q", want, err)
						}
					}
				})
			}
		})
	}
}
