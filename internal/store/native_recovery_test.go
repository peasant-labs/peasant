package store

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/native_recovery.yaml
var nativeRecoveryYAML []byte

type nativeRecoveryCase struct {
	Name              string `yaml:"name"`
	Completeness      string `yaml:"completeness"`
	Fault             string `yaml:"fault"`
	Prior             string `yaml:"prior"`
	CarrierNamespace  string `yaml:"carrier_namespace"`
	CarrierKind       string `yaml:"carrier_kind"`
	CarrierSourceRef  string `yaml:"carrier_source_ref"`
	CarrierRecordIdx  int64  `yaml:"carrier_record_index"`
	CarrierPosition   int64  `yaml:"carrier_position"`
	CarrierPointer    string `yaml:"carrier_pointer"`
	CarrierPayload    string `yaml:"carrier_payload"`
	WantDisposition   string `yaml:"want_disposition"`
	WantOccurrences   int    `yaml:"want_occurrences"`
	WantActive        string `yaml:"want_active"`
	WantRows          string `yaml:"want_rows"`
	WantErrorContains string `yaml:"want_error_contains"`
}

type nativeRecoveryDocument struct {
	Required []string             `yaml:"required_names"`
	Cases    []nativeRecoveryCase `yaml:"cases"`
}

func loadNativeRecovery(t *testing.T) nativeRecoveryDocument {
	t.Helper()
	var doc nativeRecoveryDocument
	decoder := yaml.NewDecoder(bytes.NewReader(nativeRecoveryYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("trailing fixture document")
	}
	names := map[string]bool{}
	var actual []string
	for _, c := range doc.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("invalid fixture %q", c.Name)
		}
		names[c.Name] = true
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(doc.Required, actual, "native recovery"); err != nil {
		t.Fatal(err)
	}
	return doc
}

func buildRecoveryCarrier(t *testing.T, sid schema.SessionID, index int, c nativeRecoveryCase) schema.SessionEntry {
	t.Helper()
	position := ingest.UnknownSourcePosition{
		Public:   &ingest.UnknownPublicPosition{SourceRef: c.CarrierSourceRef, RecordIndex: c.CarrierRecordIdx, Position: c.CarrierPosition},
		SourceID: c.CarrierSourceRef,
	}
	position.JSONPointer = c.CarrierPointer
	record, err := ingest.NewRetainedUnknown(ingest.Harness("claude-code"), c.CarrierNamespace, c.CarrierKind, position, []byte(c.CarrierPayload))
	if err != nil {
		t.Fatal(err)
	}
	entry, err := ingest.RetainedUnknownEntry(ingest.SessionID(sid), index, record)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func buildRecoveryV2(t *testing.T, sid schema.SessionID, genID, completeness string, withCarrier bool, c nativeRecoveryCase) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	// The text varies per identifier so a prior and its refresh never
	// compare equal by accident: identical bytes must skip, so the matrix
	// needs distinct bytes wherever it expects a commit.
	text := "recovery text " + genID
	var v2 indexformat.V2
	var blobs map[schema.SourceEntryRef][]byte
	if completeness == "complete" {
		v2, blobs = buildTestGeneration(t, sid, genID, text, "recovery input "+genID, "recovery output "+genID)
	} else {
		v2, blobs = buildIncompleteGeneration(t, sid, genID, text, "recovery preview input "+genID, "recovery preview output "+genID)
	}
	if withCarrier {
		carrier := buildRecoveryCarrier(t, sid, len(v2.Generation.Main.Entries), c)
		v2.Generation.Main.Entries = append(v2.Generation.Main.Entries, carrier)
		v2.Generation.Metadata.Stats.TurnCount = len(v2.Generation.Main.Entries)
	}
	// Every matrix candidate carries one retained (non-emitted) content
	// record with its bytes, so the missing-bytes faults have a binding to
	// break: the record is retained through a captured context segment, and
	// its descriptor reaches the blob store while emitted refs stay inline.
	retained := schema.SourceEntryRef("e_retained_recovery")
	v2.Generation.Content = append(v2.Generation.Content, indexformat.ContentRecord{Ref: retained})
	v2.Generation.Segments = append(v2.Generation.Segments, indexformat.ContextSegment{
		Ordinal:          len(v2.Generation.Segments),
		PhysicalSourceID: "recovery-source",
		Coordinates:      indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindSnapshotOnly},
		Inclusion:        indexformat.SegmentInclusionInherited,
		CapturedRefs:     []schema.SourceEntryRef{retained},
	})
	blobs[retained] = []byte("retained recovery bytes")
	return v2, blobs
}

func recoveryCapture(t *testing.T, v2 indexformat.V2) ingest.SessionContentCaptureWrite {
	t.Helper()
	assessment, err := ingest.AssessCapture(ingest.CaptureFacts{
		Harness: ingest.Harness("claude-code"), Result: v2,
		Policy: ingest.CaptureFreshCandidate, Authoritative: true,
	})
	if err != nil {
		t.Fatalf("assessment: %v", err)
	}
	capture, err := assessment.ContentCapture(ingest.ContentSourceNewIngest, ingest.TranscriptOriginFile, 1)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	return capture
}

func TestNativeRecoveryMatrix(t *testing.T) {
	doc := loadNativeRecovery(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			s, _ := openGenerationStore(t)
			sid := schema.SessionID("99d59925-36bc-424c-a789-8be54d9702ba")
			seedGenerationSession(t, s, string(sid))
			// Prior full authority when requested.
			priorID := "gen-recovery-prior-" + c.Name
			if c.Prior == "full" {
				priorV2, priorBlobs := buildRecoveryV2(t, sid, priorID, "complete", true, c)
				priorCapture := recoveryCapture(t, priorV2)
				if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{
					Generation: priorV2, Blobs: priorBlobs,
					IndexerVersion: 1, IndexedAtMs: 1, ContentCapture: priorCapture,
				}); err != nil {
					t.Fatalf("prior full: %v", err)
				}
			}
			genID := "gen-recovery-" + c.Name
			v2, blobs := buildRecoveryV2(t, sid, genID, c.Completeness, true, c)
			capture := recoveryCapture(t, v2)
			// Candidates for per-invocation counting via real assessment.
			assessment, err := ingest.AssessCapture(ingest.CaptureFacts{
				Harness: ingest.Harness("claude-code"), Result: v2,
				Policy: ingest.CaptureFreshCandidate, Authoritative: true,
			})
			if err != nil {
				t.Fatalf("assessment: %v", err)
			}
			candidates := assessment.CandidateCounts()
			candidateOccurrences := 0
			for _, count := range candidates {
				candidateOccurrences += count.Occurrences
			}
			// Fault setup and execution per matrix row.
			var outcome ingest.ActivationOutcome
			var actErr error
			execActivate := func(captureOverride ingest.SessionContentCaptureWrite, blobsOverride map[schema.SourceEntryRef][]byte, expected *ingest.SessionIndexState) (ingest.ActivationOutcome, error) {
				filled := filledCandidateForValidation(t, v2, blobs)
				return s.ActivateGeneration(context.Background(), GenerationActivation{
					Generation: filled, Blobs: blobsOverride,
					IndexerVersion: 1, IndexedAtMs: 2, ContentCapture: captureOverride, ExpectedState: expected,
				})
			}
			switch c.Fault {
			case "none":
				outcome, actErr = execActivate(capture, blobs, nil)
			case "missing_blob":
				broken := map[schema.SourceEntryRef][]byte{}
				for ref, data := range blobs {
					broken[ref] = data
				}
				delete(broken, schema.SourceEntryRef("e_retained_recovery"))
				outcome, actErr = execActivate(capture, broken, nil)
			case "commit_fault":
				installHarmonizedFault(t, harmonizedSeamAtCommit)
				outcome, actErr = execActivate(capture, blobs, nil)
				clearHarmonizedFault()
			case "stale":
				staleState, err := s.ReadIndexState(context.Background(), ingest.SessionID(sid))
				if err != nil {
					t.Fatalf("read state: %v", err)
				}
				// Advance with an intermediate so the captured state is stale.
				midV2, midBlobs := buildRecoveryV2(t, sid, "gen-recovery-mid-"+c.Name, "complete", false, c)
				midCapture := recoveryCapture(t, midV2)
				if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{
					Generation: midV2, Blobs: midBlobs,
					IndexerVersion: 1, IndexedAtMs: 2, ContentCapture: midCapture,
				}); err != nil {
					t.Fatalf("mid advance: %v", err)
				}
				outcome, actErr = execActivate(capture, blobs, staleState)
			case "unrelated_staged":
				// Objects staged for another candidate are inert: they
				// install no generation row, so the requested activation
				// commits over them.
				unrelatedID := "gen-unrelated-" + c.Name
				unrelatedV2, unrelatedBlobs := buildRecoveryV2(t, sid, unrelatedID, "complete", false, c)
				if _, err := s.StageGeneration(context.Background(), GenerationActivation{
					Generation: unrelatedV2, Blobs: unrelatedBlobs,
					IndexerVersion: 1, IndexedAtMs: 2,
				}); err != nil {
					t.Fatalf("stage unrelated: %v", err)
				}
				outcome, actErr = execActivate(capture, blobs, nil)
			case "prestaged":
				// The requested candidate's own objects are staged first;
				// the activation commits them through the prepared handle.
				filled := filledCandidateForValidation(t, v2, blobs)
				prepared, err := s.StageGeneration(context.Background(), GenerationActivation{
					Generation: filled, Blobs: blobs,
					IndexerVersion: 1, IndexedAtMs: 2, ContentCapture: capture,
				})
				if err != nil {
					t.Fatalf("prestage requested: %v", err)
				}
				outcome, actErr = s.ActivateGeneration(context.Background(), GenerationActivation{
					Generation: filled, Blobs: blobs,
					IndexerVersion: 1, IndexedAtMs: 2, ContentCapture: capture, ExpectedState: nil,
					Prepared: prepared,
				})
			case "retry", "retry_commit_fault":
				// First commit, then retry the same candidate. A fault on
				// the retry's commit needs a write path, so the faulted
				// retry carries altered content under a fresh identifier
				// while the first commit stands.
				firstOutcome, err := execActivate(capture, blobs, nil)
				if err != nil || firstOutcome.Disposition != ingest.ActivationCommittedNow {
					t.Fatalf("first commit: %+v err=%v", firstOutcome, err)
				}
				if c.Fault == "retry_commit_fault" {
					altered, alteredBlobs := buildRecoveryV2(t, sid, genID+"-bis", "complete", false, c)
					alteredCapture := recoveryCapture(t, altered)
					installHarmonizedFault(t, harmonizedSeamAtCommit)
					filled := filledCandidateForValidation(t, altered, alteredBlobs)
					outcome, actErr = s.ActivateGeneration(context.Background(), GenerationActivation{
						Generation: filled, Blobs: alteredBlobs,
						IndexerVersion: 1, IndexedAtMs: 2, ContentCapture: alteredCapture,
					})
					clearHarmonizedFault()
				} else {
					outcome, actErr = execActivate(capture, blobs, nil)
				}
			case "forged_capture":
				// Forged capture: an incomplete_new candidate that claims a
				// full/complete capture. The guarded write refuses the
				// forged claim and preserves last-good authority.
				forged := ingest.SessionContentCaptureWrite{
					Status: ingest.ContentCaptureComplete, SourceAuthority: ingest.ContentSourceNewIngest,
					TranscriptOrigin: ingest.TranscriptOriginFile, CaptureFormat: ingest.ContentCaptureFormatFull, CapturedAtMs: 2,
				}
				outcome, actErr = execActivate(forged, blobs, nil)
			case "same_id_mismatched":
				// The requested identifier commits first; a second
				// candidate under the same identifier with different entry
				// bytes is refused against the immutable installed
				// identifier.
				if _, err := execActivate(capture, blobs, nil); err != nil {
					t.Fatalf("first commit: %v", err)
				}
				alteredV2, _ := buildRecoveryV2(t, sid, genID, "complete", false, c)
				altered := "altered recovery text"
				alteredV2.Generation.Main.Entries[0].ContentPreview = &altered
				filledAlter := filledCandidateForValidation(t, alteredV2, blobs)
				outcome, actErr = s.ActivateGeneration(context.Background(), GenerationActivation{
					Generation: filledAlter, Blobs: blobs,
					IndexerVersion: 1, IndexedAtMs: 2, ContentCapture: capture,
				})
			case "same_id_unbindable":
				// The requested envelope itself cannot be bound (the
				// retained blob is missing): refused with the binding
				// category, last-good authority unchanged.
				requestBlobs := map[schema.SourceEntryRef][]byte{}
				for ref, data := range blobs {
					requestBlobs[ref] = data
				}
				delete(requestBlobs, schema.SourceEntryRef("e_retained_recovery"))
				outcome, actErr = execActivate(capture, requestBlobs, nil)
			default:
				t.Fatalf("unknown fault %q", c.Fault)
			}
			if c.WantErrorContains != "" && (actErr == nil || !strings.Contains(actErr.Error(), c.WantErrorContains)) {
				t.Fatalf("error = %v, want it to contain %q", actErr, c.WantErrorContains)
			}
			// Disposition.
			wantDisposition := map[string]ingest.ActivationDisposition{
				"committed_now":     ingest.ActivationCommittedNow,
				"already_committed": ingest.ActivationAlreadyCommitted,
				"not_committed":     ingest.ActivationNotCommitted,
			}[c.WantDisposition]
			if outcome.Disposition != wantDisposition {
				t.Fatalf("disposition = %v, want %v (err=%v)", outcome.Disposition, wantDisposition, actErr)
			}
			if c.WantDisposition == "not_committed" && actErr == nil {
				t.Fatal("want pre-commit refusal, got nil error")
			}
			// Per-invocation counts: CommittedNow counts candidates once,
			// AlreadyCommitted/NotCommitted zero. Preview CommittedNow carries
			// zero candidates by assessment.
			gotOccurrences := 0
			if outcome.Disposition == ingest.ActivationCommittedNow {
				gotOccurrences = candidateOccurrences
			}
			if gotOccurrences != c.WantOccurrences {
				t.Fatalf("occurrences = %d, want %d (candidates=%+v disposition=%v)", gotOccurrences, c.WantOccurrences, candidates, outcome.Disposition)
			}
			// Authority: active generation reflects outcome and prior. The stale
			// row advances to an intermediate before the stale attempt, so its
			// refusal preserves the intermediate, not the original prior.
			active, err := s.activeGenerationID(context.Background(), sid)
			if err != nil {
				t.Fatal(err)
			}
			wantActiveID := ""
			switch c.WantActive {
			case "candidate":
				wantActiveID = genID
			case "prior":
				wantActiveID = priorID
				if c.Fault == "stale" {
					wantActiveID = "gen-recovery-mid-" + c.Name
				}
			case "none":
				wantActiveID = ""
			default:
				t.Fatalf("unknown want_active %q", c.WantActive)
			}
			if active != wantActiveID {
				t.Fatalf("active = %q, want %q", active, wantActiveID)
			}
			// Committed rows: the generation row, its mapping and its bodies
			// are present exactly when a commit happened, never on authority
			// alone.
			present := rowPresent(t, s, sid, genID)
			switch c.WantRows {
			case "present":
				if !present {
					t.Fatalf("candidate %q has no generation row; want committed rows", genID)
				}
				assertHarmonizedContent(t, s, sid, genID, v2, blobs)
			case "absent":
				if present {
					t.Fatalf("candidate %q has a generation row; want no committed rows", genID)
				}
			default:
				t.Fatalf("unknown want_rows %q", c.WantRows)
			}
			// Raw-never-in-errors: refused and repair-pending errors must not
			// echo payload bytes, source labels, or coordinate namespaces. The
			// secrets are YAML-owned, like the carrier that introduced them.
			if actErr != nil {
				msg := actErr.Error()
				secrets := []string{c.CarrierPayload, c.CarrierSourceRef, c.CarrierNamespace}
				if i := strings.Index(c.CarrierPayload, ","); i > 0 {
					secrets = append(secrets, c.CarrierPayload[:i]+`"`)
				}
				for _, secret := range secrets {
					if secret != "" && strings.Contains(msg, secret) {
						t.Fatalf("error echoes raw evidence %q: %v", secret, actErr)
					}
				}
			}
		})
	}
}
