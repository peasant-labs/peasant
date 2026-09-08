package ingest_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/adapter_replay.yaml
var adapterReplayYAML []byte

type adapterReplayFixtures struct {
	RequiredNames []string `yaml:"requiredNames"`
	Cases         []struct {
		Name          string         `yaml:"name"`
		Harness       ingest.Harness `yaml:"harness"`
		Transcript    string         `yaml:"transcript"`
		Turns         int            `yaml:"turns"`
		TokensIn      int            `yaml:"tokensIn"`
		TokensOut     int            `yaml:"tokensOut"`
		PreserveTimes bool           `yaml:"preserveTimes"`
		Insufficient  bool           `yaml:"insufficient"`
	} `yaml:"cases"`
}

func LoadAdapterReplayFixtures(t *testing.T) adapterReplayFixtures {
	t.Helper()
	var fixtures adapterReplayFixtures
	if err := yaml.Unmarshal(adapterReplayYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, row := range fixtures.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid replay fixture %q", row.Name)
		}
		seen[row.Name] = true
	}
	for _, name := range fixtures.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing replay fixture %q", name)
		}
	}
	return fixtures
}

func TestAdaptersReplayRetainedJSONL(t *testing.T) {
	for _, row := range LoadAdapterReplayFixtures(t).Cases {
		t.Run(row.Name, func(t *testing.T) {
			harness := row.Harness
			if _, ok := ingest.DefaultAdapterRegistry[harness]; !ok {
				t.Fatalf("unknown harness %q", harness)
			}
			original := makeReindexMeta(t, testSessionID, "/missing/native/session.jsonl")
			original.ModelHarness = harness
			original.Timestamp.Start = 1000
			original.Timestamp.End = 2000
			ingested := int64(3000)
			original.Timestamp.Ingested = &ingested
			original.Stats.SubagentCount = 7
			original.AdapterVersion = nil
			before, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			// Nil native dependencies make accidental filesystem/Git access fail.
			adapter := ingest.DefaultAdapterRegistry[harness](nil, nil, salt.Salt{})
			replayer, ok := adapter.(ingest.RetainedInputReplayer)
			if !ok {
				t.Fatal("adapter has no retained replay capability")
			}
			metadata, transcript, err := replayer.ReplayRetained(t.Context(), []byte(row.Transcript), original)
			if row.Insufficient {
				var insufficient *ingest.InsufficientRetainedInputError
				if !errors.As(err, &insufficient) || metadata != nil || transcript != nil {
					t.Fatalf("expected insufficient retained input, got %+v %v", metadata, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(transcript, []byte(row.Transcript)) {
				t.Fatal("replay changed retained transcript bytes")
			}
			if metadata.Stats.TurnCount != row.Turns || metadata.Stats.TokensIn != row.TokensIn || metadata.Stats.TokensOut != row.TokensOut {
				t.Fatalf("replay did not parse represented statistics: %+v", metadata.Stats)
			}
			if metadata.SessionID != original.SessionID || metadata.HostSlug != original.HostSlug || !reflect.DeepEqual(metadata.Project, original.Project) || !reflect.DeepEqual(metadata.Git, original.Git) || !reflect.DeepEqual(metadata.Source, original.Source) || metadata.Stats.SubagentCount != original.Stats.SubagentCount || metadata.Timestamp.Ingested == nil || *metadata.Timestamp.Ingested != ingested || metadata.AdapterVersion != nil {
				t.Fatalf("replay replaced retained context or producer evidence: %+v", metadata)
			}
			if row.PreserveTimes && (metadata.Timestamp.Start != original.Timestamp.Start || metadata.Timestamp.End != original.Timestamp.End) {
				t.Fatal("replay replaced retained fallback timestamps")
			}
			after, err := json.Marshal(original)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("replay mutated original metadata")
			}
		})
	}
}
