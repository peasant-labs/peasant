package store_test

import (
	"context"
	"embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/testkit/contentparity"
)

//go:embed testdata/content_model_parity.yaml testdata/content_model_parity.manifest.yaml testdata/content_model_parity_goldens/*.json
var contentParityFixtures embed.FS

func LoadContentModelParityFixtures(t *testing.T) []contentparity.Case {
	t.Helper()
	sources, err := contentParityFixtures.ReadFile("testdata/content_model_parity.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := contentParityFixtures.ReadFile("testdata/content_model_parity.manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cases, err := contentparity.Load(sources, manifest)
	if err != nil {
		t.Fatal(err)
	}
	return cases
}

func TestContentModelParityFixtureManifest(t *testing.T) {
	LoadContentModelParityFixtures(t)
}

func TestContentModelParity(t *testing.T) {
	for _, c := range LoadContentModelParityFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			raw, err := contentParityFixtures.ReadFile("testdata/content_model_parity_goldens/" + c.Name + ".json")
			if err != nil {
				t.Fatal(err)
			}
			golden, err := contentparity.LoadCorpus(raw)
			if err != nil {
				t.Fatal(err)
			}
			observed, err := contentparity.Run(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			if err = contentparity.Compare(golden, observed); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The mutation changes exactly one frozen surface, not the fixture source or
// writer. It proves that the comparator rejects a one-byte payload drift.
func TestContentModelParityByteMutation(t *testing.T) {
	c := LoadContentModelParityFixtures(t)[0]
	raw, err := contentParityFixtures.ReadFile("testdata/content_model_parity_goldens/" + c.Name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	golden, err := contentparity.LoadCorpus(raw)
	if err != nil {
		t.Fatal(err)
	}
	mutated := golden
	mutated.Detail.Bytes = append([]byte(nil), golden.Detail.Bytes...)
	if len(mutated.Detail.Bytes) == 0 {
		t.Fatal("mutation subject has no successful detail bytes")
	}
	mutated.Detail.Bytes[0] ^= 1
	if err = contentparity.Compare(golden, mutated); err == nil {
		t.Fatal("single-byte detail drift escaped the comparator")
	} else {
		t.Log(err)
	}
}
