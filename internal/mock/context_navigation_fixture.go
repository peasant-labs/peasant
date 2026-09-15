package mock

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/context_navigation.yaml
var contextNavigationFixtureYAML []byte

// The canonical mounted current-parent navigation fixture. See the YAML header
// for what the three stored sessions demonstrate. The Go mock provider and the
// real-binary visual smoke read the same file.
type contextNavigationTurnFixture struct {
	Index     int    `yaml:"index"`
	Role      string `yaml:"role"`
	EntryType string `yaml:"entryType"`
	Depth     int    `yaml:"depth"`
	Ref       string `yaml:"ref"`
	Content   string `yaml:"content"`
	Timestamp string `yaml:"timestamp"`
}

type contextNavigationEarlierFixture struct {
	State string                         `yaml:"state"`
	Turns []contextNavigationTurnFixture `yaml:"turns"`
}

type contextNavigationRelationshipFixture struct {
	Kind          string `yaml:"kind"`
	TargetState   string `yaml:"targetState"`
	TargetLocalID string `yaml:"targetLocalId"`
	Evidence      string `yaml:"evidence"`
}

type contextNavigationSessionFixture struct {
	ID                   string                                 `yaml:"id"`
	Project              string                                 `yaml:"project"`
	Harness              string                                 `yaml:"harness"`
	Model                string                                 `yaml:"model"`
	StartTime            string                                 `yaml:"startTime"`
	EndTime              string                                 `yaml:"endTime"`
	DurationMins         float64                                `yaml:"durationMins"`
	TotalTokens          int                                    `yaml:"totalTokens"`
	TokensIn             int                                    `yaml:"tokensIn"`
	TokensOut            int                                    `yaml:"tokensOut"`
	TurnCount            int                                    `yaml:"turnCount"`
	ToolCallCount        int                                    `yaml:"toolCallCount"`
	InputSubmissionCount *int64                                 `yaml:"inputSubmissionCount"`
	Turns                []contextNavigationTurnFixture         `yaml:"turns"`
	EarlierHistory       []contextNavigationEarlierFixture      `yaml:"earlierHistory"`
	Relationships        []contextNavigationRelationshipFixture `yaml:"relationships"`
}

type contextNavigationExpectedFixture struct {
	ContextLabel      string `yaml:"contextLabel"`
	StarterLabel      string `yaml:"starterLabel"`
	StarterLinkAction string `yaml:"starterLinkAction"`
	EarlierSummary    string `yaml:"earlierSummary"`
}

type contextNavigationFixture struct {
	Project  strikeProjectFixture             `yaml:"project"`
	Child    contextNavigationSessionFixture  `yaml:"child"`
	Source   contextNavigationSessionFixture  `yaml:"source"`
	Parent   contextNavigationSessionFixture  `yaml:"parent"`
	Expected contextNavigationExpectedFixture `yaml:"expected"`
}

var canonicalContextNavigationFixture = mustLoadContextNavigationFixture()

func mustLoadContextNavigationFixture() contextNavigationFixture {
	fixture, err := loadContextNavigationFixture(contextNavigationFixtureYAML)
	if err != nil {
		panic(fmt.Sprintf("embedded mounted current-parent navigation fixture could not be loaded because its shared payload is invalid at internal/mock/testdata/context_navigation.yaml during mock-provider initialization; the transcript navigation validation surface cannot start safely; fix the named fixture field and rebuild Peasant: %v", err))
	}
	return fixture
}

func loadContextNavigationFixture(source []byte) (contextNavigationFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	var fixture contextNavigationFixture
	if err := decoder.Decode(&fixture); err != nil {
		return contextNavigationFixture{}, fmt.Errorf("decode strict YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return contextNavigationFixture{}, fmt.Errorf("expected exactly one YAML document, received trailing content")
	}

	projectHash, err := schema.NewProjectHash(fixture.Project.Hash)
	if err != nil {
		return contextNavigationFixture{}, fmt.Errorf("project.hash %q is invalid: %w", fixture.Project.Hash, err)
	}
	if projectHash != mockProjectHash(fixture.Project.Name) {
		return contextNavigationFixture{}, fmt.Errorf("project.hash %q does not match mock project %q", projectHash, fixture.Project.Name)
	}
	for name, session := range map[string]contextNavigationSessionFixture{
		"child":  fixture.Child,
		"source": fixture.Source,
		"parent": fixture.Parent,
	} {
		if _, err := schema.NewSessionID(session.ID); err != nil {
			return contextNavigationFixture{}, fmt.Errorf("%s.id %q is invalid: %w", name, session.ID, err)
		}
		if session.Project != fixture.Project.Name {
			return contextNavigationFixture{}, fmt.Errorf("%s.project %q must be %q", name, session.Project, fixture.Project.Name)
		}
		if session.TurnCount != len(session.Turns) {
			return contextNavigationFixture{}, fmt.Errorf("%s.turnCount %d must equal its %d turns", name, session.TurnCount, len(session.Turns))
		}
		if _, err := time.Parse(time.RFC3339, session.StartTime); err != nil {
			return contextNavigationFixture{}, fmt.Errorf("%s.startTime %q is invalid: %w", name, session.StartTime, err)
		}
		if _, err := time.Parse(time.RFC3339, session.EndTime); err != nil {
			return contextNavigationFixture{}, fmt.Errorf("%s.endTime %q is invalid: %w", name, session.EndTime, err)
		}
	}
	if fixture.Child.InputSubmissionCount == nil || *fixture.Child.InputSubmissionCount != 1 {
		return contextNavigationFixture{}, fmt.Errorf("child.inputSubmissionCount must be present and 1 for the one-submission fixture")
	}
	if fixture.Child.TurnCount != 5 {
		return contextNavigationFixture{}, fmt.Errorf("child.turnCount must be 5, received %d", fixture.Child.TurnCount)
	}
	if len(fixture.Child.EarlierHistory) != 1 || len(fixture.Child.EarlierHistory[0].Turns) == 0 {
		return contextNavigationFixture{}, fmt.Errorf("child.earlierHistory must carry exactly one non-empty retained partition")
	}
	if len(fixture.Child.Relationships) != 2 {
		return contextNavigationFixture{}, fmt.Errorf("child.relationships must carry the context_from source and the started_by parent, received %d", len(fixture.Child.Relationships))
	}
	for kind, want := range map[string]string{
		string(schema.SessionRelationshipContextFrom): fixture.Source.ID,
		string(schema.SessionRelationshipStartedBy):   fixture.Parent.ID,
	} {
		found := ""
		for _, relationship := range fixture.Child.Relationships {
			if relationship.Kind == kind {
				found = relationship.TargetLocalID
			}
		}
		if found != want {
			return contextNavigationFixture{}, fmt.Errorf("child relationship %q must target %q, received %q", kind, want, found)
		}
	}
	if fixture.Expected.ContextLabel == "" || fixture.Expected.StarterLabel == "" || fixture.Expected.StarterLinkAction == "" || fixture.Expected.EarlierSummary == "" {
		return contextNavigationFixture{}, fmt.Errorf("expected labels and summary must be non-empty")
	}
	return fixture, nil
}

func (fixture contextNavigationFixture) storedSessions() []ingest.Session {
	sessions := make([]ingest.Session, 0, 3)
	sessions = append(sessions, fixture.session(fixture.Child), fixture.session(fixture.Source), fixture.session(fixture.Parent))
	return sessions
}

func (fixture contextNavigationFixture) session(source contextNavigationSessionFixture) ingest.Session {
	startTime, _ := time.Parse(time.RFC3339, source.StartTime)
	endTime, _ := time.Parse(time.RFC3339, source.EndTime)
	session := ingest.Session{
		ID:        schema.SessionID(source.ID),
		Project:   source.Project,
		Harness:   schema.Harness(source.Harness),
		StartTime: startTime,
		EndTime:   endTime,
		Turns:     contextNavigationTurns(source.Turns),
		Model:     source.Model,
		Metadata: ingest.SessionMetadata{
			TokensIn:      source.TokensIn,
			TokensOut:     source.TokensOut,
			TotalTokens:   source.TotalTokens,
			Duration:      endTime.Sub(startTime),
			TurnCount:     source.TurnCount,
			ToolCallCount: source.ToolCallCount,
		},
		InputSubmissionCount: source.InputSubmissionCount,
	}
	for _, earlier := range source.EarlierHistory {
		session.EarlierHistory = append(session.EarlierHistory, ingest.EarlierHistorySection{
			State: schema.EarlierHistoryState(earlier.State),
			Turns: contextNavigationTurns(earlier.Turns),
		})
	}
	for _, relationship := range source.Relationships {
		target := schema.SessionID(relationship.TargetLocalID)
		session.Relationships = append(session.Relationships, schema.SessionRelationship{
			Kind:          schema.SessionRelationshipKind(relationship.Kind),
			TargetState:   schema.RelationshipTargetState(relationship.TargetState),
			TargetLocalID: &target,
			Evidence:      schema.EvidenceKind(relationship.Evidence),
		})
	}
	return session
}

func contextNavigationTurns(sources []contextNavigationTurnFixture) []ingest.Turn {
	turns := make([]ingest.Turn, len(sources))
	for index, source := range sources {
		timestamp, _ := time.Parse(time.RFC3339, source.Timestamp)
		turns[index] = ingest.Turn{
			Index:          source.Index,
			Role:           schema.Role(source.Role),
			Content:        source.Content,
			Timestamp:      timestamp,
			Depth:          source.Depth,
			EntryType:      schema.EntryType(source.EntryType),
			SourceEntryRef: source.Ref,
		}
	}
	return turns
}
