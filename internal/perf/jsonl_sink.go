package perf

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

type JSONLTraceSink struct {
	mu sync.Mutex
	w  io.Writer
	c  io.Closer
}

var _ TraceSink = (*JSONLTraceSink)(nil)

func NewJSONLTraceSink(w io.Writer) *JSONLTraceSink {
	return &JSONLTraceSink{w: w}
}

func NewJSONLTraceSinkCloser(w io.WriteCloser) *JSONLTraceSink {
	return &JSONLTraceSink{w: w, c: w}
}

func (s *JSONLTraceSink) WriteEvent(event Event) error {
	if s == nil || s.w == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := json.NewEncoder(s.w).Encode(event); err != nil {
		return fmt.Errorf("write profile JSONL trace event: encode failed; profile trace may be incomplete and caller should retry with a writable trace destination: %w", err)
	}
	return nil
}

func (s *JSONLTraceSink) Close() error {
	if s == nil || s.c == nil {
		return nil
	}
	return s.c.Close()
}
