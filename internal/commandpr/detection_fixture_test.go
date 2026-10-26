package commandpr_test

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/commandpr"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

// detectionFixturePath names the corpus shared by the detection tests.
const detectionFixturePath = "internal/commandpr/testdata/detection.yaml"

//go:embed testdata/detection.yaml
var detectionFixtureYAML []byte

// detectionFixture is the strictly decoded pull request detection corpus.
type detectionFixture struct {
	// RequiredNames is the deletion-protection manifest for the call cases:
	// adding a case needs no change here, removing or renaming a required one
	// fails the load.
	RequiredNames []string `yaml:"requiredNames"`
	// URLRequiredNames is the same manifest for the URL edge cases.
	URLRequiredNames []string           `yaml:"urlRequiredNames"`
	Cases            []detectionCase    `yaml:"cases"`
	URLs             []detectionURLCase `yaml:"urls"`
}

// detectionCase is one tool-call fixture: what the harness recorded and the
// pull requests detection must return for it.
type detectionCase struct {
	Name     string           `yaml:"name"`
	Calls    []detectionCall  `yaml:"calls"`
	Expected []pullRequestRow `yaml:"expected"`
}

// detectionCall is one recorded tool call, in the shape the store keeps.
type detectionCall struct {
	Arguments string `yaml:"arguments"`
	Result    string `yaml:"result"`
	IsError   bool   `yaml:"isError"`
}

// detectionURLCase exercises result-text extraction through the same public
// detection entry point a real tool call uses.
type detectionURLCase struct {
	Name     string         `yaml:"name"`
	Text     string         `yaml:"text"`
	Expected pullRequestRow `yaml:"expected"`
}

// pullRequestRow is one expected detected pull request, as the fixture spells
// it. The fixture carries no source field, because every detected row carries
// the same detector constant, so the conversion below supplies it.
type pullRequestRow struct {
	Owner  string `yaml:"owner"`
	Repo   string `yaml:"repo"`
	Number int    `yaml:"number"`
	URL    string `yaml:"url"`
}

// loadDetectionFixture returns the one embedded, strictly validated corpus.
func loadDetectionFixture(t *testing.T) detectionFixture {
	t.Helper()
	fixture, err := decodeDetectionFixture(detectionFixtureYAML)
	if err != nil {
		t.Fatalf("load detection fixture: %v", err)
	}
	return fixture
}

func decodeDetectionFixture(source []byte) (detectionFixture, error) {
	var fixture detectionFixture
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return detectionFixture{}, fmt.Errorf("decode detection fixture %s: typed fields do not match the corpus schema: %w; fix the named YAML field", detectionFixturePath, err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return detectionFixture{}, fmt.Errorf("decode detection fixture %s: expected exactly one YAML document; remove the trailing document", detectionFixturePath)
		}
		return detectionFixture{}, fmt.Errorf("decode detection fixture %s trailing content: %w; remove or repair the trailing document", detectionFixturePath, err)
	}

	caseNames := make(map[string]bool, len(fixture.Cases))
	for index := range fixture.Cases {
		testCase := &fixture.Cases[index]
		if strings.TrimSpace(testCase.Name) == "" {
			return detectionFixture{}, fmt.Errorf("decode detection fixture %s: cases[%d] has a blank name; give every row a stable unique name", detectionFixturePath, index)
		}
		if caseNames[testCase.Name] {
			return detectionFixture{}, fmt.Errorf("decode detection fixture %s: case name %q is duplicated; give every row a unique name", detectionFixturePath, testCase.Name)
		}
		caseNames[testCase.Name] = true
		if len(testCase.Calls) == 0 {
			return detectionFixture{}, fmt.Errorf("decode detection fixture %s: case %q records no tool calls; every case describes what a session recorded", detectionFixturePath, testCase.Name)
		}
	}
	if err := testutil.RequireFixtureNames(fmt.Sprintf("detection fixture %s", detectionFixturePath), "case", fixture.RequiredNames, caseNames); err != nil {
		return detectionFixture{}, err
	}

	urlNames := make(map[string]bool, len(fixture.URLs))
	for index := range fixture.URLs {
		urlCase := &fixture.URLs[index]
		if strings.TrimSpace(urlCase.Name) == "" {
			return detectionFixture{}, fmt.Errorf("decode detection fixture %s: urls[%d] has a blank name; give every row a stable unique name", detectionFixturePath, index)
		}
		if urlNames[urlCase.Name] {
			return detectionFixture{}, fmt.Errorf("decode detection fixture %s: URL case name %q is duplicated; give every row a unique name", detectionFixturePath, urlCase.Name)
		}
		urlNames[urlCase.Name] = true
	}
	if err := testutil.RequireFixtureNames(fmt.Sprintf("detection fixture %s", detectionFixturePath), "URL case", fixture.URLRequiredNames, urlNames); err != nil {
		return detectionFixture{}, err
	}

	return fixture, nil
}

// expectedPullRequests converts fixture rows to the contract rows the detector
// returns.
func expectedPullRequests(rows []pullRequestRow) []schema.SessionPullRequest {
	if len(rows) == 0 {
		return nil
	}
	expected := make([]schema.SessionPullRequest, len(rows))
	for index, row := range rows {
		expected[index] = schema.SessionPullRequest{
			Owner:  row.Owner,
			Repo:   row.Repo,
			Number: row.Number,
			URL:    row.URL,
			Source: commandpr.SourceGHPRCreate,
		}
	}
	return expected
}
