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
// for what the four stored sessions demonstrate. The Go mock provider and the
// real-binary visual harness read the same file.
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

// contextNavigationTeammateFixture is the recorded Claude Code teammate identity
// a session was spawned under. It is local read evidence, so only a session
// whose parent transcript records the spawn edge carries one.
type contextNavigationTeammateFixture struct {
	Team     string `yaml:"team"`
	Name     string `yaml:"name"`
	Evidence string `yaml:"evidence"`
}

type contextNavigationSessionFixture struct {
	ID                   string                                 `yaml:"id"`
	ParentSessionID      string                                 `yaml:"parentSessionId"`
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
	Teammate             *contextNavigationTeammateFixture      `yaml:"teammate"`
}

type contextNavigationExpectedFixture struct {
	ContextLabel      string `yaml:"contextLabel"`
	StarterLabel      string `yaml:"starterLabel"`
	StarterLinkAction string `yaml:"starterLinkAction"`
	EarlierSummary    string `yaml:"earlierSummary"`
}

type contextNavigationFixture struct {
	Project    strikeProjectFixture             `yaml:"project"`
	Child      contextNavigationSessionFixture  `yaml:"child"`
	Source     contextNavigationSessionFixture  `yaml:"source"`
	Parent     contextNavigationSessionFixture  `yaml:"parent"`
	Unresolved contextNavigationSessionFixture  `yaml:"unresolved"`
	Expected   contextNavigationExpectedFixture `yaml:"expected"`
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
		"child":      fixture.Child,
		"source":     fixture.Source,
		"parent":     fixture.Parent,
		"unresolved": fixture.Unresolved,
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
		if session.Teammate != nil {
			if err := session.Teammate.validate(name); err != nil {
				return contextNavigationFixture{}, err
			}
		}
		if session.ParentSessionID != "" {
			if _, err := schema.NewSessionID(session.ParentSessionID); err != nil {
				return contextNavigationFixture{}, fmt.Errorf("%s.parentSessionId %q is invalid: %w", name, session.ParentSessionID, err)
			}
		}
	}
	// The child is the session the mounted transcript opens, so its recorded
	// teammate identity is the evidence the read projection must carry. Requiring
	// it here keeps that evidence from disappearing silently when the fixture is
	// edited.
	if fixture.Child.Teammate == nil {
		return contextNavigationFixture{}, fmt.Errorf("child.teammate must be present: the mounted child is the transcript whose recorded teammate identity the local read serves, and without it the captured identity line would prove nothing")
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
	// A stored child's durable parent and its started_by target name the same
	// session: the served snapshot takes the generation's started_by target as
	// the logical parent, and the transcript's parent link reads that durable
	// field. A fixture that let the two disagree would prove a link the stored
	// read cannot support.
	if fixture.Child.ParentSessionID != fixture.Parent.ID {
		return contextNavigationFixture{}, fmt.Errorf("child.parentSessionId %q must be the started_by target %q, because a stored child's durable parent is the session its recorded spawn edge names", fixture.Child.ParentSessionID, fixture.Parent.ID)
	}
	if fixture.Expected.ContextLabel == "" || fixture.Expected.StarterLabel == "" || fixture.Expected.StarterLinkAction == "" || fixture.Expected.EarlierSummary == "" {
		return contextNavigationFixture{}, fmt.Errorf("expected labels and summary must be non-empty")
	}
	if len(fixture.Unresolved.Relationships) != 1 || fixture.Unresolved.Relationships[0].Kind != string(schema.SessionRelationshipStartedBy) {
		return contextNavigationFixture{}, fmt.Errorf("unresolved.relationships must carry the one started_by relationship whose target is no longer stored")
	}
	absent := fixture.Unresolved.Relationships[0].TargetLocalID
	for _, stored := range []string{fixture.Child.ID, fixture.Source.ID, fixture.Parent.ID, fixture.Unresolved.ID} {
		if absent == stored {
			return contextNavigationFixture{}, fmt.Errorf("unresolved relationship target %q names a stored session; the unavailable reference case requires an absent target", absent)
		}
	}
	return fixture, nil
}

// validate applies the contract's own teammate rules to the declared identity
// and additionally pins the evidence kind: the identity validates through
// schema.SessionTeammate so the fixture cannot state an identity the local read
// would refuse, and the evidence check keeps this fixture naming the one record
// kind a recorded spawn edge produces even if the contract's menu widens.
func (teammate contextNavigationTeammateFixture) validate(where string) error {
	const why = "the mounted transcript would render an identity the local read cannot serve; emit the team and name the parent transcript recorded, with the spawn evidence kind, or omit teammate"
	identity := schema.SessionTeammate{
		Team:     teammate.Team,
		Name:     teammate.Name,
		Evidence: schema.SessionTeammateEvidence(teammate.Evidence),
	}
	if err := identity.Validate(); err != nil {
		return fmt.Errorf("%s.teammate is invalid: %w; %s", where, err, why)
	}
	if identity.Evidence != schema.SessionTeammateEvidenceNativeDynamicWorkflow {
		return fmt.Errorf("%s.teammate.evidence %q must be %q: only a recorded native dynamic workflow spawn names a teammate, so another kind would claim evidence the stored parent transcript does not carry; %s",
			where, identity.Evidence, schema.SessionTeammateEvidenceNativeDynamicWorkflow, why)
	}
	return nil
}

// teammateIdentities returns the recorded teammate identity of each stored
// session that declares one, keyed by session identifier. The mock provider
// serves these on the local detail read exactly as the generation-backed store
// serves the spawn evidence it read from the parent transcript.
func (fixture contextNavigationFixture) teammateIdentities() map[string]schema.SessionTeammate {
	identities := make(map[string]schema.SessionTeammate, 1)
	for _, source := range []contextNavigationSessionFixture{fixture.Child, fixture.Source, fixture.Parent, fixture.Unresolved} {
		if source.Teammate == nil {
			continue
		}
		identities[source.ID] = schema.SessionTeammate{
			Team:     source.Teammate.Team,
			Name:     source.Teammate.Name,
			Evidence: schema.SessionTeammateEvidence(source.Teammate.Evidence),
		}
	}
	return identities
}

func (fixture contextNavigationFixture) storedSessions() []ingest.Session {
	sessions := make([]ingest.Session, 0, 4)
	sessions = append(sessions,
		fixture.session(fixture.Child),
		fixture.session(fixture.Source),
		fixture.session(fixture.Parent),
		fixture.session(fixture.Unresolved),
	)
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
	if source.ParentSessionID != "" {
		parent := schema.SessionID(source.ParentSessionID)
		session.ParentSessionID = &parent
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
