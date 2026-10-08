package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/generation_snapshot_race.yaml
var snapshotRaceYAML []byte

type snapshotRaceCase struct {
	Name      string `yaml:"name"`
	FirstText string `yaml:"firstText"`
	NextText  string `yaml:"nextText"`
	Sweep     bool   `yaml:"sweep"`
	Surface   string `yaml:"surface"`
}

func loadSnapshotRaceFixtures(t *testing.T) []snapshotRaceCase {
	t.Helper()
	var fixtures struct {
		RequiredNames []string           `yaml:"requiredNames"`
		Cases         []snapshotRaceCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(snapshotRaceYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, c := range fixtures.Cases {
		if c.Name == "" || c.FirstText == "" || c.NextText == "" || names[c.Name] {
			t.Fatalf("invalid snapshot race fixture: %+v", c)
		}
		names[c.Name] = true
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("missing snapshot race case %q", name)
		}
		delete(names, name)
	}
	if len(names) != 0 {
		t.Fatalf("unmanifested snapshot race cases: %v", names)
	}
	return fixtures.Cases
}

type capturedSnapshotReader struct{ snapshot indexformat.ReadSnapshot }

func (r capturedSnapshotReader) WithSessionSnapshot(_ context.Context, _ schema.SessionID, fn func(indexformat.ReadSnapshot) error) error {
	return fn(r.snapshot)
}

var _ indexformat.SnapshotReader = capturedSnapshotReader{}

// snapshotActivation builds an exported-API candidate with exact content
// descriptors. No test-only writer or mutable source supplies its bytes.
func snapshotActivation(id schema.SessionID, generation, text string) store.GenerationActivation {
	count := int64(1)
	ref := schema.SourceEntryRef("snapshot:0")
	sum := sha256.Sum256([]byte(text))
	digest := hex.EncodeToString(sum[:])
	return store.GenerationActivation{
		Generation: indexformat.V2{Generation: indexformat.Generation{
			ID: generation, Completeness: indexformat.GenerationCompletenessComplete,
			Metadata:             schema.UnifiedMetadata{SchemaVersion: 11, SessionID: id, ModelHarness: schema.HarnessOpenCode, Stats: schema.SessionStats{TurnCount: 1, InputSubmissionCount: &count}},
			Main:                 indexformat.Partition{Entries: []schema.SessionEntry{{SessionID: id, EntryIndex: 0, Harness: schema.HarnessOpenCode, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &text, SourceEntryRef: ref}}},
			Content:              []indexformat.ContentRecord{{Ref: ref, RelativeBlob: "c_" + digest + ".blob", ByteLength: int64(len(text)), Digest: digest}},
			SourceEvidenceDigest: strings.Repeat("a", 64), TitleRefs: []schema.SourceEntryRef{ref},
		}},
		Blobs: map[schema.SourceEntryRef][]byte{ref: []byte(text)}, IndexerVersion: 1, IndexedAtMs: 1,
	}
}

func TestHarmonizedFullSnapshotSurvivesActivationSweep(t *testing.T) {
	for _, c := range loadSnapshotRaceFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			switch c.Surface {
			case "detail":
				runSnapshotActivationSweep(t, c)
			case "publication":
				runPublicationActivationSweep(t, c)
			default:
				t.Fatalf("unknown snapshot race surface %q", c.Surface)
			}
		})
	}
}

func runPublicationActivationSweep(t *testing.T, c snapshotRaceCase) {
	t.Helper()
	s, sid := seedFullReadCorruption(t, "body-column", c.FirstText)
	_, wantPayload, err := push.LoadPublicationInput(t.Context(), s, string(sid))
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(wantPayload)
	if err != nil {
		t.Fatal(err)
	}
	err = s.WithCommittedPublicationInput(t.Context(), sid, func(input ingest.PublicationInputBundle) error {
		if input.Generation == nil || input.Readiness != ingest.PublicationReady {
			t.Fatal("publication input is not the committed generation")
		}
		next := snapshotActivation(sid, "publication-next", c.NextText)
		next.Generation.Generation.Metadata = input.Generation.Metadata
		next.Generation.Generation.Main.Entries[0].Harness = input.Generation.Metadata.ModelHarness
		next.CaptureRevision = input.CaptureRevision
		next.ContentCapture = ingest.SessionContentCaptureWrite{Status: ingest.ContentCaptureComplete, SourceAuthority: ingest.ContentSourceNewIngest, TranscriptOrigin: ingest.TranscriptOriginFile, CaptureFormat: ingest.ContentCaptureFormatFull, CapturedAtMs: 2}
		if _, err := s.ActivateGeneration(t.Context(), next); err != nil {
			return err
		}
		if c.Sweep {
			if _, err := s.SweepSession(t.Context(), sid); err != nil {
				return err
			}
		}
		payload, err := transcript.SnapshotToDetailValidated(t.Context(), *input.Generation, s)
		if err != nil {
			return err
		}
		got, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("captured publication bytes changed after activation and sweep:\ngot %s\nwant %s", got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func runSnapshotActivationSweep(t *testing.T, c snapshotRaceCase) {
	t.Helper()
	s := openSnapshotStore(t, 1)
	ctx := t.Context()
	sid := schema.SessionID("d4d4d4d4-d4d4-44d4-84d4-d4d4d4d4d4d4")
	seedHarmonizedSnapshot(t, s, string(sid), "seed-snapshot")
	activation := snapshotActivation(sid, "snapshot-first", c.FirstText)
	if err := activation.Generation.Generation.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateGeneration(ctx, activation); err != nil {
		t.Fatal(err)
	}
	want, _, err := transcript.BuildSnapshotDetailBytes(ctx, s, s, sid)
	if err != nil {
		t.Fatal(err)
	}
	err = s.WithSessionSnapshot(ctx, sid, func(snapshot indexformat.ReadSnapshot) error {
		if _, err := s.ActivateGeneration(ctx, snapshotActivation(sid, "snapshot-next", c.NextText)); err != nil {
			return err
		}
		if c.Sweep {
			if _, err := s.SweepSession(ctx, sid); err != nil {
				return err
			}
		}
		got, payload, err := transcript.BuildSnapshotDetailBytes(ctx, capturedSnapshotReader{snapshot}, s, sid)
		if err != nil {
			return err
		}
		if payload == nil || !bytes.Equal(got, want) {
			t.Fatalf("captured detail changed after activation and sweep:\ngot %s\nwant %s", got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
