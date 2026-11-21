package mock_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/mock"
	"github.com/peasant-labs/schema"
)

// TestSegmentReader_ServesEveryMockSessionHeaderTurn reads every mock session
// the transcript routes accept through the production segment routes and
// checks the contract the detail page relies on: the directory's totalTurns
// equals the detail header's turnCount at the same revision, and every segment
// it names is served. The context-navigation sessions the visual scripts open
// must be among them.
func TestSegmentReader_ServesEveryMockSessionHeaderTurn(t *testing.T) {
	ctx := context.Background()
	provider := mock.NewProvider()
	base := serveMockSegments(t, provider)

	sessions, err := provider.Sessions(ctx)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	required := map[string]bool{"sess_contextchild": false, "sess_contextunresolved": false}
	for i := range sessions {
		id := string(sessions[i].ID)
		if _, err := schema.NewSessionID(id); err != nil {
			continue
		}
		if _, ok := required[id]; ok {
			required[id] = true
		}
		t.Run(id, func(t *testing.T) {
			detail, err := provider.DetailReadPayload(ctx, id)
			if err != nil {
				t.Fatalf("DetailReadPayload: %v", err)
			}
			var directory schema.TranscriptSegmentDirectory
			getJSON(t, fmt.Sprintf("%s/api/v1/sessions/%s/transcript/segments?revision=%s", base, id, detail.Revision), &directory)
			if directory.TotalTurns != detail.TurnCount {
				t.Fatalf("directory totalTurns = %d, detail header turnCount = %d", directory.TotalTurns, detail.TurnCount)
			}
			if detail.TurnCount > 0 && directory.TotalSegments == 0 {
				t.Fatalf("the header counts %d turns but the directory names no segment", detail.TurnCount)
			}
			served := 0
			for _, ref := range directory.Segments {
				var segment schema.TranscriptSegment
				getJSON(t, fmt.Sprintf("%s/api/v1/sessions/%s/transcript/segments/%d?revision=%s", base, id, ref.Index, directory.Revision), &segment)
				served += len(segment.Turns)
			}
			if served != detail.TurnCount {
				t.Fatalf("segment reads served %d turns, detail header turnCount = %d", served, detail.TurnCount)
			}
		})
	}
	for id, seen := range required {
		if !seen {
			t.Errorf("mock session %s is not served with a valid session identifier", id)
		}
	}
}

func serveMockSegments(t *testing.T, provider *mock.Provider) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	srv := api.NewServer(api.ServerConfig{Port: 0, Provider: provider, SegmentReader: mock.NewSegmentReader(provider)})
	if err := srv.Listen(ctx); err != nil {
		cancel()
		t.Fatalf("Listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && err != context.Canceled {
			t.Errorf("segment server shutdown: %v", err)
		}
	})
	return "http://" + srv.Addr().String()
}

func getJSON(t *testing.T, url string, into any) {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, body %s", url, response.StatusCode, body)
	}
	if err := json.Unmarshal(body, into); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}
