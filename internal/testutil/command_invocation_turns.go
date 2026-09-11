package testutil

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

// CommandInvocationTurnFixturePath names the corpus shared by the local detail,
// export, and push projection tests.
const CommandInvocationTurnFixturePath = "internal/testutil/testdata/command_invocation_turns.yaml"

//go:embed testdata/command_invocation_turns.yaml
var commandInvocationTurnFixtureYAML []byte

// CommandInvocationTurnFixture is the strictly decoded command-projection corpus.
type CommandInvocationTurnFixture struct {
	// RequiredNames lists every case name the corpus must retain. It is a
	// deletion-protection manifest, not a row count: adding a new case does not
	// require touching this list, but removing a required case (or renaming it
	// without updating this list) fails the load.
	RequiredNames []string                    `yaml:"requiredNames"`
	Cases         []CommandInvocationTurnCase `yaml:"cases"`
}

// CommandInvocationTurnCase describes one stored entry and the invocation the
// wire must carry for it.
type CommandInvocationTurnCase struct {
	Name    string         `yaml:"name"`
	Harness schema.Harness `yaml:"harness"`
	// SourceRole is the role the indexer stored on the entry. It is required
	// because the harnesses differ: two record a command only on a user entry,
	// while one also records a skill call on the assistant message that made
	// it, and the projection must not move that turn off its author.
	SourceRole schema.Role `yaml:"sourceRole"`
	// Content is the stored ContentPreview. It is empty for a harness shape that
	// records the invocation and no text.
	Content string `yaml:"content"`
	// StoredName and StoredArgs are the session_commands values as written at
	// index time, before the producer adds any missing leading slash.
	StoredName string `yaml:"storedName"`
	StoredArgs string `yaml:"storedArgs,omitempty"`
	// ExpectedRole is the rendered role, which the ratified wrapper gate may
	// have moved away from the stored user role.
	ExpectedRole schema.Role `yaml:"expectedRole"`
	// ExpectedCommand says whether the wire must carry an invocation at all.
	ExpectedCommand bool   `yaml:"expectedCommand"`
	ExpectedName    string `yaml:"expectedName,omitempty"`
	ExpectedArgs    string `yaml:"expectedArgs,omitempty"`

	// StoredExtra is the exact Extra JSON the case sends through production. The
	// loader computes it so every surface sends identical bytes; callers must
	// not reproduce that transformation independently.
	StoredExtra string `yaml:"-"`
}

// storedCommandExtra mirrors the Extra JSON the indexers write for a recorded
// command: command_args is absent, not empty, when the harness recorded none.
type storedCommandExtra struct {
	CommandName string `json:"command_name"`
	CommandArgs string `json:"command_args,omitempty"`
}

// LoadCommandInvocationTurnFixture returns the one embedded, strictly validated
// corpus shared by every projection boundary test.
func LoadCommandInvocationTurnFixture() (CommandInvocationTurnFixture, error) {
	return decodeCommandInvocationTurnFixture(commandInvocationTurnFixtureYAML)
}

func decodeCommandInvocationTurnFixture(source []byte) (CommandInvocationTurnFixture, error) {
	var fixture CommandInvocationTurnFixture
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return CommandInvocationTurnFixture{}, fmt.Errorf("decode command invocation turn fixture %s: typed fields do not match the corpus schema: %w; fix the named YAML field", CommandInvocationTurnFixturePath, err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return CommandInvocationTurnFixture{}, fmt.Errorf("decode command invocation turn fixture %s: expected exactly one YAML document; remove the trailing document", CommandInvocationTurnFixturePath)
		}
		return CommandInvocationTurnFixture{}, fmt.Errorf("decode command invocation turn fixture %s trailing content: %w; remove or repair the trailing document", CommandInvocationTurnFixturePath, err)
	}

	seenNames := make(map[string]bool, len(fixture.Cases))
	for index := range fixture.Cases {
		testCase := &fixture.Cases[index]
		if strings.TrimSpace(testCase.Name) == "" {
			return CommandInvocationTurnFixture{}, fmt.Errorf("decode command invocation turn fixture %s: cases[%d] has a blank name; give every row a stable unique name", CommandInvocationTurnFixturePath, index)
		}
		if seenNames[testCase.Name] {
			return CommandInvocationTurnFixture{}, fmt.Errorf("decode command invocation turn fixture %s: case name %q is duplicated; give every row a unique name", CommandInvocationTurnFixturePath, testCase.Name)
		}
		seenNames[testCase.Name] = true
		if !testCase.Harness.IsKnown() {
			return CommandInvocationTurnFixture{}, fmt.Errorf("decode command invocation turn fixture %s: case %q has unknown harness %q; use a schema harness value", CommandInvocationTurnFixturePath, testCase.Name, testCase.Harness)
		}
		if !testCase.SourceRole.IsValid() {
			return CommandInvocationTurnFixture{}, fmt.Errorf("decode command invocation turn fixture %s: case %q has unknown source role %q; use a schema role value", CommandInvocationTurnFixturePath, testCase.Name, testCase.SourceRole)
		}
		if !testCase.ExpectedRole.IsValid() {
			return CommandInvocationTurnFixture{}, fmt.Errorf("decode command invocation turn fixture %s: case %q has unknown expected role %q; use a schema role value", CommandInvocationTurnFixturePath, testCase.Name, testCase.ExpectedRole)
		}
		if testCase.StoredName == "" {
			return CommandInvocationTurnFixture{}, fmt.Errorf("decode command invocation turn fixture %s: case %q stores no command name; every row describes a stored session_commands row", CommandInvocationTurnFixturePath, testCase.Name)
		}
		if testCase.ExpectedCommand && testCase.ExpectedName == "" {
			return CommandInvocationTurnFixture{}, fmt.Errorf("decode command invocation turn fixture %s: case %q expects an invocation but names none; set expectedName", CommandInvocationTurnFixturePath, testCase.Name)
		}
		if !testCase.ExpectedCommand && (testCase.ExpectedName != "" || testCase.ExpectedArgs != "") {
			return CommandInvocationTurnFixture{}, fmt.Errorf("decode command invocation turn fixture %s: case %q expects no invocation but declares one; clear expectedName and expectedArgs", CommandInvocationTurnFixturePath, testCase.Name)
		}
		encoded, err := json.Marshal(storedCommandExtra{CommandName: testCase.StoredName, CommandArgs: testCase.StoredArgs})
		if err != nil {
			return CommandInvocationTurnFixture{}, fmt.Errorf("decode command invocation turn fixture %s: case %q stored extra cannot be encoded: %w; use plain UTF-8 in storedName and storedArgs", CommandInvocationTurnFixturePath, testCase.Name, err)
		}
		testCase.StoredExtra = string(encoded)
	}

	if err := RequireFixtureNames(fmt.Sprintf("command invocation turn fixture %s", CommandInvocationTurnFixturePath), "case", fixture.RequiredNames, seenNames); err != nil {
		return CommandInvocationTurnFixture{}, err
	}

	return fixture, nil
}
