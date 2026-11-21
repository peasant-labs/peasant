package store_test

import (
	"bytes"
	"encoding/json"
	"reflect"
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
		target := schema.SessionID(parent)
		typed = []schema.SessionRelationship{{
			Kind: schema.SessionRelationshipStartedBy, TargetState: schema.RelationshipTargetKnown,
			TargetLocalID: &target, Evidence: schema.EvidenceExistingAdapter,
		}}
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
	pinUnifiedMetadataInventory(t, changes)
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

// nonContextUnifiedMetadataFields lists the schema.UnifiedMetadata leaf JSON
// paths that are identity, transcript-derived, stats, diagnostic, or stamp
// fields rather than captured context variations in context_changes.yaml.
var nonContextUnifiedMetadataFields = map[string]bool{
	"adapterVersion":                   true,
	"contentHash":                      true,
	"derivedAt":                        true,
	"diagnostics.partial":              true,
	"diagnostics.warnings":             true,
	"git.associations":                 true,
	"git.commits":                      true,
	"git.tracking":                     true,
	"harness":                          true,
	"metadataHash":                     true,
	"model":                            true,
	"project.filePath":                 true,
	"redaction.applied":                true,
	"redaction.content_hash_at_redact": true,
	"redaction.redacted_at_ms":         true,
	"redaction.rule_set_version":       true,
	"schemaVersion":                    true,
	"sessionId":                        true,
	"source.filePath":                  true,
	"source.format":                    true,
	"stats.cachedReadTokens":           true,
	"stats.cachedWriteTokens":          true,
	"stats.conversationTurnCount":      true,
	"stats.durationMs":                 true,
	"stats.inputSubmissionCount":       true,
	"stats.subagentCount":              true,
	"stats.thoughtTokens":              true,
	"stats.tokensIn":                   true,
	"stats.tokensOut":                  true,
	"stats.toolCallCount":              true,
	"stats.turnCount":                  true,
	"subagents":                        true,
	"thinkingLevel":                    true,
	"thinkingLevelHistory":             true,
	"thinkingLevelRaw":                 true,
	"timestamp.end":                    true,
	"timestamp.ingested":               true,
	"timestamp.start":                  true,
	"version":                          true,
}

func pinUnifiedMetadataInventory(t *testing.T, changes contextChangeFixture) {
	t.Helper()
	var collect func(prefix string, typ reflect.Type, out map[string]bool)
	collect = func(prefix string, typ reflect.Type, out map[string]bool) {
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct {
			if prefix != "" {
				out[prefix] = true
			}
			return
		}
		for i := range typ.NumField() {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			tag := field.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, _, _ := strings.Cut(tag, ",")
			if name == "" {
				name = field.Name
			}
			path := name
			if prefix != "" {
				path = prefix + "." + name
			}
			ft := field.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				collect(path, ft, out)
			} else {
				out[path] = true
			}
		}
	}
	actual := map[string]bool{}
	collect("", reflect.TypeOf(schema.UnifiedMetadata{}), actual)
	covered := map[string]bool{}
	for _, c := range changes.Changes {
		covered[c.Field] = true
		if !actual[c.Field] {
			t.Errorf("context_changes.yaml names %q, which is not a field of schema.UnifiedMetadata", c.Field)
		}
	}
	for path := range actual {
		if !covered[path] && !nonContextUnifiedMetadataFields[path] {
			t.Errorf("schema.UnifiedMetadata field %q is neither in context_changes.yaml nor in nonContextUnifiedMetadataFields", path)
		}
	}
	for path := range nonContextUnifiedMetadataFields {
		if !actual[path] {
			t.Errorf("nonContextUnifiedMetadataFields names %q, which is not a field of schema.UnifiedMetadata", path)
		}
	}
}
