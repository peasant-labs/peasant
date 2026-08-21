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

	"gopkg.in/yaml.v3"
)

const expectedCaseCount = 31

//go:embed testdata/opencode_sqlite.yaml
var fixtureYAML []byte

type sourceFormat string

const (
	sourceFormatSQLite  sourceFormat = "sqlite"
	sourceFormatCorrupt sourceFormat = "corrupt"
)

type schemaKind string

const (
	schemaEmpty              schemaKind = "empty"
	schemaLegacy             schemaKind = "legacy"
	schemaCurrent            schemaKind = "current"
	schemaHybrid             schemaKind = "hybrid"
	schemaCurrentMissingSeq  schemaKind = "current_missing_seq"
	schemaCurrentNullableSeq schemaKind = "current_nullable_seq"
	schemaCurrentPartialSeq  schemaKind = "current_partial_seq"
	schemaUnsupported        schemaKind = "unsupported"
)

type journalMode string

const (
	journalDelete journalMode = "delete"
	journalWAL    journalMode = "wal"
)

// sessionClockMode controls how the synthetic session table carries the
// per-session update clock, so tests can cover the floor path and clock lag.
type sessionClockMode string

const (
	// sessionClockMirror advances time_updated to the newest row time, so the
	// clock never lags. It is the default upstream-faithful behaviour.
	sessionClockMirror sessionClockMode = ""
	// sessionClockLagging leaves time_updated behind the newest row time, so
	// the newest row time is the changed time and a row change is still seen.
	sessionClockLagging sessionClockMode = "lagging"
	// sessionClockAbsent omits the time_updated column, so the session has no
	// usable clock and freshness falls back to the database and WAL mtime floor.
	sessionClockAbsent sessionClockMode = "absent"
)

func (m sessionClockMode) validate() error {
	switch m {
	case sessionClockMirror, sessionClockLagging, sessionClockAbsent:
		return nil
	default:
		return fmt.Errorf("unknown session clock mode %q", m)
	}
}

type corruptionKind string

const (
	corruptionNone            corruptionKind = ""
	corruptionNonSQLite       corruptionKind = "non_sqlite"
	corruptionTruncatedSQLite corruptionKind = "truncated_sqlite"
)

type historyKind string

const (
	historyEvent     historyKind = "event"
	historyDelta     historyKind = "delta"
	historyInput     historyKind = "input"
	historyContext   historyKind = "context"
	historyMigration historyKind = "migration"
)

type corpus struct {
	DeclaredCases int        `yaml:"declared_cases"`
	Cases         []caseSpec `yaml:"cases"`
}

type caseSpec struct {
	Name            string              `yaml:"name"`
	LogicalPath     string              `yaml:"logical_path"`
	Format          sourceFormat        `yaml:"format"`
	Schema          schemaKind          `yaml:"schema"`
	JournalMode     journalMode         `yaml:"journal_mode"`
	SessionClock    sessionClockMode    `yaml:"session_clock"`
	Corruption      corruptionKind      `yaml:"corruption"`
	DeclaredRows    declaredRowCounts   `yaml:"declared_rows"`
	ExpectedCatalog expectedCatalogSpec `yaml:"expected_catalog"`
	CatalogPadding  catalogPaddingSpec  `yaml:"catalog_padding"`
	LegacyMessages  []legacyMessage     `yaml:"legacy_messages"`
	LegacyParts     []legacyPart        `yaml:"legacy_parts"`
	CurrentMessages []currentMessage    `yaml:"current_messages"`
	IgnoredHistory  []historyRow        `yaml:"ignored_history"`
}

type catalogPaddingSpec struct {
	Tables  int `yaml:"tables"`
	Columns int `yaml:"columns"`
	Indexes int `yaml:"indexes"`
}

type declaredRowCounts struct {
	LegacyMessages  int `yaml:"legacy_messages"`
	LegacyParts     int `yaml:"legacy_parts"`
	CurrentMessages int `yaml:"current_messages"`
	IgnoredHistory  int `yaml:"ignored_history"`
}

// seqExpectation describes how catalog probing should observe session_message.seq.
type seqExpectation string

const (
	seqAbsent   seqExpectation = "absent"
	seqNullable seqExpectation = "nullable"
	seqNotNull  seqExpectation = "not_null"
)

type expectedCatalogSpec struct {
	Tables  []string       `yaml:"tables"`
	Indexes []string       `yaml:"indexes"`
	Seq     seqExpectation `yaml:"seq"`
}

// CatalogExpectation is immutable expected evidence attached to a named fixture.
type CatalogExpectation struct {
	tables  []string
	indexes []string
	seq     seqExpectation
}

// Tables returns a detached copy of expected user-table names.
func (c CatalogExpectation) Tables() []string { return append([]string(nil), c.tables...) }

// Indexes returns a detached copy of expected explicit index names.
func (c CatalogExpectation) Indexes() []string { return append([]string(nil), c.indexes...) }

type legacyMessage struct {
	ID          string `yaml:"id"`
	SessionID   string `yaml:"session_id"`
	TimeCreated int64  `yaml:"time_created"`
	TimeUpdated int64  `yaml:"time_updated"`
	Data        string `yaml:"data"`
}

// legacyPart is one row in the legacy part table.
type legacyPart struct {
	ID          string `yaml:"id"`
	MessageID   string `yaml:"message_id"`
	SessionID   string `yaml:"session_id"`
	TimeCreated int64  `yaml:"time_created"`
	TimeUpdated int64  `yaml:"time_updated"`
	Data        string `yaml:"data"`
}

// currentMessage is one ordered row in the current session_message projection.
type currentMessage struct {
	ID          string `yaml:"id"`
	SessionID   string `yaml:"session_id"`
	Type        string `yaml:"type"`
	TimeCreated int64  `yaml:"time_created"`
	TimeUpdated int64  `yaml:"time_updated"`
	Data        string `yaml:"data"`
	Seq         int64  `yaml:"seq"`
}

type historyRow struct {
	Kind        historyKind `yaml:"kind"`
	ID          string      `yaml:"id"`
	SessionID   string      `yaml:"session_id"`
	StableID    string      `yaml:"stable_id"`
	TimeCreated int64       `yaml:"time_created"`
	Data        string      `yaml:"data"`
}

func caseByName(name string) (caseSpec, error) {
	fixtures, err := loadCorpus(fixtureYAML)
	if err != nil {
		return caseSpec{}, err
	}
	for _, fixtureCase := range fixtures.Cases {
		if fixtureCase.Name == name {
			return fixtureCase, nil
		}
	}
	return caseSpec{}, fmt.Errorf("load synthetic OpenCode source fixture %q: no such case; choose a name declared in testdata/opencode_sqlite.yaml", name)
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

func (c caseSpec) validate() error {
	if err := requiredToken("case name", c.Name); err != nil {
		return fmt.Errorf("validate synthetic OpenCode source fixture: %w", err)
	}
	if err := validateLogicalPath(c.LogicalPath); err != nil {
		return fmt.Errorf("validate synthetic OpenCode source fixture %q: %w", c.Name, err)
	}
	if err := c.Format.validate(); err != nil {
		return fmt.Errorf("validate synthetic OpenCode source fixture %q: %w", c.Name, err)
	}
	if c.Format == sourceFormatCorrupt {
		if c.Schema != "" || c.JournalMode != "" || c.SessionClock != sessionClockMirror {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: corrupt sources must not declare a SQLite schema, journal mode, or session clock", c.Name)
		}
		if err := c.Corruption.validate(); err != nil {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: corrupt source has invalid evidence: %w", c.Name, err)
		}
		if c.Corruption == corruptionNone {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: corrupt source requires a corruption kind so tests know which fixed malformed bytes to materialize", c.Name)
		}
		if c.CatalogPadding != (catalogPaddingSpec{}) {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: corrupt sources cannot declare catalog padding", c.Name)
		}
	} else {
		if err := c.Schema.validate(); err != nil {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: %w", c.Name, err)
		}
		if err := c.JournalMode.validate(); err != nil {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: %w", c.Name, err)
		}
		if c.Corruption != corruptionNone {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: SQLite sources must not declare corruption %q", c.Name, c.Corruption)
		}
		if err := c.SessionClock.validate(); err != nil {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: %w", c.Name, err)
		}
		if c.SessionClock != sessionClockMirror && c.Schema != schemaLegacy && c.Schema != schemaCurrent && c.Schema != schemaHybrid {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: session clock mode %q requires a legacy, current, or hybrid schema", c.Name, c.SessionClock)
		}
	}
	if err := c.CatalogPadding.validate(c.Schema); err != nil {
		return fmt.Errorf("validate synthetic OpenCode source fixture %q catalog padding: %w", c.Name, err)
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

func (p catalogPaddingSpec) validate(schema schemaKind) error {
	if p.Tables < 0 || p.Columns < 0 || p.Indexes < 0 || p.Tables > 300 || p.Columns > 300 || p.Indexes > 300 {
		return fmt.Errorf("table/column/index counts must each be between 0 and 300, got %d/%d/%d", p.Tables, p.Columns, p.Indexes)
	}
	if (p.Columns != 0 || p.Indexes != 0) && schema != schemaCurrent && schema != schemaHybrid {
		return fmt.Errorf("column/index padding requires current or hybrid session_message schema, got %q", schema)
	}
	return nil
}

func (c expectedCatalogSpec) validate() error {
	if c.Seq != seqAbsent && c.Seq != seqNullable && c.Seq != seqNotNull {
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

func (c caseSpec) validateRowCounts() error {
	counts := c.DeclaredRows
	if counts.LegacyMessages < 0 || counts.LegacyParts < 0 || counts.CurrentMessages < 0 || counts.IgnoredHistory < 0 {
		return fmt.Errorf("validate synthetic OpenCode source fixture %q: declared row counts cannot be negative", c.Name)
	}
	if counts.LegacyMessages != len(c.LegacyMessages) || counts.LegacyParts != len(c.LegacyParts) || counts.CurrentMessages != len(c.CurrentMessages) || counts.IgnoredHistory != len(c.IgnoredHistory) {
		return fmt.Errorf(
			"validate synthetic OpenCode source fixture %q row guard: declared legacy_messages=%d legacy_parts=%d current_messages=%d ignored_history=%d; actual=%d/%d/%d/%d",
			c.Name,
			counts.LegacyMessages,
			counts.LegacyParts,
			counts.CurrentMessages,
			counts.IgnoredHistory,
			len(c.LegacyMessages),
			len(c.LegacyParts),
			len(c.CurrentMessages),
			len(c.IgnoredHistory),
		)
	}
	return nil
}

func (c caseSpec) validateRows() error {
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
	hasDuplicateCurrentSeq := false
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
			if c.Schema != schemaCurrentPartialSeq {
				return fmt.Errorf("validate synthetic OpenCode source fixture %q: duplicate seq %d for current session %q", c.Name, row.Seq, row.SessionID)
			}
			hasDuplicateCurrentSeq = true
		}
		currentSeqs[seqKey] = struct{}{}
	}
	if c.Schema == schemaCurrentPartialSeq && !hasDuplicateCurrentSeq {
		return fmt.Errorf("validate synthetic OpenCode source fixture %q: partial current ordering schema must contain a duplicate current sequence that proves the cursor-loss hazard", c.Name)
	}
	historyIDs := make(map[string]struct{}, len(c.IgnoredHistory))
	for index, row := range c.IgnoredHistory {
		if err := validateHistoryRow(row); err != nil {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q ignored history %d: %w", c.Name, index, err)
		}
		if _, duplicate := historyIDs[row.ID]; duplicate {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: duplicate ignored history id %q", c.Name, row.ID)
		}
		historyIDs[row.ID] = struct{}{}
	}
	return nil
}

func (c caseSpec) validateSchemaRows() error {
	hasLegacy := len(c.LegacyMessages) != 0 || len(c.LegacyParts) != 0
	hasCurrent := len(c.CurrentMessages) != 0
	switch c.Schema {
	case schemaLegacy:
		if hasCurrent {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: legacy schema cannot contain current rows", c.Name)
		}
	case schemaCurrent, schemaCurrentPartialSeq:
		if hasLegacy {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: current schema cannot contain legacy rows", c.Name)
		}
	case schemaHybrid:
		// Both table families are available; either may intentionally be empty.
	case schemaEmpty, schemaCurrentMissingSeq, schemaCurrentNullableSeq, schemaUnsupported, "":
		if hasLegacy || hasCurrent {
			return fmt.Errorf("validate synthetic OpenCode source fixture %q: schema %q cannot contain transcript rows", c.Name, c.Schema)
		}
	}
	return nil
}

func validateLegacyMessage(row legacyMessage) error {
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

func validateLegacyPart(row legacyPart) error {
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

func validateCurrentMessage(row currentMessage) error {
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

func validateHistoryRow(row historyRow) error {
	if err := row.Kind.validate(); err != nil {
		return err
	}
	if err := requiredToken("id", row.ID); err != nil {
		return err
	}
	if err := requiredToken("session_id", row.SessionID); err != nil {
		return err
	}
	if err := requiredToken("stable_id", row.StableID); err != nil {
		return err
	}
	if err := requiredToken("data", row.Data); err != nil {
		return err
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

func (f sourceFormat) validate() error {
	switch f {
	case sourceFormatSQLite, sourceFormatCorrupt:
		return nil
	default:
		return fmt.Errorf("unknown source format %q", f)
	}
}

func (s schemaKind) validate() error {
	switch s {
	case schemaEmpty, schemaLegacy, schemaCurrent, schemaHybrid, schemaCurrentMissingSeq, schemaCurrentNullableSeq, schemaCurrentPartialSeq, schemaUnsupported:
		return nil
	default:
		return fmt.Errorf("unknown schema kind %q", s)
	}
}

func (m journalMode) validate() error {
	switch m {
	case journalDelete, journalWAL:
		return nil
	default:
		return fmt.Errorf("unknown journal mode %q", m)
	}
}

func (c corruptionKind) validate() error {
	switch c {
	case corruptionNone, corruptionNonSQLite, corruptionTruncatedSQLite:
		return nil
	default:
		return fmt.Errorf("unknown corruption kind %q", c)
	}
}

func (h historyKind) validate() error {
	switch h {
	case historyEvent, historyDelta, historyInput, historyContext, historyMigration:
		return nil
	default:
		return fmt.Errorf("unknown ignored history kind %q", h)
	}
}
