package ingest

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/claude_slug_windows.yaml
var claudeSlugWindowsYAML []byte

// claudeSlugWindowsCase is one decodeProjectSlug case for issue #368 (Windows
// drive-letter slugs), expressed as ordered path SEGMENTS rather than a
// literal path string. See testdata/claude_slug_windows.yaml for why: a
// hardcoded "C:\..." or "/..." string can only ever be correct on one
// platform, so both the virtual directory tree and the expected match are
// built from these segments with filepath.Join at run time instead.
type claudeSlugWindowsCase struct {
	Name string `yaml:"name"`
	// Encoded is the Claude project slug under test.
	Encoded string `yaml:"encoded"`
	// Root is a bare drive letter (e.g. "C") for a Windows-shaped case, or
	// empty for a unix-shaped case that decodes from "/".
	Root string `yaml:"root"`
	// Dirs lists every directory the virtual filesystem contains, each as
	// ordered segments from Root.
	Dirs [][]string `yaml:"dirs"`
	// Want is the ordered segments of the expected decoded match from Root,
	// or empty if decoding is expected to match nothing.
	Want []string `yaml:"want"`
}

type claudeSlugWindowsFixture struct {
	DeclaredRows  int                     `yaml:"declared_rows"`
	RequiredCases []string                `yaml:"required_cases"`
	Cases         []claudeSlugWindowsCase `yaml:"cases"`
}

func loadClaudeSlugWindowsFixture(t *testing.T) claudeSlugWindowsFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(claudeSlugWindowsYAML))
	decoder.KnownFields(true)
	var fixture claudeSlugWindowsFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode the Windows project-slug fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("the Windows project-slug fixture must hold exactly one YAML document: %v", err)
	}
	if fixture.DeclaredRows != len(fixture.Cases) {
		t.Fatalf("Windows project-slug fixture row guard failed: declared=%d actual=%d",
			fixture.DeclaredRows, len(fixture.Cases))
	}
	seen := make(map[string]struct{}, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if c.Name == "" || c.Encoded == "" {
			t.Fatalf("Windows project-slug fixture has an incomplete case: %#v", c)
		}
		if _, duplicate := seen[c.Name]; duplicate {
			t.Fatalf("duplicate Windows project-slug fixture case %q", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	for _, required := range fixture.RequiredCases {
		if _, ok := seen[required]; !ok {
			t.Fatalf("required fixture case %q is missing; the rule it pins would stop being tested", required)
		}
	}
	return fixture
}

// claudeSlugWindowsRoot returns the filesystem root the case's segments are
// relative to, built exactly the way splitSlugRoot does: a bare "/" for a
// unix-shaped case, or a drive letter followed by ":" and the platform's
// native separator for a Windows-shaped one. Mirroring splitSlugRoot's own
// formula here (rather than asserting a literal "C:\" or "C:/") is what
// keeps this fixture green on both Linux and Windows: filepath.Join below
// then normalizes every joined segment to whatever separator the running
// platform actually uses, on both sides of the comparison.
func claudeSlugWindowsRoot(driveLetter string) string {
	if driveLetter == "" {
		return "/"
	}
	return driveLetter + ":" + string(filepath.Separator)
}

// claudeSlugWindowsJoin joins segments onto root with filepath.Join, exactly
// as decodeProjectSlug does, so the resulting string carries native
// separators on whichever platform ran the test.
func claudeSlugWindowsJoin(root string, segments []string) string {
	return filepath.Join(append([]string{root}, segments...)...)
}

// TestDecodeClaudeSlug_WindowsDriveLetterFixture exercises decodeProjectSlug
// (via DecodeClaudeSlug) over the fixture cases for issue #368: Windows
// drive-letter slugs, a space inside a decoded directory name, and a unix
// slug carried along as a regression guard. See splitSlugRoot in utils.go
// for the decoder change and testdata/claude_slug_windows.yaml for why every
// expectation here is segments, not a literal path.
func TestDecodeClaudeSlug_WindowsDriveLetterFixture(t *testing.T) {
	fixture := loadClaudeSlugWindowsFixture(t)
	for _, c := range fixture.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			root := claudeSlugWindowsRoot(c.Root)
			dirs := make(map[string]bool, len(c.Dirs))
			for _, segments := range c.Dirs {
				dirs[claudeSlugWindowsJoin(root, segments)] = true
			}
			dirExists := func(path string) bool { return dirs[path] }

			want := ""
			if len(c.Want) > 0 {
				want = claudeSlugWindowsJoin(root, c.Want)
			}

			got := DecodeClaudeSlug(c.Encoded, dirExists)
			if got != want {
				t.Errorf("DecodeClaudeSlug(%q) = %q, want %q", c.Encoded, got, want)
			}
		})
	}
}
