package push

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
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
	Cases []publicationMetadataLimitCase `yaml:"cases"`
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
	names := make([]string, len(fixture.Cases))
	for i, c := range fixture.Cases {
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

// Large fixtures are serial: these are memory-only production mapper/preflight
// paths, with no database, transport, subprocess or concurrent state to model.
func TestPublicationMetadataLimits(t *testing.T) {
	fixture := loadPublicationMetadataLimits(t)
	content := []byte(fixture.Recipe.Transcript)
	if _, err := schema.DecodeTranscriptContentRaw(content); err != nil {
		t.Fatalf("invalid transcript recipe: %v", err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			count := c.EntryCount
			if count == 0 {
				count = 1
			}
			opts := metadataLimitOptions(t, fixture, count)
			base, err := MapMetadata(opts)
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
					restored, err := MapMetadata(opts)
					if err != nil {
						t.Fatal(err)
					}
					redactor.contentBytes = c.DocumentBytes - len(restored) + 1
					redactor.called = false
				}
				opts.Redactor = redactor
				body, err := MapMetadata(opts)
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
				request, err := buildAuthoritativeRequest(metadata, content)
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
				if len(metadata) >= c.DocumentBytes || len(metadata) > 134217728 {
					t.Fatalf("final preflight recipe input bytes=%d must fit before promotion", len(metadata))
				}
				actual, err := buildAuthoritativeRequest(metadata, content)
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
