package mock

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/review_timeline_order.yaml
var reviewTimelineOrderYAML []byte

const reviewTimelineOrderCaseCount = 1

var requiredReviewTimelineOrderNames = map[string]struct{}{
	"noncanonical sessions and bindings normalize together": {},
}

type reviewTimelineOrderFixture struct {
	ExpectedCaseCount int      `yaml:"expectedCaseCount"`
	RequiredNames     []string `yaml:"requiredNames"`
	Cases             []struct {
		Name     string                   `yaml:"name"`
		Input    schema.ReviewListPayload `yaml:"input"`
		Expected struct {
			SessionCount       int                `yaml:"sessionCount"`
			CommitCount        int                `yaml:"commitCount"`
			SessionOrder       []schema.SessionID `yaml:"sessionOrder"`
			CommitBindingOrder []schema.SessionID `yaml:"commitBindingOrder"`
		} `yaml:"expected"`
	} `yaml:"cases"`
}

func decodeReviewTimelineOrder(source []byte) (reviewTimelineOrderFixture, error) {
	var fixture reviewTimelineOrderFixture
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, fmt.Errorf("decode review timeline ordering fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fixture, fmt.Errorf("review timeline ordering fixture must contain exactly one YAML document: %v", err)
	}
	if fixture.ExpectedCaseCount != reviewTimelineOrderCaseCount || len(fixture.RequiredNames) != reviewTimelineOrderCaseCount || len(fixture.Cases) != reviewTimelineOrderCaseCount {
		return fixture, fmt.Errorf("review timeline ordering fixture has invalid cardinality")
	}
	required := make(map[string]struct{}, len(fixture.RequiredNames))
	for _, name := range fixture.RequiredNames {
		if name == "" {
			return fixture, fmt.Errorf("review timeline ordering fixture has an empty required name")
		}
		if _, ok := requiredReviewTimelineOrderNames[name]; !ok {
			return fixture, fmt.Errorf("review timeline ordering fixture has unknown required name %q", name)
		}
		if _, duplicate := required[name]; duplicate {
			return fixture, fmt.Errorf("review timeline ordering fixture repeats required name %q", name)
		}
		required[name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" {
			return fixture, fmt.Errorf("review timeline ordering fixture has an empty case name")
		}
		if _, duplicate := seen[testCase.Name]; duplicate {
			return fixture, fmt.Errorf("review timeline ordering fixture repeats case %q", testCase.Name)
		}
		if _, ok := required[testCase.Name]; !ok {
			return fixture, fmt.Errorf("review timeline ordering fixture has unknown case %q", testCase.Name)
		}
		seen[testCase.Name] = struct{}{}
		if testCase.Expected.SessionCount <= 0 || testCase.Expected.CommitCount <= 0 || len(testCase.Input.Sessions) != testCase.Expected.SessionCount || len(testCase.Input.RecentCommits) != testCase.Expected.CommitCount || len(testCase.Expected.SessionOrder) != testCase.Expected.SessionCount || len(testCase.Expected.CommitBindingOrder) != testCase.Expected.SessionCount {
			return fixture, fmt.Errorf("review timeline ordering fixture case %q has incomplete cardinality", testCase.Name)
		}
		ids := make(map[schema.SessionID]struct{}, testCase.Expected.SessionCount)
		for _, session := range testCase.Input.Sessions {
			if session.SessionID == "" {
				return fixture, fmt.Errorf("review timeline ordering fixture case %q has an empty session ID", testCase.Name)
			}
			if _, duplicate := ids[session.SessionID]; duplicate {
				return fixture, fmt.Errorf("review timeline ordering fixture case %q repeats session ID %q", testCase.Name, session.SessionID)
			}
			ids[session.SessionID] = struct{}{}
		}
	}
	return fixture, nil
}

func TestCanonicalizeReviewTimeline_FromNoncanonicalFixture(t *testing.T) {
	fixture, err := decodeReviewTimelineOrder(reviewTimelineOrderYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			payload := testCase.Input
			if err := payload.Validate(); err == nil {
				t.Fatal("deliberately noncanonical review fixture unexpectedly validated before normalization")
			}
			canonicalizeReviewTimeline(&payload)
			if err := payload.Validate(); err != nil {
				t.Fatalf("canonical review fixture did not validate: %v", err)
			}
			for index, sessionID := range testCase.Expected.SessionOrder {
				if payload.Sessions[index].SessionID != sessionID {
					t.Fatalf("canonical session rank %d = %q, want %q", index, payload.Sessions[index].SessionID, sessionID)
				}
			}
			for index, sessionID := range testCase.Expected.CommitBindingOrder {
				if payload.RecentCommits[0].SessionIDs[index] != sessionID {
					t.Fatalf("canonical binding rank %d = %q, want %q", index, payload.RecentCommits[0].SessionIDs[index], sessionID)
				}
			}
		})
	}
}

func TestReviewTimelineOrderFixtureRejectsStructuralAndSemanticMutations(t *testing.T) {
	mutations := map[string][]byte{
		"unknown field":           bytes.Replace(reviewTimelineOrderYAML, []byte("expectedCaseCount:"), []byte("unknown: true\nexpectedCaseCount:"), 1),
		"trailing document":       append(append([]byte{}, reviewTimelineOrderYAML...), []byte("\n---\nextra: true\n")...),
		"duplicate required name": bytes.Replace(reviewTimelineOrderYAML, []byte("requiredNames:\n  - noncanonical sessions and bindings normalize together"), []byte("requiredNames:\n  - noncanonical sessions and bindings normalize together\n  - noncanonical sessions and bindings normalize together"), 1),
		"renamed case":            bytes.Replace(reviewTimelineOrderYAML, []byte("  - name: noncanonical sessions and bindings normalize together"), []byte("  - name: renamed ordering behavior"), 1),
		"deleted case":            bytes.Replace(reviewTimelineOrderYAML, []byte("cases:\n  - name:"), []byte("cases: []\nremoved:\n  - name:"), 1),
	}
	for name, source := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeReviewTimelineOrder(source); err == nil {
				t.Fatalf("%s mutation unexpectedly validated", name)
			}
		})
	}
	if !strings.Contains(string(reviewTimelineOrderYAML), "commitBindingOrder") {
		t.Fatal("fixture no longer owns the expected commit binding order")
	}
}
