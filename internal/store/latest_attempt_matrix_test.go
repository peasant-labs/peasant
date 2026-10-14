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

//go:embed testdata/latest_attempt.yaml
var latestAttemptYAML []byte

//go:embed testdata/latest_attempt.manifest.yaml
var latestAttemptManifestYAML []byte

// The route names a matrix row may exercise. single and batch drive the two
// activation entry points; batch_write drives the batched writer directly, the
// only route that can carry a non-managed result or a non-ordinary mode far
// enough to reach the predicate.
const (
	latestAttemptRouteSingle     = "single"
	latestAttemptRouteBatch      = "batch"
	latestAttemptRouteBatchWrite = "batch_write"
)

// The generation identifiers the matrix builds and asserts.
const (
	latestAttemptMatrixFullPrior  = "gen_full_prior"
	latestAttemptMatrixCandidate  = "gen_matrix_candidate"
	latestAttemptMatrixPreviewPri = "gen_matrix_preview_prior"
)

type latestAttemptFixture struct {
	PairIdentity string `yaml:"pairIdentity"`
	Session      struct {
		ID string `yaml:"id"`
	} `yaml:"session"`
	Cases []latestAttemptCase `yaml:"cases"`
}

// latestAttemptCase varies one clause of the eligibility predicate. An
// omitted field keeps the passing value; Mode is a pointer so the ordinary
// empty-string mode is distinguishable from an absent field. WantOK marks a
// row whose ordinary path must commit; it is mutually exclusive with
// WantErrorContains, and one of the two is required on every negative row so a
// row cannot pass on an unrelated downstream guard.
type latestAttemptCase struct {
	Name              string   `yaml:"name"`
	Routes            []string `yaml:"routes"`
	Result            string   `yaml:"result"`
	Completeness      string   `yaml:"completeness"`
	Capture           string   `yaml:"capture"`
	Mode              *string  `yaml:"mode"`
	Certificate       string   `yaml:"certificate"`
	Identity          string   `yaml:"identity"`
	WantLatestAttempt bool     `yaml:"want_latest_attempt"`
	WantOK            bool     `yaml:"want_ok"`
	WantErrorContains string   `yaml:"want_error_contains"`
}

func loadLatestAttemptFixture(t testing.TB) latestAttemptFixture {
	t.Helper()
	var fixture latestAttemptFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(latestAttemptYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode latest_attempt.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(latestAttemptManifestYAML)
	if err != nil {
		t.Fatalf("decode latest_attempt manifest: %v", err)
	}
	actual := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "latest attempt eligibility"); err != nil {
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

func latestAttemptFixtureDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// TestLatestAttemptEligibilityMatrix runs the clause-complete eligibility fixture:
// each row varies one clause of the predicate, keeps every other clause at its
// passing value, and asserts the disposition plus the latest-attempt-pointer state on
// every route it names.
func TestLatestAttemptEligibilityMatrix(t *testing.T) {
	fixture := loadLatestAttemptFixture(t)
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			for _, route := range c.Routes {
				t.Run(route, func(t *testing.T) {
					runLatestAttemptMatrixRoute(t, fixture, c, route)
				})
			}
		})
	}
}

func runLatestAttemptMatrixRoute(t *testing.T, fixture latestAttemptFixture, c latestAttemptCase, route string) {
	t.Helper()
	if c.WantOK && c.WantErrorContains != "" {
		t.Fatalf("row %q expects both success and an error; want_ok and want_error_contains are mutually exclusive", c.Name)
	}
	result := latestAttemptFixtureDefault(c.Result, "managed")
	completeness := latestAttemptFixtureDefault(c.Completeness, "complete")
	captureKind := latestAttemptFixtureDefault(c.Capture, "assessed")
	mode := string(ingest.SessionEntryWriteReplaceAll)
	if c.Mode != nil {
		mode = *c.Mode
	}
	certificate := latestAttemptFixtureDefault(c.Certificate, "publishable")
	identity := latestAttemptFixtureDefault(c.Identity, "stored")

	s, _ := openGenerationStore(t)
	sid := schema.SessionID(fixture.Session.ID)

	switch certificate {
	case "publishable":
		seedFullAuthority(t, s, sid)
	case "absent":
		seedGenerationSession(t, s, string(sid))
	case "non_publishable":
		seedGenerationSession(t, s, string(sid))
		if _, err := s.ActivateGeneration(context.Background(), testActivation(t, sid, latestAttemptMatrixPreviewPri)); err != nil {
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

	v2, blobs := buildTestGeneration(t, sid, latestAttemptMatrixCandidate, "matrix text", "matrix input", "matrix output")
	if completeness == "incomplete_new" {
		v2, blobs = buildIncompleteGeneration(t, sid, latestAttemptMatrixCandidate, "matrix text", "matrix input", "matrix output")
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
		latestAttempt bool
		routeErr      error
	)
	switch route {
	case latestAttemptRouteSingle, latestAttemptRouteBatch:
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
		if route == latestAttemptRouteSingle {
			outcome, err := s.ActivateGeneration(context.Background(), activation)
			latestAttempt = outcome.Disposition == ingest.ActivationLatestAttemptPreview
			routeErr = err
		} else {
			results := s.ActivateGenerationBatch(context.Background(), []GenerationActivation{activation})
			if len(results) != 1 {
				t.Fatalf("batch returned %d results, want 1", len(results))
			}
			latestAttempt = results[0].Outcome.Disposition == ingest.ActivationLatestAttemptPreview
			routeErr = results[0].Err
		}
	case latestAttemptRouteBatchWrite:
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
		latestAttempt = results[0].LatestAttemptPreview
		routeErr = results[0].Err
	default:
		t.Fatalf("unknown route %q", route)
	}

	if c.WantLatestAttempt {
		if routeErr != nil {
			t.Fatalf("latest-attempt row returned error %v", routeErr)
		}
		if !latestAttempt {
			t.Fatal("write did not produce a latest attempt, want one")
		}
		pointer := latestAttemptPointerFor(t, s, sid)
		if pointer.generationID != latestAttemptMatrixCandidate {
			t.Fatalf("latest-attempt generation = %q, want %q", pointer.generationID, latestAttemptMatrixCandidate)
		}
		if pointer.artifactHash != fixture.PairIdentity {
			t.Fatalf("latest-attempt pair identity = %q, want %q", pointer.artifactHash, fixture.PairIdentity)
		}
		if certificate == "publishable" {
			if active, err := s.activeGenerationID(context.Background(), sid); err != nil || active != latestAttemptMatrixFullPrior {
				t.Fatalf("active = %q (err=%v), want the untouched full prior", active, err)
			}
		}
		return
	}
	if latestAttempt {
		t.Fatal("write produced a latest attempt, want the predicate to refuse it")
	}
	if pointer := latestAttemptPointerFor(t, s, sid); pointer.found() {
		t.Fatalf("latest-attempt pointer = %+v, want none", pointer)
	}
	if c.WantOK {
		if routeErr != nil {
			t.Fatalf("row expects the ordinary path to commit; route error = %v, want nil", routeErr)
		}
		return
	}
	if c.WantErrorContains != "" {
		if routeErr == nil || !strings.Contains(routeErr.Error(), c.WantErrorContains) {
			t.Fatalf("route error = %v, want it to contain %q", routeErr, c.WantErrorContains)
		}
	}
}
