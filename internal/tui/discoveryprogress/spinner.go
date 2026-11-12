// Package discoveryprogress renders the inline kickstart discovery spinner.
package discoveryprogress

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/peasant-labs/peasant/internal/discovery"
)

// frameInterval is how often a terminal spinner redraws its line and advances
// to the next frame.
const frameInterval = 80 * time.Millisecond

// frames is the Braille animation a terminal spinner cycles through, one frame
// per frameInterval.
var frames = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// rereadsNotReported is the re-read count of a phase whose adapter has not
// reported one. Any negative count means the same.
const rereadsNotReported = -1

// Spinner shows the current discovery phase and counts on one terminal line.
// When terminal is false it records updates but does not write to the output.
type Spinner struct {
	w        io.Writer
	terminal bool
	mu       sync.Mutex
	phase    string
	count    int
	total    int
	rereads  int
	done     bool
	frame    int
	wg       sync.WaitGroup
}

var _ discovery.ScanProgress = (*Spinner)(nil)
var _ discovery.ScanRereadProgress = (*Spinner)(nil)

// New starts the spinner only for a terminal. The caller determines whether
// the destination is a terminal, allowing other writers to remain quiet.
func New(w io.Writer, terminal bool) *Spinner {
	s := &Spinner{w: w, terminal: terminal, rereads: rereadsNotReported}
	if terminal {
		s.wg.Add(1)
		go s.run()
	}
	return s
}

// SetPhase changes the displayed phase and clears counts from the last phase.
func (s *Spinner) SetPhase(phase string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.phase = phase
	s.count = 0
	s.total = 0
	s.rereads = rereadsNotReported
	s.mu.Unlock()
}

// SetDiscoveryRequiredRereads sets how many sources this phase has had to read
// again so far. A negative count means the adapter does not report one, and
// the line shows no re-read count.
func (s *Spinner) SetDiscoveryRequiredRereads(count int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.rereads = count
	s.mu.Unlock()
}

// SetProgress updates the count and total for the current phase.
func (s *Spinner) SetProgress(count, total int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.count = count
	s.total = total
	s.mu.Unlock()
}

// Increment atomically increments the current count.
func (s *Spinner) Increment() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.count++
	s.mu.Unlock()
}

// View returns the current spinner line, without terminal cursor controls.
// The terminal writer uses the same view on every refresh.
func (s *Spinner) View() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view()
}

// view requires the mutex; it keeps a snapshot consistent while rendering.
func (s *Spinner) view() string {
	ch := frames[s.frame%len(frames)]
	if s.total > 0 {
		line := fmt.Sprintf("%s %s (%d/%d)", ch, s.phase, s.count, s.total)
		if s.rereads >= 0 {
			line += fmt.Sprintf(" · %d required re-read", s.rereads)
		}
		return line
	}
	if s.count > 0 {
		return fmt.Sprintf("%s %s (%d found)", ch, s.phase, s.count)
	}
	return fmt.Sprintf("%s %s", ch, s.phase)
}

// Stop ends the spinner and clears the line. It is safe to call on nil.
func (s *Spinner) Stop() {
	if s == nil || !s.terminal {
		return
	}
	s.mu.Lock()
	s.done = true
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Spinner) run() {
	defer s.wg.Done()
	ticker := time.NewTicker(frameInterval)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		if s.done {
			fmt.Fprint(s.w, "\r\x1b[2K")
			s.mu.Unlock()
			return
		}
		fmt.Fprintf(s.w, "\r\x1b[2K%s", s.view())
		s.frame++
		s.mu.Unlock()
	}
}
