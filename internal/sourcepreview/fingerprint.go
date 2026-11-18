package sourcepreview

import (
	"crypto/sha256"
	"encoding/json"
	"os"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// A capture fails closed when its source changes under what it already read.
// A file is checked before every continuation read; a current OpenCode session
// is checked through the source's change marker (see stepSQLiteResumable).
// Appended data past the extent fixed at open is not a change: the capture
// never reads it and says the session grew. A rewrite inside the consumed
// part is.

// verifyFile proves the bytes already loaded are still the file's bytes before
// a continuation reads past them: the same file, at least as long, and the
// same last bytes before the consumed offset.
func (c *Capture) verifyFile(path string) error {
	consumed := int64(len(c.loaded))
	info, err := c.reader.fs.Stat(path)
	if err != nil {
		return captureError(CloseSourceChanged, err, "the transcript could not be measured before reading past byte %d", consumed)
	}
	if info.Size() < consumed {
		return captureError(CloseSourceChanged, nil, "the transcript is %d bytes, shorter than the %d already read", info.Size(), consumed)
	}
	comparable := c.identity != nil && osBacked(c.identity) && osBacked(info)
	if comparable && !os.SameFile(c.identity, info) {
		return captureError(CloseSourceChanged, nil, "the transcript was replaced by another file after %d bytes were read", consumed)
	}
	tail := min(consumed, fingerprintTailBytes)
	if tail == 0 {
		return nil
	}
	data, err := c.ranged.ReadFileRange(path, consumed-tail, tail)
	if err != nil {
		return captureError(CloseSourceChanged, err, "the last %d bytes already read could not be read again", tail)
	}
	if string(data) != string(c.loaded[consumed-tail:]) {
		return captureError(CloseSourceChanged, nil, "the %d bytes before byte %d differ from what was read", tail, consumed)
	}
	return nil
}

// fingerprintTailBytes is how much of the consumed prefix a continuation
// compares before it reads on.
const fingerprintTailBytes int64 = 4 << 10

// osBacked reports a FileInfo from the operating system, the only kind
// os.SameFile can compare.
func osBacked(info os.FileInfo) bool { return info.Sys() != nil }

// turnDigest is the SHA-256 of a turn's canonical JSON. Two reads that fold a
// turn the same way give the same digest.
func turnDigest(turn ingest.Turn) [sha256.Size]byte {
	encoded, err := json.Marshal(turn)
	if err != nil {
		// ingest.Turn holds only JSON-encodable fields; a future field that
		// is not must not let two different turns share a digest.
		panic("sourcepreview: encode a turn for its digest: " + err.Error())
	}
	return sha256.Sum256(encoded)
}
