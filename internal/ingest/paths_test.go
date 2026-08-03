package ingest

import "testing"

func TestSessionDir(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		base     string
		hostSlug string
		id       string
		parentID string
		want     string
	}{
		{
			name:     "root session",
			base:     "/sync",
			hostSlug: "host-a",
			id:       "sess-1",
			parentID: "",
			want:     "/sync/host-a/sess-1",
		},
		{
			name:     "subagent session",
			base:     "/sync",
			hostSlug: "host-a",
			id:       "agent-1",
			parentID: "sess-1",
			want:     "/sync/host-a/sess-1/subagents/agent-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SessionDir(tc.base, tc.hostSlug, tc.id, tc.parentID)
			if got != tc.want {
				t.Errorf("SessionDir(%q,%q,%q,%q) = %q, want %q",
					tc.base, tc.hostSlug, tc.id, tc.parentID, got, tc.want)
			}
		})
	}
}

func TestSessionMetadataPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		base     string
		hostSlug string
		id       string
		parentID string
		want     string
	}{
		{
			name:     "root session metadata",
			base:     "/sync",
			hostSlug: "host-a",
			id:       "sess-1",
			parentID: "",
			want:     "/sync/host-a/sess-1/sess-1--metadata.json",
		},
		{
			name:     "subagent session metadata",
			base:     "/sync",
			hostSlug: "host-a",
			id:       "agent-1",
			parentID: "sess-1",
			want:     "/sync/host-a/sess-1/subagents/agent-1/agent-1--metadata.json",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SessionMetadataPath(tc.base, tc.hostSlug, tc.id, tc.parentID)
			if got != tc.want {
				t.Errorf("SessionMetadataPath(%q,%q,%q,%q) = %q, want %q",
					tc.base, tc.hostSlug, tc.id, tc.parentID, got, tc.want)
			}
		})
	}
}
