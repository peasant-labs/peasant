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

//go:embed testdata/strike_mounted_web.yaml
var strikeMountedWebFixtureYAML []byte

type strikeProjectFixture struct {
	Hash string `yaml:"hash"`
	Name string `yaml:"name"`
}

type strikeTurnFixture struct {
	Index      int    `yaml:"index"`
	Role       string `yaml:"role"`
	Depth      int    `yaml:"depth"`
	Content    string `yaml:"content"`
	Timestamp  string `yaml:"timestamp"`
	StopReason string `yaml:"stopReason"`
	ToolCalls  []any  `yaml:"toolCalls"`
}

type strikeSessionDetailFixture struct {
	ID            string              `yaml:"id"`
	Project       string              `yaml:"project"`
	Harness       string              `yaml:"harness"`
	Model         string              `yaml:"model"`
	StartTime     string              `yaml:"startTime"`
	EndTime       string              `yaml:"endTime"`
	DurationMins  float64             `yaml:"durationMins"`
	TotalTokens   int                 `yaml:"totalTokens"`
	TokensIn      int                 `yaml:"tokensIn"`
	TokensOut     int                 `yaml:"tokensOut"`
	TurnCount     int                 `yaml:"turnCount"`
	ToolCallCount int                 `yaml:"toolCallCount"`
	Turns         []strikeTurnFixture `yaml:"turns"`
}

type strikeMapSessionFixture struct {
	ID            string  `yaml:"id"`
	Harness       string  `yaml:"harness"`
	StartTime     string  `yaml:"startTime"`
	DurationMins  float64 `yaml:"durationMins"`
	TotalTokens   int     `yaml:"totalTokens"`
	TurnCount     int     `yaml:"turnCount"`
	ToolCallCount int     `yaml:"toolCallCount"`
	Project       string  `yaml:"project"`
	ProjectHash   string  `yaml:"projectHash"`
	Preview       string  `yaml:"preview"`
}

type strikeMapCommitFixture struct {
	Hash    string `yaml:"hash"`
	Subject string `yaml:"subject"`
	TimeMS  int64  `yaml:"timeMs"`
}

type strikeReviewChangeFixture struct {
	Branch       string `yaml:"branch"`
	AheadCount   int    `yaml:"aheadCount"`
	BehindCount  int    `yaml:"behindCount"`
	FilesChanged int    `yaml:"filesChanged"`
	SessionCount int    `yaml:"sessionCount"`
	TaskCount    int    `yaml:"taskCount"`
	NewEdges     int    `yaml:"newEdges"`
	RemovedEdges int    `yaml:"removedEdges"`
	Violations   int    `yaml:"violations"`
	Merged       bool   `yaml:"merged"`
	TipCommitMS  int64  `yaml:"tipCommitMs"`
}

type strikeReviewSessionFixture struct {
	SessionID        string `yaml:"sessionId"`
	Title            string `yaml:"title"`
	Harness          string `yaml:"harness"`
	StartMS          int64  `yaml:"startMs"`
	HasCommitBinding bool   `yaml:"hasCommitBinding"`
}

type strikeReviewListFixture struct {
	ProjectHash      string                       `yaml:"projectHash"`
	RepoFound        bool                         `yaml:"repoFound"`
	DefaultBranch    string                       `yaml:"defaultBranch"`
	Changes          []strikeReviewChangeFixture  `yaml:"changes"`
	RecentCommits    []any                        `yaml:"recentCommits"`
	Sessions         []strikeReviewSessionFixture `yaml:"sessions"`
	RewrittenCommits []any                        `yaml:"rewrittenCommits"`
}

type strikeExpectedFixture struct {
	AssistantContent     string `yaml:"assistantContent"`
	MapConversationTitle string `yaml:"mapConversationTitle"`
	ReviewSessionTitle   string `yaml:"reviewSessionTitle"`
}

type strikeMountedWebFixture struct {
	Project       strikeProjectFixture       `yaml:"project"`
	SessionDetail strikeSessionDetailFixture `yaml:"sessionDetail"`
	MapSession    strikeMapSessionFixture    `yaml:"mapSession"`
	MapCommit     strikeMapCommitFixture     `yaml:"mapCommit"`
	ReviewList    strikeReviewListFixture    `yaml:"reviewList"`
	Expected      strikeExpectedFixture      `yaml:"expected"`
}

var canonicalStrikeMockFixture = mustLoadStrikeMountedWebFixture()

func mustLoadStrikeMountedWebFixture() strikeMountedWebFixture {
	fixture, err := loadStrikeMountedWebFixture(strikeMountedWebFixtureYAML)
	if err != nil {
		panic(fmt.Sprintf("embedded Strike mock fixture could not be loaded because its shared payload is invalid at internal/mock/testdata/strike_mounted_web.yaml during mock-provider initialization; the transcript, Map, and Changes validation surfaces cannot start safely; fix the named fixture field and rebuild Peasant: %v", err))
	}
	return fixture
}

func loadStrikeMountedWebFixture(source []byte) (strikeMountedWebFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	var fixture strikeMountedWebFixture
	if err := decoder.Decode(&fixture); err != nil {
		return strikeMountedWebFixture{}, fmt.Errorf("decode strict YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return strikeMountedWebFixture{}, fmt.Errorf("expected exactly one YAML document, received trailing content")
	}

	projectHash, err := schema.NewProjectHash(fixture.Project.Hash)
	if err != nil {
		return strikeMountedWebFixture{}, fmt.Errorf("project.hash %q is invalid: %w", fixture.Project.Hash, err)
	}
	if projectHash != mockProjectHash(fixture.Project.Name) {
		return strikeMountedWebFixture{}, fmt.Errorf("project.hash %q does not match mock project %q", projectHash, fixture.Project.Name)
	}
	sessionID, err := schema.NewSessionID(fixture.SessionDetail.ID)
	if err != nil {
		return strikeMountedWebFixture{}, fmt.Errorf("sessionDetail.id %q is invalid: %w", fixture.SessionDetail.ID, err)
	}
	if fixture.SessionDetail.Harness != schema.HarnessStrike.String() || fixture.MapSession.Harness != schema.HarnessStrike.String() {
		return strikeMountedWebFixture{}, fmt.Errorf("sessionDetail and mapSession harnesses must both be %q", schema.HarnessStrike)
	}
	if fixture.SessionDetail.Project != fixture.Project.Name || fixture.MapSession.Project != fixture.Project.Name || fixture.MapSession.ProjectHash != fixture.Project.Hash {
		return strikeMountedWebFixture{}, fmt.Errorf("project identity must agree across project, sessionDetail, and mapSession")
	}
	if fixture.MapSession.ID != sessionID.String() || fixture.MapSession.Preview != fixture.Expected.MapConversationTitle {
		return strikeMountedWebFixture{}, fmt.Errorf("mapSession identity and preview must match the canonical session and expected conversation title")
	}
	if fixture.SessionDetail.TurnCount != len(fixture.SessionDetail.Turns) || len(fixture.SessionDetail.Turns) != 2 {
		return strikeMountedWebFixture{}, fmt.Errorf("sessionDetail must contain exactly two turns and a matching turnCount, received %d/%d", len(fixture.SessionDetail.Turns), fixture.SessionDetail.TurnCount)
	}
	if fixture.SessionDetail.ToolCallCount != 0 {
		return strikeMountedWebFixture{}, fmt.Errorf("sessionDetail.toolCallCount must be zero for the deterministic fixture")
	}
	startTime, err := time.Parse(time.RFC3339, fixture.SessionDetail.StartTime)
	if err != nil {
		return strikeMountedWebFixture{}, fmt.Errorf("sessionDetail.startTime %q is invalid: %w", fixture.SessionDetail.StartTime, err)
	}
	endTime, err := time.Parse(time.RFC3339, fixture.SessionDetail.EndTime)
	if err != nil {
		return strikeMountedWebFixture{}, fmt.Errorf("sessionDetail.endTime %q is invalid: %w", fixture.SessionDetail.EndTime, err)
	}
	if endTime.Sub(startTime).Minutes() != fixture.SessionDetail.DurationMins || fixture.MapSession.StartTime != fixture.SessionDetail.StartTime || fixture.MapSession.DurationMins != fixture.SessionDetail.DurationMins {
		return strikeMountedWebFixture{}, fmt.Errorf("sessionDetail and mapSession times must describe the same duration")
	}
	foundAssistantContent := false
	for index, turn := range fixture.SessionDetail.Turns {
		role := schema.Role(turn.Role)
		if !role.IsValid() {
			return strikeMountedWebFixture{}, fmt.Errorf("sessionDetail.turns[%d].role %q is invalid", index, turn.Role)
		}
		if _, err := time.Parse(time.RFC3339, turn.Timestamp); err != nil {
			return strikeMountedWebFixture{}, fmt.Errorf("sessionDetail.turns[%d].timestamp %q is invalid: %w", index, turn.Timestamp, err)
		}
		if len(turn.ToolCalls) != 0 {
			return strikeMountedWebFixture{}, fmt.Errorf("sessionDetail.turns[%d].toolCalls must be empty for the deterministic fixture", index)
		}
		if turn.StopReason != "" && !schema.StopReason(turn.StopReason).IsValid() {
			return strikeMountedWebFixture{}, fmt.Errorf("sessionDetail.turns[%d].stopReason %q is invalid", index, turn.StopReason)
		}
		if turn.Role == schema.RoleAssistant.String() && turn.Content == fixture.Expected.AssistantContent {
			foundAssistantContent = true
		}
	}
	if !foundAssistantContent {
		return strikeMountedWebFixture{}, fmt.Errorf("expected.assistantContent must identify the canonical assistant turn")
	}
	if len(fixture.ReviewList.Sessions) != 1 || len(fixture.ReviewList.Changes) != 1 {
		return strikeMountedWebFixture{}, fmt.Errorf("reviewList must contain exactly one session and one change, received %d/%d", len(fixture.ReviewList.Sessions), len(fixture.ReviewList.Changes))
	}
	reviewSession := fixture.ReviewList.Sessions[0]
	if fixture.ReviewList.ProjectHash != fixture.Project.Hash || reviewSession.SessionID != sessionID.String() || reviewSession.Harness != schema.HarnessStrike.String() || reviewSession.Title != fixture.Expected.ReviewSessionTitle || !reviewSession.HasCommitBinding {
		return strikeMountedWebFixture{}, fmt.Errorf("reviewList must preserve the canonical project, bound Strike session, and expected title")
	}
	if fixture.Expected.AssistantContent == "" || fixture.Expected.MapConversationTitle == "" || fixture.Expected.ReviewSessionTitle == "" || fixture.MapCommit.Hash == "" || fixture.MapCommit.Subject == "" || fixture.MapCommit.TimeMS <= 0 {
		return strikeMountedWebFixture{}, fmt.Errorf("expected strings and mapCommit identity must be non-empty")
	}
	return fixture, nil
}

func (fixture strikeMountedWebFixture) session() ingest.Session {
	startTime, _ := time.Parse(time.RFC3339, fixture.SessionDetail.StartTime)
	endTime, _ := time.Parse(time.RFC3339, fixture.SessionDetail.EndTime)
	turns := make([]ingest.Turn, len(fixture.SessionDetail.Turns))
	for index, source := range fixture.SessionDetail.Turns {
		role := schema.Role(source.Role)
		timestamp, _ := time.Parse(time.RFC3339, source.Timestamp)
		var stopReason *schema.StopReason
		if source.StopReason != "" {
			value := schema.StopReason(source.StopReason)
			stopReason = &value
		}
		turns[index] = ingest.Turn{
			Index:      source.Index,
			Role:       role,
			Content:    source.Content,
			Timestamp:  timestamp,
			Depth:      source.Depth,
			EntryType:  schema.EntryTypeText,
			StopReason: stopReason,
		}
	}
	sessionID, _ := schema.NewSessionID(fixture.SessionDetail.ID)
	return ingest.Session{
		ID:        sessionID,
		Project:   fixture.SessionDetail.Project,
		Harness:   schema.HarnessStrike,
		StartTime: startTime,
		EndTime:   endTime,
		Turns:     turns,
		Model:     fixture.SessionDetail.Model,
		Metadata: ingest.SessionMetadata{
			TokensIn:      fixture.SessionDetail.TokensIn,
			TokensOut:     fixture.SessionDetail.TokensOut,
			TotalTokens:   fixture.SessionDetail.TotalTokens,
			Duration:      endTime.Sub(startTime),
			TurnCount:     fixture.SessionDetail.TurnCount,
			ToolCallCount: fixture.SessionDetail.ToolCallCount,
		},
	}
}
