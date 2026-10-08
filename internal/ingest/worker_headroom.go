package ingest

import "fmt"

// checkCapacities checks per-worker parser headroom without allocating unused
// scratch buffers. Native candidates have separate byte admission accounting.
func (w WriteConfig) checkCapacities(workers int) error {
	reserved := int64(workers) * w.BufferBytes
	if reserved > w.StagedMemoryBytes {
		return fmt.Errorf("ingest: %d workers need %d bytes of parser headroom, past write.stagedMemoryBytes=%d; raise the staged-memory budget, lower write.bufferBytes, or run fewer workers", workers, reserved, w.StagedMemoryBytes)
	}
	return nil
}
