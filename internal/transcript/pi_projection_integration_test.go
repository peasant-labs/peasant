package transcript_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/pi_projection_integrated.yaml
var piProjectionYAML []byte

//go:embed testdata/pi_projection_integrated.manifest.yaml
var piProjectionManifest []byte

type piProjectionEntry struct {
	ID        string `yaml:"id"`
	Role      string `yaml:"role"`
	Type      string `yaml:"type"`
	Content   string `yaml:"content"`
	Usage     string `yaml:"usage"`
	Tool      string `yaml:"tool"`
	Parent    *int   `yaml:"parent"`
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
	Arguments string `yaml:"arguments"`
	Metadata  string `yaml:"metadata"`
	Kind      string `yaml:"kind"`
	Source    string `yaml:"source"`
	Custom    string `yaml:"custom"`
	Summary   bool   `yaml:"summary"`
	Carrier   bool   `yaml:"carrier"`
	NoOwner   bool   `yaml:"no_owner"`
}
type piProjectionCase struct {
	Name             string              `yaml:"name"`
	Entries          []piProjectionEntry `yaml:"entries"`
	WantTurns        int                 `yaml:"want_turns"`
	WantMetadata     int                 `yaml:"want_metadata"`
	WantScopes       []string            `yaml:"want_scopes"`
	WantCompleteness []string            `yaml:"want_completeness"`
	WantCosts        []string            `yaml:"want_costs"`
	WantContent      string              `yaml:"want_content"`
	Error            string              `yaml:"error"`
}

func TestPiProjectionSQLiteOutbound(t *testing.T) {
	var fixture struct {
		Cases         []piProjectionCase `yaml:"cases"`
		InvalidExtras []struct {
			Name  string `yaml:"name"`
			Extra string `yaml:"extra"`
		} `yaml:"invalid_extras"`
	}
	d := yaml.NewDecoder(bytes.NewReader(piProjectionYAML))
	d.KnownFields(true)
	if err := d.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		t.Fatalf("trailing fixture document: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(piProjectionManifest, "Pi projection")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for i, c := range fixture.Cases {
		names[i] = c.Name
	}
	for _, c := range fixture.InvalidExtras {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "Pi projection"); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			ctx := context.Background()
			sid := schema.SessionID(testutil.TestSessionUUID)
			path := storetest.CopyGoldenDB(t)
			db, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			seedPiStore(t, db, sid)
			// Exercise replacement, not only a first insert.
			prior := "obsolete conversation"
			if err := db.IndexSessionEntries(ctx, sid, []schema.SessionEntry{{SessionID: sid, EntryIndex: 99, Role: schema.RoleUser, EntryType: schema.EntryTypeText, ContentPreview: &prior}}); err != nil {
				t.Fatal(err)
			}
			entries := make([]schema.SessionEntry, len(c.Entries))
			for i, e := range c.Entries {
				entries[i] = piFixtureEntry(t, sid, i, e)
			}
			if err := db.IndexSessionEntries(ctx, sid, entries); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			stored, err := db.ListEntries(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := db.Pool().Take(ctx)
			if err != nil {
				t.Fatal(err)
			}
			matches := 0
			err = sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM session_entries_fts WHERE session_id = ? AND session_entries_fts MATCH 'obsolete OR state'`, &sqlitex.ExecOptions{Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error { matches = stmt.ColumnInt(0); return nil }})
			db.Pool().Put(conn)
			if err != nil {
				t.Fatal(err)
			}
			if matches != 0 {
				t.Fatal("stale conversation or private carrier data remains searchable")
			}
			if c.WantTurns == 0 {
				engine := metrics.NewEngine(db)
				engine.SetForce(true)
				if _, err := engine.ComputeMetrics(ctx, []ingest.SessionID{sid}); err != nil {
					t.Fatal(err)
				}
				m, err := db.GetMetrics(ctx, sid)
				if err != nil || m == nil || m.TurnCount == nil || *m.TurnCount != 0 {
					t.Fatalf("carrier-only metrics not empty: %+v error=%v", m, err)
				}
			}
			p, err := transcript.EntriesToProjectionValidated(stored, transcript.ProjectionOptions{Harness: schema.HarnessPi})
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Turns) != c.WantTurns || len(p.NativeMetadata) != c.WantMetadata {
				t.Fatalf("turns/metadata=%d/%d want %d/%d", len(p.Turns), len(p.NativeMetadata), c.WantTurns, c.WantMetadata)
			}
			if c.WantContent != "" && p.Turns[0].Content != c.WantContent {
				t.Fatalf("folded content=%q want %q", p.Turns[0].Content, c.WantContent)
			}
			if len(p.UsageOwners) != len(c.WantScopes) {
				t.Fatalf("owners=%d want %d", len(p.UsageOwners), len(c.WantScopes))
			}
			for i, u := range p.UsageOwners {
				cost := ""
				if u.Cost != nil && u.Cost.Total != nil {
					cost = string(*u.Cost.Total)
				}
				if string(u.Scope) != c.WantScopes[i] || string(u.Completeness) != c.WantCompleteness[i] || cost != c.WantCosts[i] {
					t.Fatalf("owner %d = %+v cost %s", i, u, cost)
				}
				if _, ok := p.SourceMap[u.SourceEntryRef]; !ok {
					t.Fatalf("owner has no exact source target: %s", u.SourceEntryRef)
				}
			}
			meta := &ingest.UnifiedMetadata{SessionID: sid, ModelHarness: schema.HarnessPi}
			content, err := push.BuildTranscriptContentValidated(meta, stored, defaults.PublishSchemaVersion, config.PushFieldVisibility{}, sessionorigin.Unknown)
			if c.Error != "" {
				if err == nil || !strings.Contains(err.Error(), c.Error) {
					t.Fatalf("lossy namespace conversion was not refused: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(content)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := schema.DecodeTranscriptContentRaw(raw)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded.SessionDetail.NativeMetadata, p.NativeMetadata) {
				t.Fatal("outbound metadata changed")
			}
			provider := api.NewStoreDataProvider(db, sessionvisibility.All())
			local, err := provider.SessionByID(ctx, string(sid))
			if err != nil {
				t.Fatal(err)
			}
			localDetail := api.SessionToDetail(local)
			exported, err := export.ExportSession(ctx, db, testutil.NewMemFS(), string(sid))
			if err != nil {
				t.Fatal(err)
			}
			if localDetail == nil || !reflect.DeepEqual(localDetail.Turns, decoded.SessionDetail.Turns) || !reflect.DeepEqual(exported.Turns, localDetail.Turns) || !reflect.DeepEqual(exported.NativeMetadata, localDetail.NativeMetadata) {
				t.Fatal("local detail, export and upload diverged after reopening SQLite")
			}
			if bytes.Contains(raw, []byte("pi.carrier")) || bytes.Contains(raw, []byte("obsolete conversation")) || bytes.Contains(raw, []byte("\"extra\"")) {
				t.Fatal("private/stale evidence escaped")
			}
		})
	}
	for _, c := range fixture.InvalidExtras {
		t.Run(c.Name, func(t *testing.T) {
			ctx := context.Background()
			sid := schema.SessionID(testutil.TestSessionUUID)
			db := storetest.Open(t)
			seedPiStore(t, db, sid)
			entry := piFixtureEntry(t, sid, 0, piProjectionEntry{ID: "a", Role: "assistant", Content: "preserved"})
			extra, _, err := ingest.DecodePiExtra(entry.Extra)
			if err != nil {
				t.Fatal(err)
			}
			extra.ModelID = "fixture-model"
			entry.Extra, err = ingest.EncodePiExtra(extra)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.IndexSessionEntries(ctx, sid, []schema.SessionEntry{entry}); err != nil {
				t.Fatal(err)
			}
			bad := entry
			bad.Extra = &c.Extra
			if err := db.IndexSessionEntries(ctx, sid, []schema.SessionEntry{bad}); err == nil {
				t.Fatal("raw invalid Pi Extra reached storage maps")
			}
			stored, err := db.ListEntries(ctx, sid)
			if err != nil || len(stored) != 1 || *stored[0].ContentPreview != "preserved" {
				t.Fatalf("failed replacement changed prior state: %v", err)
			}
			// Simulate old/corrupt persisted input at the read boundary, retaining the
			// model ext row that used to trigger a lossy generic map merge.
			conn, err := db.Pool().Take(ctx)
			if err != nil {
				t.Fatal(err)
			}
			err = sqlitex.ExecuteTransient(conn, `UPDATE session_entries SET extra=? WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{c.Extra, string(sid)}})
			db.Pool().Put(conn)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.ListEntries(ctx, sid); err == nil {
				t.Fatal("raw invalid Pi Extra collapsed during read rehydration")
			}
			if _, err := db.ListEntriesRange(ctx, sid, 0, 0); err == nil {
				t.Fatal("range read bypassed raw Extra validation")
			}
		})
	}
}

func seedPiStore(t *testing.T, db *store.Store, sid schema.SessionID) {
	t.Helper()
	storetest.SeedSession(t, db, string(sid))
	ingested := int64(3)
	meta := &ingest.UnifiedMetadata{SessionID: sid, ModelHarness: schema.HarnessPi, Model: "fixture-model", HostSlug: "testslug", Project: schema.ProjectContext{Hash: "testprojhash0000000000000000000000000000000000000000000000000000", Name: "testproj", FilePath: "/fixture"}, Timestamp: schema.TimestampInfo{Start: 1, End: 2, Ingested: &ingested}, Source: schema.SourceInfo{FilePath: "/fixture.jsonl", Format: schema.SourceFormatJSONL}}
	if err := db.InsertSessions(context.Background(), []ingest.StoreEntry{{Metadata: meta}}); err != nil {
		t.Fatal(err)
	}
}

func piFixtureEntry(t *testing.T, sid schema.SessionID, index int, e piProjectionEntry) schema.SessionEntry {
	t.Helper()
	role := schema.Role(e.Role)
	if !role.IsValid() {
		t.Fatal("invalid fixture role")
	}
	var err error
	kind := schema.EntryTypeText
	if e.Type != "" {
		kind = schema.EntryType(e.Type)
		if !kind.IsValid() {
			t.Fatal("invalid fixture entry type")
		}
	}
	extra := ingest.PiExtra{Kind: ingest.PiExtraState, Harness: schema.HarnessPi, SourceRef: ingest.PiPublicRef(string(sid), "entry", e.ID)}
	extra.Namespace = e.Namespace
	entry := schema.SessionEntry{SessionID: sid, Harness: schema.HarnessPi, EntryIndex: index, Role: role, EntryType: kind, ContentPreview: &e.Content, ParentIndex: e.Parent}
	if e.Parent != nil {
		entry.Depth = 1
	}
	if e.Tool != "" {
		id := ingest.PiPublicRef(string(sid), "tool", e.Tool)
		entry.ToolCallID = &id
		entry.ToolNamesCSV = &e.Name
		entry.ToolInput = &e.Arguments
		if kind == schema.EntryTypeToolResult {
			entry.ToolOutput = &e.Content
		}
	}
	if !e.NoOwner && ((role == schema.RoleAssistant && kind != schema.EntryTypeToolUse) || kind == schema.EntryTypeToolResult || e.Summary) {
		scope := schema.UsageScopeAssistant
		if kind == schema.EntryTypeToolResult {
			scope = schema.UsageScopeTool
		}
		if e.Summary {
			scope = schema.UsageScopeSummary
		}
		u, err := ingest.PiUsageFromRaw(string(sid), e.ID, scope, json.RawMessage(e.Usage))
		if err != nil {
			t.Fatal(err)
		}
		extra.Usage = &u
	}
	if e.Metadata != "" {
		k, err := schema.NewNativeMetadataKind(e.Kind)
		if err != nil {
			t.Fatal(err)
		}
		source, err := schema.NewNativeMetadataSourceType(e.Source)
		if err != nil {
			t.Fatal(err)
		}
		m := schema.NativeMetadataRecord{ID: ingest.PiPublicRef(string(sid), "metadata", e.ID), Kind: k, Source: schema.NativeSourceRef{EntryRef: extra.SourceRef, SourceType: source}, Data: json.RawMessage(e.Metadata), CustomType: e.Custom}
		if kind == schema.EntryTypeToolResult {
			m.Source.MessageRole = schema.NativePiMessageRoleToolResult
		}
		extra.Metadata = []schema.NativeMetadataRecord{m}
	}
	if e.Carrier {
		extra.Kind = ingest.PiExtraCarrier
		entry, err = ingest.NewPiCarrier(sid, index, extra)
	} else {
		entry.Extra, err = ingest.EncodePiExtra(extra)
	}
	if err != nil {
		t.Fatal(err)
	}
	return entry
}
