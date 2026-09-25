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
	WantRepairPending bool   `yaml:"want_repair_pending"`
	WantActive        string `yaml:"want_active"`
	WantFullRead      string `yaml:"want_full_read"`
	WantAvailable     string `yaml:"want_available"`
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
	var v2 indexformat.V2
	var blobs map[schema.SourceEntryRef][]byte
	if completeness == "complete" {
		v2, blobs = buildTestGeneration(t, sid, genID, "recovery text", "recovery input", "recovery output")
	} else {
		v2, blobs = buildIncompleteGeneration(t, sid, genID, "recovery preview text", "recovery preview input", "recovery preview output")
	}
	if withCarrier {
		carrier := buildRecoveryCarrier(t, sid, len(v2.Generation.Main.Entries), c)
		v2.Generation.Main.Entries = append(v2.Generation.Main.Entries, carrier)
		v2.Generation.Metadata.Stats.TurnCount = len(v2.Generation.Main.Entries)
	}
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
				return s.ActivateGeneration(context.Background(), GenerationActivation{
					Generation: v2, Blobs: blobsOverride,
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
				for _, record := range v2.Generation.Content {
					delete(broken, record.Ref)
					break
				}
				outcome, actErr = execActivate(capture, broken, nil)
			case "fail_repair":
				s.generationArtifacts = faultArtifacts{GenerationArtifactStore: s.generationArtifacts, failRepair: true}
				outcome, actErr = execActivate(capture, blobs, nil)
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
			case "unrelated_prior":
				unrelatedID := "gen-unrelated-" + c.Name
				unrelatedV2, unrelatedBlobs := buildRecoveryV2(t, sid, unrelatedID, "complete", false, c)
				staged, err := s.generationArtifacts.Stage(context.Background(), unrelatedV2.Generation, unrelatedBlobs)
				if err != nil {
					t.Fatalf("stage unrelated: %v", err)
				}
				_ = staged
				if err := s.generationArtifacts.WriteIntent(context.Background(), GenerationIntent{
					SessionID: sid, GenerationID: unrelatedID,
					ManifestPath: "generations/" + unrelatedID + "/manifest.json",
					Completeness: "complete", StagedAtMs: 1,
					CandidateDigest: "mismatch-digest-for-unrelated-prior",
				}); err != nil {
					t.Fatalf("write unrelated intent: %v", err)
				}
				outcome, actErr = execActivate(capture, blobs, nil)
			case "preamble_pending", "intent_replay":
				// Stage requested candidate and record a matching intent, then
				// let preamble recovery commit it. No DB commit yet.
				digest, err := computeActivationBinding(v2.Generation, bindingFromBlobs(blobs))
				if err != nil {
					t.Fatalf("binding: %v", err)
				}
				if _, err := s.generationArtifacts.Stage(context.Background(), v2.Generation, blobs); err != nil {
					t.Fatalf("stage pending: %v", err)
				}
				if err := s.generationArtifacts.WriteIntent(context.Background(), GenerationIntent{
					SessionID: sid, GenerationID: genID,
					ManifestPath: "generations/" + genID + "/manifest.json",
					Completeness: string(v2.Generation.Completeness), StagedAtMs: 1,
					IndexerVersion: 1, IndexedAtMs: 2, ContentCapture: capture,
					CandidateDigest: digest,
				}); err != nil {
					t.Fatalf("write pending intent: %v", err)
				}
				if c.Fault == "intent_replay" {
					outcome, actErr = s.RecoverGenerationActivation(context.Background(), sid)
				} else {
					outcome, actErr = execActivate(capture, blobs, nil)
				}
			case "retry", "retry_fail_repair":
				// First commit, then retry same candidate.
				firstOutcome, err := execActivate(capture, blobs, nil)
				if err != nil || firstOutcome.Disposition != ingest.ActivationCommittedNow {
					t.Fatalf("first commit: %+v err=%v", firstOutcome, err)
				}
				if c.Fault == "retry_fail_repair" {
					s.generationArtifacts = faultArtifacts{GenerationArtifactStore: s.generationArtifacts, failRepair: true}
				}
				outcome, actErr = execActivate(capture, blobs, nil)
			case "forged_intent_replay":
				// Forged recovery: an incomplete_new candidate staged under an
				// intent that claims a full/complete capture. Recovery replays
				// the persisted envelope through the same guarded write, which
				// refuses the forged claim and preserves last-good authority.
				digest, err := computeActivationBinding(v2.Generation, bindingFromBlobs(blobs))
				if err != nil {
					t.Fatalf("binding: %v", err)
				}
				forged := ingest.SessionContentCaptureWrite{
					Status: ingest.ContentCaptureComplete, SourceAuthority: ingest.ContentSourceNewIngest,
					TranscriptOrigin: ingest.TranscriptOriginFile, CaptureFormat: ingest.ContentCaptureFormatFull, CapturedAtMs: 2,
				}
				if _, err := s.generationArtifacts.Stage(context.Background(), v2.Generation, blobs); err != nil {
					t.Fatalf("stage forged: %v", err)
				}
				if err := s.generationArtifacts.WriteIntent(context.Background(), GenerationIntent{
					SessionID: sid, GenerationID: genID,
					ManifestPath: "generations/" + genID + "/manifest.json",
					Completeness: string(v2.Generation.Completeness), StagedAtMs: 1,
					IndexerVersion: 1, IndexedAtMs: 2, ContentCapture: forged,
					CandidateDigest: digest,
				}); err != nil {
					t.Fatalf("write forged intent: %v", err)
				}
				outcome, actErr = s.RecoverGenerationActivation(context.Background(), sid)
			default:
				t.Fatalf("unknown fault %q", c.Fault)
			}
			// Disposition and repair-pending.
			wantDisposition := map[string]ingest.ActivationDisposition{
				"committed_now":     ingest.ActivationCommittedNow,
				"already_committed": ingest.ActivationAlreadyCommitted,
				"not_committed":     ingest.ActivationNotCommitted,
			}[c.WantDisposition]
			if outcome.Disposition != wantDisposition {
				t.Fatalf("disposition = %v, want %v (err=%v)", outcome.Disposition, wantDisposition, actErr)
			}
			if outcome.RepairPending != c.WantRepairPending {
				t.Fatalf("repairPending = %v, want %v (err=%v)", outcome.RepairPending, c.WantRepairPending, actErr)
			}
			var repairPendingErr *ingest.GenerationRepairPendingError
			isRepairPending := errors.As(actErr, &repairPendingErr)
			if c.WantRepairPending && !isRepairPending {
				t.Fatalf("want GenerationRepairPendingError, got %v", actErr)
			}
			if !c.WantRepairPending && isRepairPending {
				t.Fatalf("unexpected repair-pending error: %v", actErr)
			}
			if c.WantDisposition == "not_committed" && actErr == nil {
				t.Fatal("want pre-commit refusal, got nil error")
			}
			if c.WantDisposition != "not_committed" && c.Fault != "retry" && c.Fault != "retry_fail_repair" {
				// New commits and preamble commits succeed or carry
				// repair-pending; retries assert separately below.
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
			// Actual read verdicts, never readiness alone. success_empty pins
			// the refused-no-prior half the review flagged: a store bug that
			// served stale or foreign entries on available after a refused
			// activation still fails here. Unknown expectations fail closed
			// instead of passing vacuously.
			_, _, fullErr := s.LoadFullSessionEntries(context.Background(), sid, 0)
			switch c.WantFullRead {
			case "success":
				if fullErr != nil {
					t.Fatalf("full read refused committed authority: %v", fullErr)
				}
			case "refused":
				if fullErr == nil {
					t.Fatal("full read certified refused authority")
				}
			default:
				t.Fatalf("unknown want_full_read %q", c.WantFullRead)
			}
			page, err := s.ReadSessionEntries(context.Background(), ingest.SessionID(sid), ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadAvailable, Limit: 100})
			if err != nil {
				t.Fatalf("available read failed: %v", err)
			}
			switch c.WantAvailable {
			case "success":
				if len(page.Entries) == 0 {
					t.Fatal("available read returned no entries for valid authority")
				}
			case "success_empty":
				if len(page.Entries) != 0 {
					t.Fatalf("available read served %d entries with no authority; want empty", len(page.Entries))
				}
			default:
				t.Fatalf("unknown want_available %q", c.WantAvailable)
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
