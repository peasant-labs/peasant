package mock

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
	"github.com/peasant-labs/schema/route"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/transcript_segments.yaml
var transcriptSegmentsFixtureYAML []byte

const transcriptSegmentsFixturePath = "testdata/transcript_segments.yaml"

type transcriptSegmentsCase struct {
	Name               string `yaml:"name"`
	SessionID          string `yaml:"sessionId"`
	ConversationTurns  int    `yaml:"conversationTurns"`
	ToolCalls          int    `yaml:"toolCalls"`
	SameTurnIdentities bool   `yaml:"sameTurnIdentities"`
	ContextNavigation  string `yaml:"contextNavigation"`
	HTTPReadable       bool   `yaml:"httpReadable"`
}

type transcriptSegmentsFixture struct {
	Required []string                 `yaml:"required"`
	Cases    []transcriptSegmentsCase `yaml:"cases"`
}

func loadTranscriptSegmentsFixture(t *testing.T) transcriptSegmentsFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(transcriptSegmentsFixtureYAML))
	decoder.KnownFields(true)
	var fixture transcriptSegmentsFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode %s: %v", transcriptSegmentsFixturePath, err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	required := slices.Clone(fixture.Required)
	slices.Sort(required)
	slices.Sort(names)
	if !slices.Equal(required, names) {
		t.Fatalf("transcript_segments.yaml cases %v must be exactly the required names %v; restore the missing case or list the new one", names, required)
	}
	return fixture
}

// startMockSessionsServer starts the production server the way `peasant web
// start --mock-data-store=...` does: a progressive provider over the mock store
// and an empty analytics store, both handed to the server.
func startMockSessionsServer(t *testing.T, mocked []defaults.MockSection) string {
	t.Helper()
	db := storetest.Open(t)
	cfg := config.BaseConfig()
	cfg.Sources.Mock = config.MockConfig{Enabled: true, Web: mocked}
	provider := api.NewProgressiveProvider(cfg, defaults.MockComponents.Web, NewProvider(), api.NewStoreDataProvider(db, sessionvisibility.Browsing()))
	ctx, cancel := context.WithCancel(context.Background())
	srv := api.NewServer(api.ServerConfig{Port: 0, Provider: provider, Store: db, Config: cfg})
	if err := srv.Listen(ctx); err != nil {
		cancel()
		t.Fatalf("listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && err != context.Canceled {
			t.Errorf("serve: %v", err)
		}
	})
	return "http://" + srv.Addr().String()
}

func getMockJSON(t *testing.T, url string, target any) {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s answered %d: %s", url, response.StatusCode, body)
	}
	if err := json.Unmarshal(body, target); err != nil {
		t.Fatalf("decode %s: %v: %s", url, err, body)
	}
}

// servedTurn is what a reader sees of one turn, from either the production
// fold or the segment route.
type servedTurn struct {
	Index     int
	Role      schema.Role
	Content   string
	ToolCalls []servedToolCall
}

type servedToolCall struct {
	Name, Arguments, Result string
}

func servedFromFold(turns []ingest.Turn) []servedTurn {
	served := make([]servedTurn, len(turns))
	for i, turn := range turns {
		served[i] = servedTurn{Index: turn.Index, Role: turn.Role, Content: turn.Content}
		for _, call := range turn.ToolCalls {
			served[i].ToolCalls = append(served[i].ToolCalls, servedToolCall{call.Name, call.Arguments, call.Result})
		}
	}
	return served
}

func servedFromSegments(turns []schema.TranscriptTurn) []servedTurn {
	served := make([]servedTurn, len(turns))
	for i, turn := range turns {
		served[i] = servedTurn{Index: turn.Index, Role: turn.Role, Content: turn.Content}
		for _, call := range turn.ToolCalls {
			served[i].ToolCalls = append(served[i].ToolCalls, servedToolCall{call.Name, call.Arguments, call.Result})
		}
	}
	return served
}

// assertServesMockTurns checks that served is the mock session's turns, in
// order, with their tool calls, and that a context navigation case opens with
// its fixture's first turn.
func assertServesMockTurns(t *testing.T, c transcriptSegmentsCase, session *ingest.Session, served []servedTurn) {
	t.Helper()
	if len(served) != len(session.Turns) {
		t.Fatalf("served %d turns, want the mock session's %d", len(served), len(session.Turns))
	}
	toolCalls := 0
	for i, want := range session.Turns {
		got := served[i]
		if got.Role != want.Role || got.Content != want.Content {
			t.Errorf("served turn %d = %s %q, want mock turn %s %q", i, got.Role, got.Content, want.Role, want.Content)
		}
		if c.SameTurnIdentities && got.Index != want.Index {
			t.Errorf("served turn %d identity = %d, want the mock turn index %d", i, got.Index, want.Index)
		}
		wantCalls := make([]servedToolCall, 0, len(want.ToolCalls))
		for _, call := range want.ToolCalls {
			wantCalls = append(wantCalls, servedToolCall{call.Name, call.Arguments, call.Result})
		}
		if !slices.Equal(got.ToolCalls, wantCalls) {
			t.Errorf("served turn %d tool calls = %+v, want %+v", i, got.ToolCalls, wantCalls)
		}
		toolCalls += len(got.ToolCalls)
	}
	if toolCalls != c.ToolCalls {
		t.Errorf("served turns carry %d tool calls, want %d", toolCalls, c.ToolCalls)
	}
	if c.ContextNavigation == "" {
		return
	}
	source, ok := map[string]contextNavigationSessionFixture{
		"child":      canonicalContextNavigationFixture.Child,
		"source":     canonicalContextNavigationFixture.Source,
		"parent":     canonicalContextNavigationFixture.Parent,
		"unresolved": canonicalContextNavigationFixture.Unresolved,
	}[c.ContextNavigation]
	if !ok {
		t.Fatalf("contextNavigation %q names no context_navigation.yaml session; use child, source, parent or unresolved", c.ContextNavigation)
	}
	if source.ID != c.SessionID {
		t.Fatalf("contextNavigation %q is session %s, but the case reads %s", c.ContextNavigation, source.ID, c.SessionID)
	}
	if served[0].Content != source.Turns[0].Content {
		t.Errorf("transcript opens with %q, want the context_navigation.yaml opening %q", served[0].Content, source.Turns[0].Content)
	}
}

// TestMockStoreEntriesFoldIntoTheMockTurns checks the mock store's stored
// entries against the production fold: every fixture session folds back into
// its own mock turns, tool calls included, with its conversation turn count.
func TestMockStoreEntriesFoldIntoTheMockTurns(t *testing.T) {
	fixture := loadTranscriptSegmentsFixture(t)
	provider := NewProvider()
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			session, err := provider.SessionByID(context.Background(), c.SessionID)
			if err != nil {
				t.Fatalf("fixture session %s is not a mock session: %v", c.SessionID, err)
			}
			entries, err := provider.LoadAvailableSessionEntries(context.Background(), ingest.SessionID(c.SessionID))
			if err != nil {
				t.Fatalf("load the mock store entries of %s: %v", c.SessionID, err)
			}
			folded := ingest.EntriesToTurns(entries)
			if got := ingest.ConversationTurnCount(folded); got != c.ConversationTurns {
				t.Errorf("folded conversation turns = %d, want %d", got, c.ConversationTurns)
			}
			assertServesMockTurns(t, c, session, servedFromFold(folded))
		})
	}
}

// TestMockSessionsServeTranscriptSegments is the check that the session page
// opens a mock session's transcript: with the sessions mock store on, the
// segment directory of every readable fixture session is non-empty where its
// detail header counts turns, and the segments serve the mock turns in order,
// with their conversation turn positions.
func TestMockSessionsServeTranscriptSegments(t *testing.T) {
	fixture := loadTranscriptSegmentsFixture(t)
	base := startMockSessionsServer(t, []defaults.MockSection{defaults.MockSections.Sessions})
	provider := NewProvider()

	for _, c := range fixture.Cases {
		if !c.HTTPReadable {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			session, err := provider.SessionByID(context.Background(), c.SessionID)
			if err != nil {
				t.Fatalf("fixture session %s is not a mock session: %v", c.SessionID, err)
			}
			sessionPath, err := route.LocalGetSessionPath(c.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			sessionURL := base + sessionPath

			var header schema.SessionDetailReadPayload
			getMockJSON(t, sessionURL, &header)
			if header.TurnCount == 0 {
				t.Fatalf("detail header of %s counts no turns; the case proves nothing", c.SessionID)
			}

			var directory schema.TranscriptSegmentDirectory
			directoryPath, err := route.LocalGetSessionTranscriptSegmentsPath(c.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			getMockJSON(t, base+directoryPath, &directory)
			if directory.TotalTurns != header.TurnCount || directory.TotalSegments == 0 || len(directory.Segments) != directory.TotalSegments {
				t.Fatalf("segment directory of %s holds %d turns in %d segments (%d refs) while its detail header counts %d turns; the session page would show the transcript as not indexed", c.SessionID, directory.TotalTurns, directory.TotalSegments, len(directory.Segments), header.TurnCount)
			}
			if directory.TotalConversationTurns == nil || *directory.TotalConversationTurns != c.ConversationTurns {
				t.Errorf("totalConversationTurns = %v, want %d", directory.TotalConversationTurns, c.ConversationTurns)
			}

			var served []schema.TranscriptTurn
			before := 0
			for _, ref := range directory.Segments {
				if ref.ConversationTurns == nil {
					t.Fatalf("segment %d carries no conversation turn position", ref.Index)
				}
				if ref.ConversationTurns.Before != before {
					t.Errorf("segment %d conversationTurns.before = %d, want %d", ref.Index, ref.ConversationTurns.Before, before)
				}
				before += ref.ConversationTurns.Count
				var segment schema.TranscriptSegment
				segmentPath, err := route.LocalGetSessionTranscriptSegmentPath(c.SessionID, ref.Index)
				if err != nil {
					t.Fatal(err)
				}
				getMockJSON(t, base+segmentPath, &segment)
				if segment.Revision != directory.Revision {
					t.Errorf("segment %d revision = %q, want the directory's %q", ref.Index, segment.Revision, directory.Revision)
				}
				served = append(served, segment.Turns...)
			}
			if before != c.ConversationTurns {
				t.Errorf("segment conversation turn counts sum to %d, want %d", before, c.ConversationTurns)
			}
			assertServesMockTurns(t, c, session, servedFromSegments(served))
		})
	}
}

// TestUnmockedSessionsReadTheAnalyticsStore pins the other side of the
// routing: when the sessions section is not mocked, the segment reads keep
// reading the analytics store, which does not hold the mock sessions.
func TestUnmockedSessionsReadTheAnalyticsStore(t *testing.T) {
	base := startMockSessionsServer(t, []defaults.MockSection{defaults.MockSections.Dashboard})
	var directory schema.TranscriptSegmentDirectory
	directoryPath, err := route.LocalGetSessionTranscriptSegmentsPath(canonicalContextNavigationFixture.Child.ID)
	if err != nil {
		t.Fatal(err)
	}
	getMockJSON(t, base+directoryPath, &directory)
	if directory.TotalTurns != 0 || directory.TotalSegments != 0 {
		t.Fatalf("with only the dashboard mocked the segment directory of mock session %s holds %d turns in %d segments; the transcript reads must stay on the analytics store", canonicalContextNavigationFixture.Child.ID, directory.TotalTurns, directory.TotalSegments)
	}
}
