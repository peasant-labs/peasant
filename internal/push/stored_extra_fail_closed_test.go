package push

import (
	"bytes"
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/stored_extra_fail_closed.yaml
var storedExtraFailClosedYAML []byte

type storedExtraCase struct {
	Name     string `yaml:"name"`
	Extra    string `yaml:"extra"`
	Accepted bool   `yaml:"accepted"`
}

// TestRedactEntriesStaysFailClosedOnStoredExtra holds the share scan's read
// side to strict fail-closed behavior on Extra documents an earlier build
// could have stored, beside the clean forms the current build stores.
func TestRedactEntriesStaysFailClosedOnStoredExtra(t *testing.T) {
	t.Parallel()
	var document struct {
		RequiredNames []string          `yaml:"required_names"`
		Cases         []storedExtraCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(storedExtraFailClosedYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool, len(document.Cases))
	for _, row := range document.Cases {
		names[row.Name] = true
	}
	for _, name := range document.RequiredNames {
		if !names[name] {
			t.Fatalf("missing required stored-extra case %s", name)
		}
	}
	redactor, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			extra := string(bytes.ReplaceAll([]byte(row.Extra), []byte("INVALIDBYTE"), []byte{0xff}))
			part := "attachment"
			entry := schema.SessionEntry{
				SessionID: schema.SessionID(testutil.TestSessionUUID),
				Harness:   schema.HarnessClaudeCode,
				Role:      schema.RoleSystem,
				EntryType: schema.EntryTypeSystem,
				PartType:  &part,
				Extra:     &extra,
			}
			_, err := RedactEntries(redactor, []schema.SessionEntry{entry})
			if (err == nil) != row.Accepted {
				t.Fatalf("share scan accepted=%v, want %v (%v)", err == nil, row.Accepted, err)
			}
		})
	}
}
