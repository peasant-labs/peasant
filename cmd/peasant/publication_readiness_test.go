package main

import (
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"github.com/peasant-labs/redact"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/publication_readiness.yaml
var publicationReadinessYAML []byte

func TestPublicationWizardAndReportUseDatabaseReadiness(t *testing.T) {
	var cases []struct {
		Name   string `yaml:"name"`
		Action string `yaml:"action"`
		Ready  bool   `yaml:"ready"`
	}
	if err := yaml.Unmarshal(publicationReadinessYAML, &cases); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		seen[tc.Name] = true
	}
	if err := testutil.RequireFixtureNames("publication readiness", "case", strings.Fields("complete-without-sidecar legacy-needs-ingest index-mismatch-needs-ingest missing-model-needs-source-evidence"), seen); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			dir := t.TempDir()
			seedUploadableSession(t, dir, testutil.TestSessionUUID)
			db, err := store.Open(string(defaults.ResolveDBFilePathWith(dir)))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			input, err := db.LoadPublicationInput(t.Context(), testutil.TestSessionUUID)
			if err != nil {
				t.Fatal(err)
			}
			switch tc.Action {
			case "ready":
			case "legacy":
				if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{{Metadata: &input.Metadata}}); err != nil {
					t.Fatal(err)
				}
			case "invalidate":
				if err := db.IndexSessionEntries(t.Context(), input.Metadata.SessionID, nil); err != nil {
					t.Fatal(err)
				}
			case "missing-model":
				input.Metadata.Model = ""
				testutil.SeedReadyPublication(t, db, &input.Metadata, input.Entries)
			default:
				t.Fatalf("unknown fixture action %s", tc.Action)
			}
			wizardSessions, err := buildPushWizardSessions(t.Context(), db, push.PushCandidateQuery{Method: config.PushMethodAll}, nil)
			if err != nil || len(wizardSessions) != 1 {
				t.Fatalf("wizard rows=%+v err=%v", wizardSessions, err)
			}
			row := wizardSessions[0]
			if row.NeedsIngest == tc.Ready || (row.Meta != nil) != tc.Ready {
				t.Fatalf("wizard readiness=%+v want ready=%v", row, tc.Ready)
			}
			model := push.NewPushWizard(theme.New(theme.ModeDark), wizardSessions, nil)
			if (len(model.SelectedSessionIDs()) == 1) != tc.Ready {
				t.Fatal("wizard admitted incomplete capture or excluded ready capture")
			}
			// The preview read is NOT a readiness gate. Selection, the wizard and
			// the redaction record below still refuse an incomplete capture; the
			// preview must keep answering for every one of these states, because a
			// user cannot repair a session the previewer will not show them.
			if _, err = storedSessionEntries(t.Context(), db)(testutil.TestSessionUUID); err != nil {
				t.Fatalf("the preview refused a session it must still be able to show: %v", err)
			}
			record := buildRedactionRecord(t.Context(), []ingest.PushSessionRow{row.Row}, db, redact.Standard)
			if (record.MissingMetadataCount == 0) != tc.Ready {
				t.Fatalf("report readiness=%+v", record)
			}
		})
	}
}
