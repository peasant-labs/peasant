package storetest

import "github.com/peasant-labs/peasant/internal/testkit/wal"

// CountWALCommitFrames counts committed transactions in a database WAL.
// See wal.CountWALCommitFrames for the checkpoint and lifetime requirements.
func CountWALCommitFrames(dbPath string) (int, error) {
	return wal.CountWALCommitFrames(dbPath)
}
