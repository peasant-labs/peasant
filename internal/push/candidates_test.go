package push_test

import (
	"context"
	"sort"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// ids extracts and sorts session IDs for set comparison.
func ids(rows []ingest.PushSessionRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.SessionID)
	}
	sort.Strings(out)
	return out
}

// TestQueryPushCandidates_Modes asserts the single shared base-query helper
// resolves each query mode against the store the same way the old per-path
// switches did: default, force, source-provider, and by-source.
func TestQueryPushCandidates_Modes(t *testing.T) {
	t.Parallel()

	claudeUnpushed := ingest.PushSessionRow{SessionID: "claude-unpushed", ModelHarness: string(defaults.HarnessClaudeCode)}
	openUnpushed := ingest.PushSessionRow{SessionID: "open-unpushed", ModelHarness: string(defaults.HarnessOpenCode)}
	claudeAll := ingest.PushSessionRow{SessionID: "claude-all", ModelHarness: string(defaults.HarnessClaudeCode)}

	store := &testutil.StubPushStore{
		Sessions:    []ingest.PushSessionRow{claudeUnpushed, openUnpushed},
		AllSessions: []ingest.PushSessionRow{claudeUnpushed, openUnpushed, claudeAll},
	}

	cases := []struct {
		name  string
		query push.PushCandidateQuery
		want  []string
	}{
		{
			name:  "default unpushed",
			query: push.PushCandidateQuery{Method: config.PushMethodAll},
			want:  []string{"claude-unpushed", "open-unpushed"},
		},
		{
			name:  "force all",
			query: push.PushCandidateQuery{Force: true},
			want:  []string{"claude-all", "claude-unpushed", "open-unpushed"},
		},
		{
			name:  "force with source-provider filter",
			query: push.PushCandidateQuery{Force: true, SourceProvider: string(defaults.HarnessClaudeCode)},
			want:  []string{"claude-all", "claude-unpushed"},
		},
		{
			name:  "source-provider",
			query: push.PushCandidateQuery{SourceProvider: string(defaults.HarnessClaudeCode)},
			want:  []string{"claude-unpushed"},
		},
		{
			name:  "by-source",
			query: push.PushCandidateQuery{Method: config.PushMethodBySource, Sources: []string{string(defaults.HarnessClaudeCode)}},
			want:  []string{"claude-unpushed"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := push.QueryPushCandidates(context.Background(), store, tc.query)
			if err != nil {
				t.Fatalf("QueryPushCandidates: %v", err)
			}
			gotIDs := ids(got)
			if len(gotIDs) != len(tc.want) {
				t.Fatalf("want %v, got %v", tc.want, gotIDs)
			}
			for i := range tc.want {
				if gotIDs[i] != tc.want[i] {
					t.Fatalf("want %v, got %v", tc.want, gotIDs)
				}
			}
		})
	}
}
