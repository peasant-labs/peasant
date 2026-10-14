package store

import (
	_ "embed"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/content_write_corpus.yaml
var contentWriteCorpusYAML []byte

type contentWriteCorpus struct {
	Name    string `yaml:"name"`
	Entries int    `yaml:"entries"`
	Prefix  string `yaml:"prefix"`
	Repeats int    `yaml:"repeats"`
}

func loadContentWriteCorpora(tb testing.TB) []contentWriteCorpus {
	tb.Helper()
	var fixtures struct {
		RequiredNames []string             `yaml:"requiredNames"`
		Cases         []contentWriteCorpus `yaml:"cases"`
	}
	if err := yaml.Unmarshal(contentWriteCorpusYAML, &fixtures); err != nil {
		tb.Fatal(err)
	}
	var names []string
	for _, c := range fixtures.Cases {
		if c.Entries < 1 || c.Repeats < 1 || c.Prefix == "" {
			tb.Fatalf("incomplete write corpus %+v", c)
		}
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(fixtures.RequiredNames, names, "content write corpus"); err != nil {
		tb.Fatal(err)
	}
	return fixtures.Cases
}

// BenchmarkHarmonizedFullContent measures real object staging and batch
// activation. Seeding and corpus construction are outside the timed region.
func BenchmarkHarmonizedFullContent(b *testing.B) {
	for _, corpus := range loadContentWriteCorpora(b) {
		b.Run(fmt.Sprintf("entries=%d", corpus.Entries), func(b *testing.B) {
			s := openBenchGenerationStore(b)
			defer func() {
				_ = s.Close()
			}()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				sid, err := schema.NewSessionID(fmt.Sprintf("dddddddd-dddd-4ddd-8ddd-%012d", i))
				if err != nil {
					b.Fatal(err)
				}
				seedBenchGenerationSession(b, s, sid)
				v2 := benchV2InstallGeneration(sid, fmt.Sprintf("g_full_%d", i), corpus.Entries)
				for j := range v2.Generation.Main.Entries {
					text := strings.Repeat(corpus.Prefix, corpus.Repeats) + fmt.Sprintf("record %d", j)
					v2.Generation.Main.Entries[j].ContentPreview = &text
					timestamp := int64(1700000000000 + int64(j))
					v2.Generation.Main.Entries[j].TimestampMs = &timestamp
				}
				activation := GenerationActivation{Generation: v2, IndexerVersion: 1, IndexedAtMs: 1700000005000}
				b.StartTimer()
				activation.Prepared, err = s.StageGeneration(b.Context(), activation)
				if err != nil {
					b.Fatal(err)
				}
				results := s.ActivateGenerationBatch(b.Context(), []GenerationActivation{activation})
				if len(results) != 1 || results[0].Err != nil || results[0].Outcome.Disposition != ingest.ActivationCommittedNow {
					b.Fatalf("full-content activation: %+v", results)
				}
			}
		})
	}
}
