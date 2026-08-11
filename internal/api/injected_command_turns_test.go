package api_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

const injectedCommandTurnFixtureCaseCount = 18

//go:embed testdata/injected_command_turns.yaml
var injectedCommandTurnFixtureYAML []byte

type injectedCommandTurnFixture struct {
	ExpectedCaseCount int                       `yaml:"expectedCaseCount"`
	Cases             []injectedCommandTurnCase `yaml:"cases"`
}

type injectedCommandTurnCase struct {
	Name              string         `yaml:"name"`
	Harness           schema.Harness `yaml:"harness"`
	SourceRole        schema.Role    `yaml:"sourceRole"`
	Content           string         `yaml:"content"`
	PadToPreviewLimit bool           `yaml:"padToPreviewLimit,omitempty"`
	ExpectedRole      schema.Role    `yaml:"expectedRole"`
}

func decodeInjectedCommandTurnFixture(source []byte) (injectedCommandTurnFixture, error) {
	var fixture injectedCommandTurnFixture
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return injectedCommandTurnFixture{}, fmt.Errorf("decode injected command turn fixture: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return injectedCommandTurnFixture{}, errors.New("decode injected command turn fixture: expected exactly one YAML document; remove the trailing document")
		}
		return injectedCommandTurnFixture{}, fmt.Errorf("decode injected command turn fixture trailing content: %w", err)
	}

	if fixture.ExpectedCaseCount != injectedCommandTurnFixtureCaseCount || len(fixture.Cases) != injectedCommandTurnFixtureCaseCount {
		return injectedCommandTurnFixture{}, fmt.Errorf(
			"decode injected command turn fixture: case count mismatch: declared=%d rows=%d want=%d; update the fixture rows and exact count together",
			fixture.ExpectedCaseCount,
			len(fixture.Cases),
			injectedCommandTurnFixtureCaseCount,
		)
	}

	seenNames := make(map[string]struct{}, len(fixture.Cases))
	for index, testCase := range fixture.Cases {
		if strings.TrimSpace(testCase.Name) == "" {
			return injectedCommandTurnFixture{}, fmt.Errorf("decode injected command turn fixture: cases[%d] has a blank name; give every row a stable unique name", index)
		}
		if _, duplicate := seenNames[testCase.Name]; duplicate {
			return injectedCommandTurnFixture{}, fmt.Errorf("decode injected command turn fixture: case name %q is duplicated; give every row a unique name", testCase.Name)
		}
		seenNames[testCase.Name] = struct{}{}
		if !testCase.Harness.IsKnown() {
			return injectedCommandTurnFixture{}, fmt.Errorf("decode injected command turn fixture: case %q has unknown harness %q; use a schema harness value", testCase.Name, testCase.Harness)
		}
		if !testCase.SourceRole.IsValid() {
			return injectedCommandTurnFixture{}, fmt.Errorf("decode injected command turn fixture: case %q has unknown source role %q; use a schema role value", testCase.Name, testCase.SourceRole)
		}
		if !testCase.ExpectedRole.IsValid() {
			return injectedCommandTurnFixture{}, fmt.Errorf("decode injected command turn fixture: case %q has unknown expected role %q; use a schema role value", testCase.Name, testCase.ExpectedRole)
		}
		if testCase.Content == "" {
			return injectedCommandTurnFixture{}, fmt.Errorf("decode injected command turn fixture: case %q has empty content; provide the stored content under test", testCase.Name)
		}
		if testCase.PadToPreviewLimit && len(testCase.Content) >= defaults.ContentPreviewLimit {
			return injectedCommandTurnFixture{}, fmt.Errorf("decode injected command turn fixture: case %q cannot pad content length %d to preview limit %d; shorten the fixture content", testCase.Name, len(testCase.Content), defaults.ContentPreviewLimit)
		}
	}

	return fixture, nil
}

func loadInjectedCommandTurnFixture(t *testing.T) injectedCommandTurnFixture {
	t.Helper()
	fixture, err := decodeInjectedCommandTurnFixture(injectedCommandTurnFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture injectedCommandTurnFixture) caseByName(t *testing.T, name string) injectedCommandTurnCase {
	t.Helper()
	for _, testCase := range fixture.Cases {
		if testCase.Name == name {
			return testCase
		}
	}
	t.Fatalf("injected command turn fixture has no case named %q", name)
	return injectedCommandTurnCase{}
}

func TestEntriesToTurns_InjectedCommandRolesReachDetailPayload(t *testing.T) {
	t.Parallel()
	fixture := loadInjectedCommandTurnFixture(t)
	sessionID := schema.SessionID("45454545-4545-4545-4545-454545454545")

	for index, testCase := range fixture.Cases {
		index, testCase := index, testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			content := testCase.Content
			if testCase.PadToPreviewLimit {
				content = strings.Repeat(" ", defaults.ContentPreviewLimit-len(content)) + content
			}
			entries := []schema.SessionEntry{{
				SessionID:      sessionID,
				EntryIndex:     index,
				Harness:        testCase.Harness,
				EntryType:      schema.EntryTypeText,
				Role:           testCase.SourceRole,
				ContentPreview: &content,
			}}

			turns := api.EntriesToTurns(entries)
			if len(turns) != 1 {
				t.Fatalf("EntriesToTurns returned %d turns, want 1", len(turns))
			}
			if turns[0].Role != testCase.ExpectedRole {
				t.Errorf("EntriesToTurns role = %q, want %q", turns[0].Role, testCase.ExpectedRole)
			}
			if turns[0].Content != content {
				t.Errorf("EntriesToTurns content = %q, want unchanged %q", turns[0].Content, content)
			}

			session := &ingest.Session{
				ID:      sessionID,
				Harness: testCase.Harness,
				Turns:   turns,
			}
			payload := api.SessionToDetail(session)
			if len(payload.Turns) != 1 {
				t.Fatalf("SessionToDetail returned %d turns, want 1", len(payload.Turns))
			}
			if payload.Turns[0].Role != testCase.ExpectedRole {
				t.Errorf("SessionToDetail role = %q, want %q", payload.Turns[0].Role, testCase.ExpectedRole)
			}
		})
	}
}

func TestStoreDataProvider_InjectedCommandRolesReachDetailPayload(t *testing.T) {
	t.Parallel()
	fixture := loadInjectedCommandTurnFixture(t)
	wrapperOnly := fixture.caseByName(t, "command_name_only")
	mixedProse := fixture.caseByName(t, "trailing_user_prose")

	const sessionID = "45454545-4545-4545-4545-454545454546"
	db := openTestStore(t)
	storeEntry := makeStoreEntry(
		t,
		sessionID,
		hash1,
		"github.com-test",
		wrapperOnly.Harness,
		day1Ms,
		100,
		50,
		"project-injected-command",
		2,
		0,
		1000,
	)
	provider := seedStore(t, db, []ingest.StoreEntry{storeEntry})
	sid := schema.SessionID(sessionID)
	wrapperContent := wrapperOnly.Content
	mixedContent := mixedProse.Content
	entries := []schema.SessionEntry{
		{
			SessionID:      sid,
			EntryIndex:     0,
			Harness:        wrapperOnly.Harness,
			EntryType:      schema.EntryTypeText,
			Role:           wrapperOnly.SourceRole,
			ContentPreview: &wrapperContent,
		},
		{
			SessionID:      sid,
			EntryIndex:     1,
			Harness:        mixedProse.Harness,
			EntryType:      schema.EntryTypeText,
			Role:           mixedProse.SourceRole,
			ContentPreview: &mixedContent,
		},
	}
	if err := db.IndexSessionEntries(context.Background(), sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	session, err := provider.SessionByID(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	payload := api.SessionToDetail(session)
	if len(payload.Turns) != 2 {
		t.Fatalf("stored entry detail has %d turns, want 2", len(payload.Turns))
	}
	if payload.Turns[0].Role != wrapperOnly.ExpectedRole {
		t.Errorf("wrapper-only stored entry role = %q, want %q", payload.Turns[0].Role, wrapperOnly.ExpectedRole)
	}
	if payload.Turns[1].Role != mixedProse.ExpectedRole {
		t.Errorf("mixed-prose stored entry role = %q, want %q", payload.Turns[1].Role, mixedProse.ExpectedRole)
	}
}

func TestInjectedCommandTurnFixtureRejectsUnknownField(t *testing.T) {
	t.Parallel()
	mutated := bytes.Replace(
		injectedCommandTurnFixtureYAML,
		[]byte("expectedCaseCount:"),
		[]byte("unknownFixtureField: true\nexpectedCaseCount:"),
		1,
	)
	if _, err := decodeInjectedCommandTurnFixture(mutated); err == nil {
		t.Fatal("fixture decoder accepted an unknown field")
	}
}

func TestInjectedCommandTurnFixtureRejectsTrailingDocument(t *testing.T) {
	t.Parallel()
	mutated := append(append([]byte{}, injectedCommandTurnFixtureYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := decodeInjectedCommandTurnFixture(mutated); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing-document error = %v, want exact single-document rejection", err)
	}
}

func TestInjectedCommandTurnFixtureGuardsExactRowCount(t *testing.T) {
	t.Parallel()
	mutated := bytes.Replace(
		injectedCommandTurnFixtureYAML,
		[]byte("expectedCaseCount: 18"),
		[]byte("expectedCaseCount: 17"),
		1,
	)
	if _, err := decodeInjectedCommandTurnFixture(mutated); err == nil || !strings.Contains(err.Error(), "case count mismatch") {
		t.Fatalf("row-count error = %v, want exact-count rejection", err)
	}
}
