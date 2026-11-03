package ingest_test

import (
	"embed"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/hostslug_semantics*.yaml
var hostSlugSemanticsFixtures embed.FS

type hostSlugSemanticsCase struct {
	Name         string `yaml:"name"`
	Basename     string `yaml:"basename"`
	WantBasename string `yaml:"wantBasename"`
}

func loadHostSlugSemanticsFixtures(t *testing.T) []hostSlugSemanticsCase {
	t.Helper()
	data, err := hostSlugSemanticsFixtures.ReadFile("testdata/hostslug_semantics.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []hostSlugSemanticsCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(data, &doc); err != nil {
		t.Fatal(err)
	}
	data, err = hostSlugSemanticsFixtures.ReadFile("testdata/hostslug_semantics.manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(data, "host slug semantics")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range doc.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "host slug semantics"); err != nil {
		t.Fatal(err)
	}
	return doc.Cases
}

func TestDeriveHostSlugInvalidUTF8Path(t *testing.T) {
	t.Parallel()
	for _, c := range loadHostSlugSemanticsFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			// YAML strings are valid UTF-8; substitute one raw byte at this boundary.
			basename := strings.ReplaceAll(c.Basename, "{{FF}}", string([]byte{0xff}))
			path := filepath.Join(t.TempDir(), basename)
			s := salt.Salt{}
			got, err := ingest.DeriveHostSlug(s, "", path)
			if err != nil {
				t.Fatal(err)
			}
			hash, err := s.Hash(path)
			if err != nil {
				t.Fatal(err)
			}
			want := "__peasant-untracked__--" + string(hash)[:8] + "--" + c.WantBasename
			if string(got) != want || !utf8.ValidString(string(got)) {
				t.Fatalf("DeriveHostSlug(%q) = %q, want %q (valid UTF-8)", path, got, want)
			}
		})
	}
}
