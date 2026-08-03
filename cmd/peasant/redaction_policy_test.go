package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/redact"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/redaction_policy.yaml
var redactionPolicyFixtureData []byte

//go:embed testdata/redaction_policy_transcript.jsonl
var redactionPolicyTranscriptTemplate []byte

const redactionPolicyFixturePath = "cmd/peasant/testdata/redaction_policy.yaml"

// The placeholders the recorded transcript template carries, and the session it
// records. The recorded working directory is a path only the running test knows,
// so it is substituted at seed time.
const (
	recordedWorkingDirectoryPlaceholder = "{{RECORDED_WORKING_DIRECTORY}}"
	recordedSessionIDPlaceholder        = "{{SESSION_ID}}"
	recordedSessionID                   = "5a5a5a5a-1234-4123-8123-5a5a5a5a5a5a"
)

type redactionPolicyDocument struct {
	ExpectedCaseCount int                   `yaml:"expectedCaseCount"`
	Cases             []redactionPolicyCase `yaml:"cases"`
}

type redactionPolicyCase struct {
	Name                      string                `yaml:"name"`
	Level                     redact.RedactionLevel `yaml:"level"`
	RecordedDirectoryBasename string                `yaml:"recordedDirectoryBasename"`
	EntropyPinned             bool                  `yaml:"entropyPinned"`
	IngestRedacts             bool                  `yaml:"ingestRedacts"`
}

// loadRedactionPolicyFixture decodes and fully validates the corpus.
func loadRedactionPolicyFixture(data []byte) (redactionPolicyDocument, error) {
	var document redactionPolicyDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, redactionPolicyRuleError(
			"typed YAML fields must match the document schema", "loader=first-document decode",
			fmt.Sprintf("fix=remove unknown fields and match the typed schema: %v", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found another YAML document")
		}
		return document, redactionPolicyRuleError(
			"exactly one YAML document is allowed; cases below a second one prove nothing",
			"loader=end-of-document check",
			fmt.Sprintf("fix=remove the second document so the next decode returns EOF: %v", err))
	}
	if len(document.Cases) == 0 || document.ExpectedCaseCount != len(document.Cases) {
		return document, redactionPolicyRuleError(
			fmt.Sprintf("declared and actual case counts must match and be non-zero, got expectedCaseCount=%d cases=%d",
				document.ExpectedCaseCount, len(document.Cases)),
			"loader=case-count validation",
			"fix=set expectedCaseCount to the number of cases present")
	}
	seen := map[string]bool{}
	sawEntropyPinned := false
	for index, testCase := range document.Cases {
		if strings.TrimSpace(testCase.Name) == "" || seen[testCase.Name] {
			return document, redactionPolicyRuleError(
				fmt.Sprintf("case name %q is missing or duplicated", testCase.Name),
				fmt.Sprintf("loader=case index %d", index),
				"fix=give every case a unique, behaviour-naming name")
		}
		seen[testCase.Name] = true
		if !testCase.Level.IsValid() {
			return document, redactionPolicyRuleError(
				fmt.Sprintf("case %q names an unknown redaction level %q", testCase.Name, testCase.Level),
				fmt.Sprintf("loader=case index %d", index),
				"fix=use one of minimal, standard, maximum")
		}
		if strings.TrimSpace(testCase.RecordedDirectoryBasename) == "" {
			return document, redactionPolicyRuleError(
				fmt.Sprintf("case %q records no directory basename", testCase.Name),
				fmt.Sprintf("loader=case index %d", index),
				"fix=name the directory the session was recorded in; the slug is derived from it")
		}
		if testCase.EntropyPinned {
			sawEntropyPinned = true
		}
	}
	if !sawEntropyPinned {
		return document, redactionPolicyRuleError(
			"no case records a directory whose slug is proven redactable",
			"loader=risk coverage",
			"fix=add one; every remaining case would pass on a slug that was never at risk, which proves nothing")
	}
	return document, nil
}

func redactionPolicyRuleError(what, where, fix string) error {
	return fmt.Errorf(
		"redaction policy fixture rule failed: %s; a malformed or toothless corpus invalidates the only evidence that a "+
			"session recorded under any configured level can still be published; where=%s %s; when=test fixture loading; "+
			"impact=a repository could silently become permanently unpublishable again; %s",
		what, redactionPolicyFixturePath, where, fix)
}

// --- loader guards ----------------------------------------------------------

func TestLoadRedactionPolicyFixture_RejectsACorpusWithNothingAtRisk(t *testing.T) {
	t.Parallel()
	_, err := loadRedactionPolicyFixture([]byte(`expectedCaseCount: 1
cases:
  - name: ordinary-directory-only
    level: maximum
    recordedDirectoryBasename: ledger-service
    entropyPinned: false
    ingestRedacts: false
`))
	if err == nil || !strings.Contains(err.Error(), "no case records a directory whose slug is proven redactable") {
		t.Fatalf("error = %v, want rejection of a corpus in which nothing was ever at risk", err)
	}
}

// --- the corpus -------------------------------------------------------------

// TestHarvest_RecordsOneHostSlugForEveryConfiguredRedactionLevel is the
// end-to-end proof that an import can no longer make a repository permanently
// unpublishable.
//
// The slug was corrupted by a redactor running while ingest wrote: the directory
// was built from the real slug and the database row and metadata file from the
// redacted one, so push resolved a directory that never existed. No supported
// level runs a redactor at import, and the one that did is now refused outright,
// so the three records must always agree.
//
// It drives the real harvest command over a real store and a real filesystem,
// with a session recorded in a directory that has no origin remote - so its slug
// is derived from the path, which is the slug that used to be rewritten. The
// three records of that slug must be identical afterwards, and the metadata file
// must be readable at the path PUSH resolves from the database row, because that
// resolution is what used to fail forever.
//
// The corpus is entropy-pinned rather than salt-pinned: a pinned basename is
// proven against the real engine to be redactable, so a case cannot pass merely
// because its slug happened to survive.
func TestHarvest_RecordsOneHostSlugForEveryConfiguredRedactionLevel(t *testing.T) {
	t.Parallel()
	document, err := loadRedactionPolicyFixture(redactionPolicyFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			world := seedRecordedSession(t, testCase)

			output, harvestErr := runHarvestWithConfig(t, world)
			if harvestErr != nil {
				t.Fatalf("harvest at the %s level must succeed: %v\noutput: %s", testCase.Level, harvestErr, output)
			}

			recorded := readRecordedSlug(t, world)
			assertEntropyPinning(t, testCase, recorded.directorySlug)

			if recorded.directorySlug != recorded.databaseSlug {
				t.Errorf("the directory ingest wrote is %q but the database records %q; push resolves the metadata path from the "+
					"database, so it would look for a directory that does not exist and this session could never publish",
					recorded.directorySlug, recorded.databaseSlug)
			}
			if recorded.metadataSlug != recorded.databaseSlug {
				t.Errorf("the metadata file records the slug %q but the database records %q; the two describe the same session",
					recorded.metadataSlug, recorded.databaseSlug)
			}
			for label, slug := range map[string]string{
				"directory": recorded.directorySlug, "database": recorded.databaseSlug, "metadata file": recorded.metadataSlug,
			} {
				if strings.ContainsAny(slug, "<>") {
					t.Errorf("the %s slug %q carries a redaction placeholder: ingest redacted a value it also has to resolve by",
						label, slug)
				}
			}
			if testCase.IngestRedacts {
				t.Fatalf("no level this version applies redacts while ingest writes; the corpus claims otherwise for %q", testCase.Name)
			}
		})
	}
}

// recordedSlug is the same host slug read from each of the three places that
// have to agree about it.
type recordedSlug struct {
	directorySlug string
	databaseSlug  string
	metadataSlug  string
}

// harvestWorld is one isolated ingest: a recorded working directory with no
// origin remote, a claude source tree, a config at the case's level, and the
// data/config directories the command is scoped to.
type harvestWorld struct {
	root              string
	configPath        string
	sourcePath        string
	outputPath        string
	recordedDirectory string
}

// seedRecordedSession writes the transcript template into a claude source tree,
// with its recorded working directory pointed at a directory whose basename the
// case pins. That directory is deliberately NOT a git repository with a remote:
// the path-derived slug is the one the defect corrupted.
func seedRecordedSession(t *testing.T, testCase redactionPolicyCase) harvestWorld {
	t.Helper()
	root := t.TempDir()
	world := harvestWorld{
		root:              root,
		configPath:        filepath.Join(root, "config.yaml"),
		sourcePath:        filepath.Join(root, string(defaults.HarnessClaudeCode), "projects"),
		outputPath:        filepath.Join(root, "peasant-sync"),
		recordedDirectory: filepath.Join(root, "worktrees", testCase.RecordedDirectoryBasename),
	}
	if err := os.MkdirAll(world.recordedDirectory, defaults.PrivateDirPerm); err != nil {
		t.Fatalf("create the recorded working directory: %v", err)
	}
	transcriptDir := filepath.Join(world.sourcePath, "-recorded-project")
	if err := os.MkdirAll(transcriptDir, defaults.PrivateDirPerm); err != nil {
		t.Fatalf("create the claude source tree: %v", err)
	}
	transcript := strings.ReplaceAll(string(redactionPolicyTranscriptTemplate),
		recordedWorkingDirectoryPlaceholder, world.recordedDirectory)
	transcript = strings.ReplaceAll(transcript, recordedSessionIDPlaceholder, recordedSessionID)
	if strings.Contains(transcript, "{{") {
		t.Fatalf("the recorded transcript still carries an unsubstituted placeholder:\n%s", transcript)
	}
	if err := os.WriteFile(filepath.Join(transcriptDir, recordedSessionID+".jsonl"),
		[]byte(transcript), defaults.PrivateFilePerm); err != nil {
		t.Fatalf("write the recorded transcript: %v", err)
	}
	configYAML := fmt.Sprintf(`version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - %s
  opencode:
    enabled: false
  cursor:
    enabled: false
output:
  basePath: %s
redaction:
  level: %s
`, world.sourcePath, world.outputPath, testCase.Level)
	if err := os.WriteFile(world.configPath, []byte(configYAML), defaults.PrivateFilePerm); err != nil {
		t.Fatalf("write the config: %v", err)
	}
	// The configuration has to survive the run intact: the disclosure names what
	// the user asked for, so a clamp that rewrote it would have nothing to name.
	t.Cleanup(func() { assertConfiguredLevelSurvived(t, world.configPath, testCase.Level) })
	return world
}

// runHarvestWithConfig runs the production harvest command against one world.
// --include-active is required because the transcript was written moments ago,
// and ingest skips a source that is still being written to.
func runHarvestWithConfig(t *testing.T, world harvestWorld) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "peasant"}
	root.PersistentFlags().String("config", "", "")
	root.PersistentFlags().String("data-dir", "", "")
	root.PersistentFlags().String("config-dir", "", "")
	root.PersistentFlags().String("state-dir", "", "")
	root.AddCommand(BuildHarvestCommand())

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{
		"harvest",
		"--config", world.configPath,
		"--config-dir", world.root,
		"--data-dir", world.root,
		"--state-dir", world.root,
		"--include-active",
	})
	err := root.Execute()
	return out.String(), err
}

// readRecordedSlug reads the slug from all three places, resolving the metadata
// path exactly the way push does: from the database row.
func readRecordedSlug(t *testing.T, world harvestWorld) recordedSlug {
	t.Helper()
	entries, err := os.ReadDir(world.outputPath)
	if err != nil {
		t.Fatalf("read the output tree ingest wrote: %v", err)
	}
	var directories []string
	for _, entry := range entries {
		if entry.IsDir() {
			directories = append(directories, entry.Name())
		}
	}
	if len(directories) != 1 {
		t.Fatalf("one recorded session must write exactly one slug directory, got %v", directories)
	}

	db, err := store.Open(string(defaults.ResolveDBFilePathWith(world.root)))
	if err != nil {
		t.Fatalf("open the analytics store: %v", err)
	}
	defer db.Close()
	rows, err := db.UnpushedSessions(context.Background())
	if err != nil {
		t.Fatalf("read the sessions push would publish: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("push must see exactly the one recorded session, got %d", len(rows))
	}
	row := rows[0]

	// The production resolution: push builds this path from the database row.
	metadataPath := ingest.SessionMetadataPath(world.outputPath, row.HostSlug, row.SessionID, row.ParentID)
	metaBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("push resolves the metadata path from the recorded slug and would fail forever here: %v", err)
	}
	var meta ingest.UnifiedMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("parse the metadata push reads: %v", err)
	}
	return recordedSlug{
		directorySlug: directories[0],
		databaseSlug:  row.HostSlug,
		metadataSlug:  string(meta.HostSlug),
	}
}

// assertEntropyPinning proves a pinned case is not vacuous: the slug it records
// really would be replaced by a placeholder if a maximum redactor ever ran over
// it, so a green case means the fix works rather than that nothing was at risk.
//
// Only the pinned direction is asserted. An ordinary basename cannot be pinned
// the other way, because the eight-hex segment of an untracked slug is derived
// from a per-install salt and trips on its own for a large minority of installs -
// the reason this was never one unlucky path. Constructing a maximum redactor
// needs cgo, so where that is unavailable the pin is reported as unprovable
// rather than silently assumed.
func assertEntropyPinning(t *testing.T, testCase redactionPolicyCase, slug string) {
	t.Helper()
	if !testCase.EntropyPinned {
		return
	}
	if !redact.MaximumAvailable {
		t.Logf("the entropy pin is unprovable in this build: a maximum redactor cannot be constructed without cgo")
		return
	}
	redactor, err := redact.NewRedactor(redact.Maximum, nil, redact.XDGPaths{})
	if err != nil {
		t.Fatalf("construct the maximum redactor the pin is measured against: %v", err)
	}
	if !strings.Contains(redactor.RedactText(slug), "<HIGH_ENTROPY>") {
		t.Errorf("the corpus pins the recorded directory %q as redactable, but a maximum redactor leaves the slug %q intact; "+
			"an unpinned case proves nothing, so pick a basename that pins",
			testCase.RecordedDirectoryBasename, slug)
	}
}

// assertConfiguredLevelSurvived reads the configuration back off disk. Resolution
// must never rewrite it: the user's value is correct for the version that
// implements it, and the disclosure has to be able to name what they asked for.
func assertConfiguredLevelSurvived(t *testing.T, configPath string, want redact.RedactionLevel) {
	t.Helper()
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read the configuration back: %v", err)
	}
	var written struct {
		Redaction struct {
			Level redact.RedactionLevel `yaml:"level"`
		} `yaml:"redaction"`
	}
	if err := yaml.Unmarshal(data, &written); err != nil {
		t.Fatalf("parse the configuration back: %v", err)
	}
	if written.Redaction.Level != want {
		t.Errorf("the configured level was rewritten from %q to %q; resolution must leave the configuration alone",
			want, written.Redaction.Level)
	}
	// The loaded configuration keeps it too, which is what the disclosure names.
	loaded, err := config.Load(configPath, &ingest.OSFileSystem{}, &ingest.ExecGitResolver{})
	if err != nil {
		t.Fatalf("load the configuration the way the commands do: %v", err)
	}
	if loaded.Redaction.Level != want {
		t.Errorf("config.Load returned the level %q for a configuration that says %q; the configured value has to survive so the "+
			"disclosure can name it", loaded.Redaction.Level, want)
	}
}
