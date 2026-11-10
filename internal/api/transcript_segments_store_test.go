package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

type storeTranscriptTurn struct {
	index        int
	role         schema.Role
	contentBytes int
	toolResults  []int
}

func storeTranscriptEntries(t *testing.T, sessionID string, turns []storeTranscriptTurn) []schema.SessionEntry {
	t.Helper()
	entries := make([]schema.SessionEntry, 0, len(turns)*2)
	for _, turn := range turns {
		content := strings.Repeat("c", turn.contentBytes)
		index := turn.index
		timestamp := int64(1_700_000_000_000 + index)
		entries = append(entries, schema.SessionEntry{
			SessionID:      ingest.SessionID(sessionID),
			EntryIndex:     index,
			Harness:        schema.HarnessClaudeCode,
			EntryType:      schema.EntryTypeText,
			Role:           turn.role,
			TimestampMs:    &timestamp,
			ContentPreview: &content,
			Depth:          0,
		})
		for call, resultBytes := range turn.toolResults {
			id := fmt.Sprintf("tool-%d-%d", index, call)
			name := "Bash"
			input := `{"command":"echo"}`
			output := strings.Repeat("r", resultBytes)
			child := index + 1 + call
			entries = append(entries, schema.SessionEntry{
				SessionID:    ingest.SessionID(sessionID),
				EntryIndex:   child,
				Harness:      schema.HarnessClaudeCode,
				EntryType:    schema.EntryTypeToolUse,
				Role:         turn.role,
				TimestampMs:  &timestamp,
				ToolCallID:   &id,
				ToolNamesCSV: &name,
				ToolInput:    &input,
				ToolOutput:   &output,
				Depth:        1,
				ParentIndex:  &index,
			})
		}
	}
	return entries
}

func seedStoreTranscript(t *testing.T, sstore *store.Store, sessionID string, turnCount int, entries []schema.SessionEntry) {
	t.Helper()
	entry := makeStoreEntry(t, sessionID, hash1, "github.com-test",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-segments", turnCount, 0, 60000)
	if err := sstore.InsertSessions(context.Background(), []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}
	if err := testutil.WriteFullEntries(context.Background(), sstore, ingest.SessionID(sessionID), entries); err != nil {
		t.Fatalf("WriteFullEntries: %v", err)
	}
}

func TestTranscriptDirectoryByteLengthMatchesServedSegment(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	sessionID := "88888888-8888-4888-8888-888888888888"
	turns := make([]storeTranscriptTurn, 8)
	for i := range turns {
		role := schema.RoleAssistant
		if i%2 == 0 {
			role = schema.RoleUser
		}
		turns[i] = storeTranscriptTurn{index: i * 8, role: role, contentBytes: 300 << 10}
	}
	turns[3].toolResults = []int{100 << 10, 100 << 10}
	seedStoreTranscript(t, s, sessionID, len(turns), storeTranscriptEntries(t, sessionID, turns))

	ctx, cancel := context.WithCancel(context.Background())
	srv := api.NewServer(api.ServerConfig{Port: 0, Store: s})
	if err := srv.Listen(ctx); err != nil {
		cancel()
		t.Fatalf("Listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	base := "http://" + srv.Addr().String()

	response, err := http.Get(base + "/api/v1/sessions/" + sessionID + "/transcript/segments")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("directory status = %d, body %s", response.StatusCode, body)
	}
	var directory schema.TranscriptSegmentDirectory
	if err := json.Unmarshal(body, &directory); err != nil {
		t.Fatalf("decode directory: %v", err)
	}
	if directory.TotalTurns != len(turns) {
		t.Fatalf("totalTurns = %d, want %d", directory.TotalTurns, len(turns))
	}
	if len(directory.Segments) < 2 {
		t.Fatalf("directory cut to %d segment(s), want more than one so a later segment is exercised", len(directory.Segments))
	}
	for _, ref := range directory.Segments {
		if ref.ByteLength > defaults.TranscriptSegmentBudgetBytes {
			t.Fatalf("segment %d is %d bytes, over the %d per-segment budget", ref.Index, ref.ByteLength, defaults.TranscriptSegmentBudgetBytes)
		}
	}

	response, err = http.Get(fmt.Sprintf("%s/api/v1/sessions/%s/transcript/segments/%d", base, sessionID, directory.Segments[0].Index))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("segment 0 status = %d, want 200 (a healthy session must not answer a short-segment 409); body %s", response.StatusCode, body)
	}
	var segment schema.TranscriptSegment
	if err := json.Unmarshal(body, &segment); err != nil {
		t.Fatalf("decode segment: %v", err)
	}
	if segment.ByteLength != directory.Segments[0].ByteLength {
		t.Fatalf("served segment byteLength = %d, directory declared %d; the directory and the read must measure the same content", segment.ByteLength, directory.Segments[0].ByteLength)
	}
	if len(segment.Turns) == 0 {
		t.Fatal("served segment carries no turns")
	}
}

// TestTranscriptDirectoryColdBuildThroughStore measures a cold directory build
// over the real store pass on a synthetic 32 MiB transcript (1,312 turns, no
// real text, session id or personal path).
func TestTranscriptDirectoryColdBuildThroughStore(t *testing.T) {
	if testing.Short() {
		t.Skip("32 MiB store fixture is skipped in -short")
	}
	t.Parallel()
	s := openTestStore(t)
	sessionID := "99999999-9999-4999-8999-999999999999"
	const turnCount = 1312
	const contentBytes = 24 << 10
	turns := make([]storeTranscriptTurn, turnCount)
	for i := range turns {
		role := schema.RoleAssistant
		if i%2 == 0 {
			role = schema.RoleUser
		}
		turns[i] = storeTranscriptTurn{index: i * 2, role: role, contentBytes: contentBytes}
	}
	seedStoreTranscript(t, s, sessionID, turnCount, storeTranscriptEntries(t, sessionID, turns))

	ctx, cancel := context.WithCancel(context.Background())
	srv := api.NewServer(api.ServerConfig{Port: 0, Store: s})
	if err := srv.Listen(ctx); err != nil {
		cancel()
		t.Fatalf("Listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	base := "http://" + srv.Addr().String()

	started := time.Now()
	response, err := http.Get(base + "/api/v1/sessions/" + sessionID + "/transcript/segments")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	elapsed := time.Since(started)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("directory status = %d, body %s", response.StatusCode, body)
	}
	var directory schema.TranscriptSegmentDirectory
	if err := json.Unmarshal(body, &directory); err != nil {
		t.Fatalf("decode directory: %v", err)
	}
	if directory.TotalTurns != turnCount {
		t.Fatalf("totalTurns = %d, want %d", directory.TotalTurns, turnCount)
	}
	if len(directory.Segments) < 24 || len(directory.Segments) > 40 {
		t.Fatalf("32 MiB directory cut to %d segments, want the recorded 24..40 band", len(directory.Segments))
	}
	// The 400 ms target is the directory build's own recorded budget. This
	// measurement deliberately includes the store's full 32 MiB content pass and
	// its capture verification, so it is recorded beside that target rather than
	// compared as if it were the in-memory build. A pathological regression still
	// fails here.
	t.Logf("cold directory build through the store: %s (build target %s), %d segments", elapsed, defaults.TranscriptSegmentColdBuildBudget, len(directory.Segments))
	if elapsed > 2*defaults.TranscriptSegmentColdBuildBudget {
		t.Fatalf("cold build through the store took %s, far over the %s build target plus its store pass", elapsed, defaults.TranscriptSegmentColdBuildBudget)
	}
}
