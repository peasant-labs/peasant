package push

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/publication_metadata_limits.yaml
var publicationMetadataLimitsYAML []byte

//go:embed testdata/publication_metadata_limits.manifest.yaml
var publicationMetadataLimitsManifestYAML []byte

type metadataLimitPath string

const (
	metadataMapperInput    metadataLimitPath = "mapper-input"
	metadataMapperOutput   metadataLimitPath = "mapper-output"
	metadataPreflightInput metadataLimitPath = "preflight-input"
	metadataFinalPreflight metadataLimitPath = "final-preflight"
	metadataManyEntries    metadataLimitPath = "many-entries"
)

type publicationMetadataLimitCase struct {
	Name                 string            `yaml:"name"`
	Path                 metadataLimitPath `yaml:"path"`
	DocumentBytes        int               `yaml:"documentBytes"`
	LimitBytes           int               `yaml:"limitBytes"`
	EntryCount           int               `yaml:"entryCount"`
	MinimumDocumentBytes int               `yaml:"minimumDocumentBytes"`
	MaximumDocumentBytes int               `yaml:"maximumDocumentBytes"`
	Accepted             bool              `yaml:"accepted"`
	RedactorCalled       bool              `yaml:"redactorCalled"`
	ErrorContains        string            `yaml:"errorContains"`
}

type publicationMetadataLimitsFixture struct {
	Recipe struct {
		SessionID     schema.SessionID   `yaml:"sessionId"`
		SchemaVersion int                `yaml:"schemaVersion"`
		Harness       schema.Harness     `yaml:"harness"`
		Model         schema.ModelID     `yaml:"model"`
		ProjectHash   schema.ProjectHash `yaml:"projectHash"`
		ProjectName   string             `yaml:"projectName"`
		Start         int64              `yaml:"start"`
		End           int64              `yaml:"end"`
		Entry         string             `yaml:"entry"`
		Transcript    string             `yaml:"transcript"`
	} `yaml:"recipe"`
	Policy struct {
		CapBytes       int    `yaml:"capBytes"`
		BelowCapBytes  int    `yaml:"belowCapBytes"`
		AtCapBytes     int    `yaml:"atCapBytes"`
		AboveCapBytes  int    `yaml:"aboveCapBytes"`
		FormerCapBytes int    `yaml:"formerCapBytes"`
		Depth          int    `yaml:"depth"`
		Refusal        string `yaml:"refusal"`
	} `yaml:"policy"`
	Mechanics struct {
		LimitBytes       int `yaml:"limitBytes"`
		FormerLimitBytes int `yaml:"formerLimitBytes"`
		Depth            int `yaml:"depth"`
	} `yaml:"mechanics"`
	Cases         []publicationMetadataLimitCase `yaml:"cases"`
	RealSizeCases []publicationMetadataLimitCase `yaml:"realSizeCases"`
}

func loadPublicationMetadataLimits(t *testing.T) publicationMetadataLimitsFixture {
	t.Helper()
	var fixture publicationMetadataLimitsFixture
	if err := testutil.DecodeFixtureYAML(publicationMetadataLimitsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(publicationMetadataLimitsManifestYAML, "publication metadata limits")
	if err != nil {
		t.Fatal(err)
	}
	all := append(append([]publicationMetadataLimitCase(nil), fixture.Cases...), fixture.RealSizeCases...)
	names := make([]string, len(all))
	for i, c := range all {
		names[i] = c.Name
		if !c.Accepted && c.ErrorContains == "" {
			t.Fatalf("metadata limit fixture %q lacks a refusal reason", c.Name)
		}
		switch c.Path {
		case metadataMapperInput, metadataMapperOutput, metadataPreflightInput, metadataFinalPreflight:
			if c.DocumentBytes <= 0 {
				t.Fatalf("metadata limit fixture %q lacks an encoded target", c.Name)
			}
		case metadataManyEntries:
			if c.EntryCount <= 0 || c.MinimumDocumentBytes <= 0 || c.MaximumDocumentBytes < c.MinimumDocumentBytes {
				t.Fatalf("metadata limit fixture %q has invalid many-entry bounds", c.Name)
			}
		default:
			t.Fatalf("metadata limit fixture %q has unknown path %q", c.Name, c.Path)
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "publication metadata limits"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// assertPublicationMetadataPolicy ties the production metadata cap to the
// literal fixture values. The expected side is the YAML data, never the
// production constant, so a revert of the cap or a removed boundary literal
// fails here rather than tautologically agreeing with itself.
func assertPublicationMetadataPolicy(t *testing.T, f publicationMetadataLimitsFixture) {
	t.Helper()
	if defaults.PushMetadataDocumentCapBytes != f.Policy.CapBytes {
		t.Fatalf("production metadata cap=%d want literal %d", defaults.PushMetadataDocumentCapBytes, f.Policy.CapBytes)
	}
	if f.Policy.AtCapBytes != f.Policy.CapBytes || f.Policy.BelowCapBytes != f.Policy.CapBytes-1 || f.Policy.AboveCapBytes != f.Policy.CapBytes+1 {
		t.Fatalf("policy literals below/at/above=%d/%d/%d are not cap-1/cap/cap+1 of %d", f.Policy.BelowCapBytes, f.Policy.AtCapBytes, f.Policy.AboveCapBytes, f.Policy.CapBytes)
	}
	if f.Policy.FormerCapBytes != 4194304 {
		t.Fatalf("former metadata cap literal=%d want 4194304", f.Policy.FormerCapBytes)
	}
	if f.Policy.FormerCapBytes >= f.Policy.CapBytes {
		t.Fatalf("metadata cap %d does not exceed the former cap %d", f.Policy.CapBytes, f.Policy.FormerCapBytes)
	}
	if f.Policy.Depth != 64 || f.Mechanics.Depth != 64 {
		t.Fatalf("metadata depth literals=%d/%d want 64/64", f.Policy.Depth, f.Mechanics.Depth)
	}
	if !strings.Contains(f.Policy.Refusal, "schema.ScanRawJSONDocument") || !strings.Contains(f.Policy.Refusal, strconv.Itoa(f.Policy.CapBytes)) {
		t.Fatalf("metadata refusal literal %q does not name the scanner and cap %d", f.Policy.Refusal, f.Policy.CapBytes)
	}
	if f.Mechanics.LimitBytes <= f.Mechanics.FormerLimitBytes || f.Mechanics.FormerLimitBytes <= 0 {
		t.Fatalf("mechanics limit=%d must exceed the former analog %d", f.Mechanics.LimitBytes, f.Mechanics.FormerLimitBytes)
	}
}

// The double changes only entry text at the real whole-document redactor seam.
// Removing model evidence requires the production mapper to restore it before
// measuring the final document. No scanner or mapper logic is reproduced here.
type metadataLimitRedactor struct {
	contentBytes int
	called       bool
}

func (r *metadataLimitRedactor) RedactJSON(value any) any {
	r.called = true
	if r.contentBytes > 0 {
		entry := value.(map[string]any)["entries"].([]any)[0].(map[string]any)
		entry["contentPreview"] = strings.Repeat("x", r.contentBytes)
		entry["extra"] = "{}"
	}
	return value
}

var _ redact.JSONRedactor = (*metadataLimitRedactor)(nil)

func metadataLimitOptions(t *testing.T, f publicationMetadataLimitsFixture, count int) MapOptions {
	t.Helper()
	meta := ingest.NewUnifiedMetadata()
	meta.SessionID, meta.SchemaVersion = f.Recipe.SessionID, f.Recipe.SchemaVersion
	meta.ModelHarness, meta.Model = f.Recipe.Harness, f.Recipe.Model
	meta.Timestamp.Start, meta.Timestamp.End = f.Recipe.Start, f.Recipe.End
	meta.Source.Format = schema.SourceFormatJSONL
	meta.Project.Hash, meta.Project.Name = f.Recipe.ProjectHash, f.Recipe.ProjectName
	var template schema.SessionEntry
	if err := json.Unmarshal([]byte(f.Recipe.Entry), &template); err != nil {
		t.Fatal(err)
	}
	entries := make([]schema.SessionEntry, count)
	for i := range entries {
		entries[i] = template
		entries[i].EntryIndex = i
	}
	return MapOptions{Meta: &meta, Entries: entries, Fields: config.PushFieldVisibility{}.Resolve()}
}

func encodeMetadataLimitValue(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func requireMetadataEntryIdentities(t *testing.T, actual []schema.AuthoritativeSessionEntry, original []schema.SessionEntry) {
	t.Helper()
	if len(actual) != len(original) {
		t.Fatalf("decoded entries=%d want %d", len(actual), len(original))
	}
	for i, entry := range actual {
		if entry.SessionID != original[i].SessionID || entry.EntryIndex != original[i].EntryIndex || !reflect.DeepEqual(entry.Extra, original[i].Extra) {
			t.Fatalf("entry %d lost its identity or observed model evidence", i)
		}
	}
}

func requireMetadataPreflight(t *testing.T, metadata, content []byte, entries []schema.SessionEntry) schema.AuthoritativePublishRequest {
	t.Helper()
	request, err := buildAuthoritativeRequest(metadata, content)
	if err != nil {
		t.Fatal(err)
	}
	if request.ContentHash != schema.ComputeTranscriptContentHash(content) || request.VisibilityIntent != schema.VisibilityIntentPrivate {
		t.Fatal("preflight lost authoritative visibility or content hash")
	}
	if request.Identity.SessionID != entries[0].SessionID || request.Model.Model == "" || request.Project.Name == "" {
		t.Fatal("preflight lost authoritative fields, visibility or content hash")
	}
	requireMetadataEntryIdentities(t, request.Entries, entries)
	decoded, err := schema.DecodeAuthoritativePublishMetadataRaw(encodeMetadataLimitValue(t, request))
	if err != nil || !reflect.DeepEqual(decoded, request) {
		t.Fatalf("pinned schema raw decoder changed or refused the final request: %v", err)
	}
	return request
}

// The mechanics cases run every production mapper/preflight path with a small
// injected limit, so no 128 MiB document is built. Large fixtures are serial:
// these are memory-only paths with no database, transport, subprocess or
// concurrent state to model.
func TestPublicationMetadataLimits(t *testing.T) {
	fixture := loadPublicationMetadataLimits(t)
	assertPublicationMetadataPolicy(t, fixture)
	content := []byte(fixture.Recipe.Transcript)
	if _, err := schema.DecodeTranscriptContentRaw(content); err != nil {
		t.Fatalf("invalid transcript recipe: %v", err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			limit := c.LimitBytes
			if limit == 0 {
				limit = fixture.Mechanics.LimitBytes
			}
			count := c.EntryCount
			if count == 0 {
				count = 1
			}
			opts := metadataLimitOptions(t, fixture, count)
			base, err := mapMetadataWithPolicy(opts, limit)
			if err != nil {
				t.Fatal(err)
			}
			switch c.Path {
			case metadataMapperInput, metadataMapperOutput, metadataManyEntries:
				redactor := &metadataLimitRedactor{}
				if c.Path == metadataMapperInput {
					var document publishMirrorDocument
					if err := json.Unmarshal(base, &document); err != nil {
						t.Fatal(err)
					}
					text := strings.Repeat("x", c.DocumentBytes-len(base)+1)
					opts.Entries[0].ContentPreview = &text
					document.Entries = opts.Entries
					if actual := len(encodeMetadataLimitValue(t, document)); actual != c.DocumentBytes {
						t.Fatalf("mapper input bytes=%d want %d", actual, c.DocumentBytes)
					}
				} else if c.Path == metadataMapperOutput {
					// Calibrate the restored small document through the production
					// mapper first, including the same evidence-removing redactor.
					redactor.contentBytes = 1
					opts.Redactor = redactor
					restored, err := mapMetadataWithPolicy(opts, limit)
					if err != nil {
						t.Fatal(err)
					}
					redactor.contentBytes = c.DocumentBytes - len(restored) + 1
					redactor.called = false
				}
				opts.Redactor = redactor
				body, err := mapMetadataWithPolicy(opts, limit)
				if redactor.called != c.RedactorCalled {
					t.Fatalf("redactor called=%v want %v", redactor.called, c.RedactorCalled)
				}
				if !c.Accepted {
					if err == nil || !strings.Contains(err.Error(), c.ErrorContains) || body != nil {
						t.Fatalf("mapper refusal=%v emitted=%d want %q and no output", err, len(body), c.ErrorContains)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if c.Path == metadataManyEntries {
					if len(body) < c.MinimumDocumentBytes || len(body) > c.MaximumDocumentBytes {
						t.Fatalf("many-entry metadata bytes=%d outside [%d,%d]", len(body), c.MinimumDocumentBytes, c.MaximumDocumentBytes)
					}
					requireMetadataPreflight(t, body, content, opts.Entries)
				} else if len(body) != c.DocumentBytes {
					t.Fatalf("mapped output bytes=%d want %d", len(body), c.DocumentBytes)
				}
				var mirror publishMirrorDocument
				if err := json.Unmarshal(body, &mirror); err != nil {
					t.Fatal(err)
				}
				if len(mirror.Entries) != len(opts.Entries) {
					t.Fatalf("mapped entry count=%d want %d", len(mirror.Entries), len(opts.Entries))
				}
				for i, entry := range mirror.Entries {
					if entry.SessionID != opts.Entries[i].SessionID || entry.EntryIndex != i || !reflect.DeepEqual(entry.Extra, opts.Entries[i].Extra) {
						t.Fatalf("mapped entry %d lost identity or observed model evidence", i)
					}
				}
				t.Logf("mapped bytes=%d entries=%d", len(body), len(mirror.Entries))
			case metadataPreflightInput:
				metadata := append(base, bytes.Repeat([]byte(" "), c.DocumentBytes-len(base))...)
				if len(metadata) != c.DocumentBytes {
					t.Fatal("preflight input did not reach fixture byte target")
				}
				request, err := buildAuthoritativeRequestWithPolicy(metadata, content, limit)
				if !c.Accepted {
					if err == nil || !strings.Contains(err.Error(), c.ErrorContains) || !reflect.DeepEqual(request, schema.AuthoritativePublishRequest{}) {
						t.Fatalf("preflight input refusal=%v want %q and no request", err, c.ErrorContains)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				want := requireMetadataPreflight(t, base, content, opts.Entries)
				if !reflect.DeepEqual(request, want) {
					t.Fatal("whitespace boundary changed the authoritative request")
				}
			case metadataFinalPreflight:
				// Build an independent compact final-wire recipe from the valid
				// production request. Removing only hash and visibility gives a
				// smaller input; production preflight must put them back and scan
				// the growth, rather than accepting merely the initial document.
				request := requireMetadataPreflight(t, base, content, opts.Entries)
				final := encodeMetadataLimitValue(t, request)
				text := strings.Repeat("x", c.DocumentBytes-len(final)+1)
				request.Entries[0].ContentPreview = &text
				final = encodeMetadataLimitValue(t, request)
				if len(final) != c.DocumentBytes {
					t.Fatalf("final-wire recipe bytes=%d want %d", len(final), c.DocumentBytes)
				}
				var document map[string]json.RawMessage
				if err := json.Unmarshal(final, &document); err != nil {
					t.Fatal(err)
				}
				delete(document, "contentHash")
				delete(document, "visibilityIntent")
				metadata := encodeMetadataLimitValue(t, document)
				if len(metadata) >= c.DocumentBytes || len(metadata) > limit {
					t.Fatalf("final preflight recipe input bytes=%d must fit before promotion", len(metadata))
				}
				actual, err := buildAuthoritativeRequestWithPolicy(metadata, content, limit)
				if !c.Accepted {
					if err == nil || !strings.Contains(err.Error(), c.ErrorContains) || !reflect.DeepEqual(actual, schema.AuthoritativePublishRequest{}) {
						t.Fatalf("final preflight refusal=%v want %q and no request", err, c.ErrorContains)
					}
					return
				}
				if err != nil || !reflect.DeepEqual(actual, request) {
					t.Fatalf("final preflight changed or refused the full request: %v", err)
				}
				if len(encodeMetadataLimitValue(t, actual)) != c.DocumentBytes {
					t.Fatal("final request did not retain fixture encoded length")
				}
			}
		})
	}
}

// assertMetadataRealCapInput sizes entries[0] so the mapper assembles exactly
// target bytes, then asserts the serialized mirror length. The document is
// local, so its buffer is released before the production mapper builds its own.
func assertMetadataRealCapInput(t *testing.T, base []byte, entries []schema.SessionEntry, target int) {
	t.Helper()
	var document publishMirrorDocument
	if err := json.Unmarshal(base, &document); err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("x", target-len(base)+1)
	entries[0].ContentPreview = &text
	document.Entries = entries
	if actual := len(encodeMetadataLimitValue(t, document)); actual != target {
		t.Fatalf("mapper input bytes=%d want %d", actual, target)
	}
}

// TestPublicationMetadataCapMaterialization drives the one real-size metadata
// case through the PUBLIC production mapper: an accepted mirror document of
// exactly the literal 128 MiB cap, with a non-nil redactor. That proves both the
// mapper assembled-input scan and the redaction input cap accept a real-cap
// input. The former output-only refusal case could not observe this: it grew a
// small input after those scans, so a reintroduced smaller metadata input guard
// would have slipped through. Serial, non-race only; the race pass observes the
// same scans through the injected-limit mechanics above.
func TestPublicationMetadataCapMaterialization(t *testing.T) {
	if publicationRaceEnabled {
		t.Skip("the real-size metadata cap materialization runs in the non-race pass only")
	}
	fixture := loadPublicationMetadataLimits(t)
	if len(fixture.RealSizeCases) != 1 {
		t.Fatalf("metadata limits fixture declares %d real-size cases, want exactly 1", len(fixture.RealSizeCases))
	}
	for _, c := range fixture.RealSizeCases {
		t.Run(c.Name, func(t *testing.T) {
			if c.Path != metadataMapperInput {
				t.Fatalf("real-size case %q must drive the mapper input path", c.Name)
			}
			if !c.Accepted || !c.RedactorCalled {
				t.Fatalf("real-size case %q must be an accepted case that calls the redactor", c.Name)
			}
			if c.DocumentBytes != fixture.Policy.AtCapBytes {
				t.Fatalf("real-size input target=%d want the literal cap %d", c.DocumentBytes, fixture.Policy.AtCapBytes)
			}
			opts := metadataLimitOptions(t, fixture, 1)
			base, err := MapMetadata(opts)
			if err != nil {
				t.Fatal(err)
			}
			// The calibration reuses the small production base, so the test
			// builds no document beyond the production path's own.
			assertMetadataRealCapInput(t, base, opts.Entries, c.DocumentBytes)
			redactor := &metadataLimitRedactor{}
			opts.Redactor = redactor
			body, err := MapMetadata(opts)
			if !redactor.called {
				t.Fatalf("the redactor was not called: an at-cap input was refused before redaction: %v", err)
			}
			if err != nil {
				t.Fatalf("the mapper refused an at-cap input: %v", err)
			}
			// The redactor is the identity, so the accepted output is the
			// re-serialization of the accepted input: this is the actual input
			// length observed through the production path.
			if len(body) != c.DocumentBytes {
				t.Fatalf("mapper output bytes=%d want the accepted input literal %d", len(body), c.DocumentBytes)
			}
			var mirror publishMirrorDocument
			if err := json.Unmarshal(body, &mirror); err != nil {
				t.Fatal(err)
			}
			if len(mirror.Entries) != len(opts.Entries) {
				t.Fatalf("mapped entry count=%d want %d", len(mirror.Entries), len(opts.Entries))
			}
			for i, entry := range mirror.Entries {
				if entry.SessionID != opts.Entries[i].SessionID || entry.EntryIndex != i || !reflect.DeepEqual(entry.Extra, opts.Entries[i].Extra) {
					t.Fatalf("mapped entry %d lost identity or observed model evidence", i)
				}
			}
			t.Logf("at-cap mapper input accepted bytes=%d entries=%d", len(body), len(mirror.Entries))
		})
	}
}
