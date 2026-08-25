package push_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/session_origin_declaration.yaml
var sessionOriginDeclarationYAML []byte

// requiredOriginDeclarationCases is the deletion guard, by NAME. Every stored
// value the menu allows has an arm, plus the two shapes that could only arrive
// from a corrupt row and must still publish.
var requiredOriginDeclarationCases = []string{
	"a session a person drove is declared user",
	"a session a program started is declared agent",
	"a session whose origin could not be established is declared unknown",
	"an unusable stored value is declared unknown rather than refused",
	"an empty stored value is declared unknown rather than refused",
}

type originDeclarationFixture struct {
	Cases []originDeclarationCase `yaml:"cases"`
}

type originDeclarationCase struct {
	Name         string `yaml:"name"`
	StoredOrigin string `yaml:"stored_origin"`
	WantDeclared string `yaml:"want_declared"`
}

func decodeOriginDeclarations(source []byte) (originDeclarationFixture, error) {
	var fixture originDeclarationFixture
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, fmt.Errorf("decode session-origin declaration fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fixture, fmt.Errorf("session-origin declaration fixture must contain exactly one YAML document: %v", err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for index, testCase := range fixture.Cases {
		if testCase.Name == "" || present[testCase.Name] {
			return fixture, fmt.Errorf("session-origin declaration fixture cases[%d] has an empty or duplicate name %q", index, testCase.Name)
		}
		present[testCase.Name] = true
		if !schema.SessionOrigin(testCase.WantDeclared).IsValid() || testCase.WantDeclared == "" {
			return fixture, fmt.Errorf("session-origin declaration fixture cases[%d] wants %q on the wire, which is not a declared menu value; Peasant always declares", index, testCase.WantDeclared)
		}
	}
	if err := testutil.RequireFixtureNames("session-origin declaration fixture", "case", requiredOriginDeclarationCases, present); err != nil {
		return fixture, err
	}
	return fixture, nil
}

func TestOriginDeclarationFixtureRejectsCaseDeletion(t *testing.T) {
	t.Parallel()
	for _, name := range requiredOriginDeclarationCases {
		t.Run(name, func(t *testing.T) {
			mutated := bytes.Replace(sessionOriginDeclarationYAML, []byte("name: "+name+"\n"), []byte("name: removed "+name+"\n"), 1)
			if bytes.Equal(mutated, sessionOriginDeclarationYAML) {
				t.Fatalf("case %q was not present to rename; the deletion guard cannot be trusted", name)
			}
			if _, err := decodeOriginDeclarations(mutated); err == nil || !strings.Contains(err.Error(), "missing required case") {
				t.Fatalf("renamed-case fixture error = %v, want a missing-required-case rejection", err)
			}
		})
	}
}

// TestPushDeclaresOriginForEveryStoredValue proves the published document
// carries sessionOrigin for all three stored values, and that no stored value
// refuses the push.
func TestPushDeclaresOriginForEveryStoredValue(t *testing.T) {
	t.Parallel()
	fixture, err := decodeOriginDeclarations(sessionOriginDeclarationYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			content, err := push.BuildTranscriptContentValidated(
				originDeclarationMetadata(),
				originDeclarationEntries(),
				defaults.PublishSchemaVersion,
				config.DefaultPushFieldVisibility(),
				sessionorigin.Origin(testCase.StoredOrigin),
			)
			if err != nil {
				t.Fatalf("building the published document refused a session with stored origin %q: %v; a push must never fail on account of origin", testCase.StoredOrigin, err)
			}
			if content.SessionDetail == nil {
				t.Fatal("the published document carries no session detail payload")
			}
			if got := string(content.SessionDetail.SessionOrigin); got != testCase.WantDeclared {
				t.Fatalf("declared sessionOrigin = %q, want %q", got, testCase.WantDeclared)
			}
			// Declared, not merely typed: the field must survive serialization,
			// which omitempty would drop for an empty declaration.
			raw, marshalErr := json.Marshal(content.SessionDetail)
			if marshalErr != nil {
				t.Fatalf("serialize the published document: %v", marshalErr)
			}
			if want := fmt.Sprintf(`"sessionOrigin":%q`, testCase.WantDeclared); !strings.Contains(string(raw), want) {
				t.Fatalf("the serialized document does not carry %s; body=%s", want, raw)
			}
		})
	}
}

// TestPushOfAnAgentParentStillCarriesItsChildren proves origin never withholds
// anything else a publish carries. A parent classified agent
// still publishes its subagent references, and origin is not one of the fields
// push visibility settings can switch off — it names no person, path, host, or
// repository, so gating it would defeat grouping at the server for no privacy
// gain.
func TestPushOfAnAgentParentStillCarriesItsChildren(t *testing.T) {
	t.Parallel()
	meta := originDeclarationMetadata()
	child := schema.SessionID(testutil.TestSessionUUID2)
	meta.Subagents = []ingest.SubagentRef{{SessionID: child, ParentUUID: meta.SessionID}}

	// Every field switched OFF, so anything still published is published because
	// it is ungated rather than because the fixture left the gates open.
	published, err := push.MapMetadata(push.MapOptions{
		Meta:   meta,
		Fields: config.PushFieldVisibility{},
	})
	if err != nil {
		t.Fatalf("map the publish metadata for an agent-driven parent: %v", err)
	}
	var request schema.PublishRequest
	if err := json.Unmarshal(published, &request); err != nil {
		t.Fatalf("decode the published metadata: %v", err)
	}
	if len(request.Subagents) != 1 || request.Subagents[0].SessionID != child {
		t.Fatalf("published subagents = %+v, want the one child %s; a parent must still carry its children", request.Subagents, child)
	}

	content, err := push.BuildTranscriptContentValidated(
		meta,
		originDeclarationEntries(),
		defaults.PublishSchemaVersion,
		config.PushFieldVisibility{},
		sessionorigin.Agent,
	)
	if err != nil {
		t.Fatalf("build the published document for an agent-driven parent: %v", err)
	}
	if content.SessionDetail.SessionOrigin != schema.SessionOriginAgent {
		t.Fatalf("declared sessionOrigin = %q with every push field switched off, want %q; origin is not field-gated", content.SessionDetail.SessionOrigin, schema.SessionOriginAgent)
	}
}

func originDeclarationMetadata() *ingest.UnifiedMetadata {
	value := ingest.NewUnifiedMetadata()
	meta := &value
	meta.SessionID = schema.SessionID(testutil.TestSessionUUID)
	meta.ModelHarness = defaults.HarnessClaudeCode
	meta.Model = schema.ModelID(testutil.TestModel)
	return meta
}

func originDeclarationEntries() []schema.SessionEntry {
	preview := "a recorded message"
	return []schema.SessionEntry{{
		SessionID:      schema.SessionID(testutil.TestSessionUUID),
		EntryIndex:     1,
		Role:           schema.RoleUser,
		ContentPreview: &preview,
	}}
}
