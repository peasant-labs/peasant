package store

import (
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/retained_attempt.yaml
var retainedAttemptYAML []byte

//go:embed testdata/retained_attempt.manifest.yaml
var retainedAttemptManifestYAML []byte

// The route names a matrix row may exercise. single and batch drive the two
// activation entry points; batch_write drives the batched writer directly, the
// only route that can carry a non-managed result or a non-ordinary mode far
// enough to reach the predicate.
const (
	retentionRouteSingle     = "single"
	retentionRouteBatch      = "batch"
	retentionRouteBatchWrite = "batch_write"
)

// The generation identifiers the matrix builds and asserts.
const (
	retentionMatrixFullPrior  = "gen_full_prior"
	retentionMatrixCandidate  = "gen_matrix_candidate"
	retentionMatrixPreviewPri = "gen_matrix_preview_prior"
)

type retentionAttemptFixture struct {
	PairIdentity string `yaml:"pairIdentity"`
	Session      struct {
		ID string `yaml:"id"`
	} `yaml:"session"`
	Cases []retentionAttemptCase `yaml:"cases"`
}

// retentionAttemptCase varies one clause of the eligibility predicate. An
// omitted field keeps the passing value; Mode is a pointer so the ordinary
// empty-string mode is distinguishable from an absent field.
type retentionAttemptCase struct {
	Name              string   `yaml:"name"`
	Routes            []string `yaml:"routes"`
	Result            string   `yaml:"result"`
	Completeness      string   `yaml:"completeness"`
	Capture           string   `yaml:"capture"`
	Mode              *string  `yaml:"mode"`
	Certificate       string   `yaml:"certificate"`
	Identity          string   `yaml:"identity"`
	WantRetained      bool     `yaml:"want_retained"`
	WantErrorContains string   `yaml:"want_error_contains"`
}

func loadRetentionAttemptFixture(t testing.TB) retentionAttemptFixture {
	t.Helper()
	var fixture retentionAttemptFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(retainedAttemptYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode retained_attempt.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(retainedAttemptManifestYAML)
	if err != nil {
		t.Fatalf("decode retained_attempt manifest: %v", err)
	}
	actual := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "retained attempt eligibility"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// assessedStrictRefusedPreview derives the preview tuple for an unaccounted
// omission whose own source did NOT record the omission: the strict refusal,
// which the predicate does not retain.
func assessedStrictRefusedPreview(t testing.TB, v2 indexformat.V2) ingest.SessionContentCaptureWrite {
	t.Helper()
	assessment, err := ingest.AssessCapture(ingest.CaptureFacts{
		Harness: ingest.Harness("claude-code"), Result: v2,
		Policy: ingest.CaptureFreshCandidate, Authoritative: true,
		SourceOmitted: false, Unaccounted: true,
	})
	if err != nil {
		t.Fatalf("assessment: %v", err)
	}
	capture, err := assessment.ContentCapture(ingest.ContentSourceNewIngest, ingest.TranscriptOriginFile, 1)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if capture.FailureCode != ingest.ContentCaptureStrictRefused {
		t.Fatalf("derived failure code = %q, want strict_capture_refused", capture.FailureCode)
	}
	return capture
}

func retentionFixtureDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// TestRetentionEligibilityMatrix runs the clause-complete eligibility fixture:
// each row varies one clause of the predicate, keeps every other clause at its
// passing value, and asserts the disposition plus the retained-pointer state on
// every route it names.
func TestRetentionEligibilityMatrix(t *testing.T) {
	fixture := loadRetentionAttemptFixture(t)
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			for _, route := range c.Routes {
				t.Run(route, func(t *testing.T) {
					runRetentionMatrixRoute(t, fixture, c, route)
				})
			}
		})
	}
}

func runRetentionMatrixRoute(t *testing.T, fixture retentionAttemptFixture, c retentionAttemptCase, route string) {
	t.Helper()
	result := retentionFixtureDefault(c.Result, "managed")
	completeness := retentionFixtureDefault(c.Completeness, "complete")
	captureKind := retentionFixtureDefault(c.Capture, "assessed")
	mode := string(ingest.SessionEntryWriteReplaceAll)
	if c.Mode != nil {
		mode = *c.Mode
	}
	certificate := retentionFixtureDefault(c.Certificate, "publishable")
	identity := retentionFixtureDefault(c.Identity, "stored")

	s, _ := openGenerationStore(t)
	sid := schema.SessionID(fixture.Session.ID)

	switch certificate {
	case "publishable":
		seedFullAuthority(t, s, sid)
	case "absent":
		seedGenerationSession(t, s, string(sid))
	case "non_publishable":
		seedGenerationSession(t, s, string(sid))
		if _, err := s.ActivateGeneration(context.Background(), testActivation(t, sid, retentionMatrixPreviewPri)); err != nil {
			t.Fatalf("seed the non-publishable certificate: %v", err)
		}
	default:
		t.Fatalf("unknown certificate %q", certificate)
	}

	var suppliedIdentity *string
	switch identity {
	case "stored":
		setStoredArtifactHash(t, s, sid, fixture.PairIdentity)
	case "supplied":
		id := fixture.PairIdentity
		suppliedIdentity = &id
	case "none":
	default:
		t.Fatalf("unknown identity %q", identity)
	}
	var expected *ingest.SessionIndexState
	if identity == "supplied" {
		// The pair identity claim is only admitted against the captured SQL
		// state, so a supplied identity carries the snapshot it describes.
		expected = readIndexStateFor(t, s, sid)
	}

	v2, blobs := buildTestGeneration(t, sid, retentionMatrixCandidate, "matrix text", "matrix input", "matrix output")
	if completeness == "incomplete_new" {
		v2, blobs = buildIncompleteGeneration(t, sid, retentionMatrixCandidate, "matrix text", "matrix input", "matrix output")
	} else if completeness != "complete" {
		t.Fatalf("unknown completeness %q", completeness)
	}

	var capture ingest.SessionContentCaptureWrite
	switch captureKind {
	case "assessed":
		if completeness == "incomplete_new" {
			// Only the completeness clause may fail on this row, so the
			// capture is the literal assessed tuple rather than a derivation
			// that a first-discovery candidate would refuse.
			capture = sourceOmittedPreviewWrite()
		} else {
			capture = assessedSourceOmittedPreview(t, v2)
		}
	case "strict_refused":
		capture = assessedStrictRefusedPreview(t, v2)
	default:
		t.Fatalf("unknown capture %q", captureKind)
	}

	writeMode := ingest.SessionEntryWriteMode("")
	switch mode {
	case string(ingest.SessionEntryWriteReplaceAll):
		writeMode = ingest.SessionEntryWriteReplaceAll
	case "":
		writeMode = ingest.SessionEntryWriteMode("")
	case string(ingest.SessionEntryWriteExplicitRebuild):
		writeMode = ingest.SessionEntryWriteExplicitRebuild
	case string(ingest.SessionEntryWriteFormatConversion):
		writeMode = ingest.SessionEntryWriteFormatConversion
	case string(ingest.SessionEntryWriteContentBackfill):
		writeMode = ingest.SessionEntryWriteContentBackfill
	default:
		t.Fatalf("unknown mode %q", mode)
	}

	var (
		retained bool
		routeErr error
	)
	switch route {
	case retentionRouteSingle, retentionRouteBatch:
		activation := GenerationActivation{
			Generation:       v2,
			Blobs:            blobs,
			IndexerVersion:   18,
			IndexedAtMs:      2,
			ContentCapture:   capture,
			ExplicitRebuild:  writeMode == ingest.SessionEntryWriteExplicitRebuild,
			ExpectedState:    expected,
			ArtifactIdentity: suppliedIdentity,
		}
		if route == retentionRouteSingle {
			outcome, err := s.ActivateGeneration(context.Background(), activation)
			retained = outcome.Disposition == ingest.ActivationRetainedPreview
			routeErr = err
		} else {
			results := s.ActivateGenerationBatch(context.Background(), []GenerationActivation{activation})
			if len(results) != 1 {
				t.Fatalf("batch returned %d results, want 1", len(results))
			}
			retained = results[0].Outcome.Disposition == ingest.ActivationRetainedPreview
			routeErr = results[0].Err
		}
	case retentionRouteBatchWrite:
		var stored indexformat.Result = filledCandidateForValidation(t, v2, blobs)
		indexVersion := 2
		if result == "v1" {
			stored = indexformat.V1{Entries: v2.Generation.Main.Entries}
			indexVersion = 1
		} else if result != "managed" {
			t.Fatalf("unknown result %q", result)
		}
		if indexVersion == 2 {
			// The raw writer installs the catalog rows over already-staged
			// objects; only the activation routes stage for the caller.
			prepared, prepErr := prepareHarmonizedCandidate(sid, v2.Generation, blobs)
			if prepErr != nil {
				t.Fatalf("prepare the batch-write candidate: %v", prepErr)
			}
			if stageErr := s.stageHarmonizedSession(context.Background(), prepared); stageErr != nil {
				t.Fatalf("stage the batch-write candidate: %v", stageErr)
			}
		}
		indexerVersion := 18
		if writeMode == ingest.SessionEntryWriteFormatConversion {
			// A conversion claims no parser run; a non-zero revision is refused
			// before the predicate, which the row does not exercise.
			indexerVersion = 0
		}
		results := s.IndexSessionEntryBatch(context.Background(), []ingest.SessionEntryWrite{{
			SessionID:        sid,
			Result:           stored,
			IndexVersion:     indexVersion,
			Mode:             writeMode,
			ContentCapture:   capture,
			IndexerVersion:   indexerVersion,
			IndexedAtMs:      2,
			ExpectedState:    expected,
			ArtifactIdentity: suppliedIdentity,
		}})
		if len(results) != 1 {
			t.Fatalf("batch write returned %d results, want 1", len(results))
		}
		retained = results[0].RetainedPreview
		routeErr = results[0].Err
	default:
		t.Fatalf("unknown route %q", route)
	}

	if c.WantRetained {
		if routeErr != nil {
			t.Fatalf("retained row returned error %v", routeErr)
		}
		if !retained {
			t.Fatal("write was not retained, want a retained latest attempt")
		}
		pointer := retainedPointerFor(t, s, sid)
		if pointer.generationID != retentionMatrixCandidate {
			t.Fatalf("retained generation = %q, want %q", pointer.generationID, retentionMatrixCandidate)
		}
		if pointer.artifactHash != fixture.PairIdentity {
			t.Fatalf("retained pair identity = %q, want %q", pointer.artifactHash, fixture.PairIdentity)
		}
		if certificate == "publishable" {
			if active, err := s.activeGenerationID(context.Background(), sid); err != nil || active != retentionMatrixFullPrior {
				t.Fatalf("active = %q (err=%v), want the untouched full prior", active, err)
			}
		}
		return
	}
	if retained {
		t.Fatal("write was retained, want the predicate to refuse it")
	}
	if pointer := retainedPointerFor(t, s, sid); pointer.found() {
		t.Fatalf("retained pointer = %+v, want none", pointer)
	}
	if c.WantErrorContains != "" {
		if routeErr == nil || !strings.Contains(routeErr.Error(), c.WantErrorContains) {
			t.Fatalf("route error = %v, want it to contain %q", routeErr, c.WantErrorContains)
		}
	}
}
