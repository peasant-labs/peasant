package harnesslayout

import "time"

// The probes that read SQLite stores (Cursor and Antigravity) read files that
// the running tool may hold open. Each probe query reads at most
// sqliteProbeMaxRows rows, no probe decodes or reads a value larger than
// sqliteProbeMaxValueBytes, and a locked database is retried for
// sqliteProbeBusyTimeout.
const (
	sqliteProbeMaxRows       = 20000
	sqliteProbeMaxValueBytes = 16 << 20
	sqliteProbeBusyTimeout   = 5 * time.Second
)
