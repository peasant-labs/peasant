package api

import (
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/sync_redaction_categories.yaml
var syncRedactionCategoriesYAML []byte

// TestSyncRedactionHTTPLabels exercises the real HTTP producer's classification,
// grouping, location and serialization. Only detection is injected: project
// fixtures must not activate a rule the standard policy does not offer.
func TestSyncRedactionHTTPLabels(t *testing.T) {
	t.Parallel()
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name       string            `yaml:"name"`
			Categories []redact.Category `yaml:"categories"`
			Labels     []string          `yaml:"labels"`
			Status     int               `yaml:"status"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(syncRedactionCategoriesYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			world := newPublishingWorld(t)
			const id = "44445555-6666-4777-8888-9999aaaabbbb"
			seedToolCallSession(t, world.db, id)
			handler := newTestXDGHomes(t).handler(world.db, config.BaseConfig())
			handler.detectReview = func(content string) []redact.Match {
				var matches []redact.Match
				for _, category := range fixture.Categories {
					matches = append(matches, redact.Match{Category: category, Rule: category.String().String(), MatchedText: entryIndexTurnSecret, Offset: strings.LastIndex(content, entryIndexTurnSecret)})
				}
				return matches
			}
			mux := http.NewServeMux()
			mux.HandleFunc("GET "+defaults.RouteSyncRedactions.String(), handler.handleSyncRedactions)
			server := httptest.NewServer(mux)
			defer server.Close()
			response, err := server.Client().Get(server.URL + defaults.RouteSyncRedactions.String() + "?session_id=" + id + "&level=standard")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != fixture.Status {
				t.Fatalf("status=%d want=%d body=%s", response.StatusCode, fixture.Status, body)
			}
			if fixture.Status != http.StatusOK {
				return
			}
			var scan schema.SyncRedactionsResponse
			if err := json.Unmarshal(body, &scan); err != nil {
				t.Fatal(err)
			}
			var labels []string
			for _, group := range scan.Categories {
				labels = append(labels, group.Category)
				for _, rule := range group.Rules {
					for _, item := range rule.Items {
						if item.Category != group.Category || item.EntryIndex == nil || item.OriginalText != entryIndexTurnSecret {
							t.Fatalf("invalid grouped finding: %+v", item)
						}
					}
				}
			}
			if !reflect.DeepEqual(labels, fixture.Labels) {
				t.Fatalf("labels=%v want=%v", labels, fixture.Labels)
			}
		})
	}
}
