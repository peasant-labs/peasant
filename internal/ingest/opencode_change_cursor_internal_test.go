package ingest

import (
	_ "embed"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/opencode_change_cursor.yaml
var openCodeChangeCursorYAML []byte

type openCodeChangeCursorFixture struct {
	DeclaredCases int                        `yaml:"declared_cases"`
	Cases         []openCodeChangeCursorCase `yaml:"cases"`
}

type openCodeChangeCursorCase struct {
	Name       string `yaml:"name"`
	IngestedMs int64  `yaml:"ingested_ms"`
	ModTimeMs  int64  `yaml:"mod_time_ms"`
	Tracked    bool   `yaml:"tracked"`
	StoredSeq  int64  `yaml:"stored_seq"`
	EventSeq   int64  `yaml:"event_seq"`
	Expect     string `yaml:"expect"`
}

// TestOpenCodeChangeCursorTriggersReingest proves the change cursor is an
// additional trigger over the session clock: a session the clock reports
// unchanged is re-ingested when its newest event sequence moved past the stored
// cursor, while a first sighting without a stored cursor stays unchanged and the
// clock remains the primary signal.
func TestOpenCodeChangeCursorTriggersReingest(t *testing.T) {
	var fixture openCodeChangeCursorFixture
	if err := yaml.Unmarshal(openCodeChangeCursorYAML, &fixture); err != nil {
		t.Fatalf("decode change-cursor fixture: %v", err)
	}
	if fixture.DeclaredCases != len(fixture.Cases) || fixture.DeclaredCases != 4 {
		t.Fatalf("change-cursor fixture row guard: declared=%d actual=%d required=4", fixture.DeclaredCases, len(fixture.Cases))
	}
	sessionID := SessionID("ses_3cd91f52effeXd3QAJ54jOyzB1")
	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			ingested := testCase.IngestedMs
			pipeline := &Pipeline{
				config: PipelineConfig{StalenessThreshold: 0},
				locationCache: map[SessionID]SessionLocation{
					sessionID: {IngestedMs: &ingested, SchemaVersion: CurrentSchemaVersion},
				},
			}
			if testCase.Tracked {
				pipeline.seqCursorCache = map[SessionID]int64{sessionID: testCase.StoredSeq}
			}
			session := DiscoveredSession{
				SessionID: sessionID,
				Harness:   HarnessOpenCode,
				ModTime:   time.UnixMilli(testCase.ModTimeMs),
				EventSeq:  testCase.EventSeq,
			}
			got := pipeline.classifySession(session)
			if got.String() != testCase.Expect {
				t.Fatalf("classify %q = %q, want %q", testCase.Name, got.String(), testCase.Expect)
			}
		})
	}
}
