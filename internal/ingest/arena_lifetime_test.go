package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"testing"

	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/arena_lifetime.yaml
var arenaLifetimeFixtures []byte

type arenaLifetimeCase struct {
	Name         string `yaml:"name"`
	Ack          []int  `yaml:"ack"`
	Held         int    `yaml:"held"`
	ParentGated  bool   `yaml:"parent_gated"`
	ExpectedUsed int64  `yaml:"expected_used"`
}

func loadArenaLifetimeFixtures(t *testing.T) []arenaLifetimeCase {
	t.Helper()
	var fixture struct {
		RequiredNames []string            `yaml:"required_names"`
		Cases         []arenaLifetimeCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(arenaLifetimeFixtures, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.RequiredNames) == 0 {
		t.Fatal("arena lifetime fixture declares no required case names")
	}
	names := make(map[string]bool)
	for _, c := range fixture.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("arena lifetime fixture has an empty or repeated case name %q", c.Name)
		}
		names[c.Name] = true
	}
	for _, name := range fixture.RequiredNames {
		if !names[name] {
			t.Fatalf("arena lifetime fixture is missing required case %q", name)
		}
	}
	return fixture.Cases
}

func TestStagingBuffer_PreservesLiveTranscriptAcrossLaterAcks(t *testing.T) {
	for _, tc := range loadArenaLifetimeFixtures(t) {
		t.Run(tc.Name, func(t *testing.T) {
			b := NewStagingBuffer(5, 100)
			batches := make([]DrainBatch, 3)
			parent := sid("parent")
			for i := range 3 {
				r := makeResult(sid(string(rune('a'+i))), nil)
				r.transcriptData = bytes.Repeat([]byte{byte('a' + i)}, 30)
				r.meta.ContentHash = schema.ComputeTranscriptHash(r.transcriptData)
				if tc.ParentGated && i == tc.Held {
					r.meta.ParentUUID = &parent
				}
				if !b.Add(t.Context(), r) {
					t.Fatal("staging rejected a session")
				}
				batches[i] = b.Drain()
			}
			for _, i := range tc.Ack {
				// Exercise the same completion-token release as parser workers.
				token := newIndexBatchCompletion(batches[i], 1)
				batch, complete := token.completeWorkItem()
				if !complete {
					t.Fatal("finished parser did not complete its batch")
				}
				b.AckBatch(batch)
			}
			usedBeforeWrap := b.ArenaUsed()
			// Cancellation makes a full arena stage outside the slab rather than
			// block. With an incorrectly advanced tail these writes instead wrap
			// over the live input before its parser or parent gate has finished.
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			for range 2 {
				if !b.Add(ctx, workerResult{transcriptData: bytes.Repeat([]byte("z"), 30)}) {
					t.Fatal("cancellation dropped a completed session")
				}
			}
			if tc.ParentGated {
				b.Commit(parent)
				batches[tc.Held] = b.Drain()
			}
			live := batches[tc.Held].Results[0]
			if got := schema.ComputeTranscriptHash(live.transcriptData); got != live.meta.ContentHash {
				t.Fatalf("live transcript checksum changed before its own acknowledgement: got %s, want %s", got, live.meta.ContentHash)
			}
			if usedBeforeWrap != tc.ExpectedUsed {
				t.Fatalf("arena used before wrap = %d, want %d including unreclaimable later spans", usedBeforeWrap, tc.ExpectedUsed)
			}
			b.AckBatch(batches[tc.Held])
			b.AckBatch(b.Drain())
			if b.Len() != 0 || b.ArenaUsed() != 0 {
				t.Fatalf("final ack leaked results or arena capacity: len=%d used=%d", b.Len(), b.ArenaUsed())
			}
		})
	}
}

func TestStagingBuffer_RollbackCannotReleaseLiveTranscript(t *testing.T) {
	b := NewStagingBuffer(1, 100)
	r := workerResult{transcriptData: bytes.Repeat([]byte("a"), 30)}
	want := schema.ComputeTranscriptHash(r.transcriptData)
	if !b.Add(t.Context(), r) {
		t.Fatal("first Add failed")
	}
	batch := b.Drain()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// Each rejected Add has already tried to allocate bytes. Releasing these
	// later reservations must not make the still-claimed batch reusable.
	for range 3 {
		if b.Add(ctx, workerResult{transcriptData: bytes.Repeat([]byte("z"), 30)}) {
			t.Fatal("exhausted slot array accepted a result")
		}
	}
	if got := schema.ComputeTranscriptHash(batch.Results[0].transcriptData); got != want {
		t.Fatalf("rolled-back allocation overwrote live transcript: got %s, want %s", got, want)
	}
	b.AckBatch(batch)
	if b.ArenaUsed() != 0 {
		t.Fatalf("rollback leaked arena capacity: used=%d", b.ArenaUsed())
	}
}
