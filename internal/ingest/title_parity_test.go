package ingest_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/title_parity.yaml
var titleParityYAML []byte

const titleParityRows = 5

// titleParitySubject names the one piece of harness markup a parity row covers.
type titleParitySubject string

const (
	paritySubjectSystemReminder   titleParitySubject = "system_reminder"
	paritySubjectTaskNotification titleParitySubject = "task_notification"
	paritySubjectTeammateMessage  titleParitySubject = "teammate_message"
	paritySubjectLocalCommand     titleParitySubject = "local_command"
	paritySubjectSkillBody        titleParitySubject = "skill_body"
)

// titleParityKind names the shape of the declaration under comparison.
type titleParityKind string

const (
	// parityPairedTag: this repository declares an opening and a closing tag.
	parityPairedTag titleParityKind = "paired_tag"
	// parityOpenPrefix: this repository declares only the opening tag prefix,
	// because the real block carries attributes.
	parityOpenPrefix titleParityKind = "open_prefix"
	// parityFamilyPrefix: this repository declares one prefix that stands for a
	// family of wrapper names.
	parityFamilyPrefix titleParityKind = "family_prefix"
	// parityWholeTurnPrefix: the injection has no tags and is recognized by the
	// leading text of the whole turn.
	parityWholeTurnPrefix titleParityKind = "whole_turn_prefix"
)

type titleParityFixtures struct {
	DeclaredRows int               `yaml:"declared_rows"`
	Cases        []titleParityCase `yaml:"cases"`
}

type titleParityCase struct {
	Name         string             `yaml:"name"`
	Subject      titleParitySubject `yaml:"subject"`
	Kind         titleParityKind    `yaml:"kind"`
	WrapperName  string             `yaml:"wrapper_name"`
	WrapperNames []string           `yaml:"wrapper_names"`
	FamilyPrefix string             `yaml:"family_prefix"`
	TurnPrefix   string             `yaml:"turn_prefix"`
}

func loadTitleParityFixtures(t *testing.T) titleParityFixtures {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(titleParityYAML))
	decoder.KnownFields(true)
	var fixtures titleParityFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode title parity fixtures: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("title parity fixture must contain exactly one YAML document: %v", err)
	}
	if fixtures.DeclaredRows != titleParityRows || len(fixtures.Cases) != titleParityRows {
		t.Fatalf("title parity fixture row guard failed: declared=%d actual=%d expected=%d",
			fixtures.DeclaredRows, len(fixtures.Cases), titleParityRows)
	}
	seen := make(map[titleParitySubject]struct{}, len(fixtures.Cases))
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || fixture.Subject == "" || fixture.Kind == "" {
			t.Fatalf("title parity fixture has an incomplete case: %#v", fixture)
		}
		if _, exists := seen[fixture.Subject]; exists {
			t.Fatalf("duplicate title parity subject %q", fixture.Subject)
		}
		seen[fixture.Subject] = struct{}{}
	}
	return fixtures
}

// TestTitleMarkupCatalogParity proves this repository and the shared title
// pipeline name the same harness markup. A rename on either side breaks a row,
// so the reclassification gate and the title cleanup can never recognize
// different blocks.
func TestTitleMarkupCatalogParity(t *testing.T) {
	fixtures := loadTitleParityFixtures(t)
	for _, fixture := range fixtures.Cases {
		fixture := fixture
		t.Run(string(fixture.Subject), func(t *testing.T) {
			switch fixture.Subject {
			case paritySubjectSystemReminder:
				assertPairedTag(t, fixture, ingest.TagSystemReminder, ingest.TagSystemReminderClose, redact.WrapperSystemReminder)
			case paritySubjectTaskNotification:
				assertPairedTag(t, fixture, ingest.TagTaskNotification, ingest.TagTaskNotificationClose, redact.WrapperTaskNotification)
			case paritySubjectTeammateMessage:
				assertOpenPrefix(t, fixture, ingest.TagTeammateMessage, redact.WrapperTeammateMessage)
			case paritySubjectLocalCommand:
				assertLocalCommandFamily(t, fixture, ingest.TagLocalCommand, []string{
					redact.WrapperLocalCommandCaveat,
					redact.WrapperLocalCommandStdout,
					redact.WrapperLocalCommandStderr,
				})
			case paritySubjectSkillBody:
				assertWholeTurnPrefix(t, fixture, ingest.PrefixSkillBody, redact.SkillBodyPrefix)
			default:
				t.Fatalf("title parity fixture names unknown subject %q", fixture.Subject)
			}
		})
	}
}

func assertPairedTag(t *testing.T, fixture titleParityCase, openTag, closeTag, redactName string) {
	t.Helper()
	if fixture.Kind != parityPairedTag {
		t.Fatalf("subject %q must declare kind %q, got %q", fixture.Subject, parityPairedTag, fixture.Kind)
	}
	if wantOpen := "<" + fixture.WrapperName + ">"; openTag != wantOpen {
		t.Errorf("indexing opening tag = %q, want %q", openTag, wantOpen)
	}
	if wantClose := "</" + fixture.WrapperName + ">"; closeTag != wantClose {
		t.Errorf("indexing closing tag = %q, want %q", closeTag, wantClose)
	}
	assertRedactName(t, fixture.WrapperName, redactName)
	assertRedactDropsWrapper(t, fixture.WrapperName)
}

func assertOpenPrefix(t *testing.T, fixture titleParityCase, openPrefix, redactName string) {
	t.Helper()
	if fixture.Kind != parityOpenPrefix {
		t.Fatalf("subject %q must declare kind %q, got %q", fixture.Subject, parityOpenPrefix, fixture.Kind)
	}
	if want := "<" + fixture.WrapperName; openPrefix != want {
		t.Errorf("indexing opening prefix = %q, want %q", openPrefix, want)
	}
	assertRedactName(t, fixture.WrapperName, redactName)
	assertRedactDropsWrapper(t, fixture.WrapperName)
}

func assertLocalCommandFamily(t *testing.T, fixture titleParityCase, openPrefix string, redactNames []string) {
	t.Helper()
	if fixture.Kind != parityFamilyPrefix {
		t.Fatalf("subject %q must declare kind %q, got %q", fixture.Subject, parityFamilyPrefix, fixture.Kind)
	}
	if want := "<" + fixture.FamilyPrefix; openPrefix != want {
		t.Errorf("indexing opening prefix = %q, want %q", openPrefix, want)
	}
	if len(fixture.WrapperNames) != len(redactNames) {
		t.Fatalf("fixture declares %d local command wrappers, the title pipeline declares %d", len(fixture.WrapperNames), len(redactNames))
	}
	declared := make(map[string]struct{}, len(redactNames))
	for _, name := range redactNames {
		if !strings.HasPrefix(name, fixture.FamilyPrefix) {
			t.Errorf("title pipeline wrapper %q does not start with the indexed family prefix %q", name, fixture.FamilyPrefix)
		}
		declared[name] = struct{}{}
	}
	for _, name := range fixture.WrapperNames {
		if _, ok := declared[name]; !ok {
			t.Errorf("the title pipeline no longer declares local command wrapper %q", name)
			continue
		}
		assertRedactDropsWrapper(t, name)
	}
}

func assertWholeTurnPrefix(t *testing.T, fixture titleParityCase, indexPrefix, redactPrefix string) {
	t.Helper()
	if fixture.Kind != parityWholeTurnPrefix {
		t.Fatalf("subject %q must declare kind %q, got %q", fixture.Subject, parityWholeTurnPrefix, fixture.Kind)
	}
	if indexPrefix != fixture.TurnPrefix {
		t.Errorf("indexing whole-turn prefix = %q, want %q", indexPrefix, fixture.TurnPrefix)
	}
	if redactPrefix != fixture.TurnPrefix {
		t.Errorf("title pipeline whole-turn prefix = %q, want %q", redactPrefix, fixture.TurnPrefix)
	}
	if got := cleanForClaudeCode(t, fixture.TurnPrefix+" /workspace/.claude/skills/epoch\n\nrun the workflow"); got != "" {
		t.Errorf("the title pipeline kept %q for a whole-turn injection, want the empty title", got)
	}
}

func assertRedactName(t *testing.T, fixtureName, redactName string) {
	t.Helper()
	if redactName != fixtureName {
		t.Errorf("title pipeline wrapper name = %q, want %q", redactName, fixtureName)
	}
}

// assertRedactDropsWrapper proves the shared pipeline really cleans this
// wrapper for Claude Code, so the row measures the live policy rather than one
// exported string.
func assertRedactDropsWrapper(t *testing.T, wrapperName string) {
	t.Helper()
	block := "<" + wrapperName + ">harness generated content</" + wrapperName + ">"
	if got := cleanForClaudeCode(t, block); got != "" {
		t.Errorf("the title pipeline kept %q for wrapper %q, want the empty title", got, wrapperName)
	}
}

func cleanForClaudeCode(t *testing.T, turn string) string {
	t.Helper()
	pipeline, err := redact.NewTitlePipeline()
	if err != nil {
		t.Fatalf("construct the shared title pipeline: %v", err)
	}
	cleaned, err := pipeline.SimpleTitle(turn, schema.HarnessClaudeCode)
	if err != nil {
		t.Fatalf("clean the harness markup: %v", err)
	}
	return cleaned
}
