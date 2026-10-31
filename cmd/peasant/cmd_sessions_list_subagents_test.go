package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

//go:embed testdata/sessions_list_subagents.yaml
var sessionsListSubagentsYAML []byte

//go:embed testdata/sessions_list_subagents.manifest.yaml
var sessionsListSubagentsManifestYAML []byte

type subagentSeed struct {
	ID              string `yaml:"id"`
	Start           int64  `yaml:"start"`
	Parent          string `yaml:"parent"`
	ParentNotStored string `yaml:"parentNotStored"`
	Unresolved      bool   `yaml:"unresolved"`
	Root            string `yaml:"root"`
}

type subagentWantRow struct {
	ID               string `yaml:"id"`
	ParentSessionID  string `yaml:"parentSessionId"`
	ParentUnresolved bool   `yaml:"parentUnresolved"`
	RootSessionID    string `yaml:"rootSessionId"`
}

type subagentCase struct {
	Name       string            `yaml:"name"`
	Args       []string          `yaml:"args"`
	Want       []string          `yaml:"want"`
	WantAbsent []string          `yaml:"wantAbsent"`
	WantRows   []subagentWantRow `yaml:"wantRows"`
}

type subagentFixture struct {
	Sessions []subagentSeed `yaml:"sessions"`
	Cases    []subagentCase `yaml:"cases"`
}

func loadSubagentFixture(t *testing.T) subagentFixture {
	t.Helper()
	var fixture subagentFixture
	if err := yaml.Unmarshal(sessionsListSubagentsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Required []string `yaml:"required"`
	}
	if err := yaml.Unmarshal(sessionsListSubagentsManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, name := range manifest.Required {
		if !slices.ContainsFunc(fixture.Cases, func(c subagentCase) bool { return c.Name == name }) {
			t.Errorf("sessions_list_subagents.yaml lost the required case %q", name)
		}
	}
	return fixture
}

// seedSubagentStore stores the fixture's sessions and arranges their parent
// evidence directly, as the store's own parent tests do.
func seedSubagentStore(t *testing.T, dir string, fixture subagentFixture) {
	t.Helper()
	sessions := make([]seedOpts, len(fixture.Sessions))
	for i, s := range fixture.Sessions {
		sessions[i] = seedOpts{SessionID: s.ID, StartMs: s.Start * 1_700_000_000, TurnCount: 2, FirstUserMsg: "prompt of " + s.ID}
	}
	mustSeedMany(t, dir, sessions)
	db := openSeedStore(t, dir, "seedSubagentStore")
	defer db.Close()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	exec := func(query string, args ...any) {
		t.Helper()
		if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range fixture.Sessions {
		switch {
		case s.Parent != "":
			exec(`UPDATE sessions SET parent_id = ? WHERE session_id = ?`, s.Parent, s.ID)
		case s.ParentNotStored != "":
			exec(`UPDATE sessions SET latest_attempt_generation_id = 'attempt' WHERE session_id = ?`, s.ID)
			exec(`INSERT INTO session_relationship_evidence(session_id,generation_id,ordinal,kind,target_state,target_local_id,evidence) VALUES(?,'attempt',0,'started_by','target_known_retained',?,'existing_adapter')`, s.ID, s.ParentNotStored)
		case s.Unresolved:
			exec(`UPDATE sessions SET latest_attempt_generation_id = 'attempt' WHERE session_id = ?`, s.ID)
			exec(`INSERT INTO session_relationship_evidence(session_id,generation_id,ordinal,kind,target_state,evidence) VALUES(?,'attempt',0,'started_by','unknown','existing_adapter')`, s.ID)
		}
		if s.Root != "" {
			exec(`UPDATE sessions SET root_session_id = ? WHERE session_id = ?`, s.Root, s.ID)
		}
	}
}

func listSubagentRows(t *testing.T, dir string, args []string) ([]map[string]json.RawMessage, []string) {
	t.Helper()
	out, err := executeWithDataDir(t, BuildSessionsCommand(), dir, append([]string{"list", "--json"}, args...))
	if err != nil {
		t.Fatalf("sessions list %v: %v\n%s", args, err, out)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("sessions list %v: not JSON: %v\n%s", args, err, out)
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		if err := json.Unmarshal(row["id"], &ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	return rows, ids
}

func TestSessionsListSubagents(t *testing.T) {
	t.Parallel()
	fixture := loadSubagentFixture(t)
	dir := t.TempDir()
	seedSubagentStore(t, dir, fixture)

	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			rows, ids := listSubagentRows(t, dir, tc.Args)
			if tc.Want != nil && !slices.Equal(ids, tc.Want) {
				t.Errorf("ids = %v, want %v", ids, tc.Want)
			}
			for _, absent := range tc.WantAbsent {
				if slices.Contains(ids, absent) {
					t.Errorf("%s is listed by default: %v", absent, ids)
				}
			}
			for _, row := range rows {
				for _, key := range []string{"parentSessionId", "parentUnresolved", "rootSessionId"} {
					if _, ok := row[key]; !ok {
						t.Errorf("row %s has no %q key", row["id"], key)
					}
				}
			}
			byID := map[string]sessionListEntry{}
			for _, row := range rows {
				raw, err := json.Marshal(row)
				if err != nil {
					t.Fatal(err)
				}
				var entry sessionListEntry
				if err := json.Unmarshal(raw, &entry); err != nil {
					t.Fatal(err)
				}
				byID[entry.ID] = entry
			}
			for _, want := range tc.WantRows {
				got, ok := byID[want.ID]
				if !ok {
					t.Errorf("row %s is missing", want.ID)
					continue
				}
				if deref(got.ParentSessionID) != want.ParentSessionID || got.ParentUnresolved != want.ParentUnresolved || deref(got.RootSessionID) != want.RootSessionID {
					t.Errorf("row %s = parent %q unresolved %v root %q, want %+v", want.ID, deref(got.ParentSessionID), got.ParentUnresolved, deref(got.RootSessionID), want)
				}
			}
		})
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// TestSessionsListMatchesTheLocalAPIPredicate proves the default list holds
// exactly the sessions the local API's identity read calls roots: the web list
// and the CLI list cannot disagree about which session is a child.
func TestSessionsListMatchesTheLocalAPIPredicate(t *testing.T) {
	t.Parallel()
	fixture := loadSubagentFixture(t)
	dir := t.TempDir()
	seedSubagentStore(t, dir, fixture)

	_, allIDs := listSubagentRows(t, dir, []string{"--include-subagents"})
	_, mainIDs := listSubagentRows(t, dir, nil)

	db := openSeedStore(t, dir, "TestSessionsListMatchesTheLocalAPIPredicate")
	defer db.Close()
	requested := make([]ingest.PushSessionRow, 0, len(allIDs))
	full, err := db.ListSessionsFiltered(t.Context(), store.SessionListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range full {
		requested = append(requested, ingest.PushSessionRow{SessionID: row.SessionID, ParentID: deref(row.ParentID)})
	}
	identities, err := db.SyncParentIdentities(t.Context(), requested)
	if err != nil {
		t.Fatal(err)
	}
	var roots []string
	for _, identity := range identities {
		if identity.ParentIdentity == "" && !identity.ParentUnresolved {
			roots = append(roots, identity.SessionID)
		}
	}
	got := slices.Sorted(slices.Values(mainIDs))
	want := slices.Sorted(slices.Values(roots))
	if !slices.Equal(got, want) {
		t.Fatalf("main sessions %s, local API roots %s", strings.Join(got, ","), strings.Join(want, ","))
	}
	if len(got) == len(allIDs) {
		t.Fatal(fmt.Sprintf("the fixture holds no child: %v", allIDs))
	}
}
