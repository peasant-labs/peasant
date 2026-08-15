// Package testfixture builds synthetic OpenCode SQLite sources for ingest tests.
package testfixture

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const expectedCaseCount = 8

//go:embed testdata/opencode_sqlite.yaml
var fixtureYAML []byte

// SourceFormat describes the physical form materialized for a case.
type SourceFormat string

const (
	SourceFormatSQLite  SourceFormat = "sqlite"
	SourceFormatCorrupt SourceFormat = "corrupt"
)

// SchemaKind selects one fixed, safe schema constructor.
type SchemaKind string

const (
	SchemaEmpty             SchemaKind = "empty"
	SchemaLegacy            SchemaKind = "legacy"
	SchemaCurrent           SchemaKind = "current"
	SchemaHybrid            SchemaKind = "hybrid"
	SchemaCurrentMissingSeq SchemaKind = "current_missing_seq"
	SchemaUnsupported       SchemaKind = "unsupported"
)

// JournalMode controls the journal mode used during SQLite setup.
type JournalMode string

const (
	JournalDelete JournalMode = "delete"
	JournalWAL    JournalMode = "wal"
)

type corpus struct {
	DeclaredCases int    `yaml:"declared_cases"`
	Cases         []Case `yaml:"cases"`
}

// Case is one declarative synthetic source. Callers should obtain cases through
// CaseByName rather than constructing them directly.
type Case struct {
	Name            string            `yaml:"name"`
	LogicalPath     string            `yaml:"logical_path"`
	Format          SourceFormat      `yaml:"format"`
	Schema          SchemaKind        `yaml:"schema"`
	JournalMode     JournalMode       `yaml:"journal_mode"`
	DeclaredRows    DeclaredRowCounts `yaml:"declared_rows"`
	ExpectedCatalog ExpectedCatalog   `yaml:"expected_catalog"`
	LegacyMessages  []LegacyMessage   `yaml:"legacy_messages"`
	LegacyParts     []LegacyPart      `yaml:"legacy_parts"`
	CurrentMessages []CurrentMessage  `yaml:"current_messages"`
	CorruptContent  string            `yaml:"corrupt_content"`
}

// DeclaredRowCounts makes every fixture family update explicit and guarded.
type DeclaredRowCounts struct {
	LegacyMessages  int `yaml:"legacy_messages"`
	LegacyParts     int `yaml:"legacy_parts"`
	CurrentMessages int `yaml:"current_messages"`
}

// SeqExpectation describes how catalog probing should observe session_message.seq.
type SeqExpectation string

const (
	SeqAbsent  SeqExpectation = "absent"
	SeqNotNull SeqExpectation = "not_null"
)

// ExpectedCatalog pins the structural catalog exposed by a case.
type ExpectedCatalog struct {
	Tables  []string       `yaml:"tables"`
	Indexes []string       `yaml:"indexes"`
	Seq     SeqExpectation `yaml:"seq"`
}

// LegacyMessage is one row in the legacy message table.
type LegacyMessage struct {
	ID          string `yaml:"id"`
	SessionID   string `yaml:"session_id"`
	TimeCreated int64  `yaml:"time_created"`
	TimeUpdated int64  `yaml:"time_updated"`
	Data        string `yaml:"data"`
}

// LegacyPart is one row in the legacy part table.
type LegacyPart struct {
	ID          string `yaml:"id"`
	MessageID   string `yaml:"message_id"`
	SessionID   string `yaml:"session_id"`
	TimeCreated int64  `yaml:"time_created"`
	TimeUpdated int64  `yaml:"time_updated"`
	Data        string `yaml:"data"`
}

// CurrentMessage is one ordered row in the current session_message projection.
type CurrentMessage struct {
	ID          string `yaml:"id"`
	SessionID   string `yaml:"session_id"`
	Type        string `yaml:"type"`
	TimeCreated int64  `yaml:"time_created"`
	TimeUpdated int64  `yaml:"time_updated"`
	Data        string `yaml:"data"`
	Seq         int64  `yaml:"seq"`
}

// CaseByName returns a named, strictly validated case from the embedded corpus.
func CaseByName(t testing.TB, name string) Case {
	t.Helper()
	fixtures, err := loadCorpus(fixtureYAML)
	if err != nil {
		t.Fatalf("load synthetic OpenCode source fixtures: %v", err)
	}
	for _, fixtureCase := range fixtures.Cases {
		if fixtureCase.Name == name {
			return fixtureCase
		}
	}
	t.Fatalf("load synthetic OpenCode source fixture %q: no such case; choose a name declared in testdata/opencode_sqlite.yaml", name)
	return Case{}
}

func loadCorpus(data []byte) (corpus, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var fixtures corpus
	if err := decoder.Decode(&fixtures); err != nil {
		return corpus{}, fmt.Errorf("decode synthetic OpenCode source fixtures: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return corpus{}, fmt.Errorf("decode synthetic OpenCode source fixtures: expected exactly one YAML document: %w", err)
	}
	if expectedCaseCount == 0 || fixtures.DeclaredCases != expectedCaseCount || len(fixtures.Cases) != expectedCaseCount {
		return corpus{}, fmt.Errorf(
			"validate synthetic OpenCode source fixture row guard: declared cases=%d, actual=%d, required nonzero count=%d",
			fixtures.DeclaredCases,
			len(fixtures.Cases),
			expectedCaseCount,
		)
	}

	names := make(map[string]struct{}, len(fixtures.Cases))
	for index := range fixtures.Cases {
		fixtureCase := &fixtures.Cases[index]
		if err := fixtureCase.validate(); err != nil {
			return corpus{}, err
		}
		if _, duplicate := names[fixtureCase.Name]; duplicate {
			return corpus{}, fmt.Errorf("validate synthetic OpenCode source fixtures: duplicate case name %q", fixtureCase.Name)
		}
		names[fixtureCase.Name] = struct{}{}
	}
	return fixtures, nil
}

func (c Case) validate() error {
	if err := requiredToken("case name", c.Name); err != nil {
		return fmt.Errorf("validate synthetic OpenCode source fixture: %w", err)
	}
	if err := validateLogicalPath(c.LogicalPath); err != nil {
		return fmt.Errorf("validate synthetic OpenCode source fixture %q: %w", c.Name, err)
	}
	if err := c.Format.validate(); err != nil {
		return fmt.Errorf("validate synthetic OpenCode source fixture %q: %w", c.Name, err)
	}
	if c.Format == SourceFormatCorrupt {
		if c.Schema != "" || c.JournalMode != "" {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: corrupt sources must not declare a SQLite schema or journal mode", c.Name)
		}
		if c.CorruptContent == "" {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: corrupt_content is required for a corrupt source", c.Name)
		}
	} else {
		if err := c.Schema.validate(); err != nil {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: %w", c.Name, err)
		}
		if err := c.JournalMode.validate(); err != nil {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: %w", c.Name, err)
		}
		if c.CorruptContent != "" {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: SQLite sources must not declare corrupt_content", c.Name)
		}
	}
	if err := c.validateRowCounts(); err != nil {
		return err
	}
	if err := c.ExpectedCatalog.validate(); err != nil {
		return fmt.Errorf("validate synthetic OpenCode source fixture %q expected catalog: %w", c.Name, err)
	}
	if err := c.validateRows(); err != nil {
		return err
	}
	return c.validateSchemaRows()
}

func (c ExpectedCatalog) validate() error {
	if c.Seq != SeqAbsent && c.Seq != SeqNotNull {
		return fmt.Errorf("unknown seq expectation %q", c.Seq)
	}
	seen := make(map[string]struct{}, len(c.Tables)+len(c.Indexes))
	for _, name := range append(append([]string(nil), c.Tables...), c.Indexes...) {
		if err := requiredToken("catalog object name", name); err != nil {
			return err
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("duplicate catalog object name %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func (c Case) validateRowCounts() error {
	counts := c.DeclaredRows
	if counts.LegacyMessages < 0 || counts.LegacyParts < 0 || counts.CurrentMessages < 0 {
		return fmt.Errorf("validate synthetic OpenCode source fixture %q: declared row counts cannot be negative", c.Name)
	}
	if counts.LegacyMessages != len(c.LegacyMessages) || counts.LegacyParts != len(c.LegacyParts) || counts.CurrentMessages != len(c.CurrentMessages) {
		return fmt.Errorf(
			"validate synthetic OpenCode source fixture %q row guard: declared legacy_messages=%d legacy_parts=%d current_messages=%d; actual=%d/%d/%d",
			c.Name,
			counts.LegacyMessages,
			counts.LegacyParts,
			counts.CurrentMessages,
			len(c.LegacyMessages),
			len(c.LegacyParts),
			len(c.CurrentMessages),
		)
	}
	return nil
}

func (c Case) validateRows() error {
	messageIDs := make(map[string]struct{}, len(c.LegacyMessages))
	for index, row := range c.LegacyMessages {
		if err := validateLegacyMessage(row); err != nil {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q legacy message %d: %w", c.Name, index, err)
		}
		if _, duplicate := messageIDs[row.ID]; duplicate {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: duplicate legacy message id %q", c.Name, row.ID)
		}
		messageIDs[row.ID] = struct{}{}
	}
	partIDs := make(map[string]struct{}, len(c.LegacyParts))
	for index, row := range c.LegacyParts {
		if err := validateLegacyPart(row); err != nil {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q legacy part %d: %w", c.Name, index, err)
		}
		if _, duplicate := partIDs[row.ID]; duplicate {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: duplicate legacy part id %q", c.Name, row.ID)
		}
		partIDs[row.ID] = struct{}{}
	}
	currentIDs := make(map[string]struct{}, len(c.CurrentMessages))
	currentSeqs := make(map[string]struct{}, len(c.CurrentMessages))
	for index, row := range c.CurrentMessages {
		if err := validateCurrentMessage(row); err != nil {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q current message %d: %w", c.Name, index, err)
		}
		if _, duplicate := currentIDs[row.ID]; duplicate {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: duplicate current message id %q", c.Name, row.ID)
		}
		currentIDs[row.ID] = struct{}{}
		seqKey := fmt.Sprintf("%s\x00%d", row.SessionID, row.Seq)
		if _, duplicate := currentSeqs[seqKey]; duplicate {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: duplicate seq %d for current session %q", c.Name, row.Seq, row.SessionID)
		}
		currentSeqs[seqKey] = struct{}{}
	}
	return nil
}

func (c Case) validateSchemaRows() error {
	hasLegacy := len(c.LegacyMessages) != 0 || len(c.LegacyParts) != 0
	hasCurrent := len(c.CurrentMessages) != 0
	switch c.Schema {
	case SchemaLegacy:
		if hasCurrent {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: legacy schema cannot contain current rows", c.Name)
		}
	case SchemaCurrent:
		if hasLegacy {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: current schema cannot contain legacy rows", c.Name)
		}
	case SchemaHybrid:
		// Both table families are available; either may intentionally be empty.
	case SchemaEmpty, SchemaCurrentMissingSeq, SchemaUnsupported, "":
		if hasLegacy || hasCurrent {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: schema %q cannot contain transcript rows", c.Name, c.Schema)
		}
	}
	return nil
}

func validateLegacyMessage(row LegacyMessage) error {
	if err := requiredToken("id", row.ID); err != nil {
		return err
	}
	if err := requiredToken("session_id", row.SessionID); err != nil {
		return err
	}
	if err := requiredToken("data", row.Data); err != nil {
		return err
	}
	return validateJSON(row.Data)
}

func validateLegacyPart(row LegacyPart) error {
	if err := requiredToken("id", row.ID); err != nil {
		return err
	}
	if err := requiredToken("message_id", row.MessageID); err != nil {
		return err
	}
	if err := requiredToken("session_id", row.SessionID); err != nil {
		return err
	}
	if err := requiredToken("data", row.Data); err != nil {
		return err
	}
	return validateJSON(row.Data)
}

func validateCurrentMessage(row CurrentMessage) error {
	if err := requiredToken("id", row.ID); err != nil {
		return err
	}
	if err := requiredToken("session_id", row.SessionID); err != nil {
		return err
	}
	if err := requiredToken("type", row.Type); err != nil {
		return err
	}
	if err := requiredToken("data", row.Data); err != nil {
		return err
	}
	if row.Seq < 0 {
		return errors.New("seq cannot be negative")
	}
	return validateJSON(row.Data)
}

func validateJSON(raw string) error {
	if !json.Valid([]byte(raw)) {
		return fmt.Errorf("data is not valid JSON: %q", raw)
	}
	return nil
}

func requiredToken(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", field)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s %q has surrounding whitespace", field, value)
	}
	return nil
}

func validateLogicalPath(logicalPath string) error {
	if logicalPath == "" {
		return errors.New("logical_path is required")
	}
	if strings.Contains(logicalPath, "\\") || path.IsAbs(logicalPath) || path.Clean(logicalPath) != logicalPath || logicalPath == "." {
		return fmt.Errorf("logical_path %q must be a clean relative slash-separated path", logicalPath)
	}
	for _, component := range strings.Split(logicalPath, "/") {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("logical_path %q contains an unsafe component", logicalPath)
		}
	}
	physical := filepath.FromSlash(logicalPath)
	if filepath.IsAbs(physical) || filepath.VolumeName(physical) != "" {
		return fmt.Errorf("logical_path %q resolves as an absolute physical path", logicalPath)
	}
	return nil
}

func (f SourceFormat) validate() error {
	switch f {
	case SourceFormatSQLite, SourceFormatCorrupt:
		return nil
	default:
		return fmt.Errorf("unknown source format %q", f)
	}
}

func (s SchemaKind) validate() error {
	switch s {
	case SchemaEmpty, SchemaLegacy, SchemaCurrent, SchemaHybrid, SchemaCurrentMissingSeq, SchemaUnsupported:
		return nil
	default:
		return fmt.Errorf("unknown schema kind %q", s)
	}
}

func (m JournalMode) validate() error {
	switch m {
	case JournalDelete, JournalWAL:
		return nil
	default:
		return fmt.Errorf("unknown journal mode %q", m)
	}
}
