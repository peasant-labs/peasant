package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"os"
	"strconv"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
)

//go:embed testdata/opencode_projection_cap.yaml
var openCodeProjectionCapData []byte

const capProjectionPath = "/synthetic/store/opencode-managed-projection.json"

// openCodeProjectionCapCase sizes one synthetic projection file relative to the
// managed-projection bound and states whether the read must be refused.
type openCodeProjectionCapCase struct {
	Name               string `yaml:"name"`
	Origin             string `yaml:"origin"`
	OffsetFromBound    int64  `yaml:"offset_from_bound"`
	ExpectRefused      bool   `yaml:"expect_refused"`
	ExpectReadAttempts int    `yaml:"expect_read_attempts"`
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

func loadOpenCodeProjectionCapDoc(t *testing.T) openCodeProjectionCapDoc {
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
	presentCap := make(map[string]struct{}, len(doc.Cases))
	for _, c := range doc.Cases {
		presentCap[c.Name] = struct{}{}
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("projection cap fixture declares no required cases")
	}
	for _, name := range doc.RequiredCases {
		if _, ok := presentCap[name]; !ok {
			t.Fatalf("projection cap fixture is missing required case %q", name)
		}
	}
	seen := make(map[string]struct{}, len(doc.Cases))
	for _, c := range doc.Cases {
		if c.Name == "" || c.Origin == "" {
			t.Fatalf("projection cap fixture has an incomplete case: %+v", c)
		}
		if _, dup := seen[c.Name]; dup {
			t.Fatalf("projection cap fixture has a duplicate case name %q", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	return doc
}

// sizedFileInfo reports a chosen size for one synthetic path, so the cap test
// can present an oversized file without writing one.
type sizedFileInfo struct{ size int64 }

func (i sizedFileInfo) Name() string       { return "opencode-managed-projection.json" }
func (i sizedFileInfo) Size() int64        { return i.size }
func (i sizedFileInfo) Mode() os.FileMode  { return 0o600 }
func (i sizedFileInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (i sizedFileInfo) IsDir() bool        { return false }
func (i sizedFileInfo) Sys() any           { return nil }

// countingCapFileSystem reports a chosen size for the projection path and
// counts every read of it, so the test can prove the cap refuses an oversized
// file before any read and lets a within-bound file through to a read. Reads
// return a sentinel error, so the test needs no real projection bytes; it
// asserts on whether the read was attempted, not on decode.
type countingCapFileSystem struct {
	*ingest.OSFileSystem
	size  int64
	reads int
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

// TestOpenCodeIndexer_RefusesOversizedProjectionBeforeReading proves the
// defense-in-depth cap: IndexTranscript sizes the managed projection first and
// refuses anything past the bound with an actionable error naming the path and
// the size, without ever reading the file; a within-bound file passes the cap
// and reaches the read. The mutation that removes the cap makes the oversized
// case attempt the read, so its read count rises from zero to one.
func TestOpenCodeIndexer_RefusesOversizedProjectionBeforeReading(t *testing.T) {
	t.Parallel()
	doc := loadOpenCodeProjectionCapDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			fsys := &countingCapFileSystem{OSFileSystem: &ingest.OSFileSystem{}, size: c.size()}
			indexer := ingest.NewOpenCodeIndexer(fsys)
			session := ingest.DiscoveredSession{
				SessionID:        ingest.SessionID("ses_3cd91f52effeXd3QAJ54jOyzv5"),
				Harness:          ingest.HarnessOpenCode,
				SourcePath:       ingest.ResolvedPath(capProjectionPath),
				TranscriptOrigin: c.transcriptOrigin(t),
			}

			_, err := indexer.IndexTranscript(context.Background(), session)
			if err == nil {
				t.Fatalf("IndexTranscript returned no error; the sentinel read error or the cap refusal was expected")
			}
			if fsys.reads != c.ExpectReadAttempts {
				t.Fatalf("projection was read %d times, want %d", fsys.reads, c.ExpectReadAttempts)
			}
			if !c.ExpectRefused {
				if !errors.Is(err, errCapReadAttempted) {
					t.Fatalf("within-bound projection must reach the read; got %v", err)
				}
				return
			}
			if errors.Is(err, errCapReadAttempted) {
				t.Fatalf("an oversized projection must be refused before the read; the read ran")
			}
			message := err.Error()
			if !contains(message, strconv.FormatInt(c.size(), 10)) {
				t.Fatalf("refusal must name the size %d; got %q", c.size(), message)
			}
			if !contains(message, capProjectionPath) {
				t.Fatalf("refusal must name the path %q; got %q", capProjectionPath, message)
			}
		})
	}
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
