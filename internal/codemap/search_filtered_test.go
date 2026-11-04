package codemap_test

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/search_filtered.yaml
var searchFilteredYAML []byte

//go:embed testdata/search_filtered.manifest.yaml
var searchFilteredManifestYAML []byte

type filteredSearchState string

const (
	filteredSearchBoth           filteredSearchState = ""
	filteredSearchLegacyFallback filteredSearchState = "legacy_fallback"
	filteredSearchLegacyOnly     filteredSearchState = "legacy_only"
	filteredSearchUnifiedOnly    filteredSearchState = "unified_only"
	filteredSearchNeedsRebuild   filteredSearchState = "needs_rebuild"
	filteredSearchNoIndex        filteredSearchState = "no_index"
)

type filteredSearchFilters struct {
	Project *string  `yaml:"project"`
	Harness *string  `yaml:"harness"`
	Since   *int64   `yaml:"since"`
	Until   *int64   `yaml:"until"`
	Tag     *string  `yaml:"tag"`
	IDs     []string `yaml:"ids"`
}

func (f filteredSearchFilters) filters() codemap.SearchFilters {
	return codemap.SearchFilters{ProjectName: f.Project, Harness: f.Harness, StartFromMs: f.Since, StartBeforeMs: f.Until, Tag: f.Tag, SessionIDs: f.IDs}
}

type filteredSearchCase struct {
	Name               string                `yaml:"name"`
	Query              string                `yaml:"query"`
	Window             int                   `yaml:"window"`
	Offset             int                   `yaml:"offset"`
	State              filteredSearchState   `yaml:"state"`
	Filters            filteredSearchFilters `yaml:"filters"`
	Want               []string              `yaml:"want"`
	WantAny            bool                  `yaml:"wantAny"`
	Identity           bool                  `yaml:"identity"`
	Walk               bool                  `yaml:"walk"`
	ErrorContains      string                `yaml:"errorContains"`
	ProbeErrorContains string                `yaml:"probeErrorContains"`
}

type filteredSearchFamily struct {
	Sessions []struct {
		ID      string   `yaml:"id"`
		Entries []string `yaml:"entries"`
		Body    bool     `yaml:"body"`
	} `yaml:"sessions"`
	Cases  []filteredSearchCase `yaml:"cases"`
	Guards []struct {
		Name          string `yaml:"name"`
		Window        int    `yaml:"window"`
		Offset        int    `yaml:"offset"`
		ErrorContains string `yaml:"errorContains"`
	} `yaml:"guards"`
	Identities []struct {
		Name  string `yaml:"name"`
		Query string `yaml:"query"`
		Want  bool   `yaml:"want"`
	} `yaml:"identities"`
}

func loadFilteredSearchFixtures(t *testing.T) filteredSearchFamily {
	t.Helper()
	var family filteredSearchFamily
	decoder := yaml.NewDecoder(bytes.NewReader(searchFilteredYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&family); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("search filter fixture must contain exactly one document: %v", err)
	}
	names := make([]string, 0, len(family.Cases)+len(family.Guards)+len(family.Identities))
	for _, c := range family.Cases {
		names = append(names, c.Name)
		if c.Window <= 0 || c.Window > codemap.SearchMaxLimit || c.Offset < 0 {
			t.Fatalf("%s: invalid success-case window/offset", c.Name)
		}
		switch c.State {
		case filteredSearchBoth, filteredSearchLegacyFallback, filteredSearchLegacyOnly, filteredSearchUnifiedOnly, filteredSearchNeedsRebuild, filteredSearchNoIndex:
		default:
			t.Fatalf("%s: unknown index state %q", c.Name, c.State)
		}
		if c.ErrorContains == "" && c.Want == nil {
			t.Fatalf("%s: missing expected coordinates", c.Name)
		}
	}
	for _, c := range family.Guards {
		names = append(names, c.Name)
		if c.ErrorContains == "" {
			t.Fatalf("%s: guard has no refusal assertion", c.Name)
		}
	}
	for _, c := range family.Identities {
		names = append(names, c.Name)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(searchFilteredManifestYAML, "filtered search")
	if err != nil {
		t.Fatal(err)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "filtered search"); err != nil {
		t.Fatal(err)
	}
	return family
}

func setFilteredSearchState(t *testing.T, s *store.Store, state filteredSearchState) {
	t.Helper()
	conn, err := s.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool().Put(conn)
	if !store.LegacySearchIndexExists(conn) || !store.UnifiedSearchIndexExists(conn) {
		t.Fatal("filtered search fixtures require both transition indexes before state mutation")
	}
	var sql string
	switch state {
	case filteredSearchBoth:
		return
	case filteredSearchLegacyFallback:
		sql = `INSERT INTO session_search_fts(session_search_fts) VALUES('delete-all')`
	case filteredSearchLegacyOnly:
		sql = `DROP TABLE session_search_fts`
	case filteredSearchUnifiedOnly:
		sql = `DROP TABLE session_entries_fts`
	case filteredSearchNeedsRebuild:
		if err := store.SearchStateSetNeedsRebuild(t.Context(), conn); err != nil {
			t.Fatal(err)
		}
		return
	case filteredSearchNoIndex:
		if err := sqlitex.ExecuteTransient(conn, `DROP TABLE session_search_fts`, nil); err != nil {
			t.Fatal(err)
		}
		sql = `DROP TABLE session_entries_fts`
	default:
		t.Fatalf("unhandled index state %q", state)
	}
	if err := sqlitex.ExecuteTransient(conn, sql, nil); err != nil {
		t.Fatal(err)
	}
}

func searchCoordinates(results []schema.SearchResult) []string {
	coords := make([]string, 0, len(results))
	for _, result := range results {
		coords = append(coords, fmt.Sprintf("%s:%d", result.SessionID, result.EntryIndex))
	}
	return coords
}

func openFilteredSearchStore(t *testing.T) *store.Store {
	t.Helper()
	root := filepath.Join(t.TempDir(), "artifacts")
	artifacts, err := store.NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := store.NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	return storetest.OpenWith(t, store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locker))
}

func seedFilteredSearchBody(t *testing.T, s *store.Store, sid schema.SessionID, start int64, texts []string) {
	t.Helper()
	entries := make([]schema.SessionEntry, 0, len(texts))
	content := make([]indexformat.ContentRecord, 0, len(texts))
	blobs := make(map[schema.SourceEntryRef][]byte, len(texts))
	for i, text := range texts {
		ref := schema.SourceEntryRef(fmt.Sprintf("filtered-%d", i))
		entries = append(entries, schema.SessionEntry{
			SessionID: sid, EntryIndex: i, Harness: ingest.HarnessClaudeCode,
			EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &text, SourceEntryRef: ref,
		})
		content = append(content, indexformat.ContentRecord{Ref: ref})
		blobs[ref] = []byte(text)
	}
	inputCount := int64(len(texts))
	generation := indexformat.Generation{
		ID: "filtered-search", Completeness: indexformat.GenerationCompletenessComplete,
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion, SessionID: sid, ModelHarness: ingest.HarnessClaudeCode,
			Stats:     schema.SessionStats{TurnCount: len(entries), InputSubmissionCount: &inputCount},
			Timestamp: schema.TimestampInfo{Start: start, End: start + 500},
		},
		Main: indexformat.Partition{Entries: entries}, Content: content,
		SourceEvidenceDigest: strings.Repeat("a", 64), TitleRefs: []schema.SourceEntryRef{entries[0].SourceEntryRef},
	}
	if _, err := s.ActivateGeneration(t.Context(), store.GenerationActivation{
		Generation: indexformat.V2{Generation: generation}, Blobs: blobs, IndexerVersion: 1, IndexedAtMs: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSearchFilteredFixtures(t *testing.T) {
	t.Parallel()
	family := loadFilteredSearchFixtures(t)
	for _, c := range family.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			s := openFilteredSearchStore(t)
			for i, session := range family.Sessions {
				sid, err := schema.NewSessionID(session.ID)
				if err != nil {
					t.Fatal(err)
				}
				start := fxBase() + int64(i)*1000
				seedSession(t, s, session.ID, "", start, start+500)
				specs := make([]entrySpec, 0, len(session.Entries))
				for _, text := range session.Entries {
					specs = append(specs, userTurn(start, text))
				}
				if session.Body {
					seedFilteredSearchBody(t, s, sid, start, session.Entries)
				} else {
					seedEntries(t, s, session.ID, specs)
				}
			}
			// Search needs neither repository nor graph dependencies.
			svc := codemap.NewService(s, nil, nil)
			ctx := t.Context()
			setFilteredSearchState(t, s, c.State)
			f := c.Filters.filters()
			got, err := svc.SearchRankedWindowFiltered(ctx, c.Query, c.Window, c.Offset, f)
			if c.ErrorContains != "" {
				if err == nil || !strings.Contains(err.Error(), c.ErrorContains) {
					t.Fatalf("ranked read error = %v, want %q", err, c.ErrorContains)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got.Query != c.Query || got.Results == nil {
					t.Fatalf("payload must preserve raw query and a non-nil result set: %+v", got)
				}
				if coords := searchCoordinates(got.Results); !reflect.DeepEqual(coords, c.Want) {
					t.Fatalf("coordinates = %v, want %v", coords, c.Want)
				}
				if codemap.IsIdentityQuery(c.Query) != c.Identity {
					t.Fatalf("identity predicate = %v, want %v", codemap.IsIdentityQuery(c.Query), c.Identity)
				}
				// Pin all result fields, including scores and order, against the
				// unfiltered production path. Filters must not recompute relevance.
				baseline, err := svc.Search(ctx, c.Query, codemap.SearchMaxLimit)
				if err != nil {
					t.Fatal(err)
				}
				selected := make([]schema.SearchResult, 0, len(c.Want))
				for _, result := range baseline.Results {
					coord := fmt.Sprintf("%s:%d", result.SessionID, result.EntryIndex)
					for _, want := range c.Want {
						if coord == want {
							selected = append(selected, result)
						}
					}
				}
				if !reflect.DeepEqual(got.Results, selected) {
					t.Fatalf("filtered result fields = %+v, want unfiltered fields %+v", got.Results, selected)
				}
				if f.IsZero() {
					old, err := svc.SearchRankedWindow(ctx, c.Query, c.Window, c.Offset)
					if err != nil || !reflect.DeepEqual(got, old) {
						t.Fatalf("zero-filter payload = %+v, old payload = %+v, error = %v", got, old, err)
					}
				}
				if c.Walk {
					whole, err := svc.SearchRankedWindowFiltered(ctx, c.Query, codemap.SearchMaxLimit, 0, f)
					if err != nil {
						t.Fatal(err)
					}
					walked := make([]schema.SearchResult, 0, len(whole.Results))
					for offset := 0; offset <= len(whole.Results); offset += c.Window {
						page, err := svc.SearchRankedWindowFiltered(ctx, c.Query, c.Window, offset, f)
						if err != nil {
							t.Fatal(err)
						}
						walked = append(walked, page.Results...)
					}
					if !reflect.DeepEqual(walked, whole.Results) {
						t.Fatalf("page walk = %+v, want %+v", walked, whole.Results)
					}
				}
			}
			any, probeErr := svc.SearchMatchesAny(ctx, c.Query, f)
			wantProbeError := c.ProbeErrorContains
			if wantProbeError == "" {
				wantProbeError = c.ErrorContains
			}
			if wantProbeError != "" {
				if probeErr == nil || !strings.Contains(probeErr.Error(), wantProbeError) {
					t.Fatalf("probe error = %v, want %q", probeErr, wantProbeError)
				}
			} else if probeErr != nil || any != c.WantAny {
				t.Fatalf("probe = %v, %v; want %v, nil", any, probeErr, c.WantAny)
			}
		})
	}
}

func TestSearchFilteredBoundaryFixtures(t *testing.T) {
	t.Parallel()
	family := loadFilteredSearchFixtures(t)
	// A nil store makes any accidental I/O at an invalid boundary fail loudly.
	svc := codemap.NewService(nil, nil, nil)
	for _, c := range family.Guards {
		t.Run(c.Name, func(t *testing.T) {
			_, err := svc.SearchRankedWindowFiltered(context.Background(), "cache", c.Window, c.Offset, codemap.SearchFilters{})
			if err == nil || !strings.Contains(err.Error(), c.ErrorContains) {
				t.Fatalf("error = %v, want %q", err, c.ErrorContains)
			}
			_, oldErr := svc.SearchRankedWindow(context.Background(), "cache", c.Window, c.Offset)
			if oldErr == nil || oldErr.Error() != err.Error() {
				t.Fatalf("unfiltered boundary error = %v, want %v", oldErr, err)
			}
		})
	}
	for _, c := range family.Identities {
		t.Run(c.Name, func(t *testing.T) {
			if got := codemap.IsIdentityQuery(c.Query); got != c.Want {
				t.Fatalf("IsIdentityQuery(%q) = %v, want %v", c.Query, got, c.Want)
			}
		})
	}
}
