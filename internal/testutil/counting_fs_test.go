package testutil_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"io/fs"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/counting_fs.yaml
var countingFSYAML []byte

type countingFSFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	Cases         []struct {
		Name      string `yaml:"name"`
		Op        string `yaml:"op"`
		Path      string `yaml:"path"`
		Fault     bool   `yaml:"fault"`
		WantCount int    `yaml:"wantCount"`
		WantBytes int64  `yaml:"wantBytes"`
	} `yaml:"cases"`
}

func loadCountingFSFixture(t *testing.T) countingFSFixture {
	t.Helper()
	var fixture countingFSFixture
	decoder := yaml.NewDecoder(bytes.NewReader(countingFSYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("counting filesystem fixture requires one YAML document")
	}
	present := make(map[string]bool)
	for _, row := range fixture.Cases {
		if row.Name == "" || present[row.Name] {
			t.Fatalf("invalid counting filesystem case %q", row.Name)
		}
		present[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("counting filesystem fixture testdata/counting_fs.yaml", "case", fixture.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestCountingFSCountsEachOperationByPath(t *testing.T) {
	t.Parallel()
	fixture := loadCountingFSFixture(t)
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			op, ok := testutil.NewFSOp(row.Op)
			if !ok {
				t.Fatalf("fixture names unknown operation %q", row.Op)
			}
			inner := testutil.NewMemFS()
			if err := inner.WriteFile("/tree/a/file.txt", []byte("hello"), 0o600); err != nil {
				t.Fatal(err)
			}
			counting := testutil.NewCountingFS(inner)
			planted := errors.New("planted fault")
			if row.Fault {
				counting.Fail(op, row.Path, planted)
			}
			var err error
			switch op {
			case testutil.FSOpReadFile:
				_, err = counting.ReadFile(row.Path)
			case testutil.FSOpWriteFile:
				err = counting.WriteFile(row.Path, []byte("new"), 0o600)
			case testutil.FSOpRename:
				err = counting.Rename("/tree/a/file.txt", row.Path)
			case testutil.FSOpWalkDir:
				err = counting.WalkDir(row.Path, func(string, fs.DirEntry, error) error { return nil })
			default:
				t.Fatalf("fixture operation %q has no driver in this test", op)
			}
			if row.Fault {
				if !errors.Is(err, planted) {
					t.Fatalf("planted fault did not fail the operation: %v", err)
				}
				if _, statErr := inner.Stat("/tree/a/file.txt"); statErr != nil {
					t.Fatalf("a failed rename moved the source anyway: %v", statErr)
				}
				if got := counting.Count(op, "/tree/a/file.txt"); got != 0 {
					t.Fatalf("the fault counted against the source path (%d), not only the destination", got)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got := counting.Count(op, row.Path); got != row.WantCount {
				t.Fatalf("%s on %s counted %d, want %d", op, row.Path, got, row.WantCount)
			}
			if got := counting.CountUnder(op, "/tree"); got != row.WantCount {
				t.Fatalf("%s under /tree counted %d, want %d", op, got, row.WantCount)
			}
			if got := counting.CountUnder(op, "/elsewhere"); got != 0 {
				t.Fatalf("%s under an unrelated prefix counted %d", op, got)
			}
			if got := counting.BytesReadUnder("/tree", ".txt"); got != row.WantBytes {
				t.Fatalf("bytes read under /tree = %d, want %d", got, row.WantBytes)
			}
			counting.ResetCounts()
			if counting.Total(op) != 0 {
				t.Fatal("reset left a count behind")
			}
		})
	}
}
