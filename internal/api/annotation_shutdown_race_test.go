package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// Regression for the shutdown race where the annotation-create WebSocket
// broadcast goroutine outlived the test server and dereferenced a closed
// store's pool (SIGSEGV in CI). The create handler now runs the broadcast on
// a Server-tracked goroutine and Shutdown drains it, so this
// create-then-immediately-shut-down cycle is structurally safe; the loop plus
// the race detector make a regression loud again.
func TestCreateAnnotation_ShutdownDrainsBroadcast(t *testing.T) {
	for range 10 {
		s, sessionID := openStoreWithSession(t)
		baseURL, shutdown := startAnnotationServer(t, s)

		resp := postAnnotation(t, baseURL, schema.CreateAnnotationRequest{
			SessionID:     sessionID,
			TypeID:        testutil.TestTypeIDSessionOutcome,
			Value:         "resolved",
			AnnotatorName: "human-web",
		})
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create annotation: status %d, want 201", resp.StatusCode)
		}

		// Shut down immediately: before the fix this raced the broadcast
		// goroutine against the store close that follows via t.Cleanup.
		shutdown()

		// The drained server must leave the store usable for its owner.
		if _, err := s.GetAnnotationsForSession(context.Background(), sessionID); err != nil {
			t.Fatalf("store unusable after server shutdown: %v", err)
		}
	}
}
