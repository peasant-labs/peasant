package store_test

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// contextRelationshipPrefix marks a relationships value in
// context_changes.yaml: the rest names the started_by parent.
const contextRelationshipPrefix = "started_by:"

// withContextValue returns recorded with the dotted JSON path field set to
// value, the way a recorded sidecar would carry it.
func withContextValue(t *testing.T, recorded schema.UnifiedMetadata, field, value string) schema.UnifiedMetadata {
	t.Helper()
	encoded, err := json.Marshal(recorded)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	var typed any = value
	if parent, ok := strings.CutPrefix(value, contextRelationshipPrefix); ok {
		typed = []any{map[string]any{"kind": string(schema.SessionRelationshipStartedBy), "targetState": string(schema.RelationshipTargetKnown), "targetLocalId": parent, "evidence": string(schema.EvidenceExistingAdapter)}}
	}
	steps := strings.Split(field, ".")
	object := document
	for _, step := range steps[:len(steps)-1] {
		next, ok := object[step].(map[string]any)
		if !ok {
			next = map[string]any{}
			object[step] = next
		}
		object = next
	}
	last := steps[len(steps)-1]
	if value == "" {
		delete(object, last)
	} else {
		object[last] = typed
	}
	encoded, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var changed schema.UnifiedMetadata
	if err := json.Unmarshal(encoded, &changed); err != nil {
		t.Fatalf("set %s to %q: %v", field, value, err)
	}
	changed.MetadataHash = schema.ComputeMetadataHash(&changed)
	return changed
}

// TestParseRowsIgnoreTheCapturedContext builds each managed harness's full
// generation from one transcript while only one recorded context value
// varies, for every row of context_changes.yaml. The parse projection (body
// digests, blob digests, aliases, title refs, segments, sections and native
// metadata) must not move, except for a field the harness lists as a parse
// input, which must move it.
func TestParseRowsIgnoreTheCapturedContext(t *testing.T) {
	corpus, _ := loadReaderEntrypoints(t)
	changes := LoadContextChanges(t)
	for _, h := range corpus.Harnesses {
		if !slices.Contains(changes.Harnesses, h.Harness) {
			continue
		}
		t.Run(h.Harness, func(t *testing.T) {
			seeded := seedReaderEntrypointStore(t, corpus, h)
			if !seeded.managed {
				t.Fatalf("%s is listed in context_changes.yaml but has no managed builder", h.Harness)
			}
			listed := ingest.ParseInputContextFields(seeded.harness)
			project := func(recorded schema.UnifiedMetadata) []byte {
				t.Helper()
				candidate, err := seeded.builder.BuildGeneration(t.Context(), ingest.GenerationBuildInput{
					Session: seeded.session, GenerationID: corpus.GenerationID, RecordedMetadata: &recorded,
					CapturedTranscript: seeded.transcript, Prior: readerPriorSource{db: seeded.db, sid: seeded.sid},
					Allocator: &countingRefAllocator{}, OpenCode: seeded.opencode,
				})
				if err != nil {
					t.Fatalf("build with recorded %+v: %v", recorded, err)
				}
				projection, err := store.ParseProjectionForTest(candidate.Result.Generation, candidate.Blobs, seeded.harness == schema.HarnessCodex)
				if err != nil {
					t.Fatal(err)
				}
				return projection
			}
			for _, change := range changes.Changes {
				t.Run(change.Name, func(t *testing.T) {
					before := project(withContextValue(t, seeded.recorded, change.Field, change.Before))
					after := project(withContextValue(t, seeded.recorded, change.Field, change.After))
					moved := !bytes.Equal(before, after)
					if parseInput := slices.Contains(listed, change.Field); moved != parseInput {
						t.Fatalf("changing %s moved the parse projection: %t; listed as a parse input: %t\nbefore %s\nafter  %s", change.Field, moved, parseInput, before, after)
					}
				})
			}
		})
	}
}
