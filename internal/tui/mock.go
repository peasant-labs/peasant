package tui

import (
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/mock"
)

// MockSessions returns deterministic sessions with realistic mock data.
func MockSessions() []ingest.Session {
	return mock.Sessions()
}
