package ingest_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

const (
	openCodeForkChild  = "ses_3cd91f52effeXd3QAJ54jOyzn1"
	openCodeForkParent = "ses_f8c3ed088ffekU7T4UA5wVXwYk"
)

// TestOpenCodeNativeForkContextRelationship proves the production snapshot
// turns the native session_v2 pointer into a context_from relationship. The
// child already holds the parent prefix, so the snapshot must not copy rows.
func TestOpenCodeNativeForkContextRelationship(t *testing.T) {
	cases := []struct {
		name          string
		boundary      string
		boundaryType  ingest.OpenCodeForkBoundaryType
		boundaryMsgID string
	}{
		{
			name:          "before boundary",
			boundary:      `{"type":"before","messageID":"msg_fork_3"}`,
			boundaryType:  ingest.OpenCodeForkBoundaryBefore,
			boundaryMsgID: "msg_fork_3",
		},
		{
			name:          "through boundary",
			boundary:      `{"type":"through","messageID":"msg_fork_2"}`,
			boundaryType:  ingest.OpenCodeForkBoundaryThrough,
			boundaryMsgID: "msg_fork_2",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			source := testfixture.MaterializeByName(t, "native-v2-only")
			writeNativeForkFixture(t, source.Path, testCase.boundary)

			adapter := newTestOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
			snapshot, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, openCodeForkChild, ingest.OpenCodeSnapshotOptions{})
			if err != nil {
				t.Fatalf("SnapshotOpenCodeProvenance: %v", err)
			}
			if snapshot.Fork != nil {
				t.Fatal("native pointer must not produce a copy proof")
			}
			if len(snapshot.Copied) != 0 {
				t.Fatalf("native fork copied %d parent rows; OpenCode already materializes the prefix", len(snapshot.Copied))
			}
			if got := len(snapshot.Messages); got != 5 {
				t.Fatalf("child messages = %d, want the materialized 5 rows (3 prefix + 2 own)", got)
			}
			if snapshot.NativeFork == nil {
				t.Fatal("native fork pointer was not captured")
			}
			if snapshot.NativeFork.SourceSessionID.String() != openCodeForkParent {
				t.Fatalf("native fork source = %q, want %q", snapshot.NativeFork.SourceSessionID.String(), openCodeForkParent)
			}
			if snapshot.NativeFork.BoundaryType != testCase.boundaryType {
				t.Fatalf("native fork boundary type = %q, want %q", snapshot.NativeFork.BoundaryType, testCase.boundaryType)
			}
			if snapshot.NativeFork.BoundaryMessageID != testCase.boundaryMsgID {
				t.Fatalf("native fork boundary message = %q, want %q", snapshot.NativeFork.BoundaryMessageID, testCase.boundaryMsgID)
			}

			relationship := contextFromRelationship(t, buildOpenCodeForkCapture(t, snapshot))
			if relationship == nil {
				t.Fatal("native fork produced no context_from relationship")
			}
			if relationship.TargetLocalID == nil || *relationship.TargetLocalID != schema.SessionID(openCodeForkParent) {
				t.Fatalf("context_from target = %v, want %q", relationship.TargetLocalID, openCodeForkParent)
			}
			if relationship.Evidence != schema.EvidenceNativeTyped {
				t.Fatalf("context_from evidence = %q, want %q", relationship.Evidence, schema.EvidenceNativeTyped)
			}
			if relationship.Anchor == nil || relationship.Anchor.Kind != schema.PublicSourceAnchorGeneral {
				t.Fatalf("context_from anchor = %+v, want general_source_session", relationship.Anchor)
			}
		})
	}
}

// TestOpenCodeNativeForkMalformedBoundaryIsAbsent proves malformed native
// evidence stays fail-safe: no relationship, no copy, and the session still
// snapshots.
func TestOpenCodeNativeForkMalformedBoundaryIsAbsent(t *testing.T) {
	source := testfixture.MaterializeByName(t, "native-v2-only")
	writeNativeForkFixture(t, source.Path, `{"type":"before","messageID":`)

	adapter := newTestOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	snapshot, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, openCodeForkChild, ingest.OpenCodeSnapshotOptions{})
	if err != nil {
		t.Fatalf("SnapshotOpenCodeProvenance: %v", err)
	}
	if snapshot.NativeFork != nil || snapshot.Fork != nil {
		t.Fatalf("malformed fork evidence produced provenance: native=%+v proof=%+v", snapshot.NativeFork, snapshot.Fork)
	}
	if len(snapshot.Copied) != 0 {
		t.Fatalf("malformed fork evidence copied %d rows", len(snapshot.Copied))
	}
	if relationship := contextFromRelationship(t, buildOpenCodeForkCapture(t, snapshot)); relationship != nil {
		t.Fatalf("malformed fork evidence produced a relationship: %+v", relationship)
	}
}

// TestOpenCodeNativeForkAbsentOnLegacyLayout proves a store without session_v2
// fork columns produces no relationship and no error.
func TestOpenCodeNativeForkAbsentOnLegacyLayout(t *testing.T) {
	source := testfixture.MaterializeByName(t, "native-current-rows")

	adapter := newTestOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	snapshot, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, openCodeNativeChild, ingest.OpenCodeSnapshotOptions{})
	if err != nil {
		t.Fatalf("SnapshotOpenCodeProvenance: %v", err)
	}
	if snapshot.NativeFork != nil || snapshot.Fork != nil {
		t.Fatalf("legacy layout produced provenance: native=%+v proof=%+v", snapshot.NativeFork, snapshot.Fork)
	}
}

func buildOpenCodeForkCapture(t *testing.T, snapshot ingest.OpenCodeHistorySnapshot) ingest.ClassifiedCapture {
	t.Helper()
	metadata := schema.UnifiedMetadata{
		SchemaVersion: 1,
		SessionID:     ingest.SessionID(openCodeForkChild),
		ModelHarness:  schema.HarnessOpenCode,
	}
	capture, err := ingest.BuildOpenCodeProvenanceCapture(
		snapshot,
		"gen-native-fork",
		metadata,
		ingest.OpenCodeProvenancePrior{Aliases: ingest.NewProjectionPriorState()},
	)
	if err != nil {
		t.Fatalf("BuildOpenCodeProvenanceCapture: %v", err)
	}
	return capture
}

func contextFromRelationship(t *testing.T, capture ingest.ClassifiedCapture) *schema.SessionRelationship {
	t.Helper()
	for index := range capture.Metadata.Relationships {
		if capture.Metadata.Relationships[index].Kind == schema.SessionRelationshipContextFrom {
			return &capture.Metadata.Relationships[index]
		}
	}
	return nil
}

func writeNativeForkFixture(t *testing.T, path, boundary string) {
	t.Helper()
	connection, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open synthetic native source: %v", err)
	}
	defer func() { _ = connection.Close() }()

	execute := func(query string, args ...any) {
		t.Helper()
		if err := sqlitex.Execute(connection, query, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatalf("write synthetic native fork fixture: %v", err)
		}
	}

	execute("INSERT INTO session_v2 (id, time_created, time_updated) VALUES (?1, ?2, ?3)", openCodeForkParent, int64(1), int64(1))
	parentRows := []string{"msg_fork_1", "msg_fork_2", "msg_fork_3"}
	for index, messageID := range parentRows {
		execute(
			"INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES (?1, ?2, 'user', ?3, ?3, ?4, ?5)",
			messageID,
			openCodeForkParent,
			int64(10+index),
			`{"text":"parent question","time":{"created":10}}`,
			int64(index),
		)
	}

	// OpenCode materializes the parent prefix in the child before the child
	// continues. Model that real shape so a copy would show up as extra rows.
	execute("DELETE FROM session_message WHERE session_id = ?1", openCodeForkChild)
	for index, messageID := range parentRows {
		execute(
			"INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES (?1, ?2, 'user', ?3, ?3, ?4, ?5)",
			"msg_child_prefix_"+messageID,
			openCodeForkChild,
			int64(10+index),
			`{"text":"parent question","time":{"created":10}}`,
			int64(index),
		)
	}
	for index := 0; index < 2; index++ {
		execute(
			"INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES (?1, ?2, 'user', ?3, ?3, ?4, ?5)",
			"msg_child_own_"+strconv.Itoa(index),
			openCodeForkChild,
			int64(30+index),
			`{"text":"child question","time":{"created":30}}`,
			int64(3+index),
		)
	}
	execute("UPDATE session_v2 SET fork_session_id = ?1, fork_boundary = ?2 WHERE id = ?3", openCodeForkParent, boundary, openCodeForkChild)
}
