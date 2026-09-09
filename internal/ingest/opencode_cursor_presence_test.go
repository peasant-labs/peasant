package ingest

import (
	"context"
	_ "embed"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/opencode_cursor_presence.yaml
var openCodeCursorPresenceYAML []byte

type cursorPresenceSource struct {
	semanticNegativeSource
	seq OpenCodeSessionSeq
}

func (source cursorPresenceSource) MaxEventSeq(context.Context, OpenCodeSessionLinkID) (OpenCodeSessionSeq, error) {
	return source.seq, nil
}

func TestCapturedOpenCodeSessionReportsCursorPresence(t *testing.T) {
	var fixtures struct {
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name     string `yaml:"name"`
			Present  bool   `yaml:"present"`
			Seq      int64  `yaml:"seq"`
			Observed bool   `yaml:"observed"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(openCodeCursorPresenceYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate cursor presence fixture %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	for _, name := range fixtures.Required {
		if !names[name] {
			t.Fatalf("missing cursor presence fixture %s", name)
		}
	}
	sid, err := NewSessionID("ses_3cd91f52effeXd3QAJ54jOyzv5")
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			source := cursorPresenceSource{seq: OpenCodeSessionSeq{Present: fixture.Present, Seq: fixture.Seq}}
			session, observed, err := capturedOpenCodeSession(context.Background(), source, DiscoveredSession{SessionID: sid, Harness: HarnessOpenCode, EventSeq: 7})
			if err != nil {
				t.Fatal(err)
			}
			if observed != fixture.Observed {
				t.Fatalf("observed = %t, want %t", observed, fixture.Observed)
			}
			if observed && session.EventSeq != fixture.Seq {
				t.Fatalf("observed cursor = %d, want %d", session.EventSeq, fixture.Seq)
			}
			// The materialized transcript carries the same presence, so the
			// publisher can tell an acquired zero from an unknown cursor.
			captured, err := newSQLiteMaterializedTranscript(&UnifiedMetadata{}, nil, session, observed)
			if err != nil {
				t.Fatal(err)
			}
			if captured.EventSeqObserved != fixture.Observed {
				t.Fatalf("materialized presence = %t, want %t", captured.EventSeqObserved, fixture.Observed)
			}
		})
	}
}
