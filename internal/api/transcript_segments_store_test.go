package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

// Compare the same seeded oversized turn through both production HTTP reads.
func TestTranscriptOversizedDurableReadComparison(t *testing.T) {
	data, err := os.ReadFile("testdata/transcript_segments.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name  string `yaml:"name"`
			Turns []struct {
				Index        int `yaml:"index"`
				ContentBytes int `yaml:"contentBytes"`
			} `yaml:"turns"`
		} `yaml:"segment_cases"`
	}
	if err := yaml.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	var size, index int
	for _, c := range fixture.Cases {
		if c.Name == "one-oversized-turn" {
			size, index = c.Turns[0].ContentBytes, c.Turns[0].Index
		}
	}
	if size == 0 {
		t.Fatal("missing oversized fixture")
	}
	db := openTestStore(t)
	const sessionID = "77777777-7777-4777-8777-777777777777"
	seedStoreTranscript(t, db, sessionID, 1, storeTranscriptEntries(t, sessionID, []storeTranscriptTurn{{index: index, role: schema.RoleAssistant, contentBytes: size}}))
	ctx, cancel := context.WithCancel(context.Background())
	srv := api.NewServer(api.ServerConfig{Port: 0, Store: db, Provider: api.NewStoreDataProvider(db, sessionvisibility.Browsing())})
	if err := srv.Listen(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && err != context.Canceled {
			t.Error(err)
		}
	})
	base := "http://" + srv.Addr().String() + "/api/v1/sessions/" + sessionID
	read := func(path string, target any) {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s: %d %s", path, resp.StatusCode, body)
		}
		if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
			t.Fatal(err)
		}
	}
	var segment schema.TranscriptSegment
	var detail schema.SessionDetailReadPayload
	read("/transcript/segments/0", &segment)
	read("", &detail)
	if len(segment.Turns) != 1 || len(detail.Turns) != 1 {
		t.Fatalf("unexpected turns: segment %d durable %d", len(segment.Turns), len(detail.Turns))
	}
	turn := segment.Turns[0]
	t.Logf("segment content bytes=%d byteLength=%d truncated=%v; durable content bytes=%d; per-field bound=%d document budget=%d; input bytes=%d", len(turn.Content), turn.ByteLength, turn.Truncated, len(detail.Turns[0].Content), defaults.ServedTextFieldBudgetBytes, defaults.ServedDetailDocumentBudgetBytes, size)
	if !turn.Truncated || len(detail.Turns[0].Content) > len(turn.Content) {
		t.Fatal("durable read exceeded the segment display")
	}
}

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

func servedSegmentZero(t *testing.T, db *store.Store, sessionID string) schema.TranscriptSegment {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	srv := api.NewServer(api.ServerConfig{Port: 0, Store: db})
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
	response, err := http.Get("http://" + srv.Addr().String() + "/api/v1/sessions/" + sessionID + "/transcript/segments/0")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("segment 0 status = %d, body %s", response.StatusCode, body)
	}
	var segment schema.TranscriptSegment
	if err := json.Unmarshal(body, &segment); err != nil {
		t.Fatalf("decode segment 0: %v", err)
	}
	return segment
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
	// This test measures one cold build and must not run beside the package's
	// other heavy transcript tests; the full-suite harness runs packages
	// concurrently, but this package's own load is avoidable.
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

	runtime.GC()
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
	if len(directory.Segments) != 32 {
		t.Fatalf("32 MiB directory cut to %d segments, want the recorded 32", len(directory.Segments))
	}
	response, err = http.Get(fmt.Sprintf("%s/api/v1/sessions/%s/transcript/segments/0", base, sessionID))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("segment 0 status = %d, body %s", response.StatusCode, body)
	}
	var segment schema.TranscriptSegment
	if err := json.Unmarshal(body, &segment); err != nil {
		t.Fatalf("decode segment: %v", err)
	}
	if segment.ByteLength != directory.Segments[0].ByteLength {
		t.Fatalf("served segment byteLength = %d, directory declared %d", segment.ByteLength, directory.Segments[0].ByteLength)
	}
	// The store-path measurement includes the full 32 MiB content pass and its
	// capture verification. It is reported against the ratified 600 ms target and
	// fails only at the 2 s catastrophic ceiling: shared-runner contention must
	// not turn a normal build into a red gate.
	t.Logf("cold directory build through the store: %d ms (target %d ms, ceiling %d ms), %d segments",
		elapsed.Milliseconds(), defaults.TranscriptSegmentColdBuildBudget.Milliseconds(), defaults.TranscriptSegmentColdBuildCeiling.Milliseconds(), len(directory.Segments))
	if elapsed > defaults.TranscriptSegmentColdBuildCeiling {
		t.Fatalf("cold build through the store took %d ms, over the %d ms catastrophic ceiling", elapsed.Milliseconds(), defaults.TranscriptSegmentColdBuildCeiling.Milliseconds())
	}
	if elapsed > defaults.TranscriptSegmentColdBuildBudget {
		t.Logf("WARNING: cold build through the store took %d ms, over the ratified %d ms target (recorded local range 430-510 ms; shared runners can be slower)", elapsed.Milliseconds(), defaults.TranscriptSegmentColdBuildBudget.Milliseconds())
	}
}
