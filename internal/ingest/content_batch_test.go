package ingest

import (
	"context"
	_ "embed"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/content_batch.yaml
var contentBatchFixtureData []byte

type captureBatchObserver struct {
	MetricsStore
	batches []int
	cancel  context.CancelFunc
}

var _ SessionEntryBatchStore = (*captureBatchObserver)(nil)

func (s *captureBatchObserver) IndexSessionEntryBatch(_ context.Context, writes []SessionEntryWrite) []SessionEntryWriteResult {
	s.batches = append(s.batches, len(writes))
	results := make([]SessionEntryWriteResult, len(writes))
	for i, write := range writes {
		results[i] = SessionEntryWriteResult{SessionID: write.SessionID, Written: true}
	}
	if s.cancel != nil {
		s.cancel()
	}
	return results
}

func TestFullContentWriteBatchBudget(t *testing.T) {
	var fixtures struct {
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name    string `yaml:"name"`
			Bytes   []int  `yaml:"session_bytes"`
			Batches []int  `yaml:"expected_batches"`
			Cancel  bool   `yaml:"cancel_after_batch"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(contentBatchFixtureData, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if names[fixture.Name] {
			t.Fatalf("duplicate fixture %s", fixture.Name)
		}
		names[fixture.Name] = true
		t.Run(fixture.Name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			observer := &captureBatchObserver{}
			if fixture.Cancel {
				observer.cancel = cancel
			}
			pipeline := &Pipeline{metricsStore: observer}
			var parsed []indexParseResult
			for i, size := range fixture.Bytes {
				id, err := NewSessionID(fmt.Sprintf("ses_captureBatch%d", i))
				if err != nil {
					t.Fatal(err)
				}
				text := strings.Repeat("a", size)
				parsed = append(parsed, indexParseResult{im: indexedMeta{session: DiscoveredSession{SessionID: id, Harness: HarnessClaudeCode}}, output: indexformat.V1{Entries: []schema.SessionEntry{{SessionID: id, ContentPreview: &text}}}, fullContent: true})
			}
			flush := pipeline.flushIndexParseResults(ctx, parsed, IndexOutcomeIndexed, "capture test", nil)
			if !reflect.DeepEqual(observer.batches, fixture.Batches) {
				t.Fatalf("batches=%v want=%v", observer.batches, fixture.Batches)
			}
			if len(flush.logEntries) != len(parsed) {
				t.Fatal("session outcome lost")
			}
		})
	}
	for _, name := range fixtures.Required {
		if !names[name] {
			t.Fatalf("missing required fixture %s", name)
		}
	}
}
