package main

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/term"
)

// discoverySpinner renders a single-line spinner on stderr during kickstart
// discovery. It shows the current phase and counts, overwriting the same line.
// In non-TTY environments (pipes, CI), it is a no-op.
type discoverySpinner struct {
	w     io.Writer
	isTTY bool
	mu    sync.Mutex
	phase string
	count int
	total int
	done  bool
	wg    sync.WaitGroup
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// newDiscoverySpinner creates a spinner that writes to w.
func newDiscoverySpinner(w io.Writer) *discoverySpinner {
	isTTY := false
	if f, ok := w.(*os.File); ok {
		isTTY = term.IsTerminal(int(f.Fd()))
	}
	s := &discoverySpinner{w: w, isTTY: isTTY}
	if isTTY {
		s.wg.Add(1)
		go s.run()
	}
	return s
}

// SetPhase updates the displayed phase label. Safe to call on nil.
func (s *discoverySpinner) SetPhase(phase string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.phase = phase
	s.count = 0
	s.total = 0
	s.mu.Unlock()
}

// SetProgress updates the count/total for the current phase. Safe to call on nil.
func (s *discoverySpinner) SetProgress(count, total int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.count = count
	s.total = total
	s.mu.Unlock()
}

// Increment atomically increments the count. Safe to call on nil.
func (s *discoverySpinner) Increment() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.count++
	s.mu.Unlock()
}

// Stop ends the spinner and clears the line. Safe to call on nil.
func (s *discoverySpinner) Stop() {
	if s == nil || !s.isTTY {
		return
	}
	s.mu.Lock()
	s.done = true
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *discoverySpinner) run() {
	defer s.wg.Done()
	frame := 0
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		s.mu.Lock()
		if s.done {
			// Erase the line and return.
			fmt.Fprintf(s.w, "\r\x1b[2K")
			s.mu.Unlock()
			return
		}
		ch := spinnerFrames[frame%len(spinnerFrames)]
		frame++

		var line string
		if s.total > 0 {
			line = fmt.Sprintf("\r\x1b[2K%s %s (%d/%d)", ch, s.phase, s.count, s.total)
		} else if s.count > 0 {
			line = fmt.Sprintf("\r\x1b[2K%s %s (%d found)", ch, s.phase, s.count)
		} else {
			line = fmt.Sprintf("\r\x1b[2K%s %s", ch, s.phase)
		}
		fmt.Fprint(s.w, line)
		s.mu.Unlock()
	}
}
