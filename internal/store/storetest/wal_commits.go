package storetest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

const (
	walHeaderBytes      = 32
	walFrameHeaderBytes = 24
	walMagicLittle      = 0x377f0682
	walMagicBig         = 0x377f0683
)

// CountWALCommitFrames counts the committed transactions recorded in the
// write-ahead log of the database at dbPath. Every SQLite transaction that
// changed a page ends in one frame whose "database size after commit" field is
// non-zero, and only that frame; a read-only transaction and a transaction that
// changed no page write no frame at all. So the count moves by exactly one per
// committing transaction, which is what a test needs to prove how many
// transactions a store call made.
//
// The count is only meaningful on a store opened with
// store.WithWALAutocheckpointDisabled: a checkpoint lets the log be reused
// from its start, and the frames of an earlier generation are then no longer
// evidence. Frames whose salt does not match the log header are from such an
// earlier generation and are not counted. A missing or empty log counts zero.
func CountWALCommitFrames(dbPath string) (int, error) {
	data, err := os.ReadFile(dbPath + "-wal")
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(data) < walHeaderBytes {
		return 0, nil
	}
	magic := binary.BigEndian.Uint32(data[0:4])
	if magic != walMagicLittle && magic != walMagicBig {
		return 0, fmt.Errorf("count commits in %s-wal: not a write-ahead log (magic %#x)", dbPath, magic)
	}
	pageSize := int(binary.BigEndian.Uint32(data[8:12]))
	if pageSize < 512 || pageSize > 65536 || pageSize&(pageSize-1) != 0 {
		return 0, fmt.Errorf("count commits in %s-wal: invalid page size %d", dbPath, pageSize)
	}
	salt1, salt2 := binary.BigEndian.Uint32(data[16:20]), binary.BigEndian.Uint32(data[20:24])
	frameBytes := walFrameHeaderBytes + pageSize
	commits := 0
	for offset := walHeaderBytes; offset+frameBytes <= len(data); offset += frameBytes {
		frame := data[offset : offset+walFrameHeaderBytes]
		if binary.BigEndian.Uint32(frame[8:12]) != salt1 || binary.BigEndian.Uint32(frame[12:16]) != salt2 {
			continue
		}
		if binary.BigEndian.Uint32(frame[4:8]) != 0 {
			commits++
		}
	}
	return commits, nil
}
