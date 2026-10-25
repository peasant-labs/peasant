package ingest_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

const (
	openCodeForkChild  = "ses_3cd91f52effeXd3QAJ54jOyzn1"
	openCodeForkParent = "ses_fork_parent"
)

// TestOpenCodeNativeForkProducer proves the production snapshot derives the
// fork proof from the native session_v2 pointer instead of a caller-supplied
// proof. The before boundary copies the settled rows before the named message;
// the through boundary copies that message too and records it as completed.
func TestOpenCodeNativeForkProducer(t *testing.T) {
	cases := []struct {
		name        string
		boundary    string
		wantBefore  *int64
		wantThrough *int64
		wantCopies  int
	}{
		{
			name:       "before boundary",
			boundary:   `{"type":"before","messageID":"msg_fork_3"}`,
			wantBefore: int64Ptr(2),
			wantCopies: 2,
		},
		{
			name:        "through boundary",
			boundary:    `{"type":"through","messageID":"msg_fork_2"}`,
			wantThrough: int64Ptr(1),
			wantCopies:  2,
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
			if snapshot.Fork == nil {
				t.Fatal("native fork evidence produced no proof")
			}
			if snapshot.Fork.SourceSessionID != openCodeForkParent {
				t.Fatalf("fork source = %q, want %q", snapshot.Fork.SourceSessionID, openCodeForkParent)
			}
			if got, want := snapshot.Fork.BeforeSeq, testCase.wantBefore; !sameInt64Ptr(got, want) {
				t.Fatalf("fork before seq = %v, want %v", int64PtrString(got), int64PtrString(want))
			}
			if got, want := snapshot.Fork.ThroughSeq, testCase.wantThrough; !sameInt64Ptr(got, want) {
				t.Fatalf("fork through seq = %v, want %v", int64PtrString(got), int64PtrString(want))
			}
			if testCase.wantThrough != nil && !snapshot.Fork.ThroughCompleted {
				t.Fatal("through boundary over settled rows was not marked completed")
			}
			if len(snapshot.Copied) != testCase.wantCopies {
				t.Fatalf("captured copies = %d, want %d", len(snapshot.Copied), testCase.wantCopies)
			}
		})
	}
}

// TestOpenCodeNativeForkProducerMalformedBoundaryIsAbsent proves malformed
// native evidence stays fail-safe: no proof, no guessed ancestry, and the
// session still snapshots.
func TestOpenCodeNativeForkProducerMalformedBoundaryIsAbsent(t *testing.T) {
	source := testfixture.MaterializeByName(t, "native-v2-only")
	writeNativeForkFixture(t, source.Path, `{"type":"before","messageID":`)

	adapter := newTestOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
	snapshot, err := adapter.SnapshotOpenCodeProvenance(context.Background(), source.Path, openCodeForkChild, ingest.OpenCodeSnapshotOptions{})
	if err != nil {
		t.Fatalf("SnapshotOpenCodeProvenance: %v", err)
	}
	if snapshot.Fork != nil {
		t.Fatalf("malformed fork evidence produced a proof: %+v", snapshot.Fork)
	}
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
	for index, messageID := range []string{"msg_fork_1", "msg_fork_2", "msg_fork_3"} {
		execute(
			"INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES (?1, ?2, 'user', ?3, ?3, ?4, ?5)",
			messageID,
			openCodeForkParent,
			int64(10+index),
			`{"text":"parent question","time":{"created":10}}`,
			int64(index),
		)
	}
	execute("UPDATE session_v2 SET fork_session_id = ?1, fork_boundary = ?2 WHERE id = ?3", openCodeForkParent, boundary, openCodeForkChild)
}

func int64Ptr(value int64) *int64 { return &value }

func sameInt64Ptr(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func int64PtrString(value *int64) string {
	if value == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%d", *value)
}
