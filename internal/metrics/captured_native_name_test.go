package metrics_test

import (
	"context"
	_ "embed"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/captured_native_name.yaml
var capturedNativeNameYAML []byte

// nativeNameArm names what the indexed rows record about the harness-native
// session name. It is the axis the fixture varies; every arm must appear.
type nativeNameArm string

const (
	// nativeNameAbsent: no row records a name, so generated prose stands.
	nativeNameAbsent nativeNameArm = "absent"
	// nativeNameApplied: a row records a name that needs no redaction.
	nativeNameApplied nativeNameArm = "applied"
	// nativeNameCleared: a row records an empty name, an explicit removal.
	nativeNameCleared nativeNameArm = "cleared"
	// nativeNameSanitized: a row records a name holding a real project path.
	nativeNameSanitized nativeNameArm = "sanitized"
)

type capturedNativeNameCase struct {
	Name          string        `yaml:"name"`
	Arm           nativeNameArm `yaml:"arm"`
	UserProse     string        `yaml:"userProse"`
	NativeName    *string       `yaml:"nativeName"`
	ExpectedTitle string        `yaml:"expectedTitle"`
}

func loadCapturedNativeNameCases(t *testing.T) []capturedNativeNameCase {
	t.Helper()
	var fixture struct {
		RequiredNames []string                 `yaml:"requiredNames"`
		Cases         []capturedNativeNameCase `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(capturedNativeNameYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	arms := map[nativeNameArm]bool{nativeNameAbsent: false, nativeNameApplied: false, nativeNameCleared: false, nativeNameSanitized: false}
	for _, tc := range fixture.Cases {
		if _, known := arms[tc.Arm]; !known {
			t.Fatalf("fixture case %q names the unknown arm %q; add the arm to nativeNameArm and its assertion, or correct the fixture", tc.Name, tc.Arm)
		}
		arms[tc.Arm] = true
		if tc.UserProse == "" {
			t.Fatalf("fixture case %q records no user prose; every case needs prose so that a title equal to it proves the native name was not applied", tc.Name)
		}
		if (tc.Arm == nativeNameAbsent) != (tc.NativeName == nil) {
			t.Fatalf("fixture case %q disagrees with itself: arm %q against recorded name %s; the absent arm records no name and every other arm records one", tc.Name, tc.Arm, describeNativeName(tc.NativeName))
		}
	}
	for arm, present := range arms {
		if !present {
			t.Fatalf("no fixture case exercises the %q arm of the harness-native session name; the rule would go untested on that arm", arm)
		}
	}
	return fixture.Cases
}

// describeNativeName reports a recorded name so a failure tells an absent name
// apart from an explicit clear, which are different inputs with different rules.
func describeNativeName(name *string) string {
	if name == nil {
		return "absent"
	}
	return fmt.Sprintf("recorded as %q", *name)
}

// piRow builds one indexed Pi row carrying the typed evidence every Pi row must
// carry. name is the harness-native session name the row records, or nil.
func piRow(t *testing.T, sid ingest.SessionID, index int, role schema.Role, preview string, name *string) schema.SessionEntry {
	t.Helper()
	extra, err := ingest.EncodePiExtra(ingest.PiExtra{Kind: ingest.PiExtraCarrier, Harness: schema.HarnessPi, SessionName: name})
	if err != nil {
		t.Fatal(err)
	}
	text := preview
	return schema.SessionEntry{SessionID: sid, Harness: schema.HarnessPi, EntryIndex: index,
		EntryType: ingest.EntryTypeText, Role: role, Depth: 0, ContentPreview: &text, Extra: extra}
}

// TestCapturedInputAppliesTheRecordedNativeSessionName pins the harness-native
// session name on the stored-input path, which is the path a harvest runs. A
// Pi session must show the name its harness recorded, an explicit clear must
// remove the title, and a recorded name must pass the title privacy policy
// before it is stored.
func TestCapturedInputAppliesTheRecordedNativeSessionName(t *testing.T) {
	for _, tc := range loadCapturedNativeNameCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "metrics.db"), store.WithPoolSize(1))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, isCaptured := any(db).(ingest.MetricInputStore); !isCaptured {
				t.Fatal("the production store no longer offers the stored-input path, so this test would silently measure the older list-entries path")
			}
			sid := mustSessionID(t, testutil.TestSessionUUID)
			seedPiSession(t, t.Context(), db, sid)
			entries := []schema.SessionEntry{piRow(t, sid, 0, ingest.RoleUser, tc.UserProse, nil)}
			if tc.NativeName != nil {
				entries = append(entries, piRow(t, sid, 1, ingest.RoleAssistant, "acknowledged", tc.NativeName))
			}
			if err := db.IndexSessionEntries(t.Context(), sid, entries); err != nil {
				t.Fatal(err)
			}
			engine := metrics.NewEngine(db)
			if count, err := engine.ComputeMetrics(t.Context(), []ingest.SessionID{sid}); err != nil || count != 1 {
				t.Fatalf("stored-input computation: count=%d error=%v", count, err)
			}
			computed, err := db.GetMetrics(t.Context(), sid)
			if err != nil || computed == nil {
				t.Fatalf("metrics were not stored: %+v %v", computed, err)
			}
			if computed.TitleGenerated == nil {
				t.Fatalf("case %q stored no title at all; want %q", tc.Name, tc.ExpectedTitle)
			}
			if *computed.TitleGenerated != tc.ExpectedTitle {
				t.Fatalf("case %q stored the title %q, want %q (the recorded native name is %s and the user prose is %q)", tc.Name, *computed.TitleGenerated, tc.ExpectedTitle, describeNativeName(tc.NativeName), tc.UserProse)
			}
			if tc.Arm == nativeNameSanitized && *computed.TitleGenerated == *tc.NativeName {
				t.Fatalf("case %q stored the recorded native name unchanged; the title privacy policy never saw it", tc.Name)
			}
		})
	}
}

// seedPiSession records one Pi session so the stored-input path can read its
// harness and project path, which are the title privacy policy's context.
func seedPiSession(t *testing.T, ctx context.Context, s *store.Store, sid ingest.SessionID) {
	t.Helper()
	projectHash, err := ingest.NewProjectHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	hostSlug, err := ingest.NewHostSlug(testutil.TestHostSlug)
	if err != nil {
		t.Fatal(err)
	}
	sourcePath, err := ingest.NewResolvedPath("/test/path/recording.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	entry := ingest.StoreEntry{
		Metadata: &ingest.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  schema.HarnessPi,
			HostSlug:      hostSlug,
			Timestamp:     ingest.TimestampInfo{Start: 1000, End: 2000, Ingested: ptrInt64(3000)},
			Source:        ingest.SourceInfo{FilePath: string(sourcePath), Format: ingest.SourceFormatJSONL},
			Project:       ingest.ProjectInfo{Hash: projectHash, Name: "test-project", FilePath: seededProjectPath},
			Stats:         ingest.StatsInfo{TurnCount: 2, ToolCallCount: 0},
		},
		Session: ingest.DiscoveredSession{SessionID: sid, Harness: schema.HarnessPi, SourceFormat: ingest.SourceFormatJSONL},
	}
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatal(err)
	}
}
