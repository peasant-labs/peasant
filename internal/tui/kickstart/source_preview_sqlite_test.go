package kickstart_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

//go:embed testdata/source_preview_sqlite.yaml
var sourcePreviewSQLiteData []byte

// sourcePreviewSQLiteCase drives one preview over a synthetic OpenCode SQLite
// source. The synthetic database is materialized by the testfixture package and
// discovered by the production adapter, so the listing carries the database
// path exactly as kickstart would.
type sourcePreviewSQLiteCase struct {
	Name          string                   `yaml:"name"`
	SourceFixture string                   `yaml:"source_fixture"`
	Origin        ftue.SessionSourceOrigin `yaml:"origin"`
	MinTurns      int                      `yaml:"min_turns"`
}

type sourcePreviewSQLiteDoc struct {
	RequiredCases []string                  `yaml:"required_cases"`
	Cases         []sourcePreviewSQLiteCase `yaml:"cases"`
}

func loadSourcePreviewSQLiteDoc(t *testing.T) sourcePreviewSQLiteDoc {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(sourcePreviewSQLiteData))
	decoder.KnownFields(true)
	var doc sourcePreviewSQLiteDoc
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("decode SQLite preview fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("SQLite preview fixture must hold exactly one document")
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("SQLite preview fixture declares no required cases")
	}
	seen := make(map[string]struct{}, len(doc.Cases))
	for _, c := range doc.Cases {
		if c.Name == "" || c.SourceFixture == "" || c.Origin == "" || c.MinTurns < 1 {
			t.Fatalf("SQLite preview fixture has an incomplete case: %+v", c)
		}
		if err := c.Origin.Validate(); err != nil {
			t.Fatalf("SQLite preview fixture case %q has an unsupported origin %q", c.Name, c.Origin)
		}
		if _, dup := seen[c.Name]; dup {
			t.Fatalf("SQLite preview fixture has a duplicate case name %q", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	for _, name := range doc.RequiredCases {
		if _, ok := seen[name]; !ok {
			t.Fatalf("SQLite preview fixture is missing required case %q", name)
		}
	}
	return doc
}

type fixedOpenCodeEnv struct{}

func (fixedOpenCodeEnv) LookupEnv(string) (string, bool) { return "", false }

// discoverOneSQLiteSession materializes the named synthetic database, discovers
// it with the production adapter, and returns the single discovered session. The
// session's SourcePath is the provider database file, exactly as kickstart holds
// it before ingest.
func discoverOneSQLiteSession(t *testing.T, fixtureName string, wantOrigin ftue.SessionSourceOrigin) ingest.DiscoveredSession {
	t.Helper()
	materialized := testfixture.MaterializeByName(t, fixtureName)
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic SQLite root: %v", err)
	}
	adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(
		&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{}, "latest",
		fixedOpenCodeEnv{}, &ingest.OSFileSystem{}, ingest.OpenOpenCodeSQLiteSource,
		ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("construct discovery adapter: %v", err)
	}
	discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatalf("discover synthetic SQLite session: %v", err)
	}
	for _, session := range discovered {
		if kickstart.ListingSource(session).Origin == wantOrigin {
			return session
		}
	}
	t.Fatalf("discovery found no %q session in %d results", wantOrigin, len(discovered))
	return ingest.DiscoveredSession{}
}

// TestSourceTurns_PreviewsUningestedSQLiteSessionThroughMaterializer proves the
// crash fix: previewing an un-ingested OpenCode SQLite session reads the one
// selected session through the materializer and returns real turns, rather than
// reading the whole database at the source path. The mutation that restores the
// raw IndexTranscript-on-path read fails this case, because that read sizes and
// reads the database file and returns an error instead of turns.
func TestSourceTurns_PreviewsUningestedSQLiteSessionThroughMaterializer(t *testing.T) {
	t.Parallel()
	doc := loadSourcePreviewSQLiteDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			session := discoverOneSQLiteSession(t, c.SourceFixture, c.Origin)
			listing := ftue.SessionListing{
				Harness:   string(session.Harness),
				SessionID: string(session.SessionID),
				Source:    kickstart.ListingSource(session),
			}
			if listing.Source.Origin != c.Origin {
				t.Fatalf("listing origin %q, want %q", listing.Source.Origin, c.Origin)
			}
			reader := kickstart.NewSourceTurns(&ingest.OSFileSystem{}, []ftue.SessionListing{listing},
				kickstart.WithSourceTurnsGitResolver(testutil.NoGitResolver()),
				kickstart.WithSourceTurnsSalt(salt.Salt{}))

			turns, err := reader.Turns(listing.SessionID)
			if err != nil {
				t.Fatalf("preview un-ingested SQLite session: %v", err)
			}
			if len(turns) < c.MinTurns {
				t.Fatalf("preview produced %d turns, want at least %d", len(turns), c.MinTurns)
			}
		})
	}
}
