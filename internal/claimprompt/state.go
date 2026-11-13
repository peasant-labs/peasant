package claimprompt

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// FileName is the runtime-state file holding the prompt's decisions.
const FileName = "claim-prompt.json"

// stateVersion is the on-disk schema for the prompt decisions.
const stateVersion = 1

// Decision records that the prompt was settled for one account scope. All is
// an explicit reassignment performed by the sign-in prompt; Unclaimed is a
// claim of the then-unclaimed rows; NotNow is a dismissal. None of these is a
// substitute for the append-only owner_claims log.
type Decision struct {
	Choice    Choice `json:"choice"`
	DecidedAt int64  `json:"decidedAt"`
}

// State holds the prompt decisions for one state directory. Decisions are
// keyed by local owner id plus the normalized Village base URL plus the Village
// user UUID, so one data directory's answer never suppresses another's prompt.
// Notices records the one-time already-signed-in notice separately, so showing
// it does not settle the real prompt.
type State struct {
	Version   int                 `json:"version"`
	Decisions map[string]Decision `json:"decisions"`
	Notices   map[string]int64    `json:"notices,omitempty"`
}

// DefaultPath returns the prompt-state path under a peasant state directory
// (the directory that already ends in "peasant").
func DefaultPath(stateDir string) string {
	return filepath.Join(stateDir, FileName)
}

// ScopeKey keys one prompt scope. The local owner id keeps two data
// directories sharing a state directory independent; the Village base URL must
// already be normalized, and a newline cannot occur in a normalized URL or a
// local owner id, so the three parts are unambiguous.
func ScopeKey(localOwnerID, villageURL, userID string) string {
	return localOwnerID + "\n" + villageURL + "\n" + userID
}

// Load reads prompt state. A missing file is an empty state, not an error.
func Load(path string) (State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{Version: stateVersion, Decisions: map[string]Decision{}}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("read local ownership prompt state at %q: %w; the prompt decision was not changed; fix state-directory access and retry", path, err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("parse local ownership prompt state at %q: %w; the prompt decision was not changed; repair or remove the file and retry", path, err)
	}
	if state.Version == 0 {
		state.Version = stateVersion
	}
	if state.Version != stateVersion {
		return State{}, fmt.Errorf("read local ownership prompt state at %q: unsupported version %d; the prompt decision was not changed; use the Peasant version that wrote it", path, state.Version)
	}
	if state.Decisions == nil {
		state.Decisions = map[string]Decision{}
	}
	if state.Notices == nil {
		state.Notices = map[string]int64{}
	}
	return state, nil
}

// Decided reports whether the prompt was already settled for this local owner
// and account scope, whatever the answer was.
func (s State) Decided(localOwnerID, villageURL, userID string) bool {
	_, ok := s.Decisions[ScopeKey(localOwnerID, villageURL, userID)]
	return ok
}

// Record marks the prompt settled for one local owner and account scope.
func (s *State) Record(localOwnerID, villageURL, userID string, choice Choice, nowMs int64) {
	s.Version = stateVersion
	if s.Decisions == nil {
		s.Decisions = map[string]Decision{}
	}
	s.Decisions[ScopeKey(localOwnerID, villageURL, userID)] = Decision{Choice: choice, DecidedAt: nowMs}
}

// Noticed reports whether the one-time already-signed-in notice was already
// shown for this local owner and account scope.
func (s State) Noticed(localOwnerID, villageURL, userID string) bool {
	_, ok := s.Notices[ScopeKey(localOwnerID, villageURL, userID)]
	return ok
}

// RecordNotice marks the one-time notice shown. It is not a prompt decision:
// the real sign-in prompt remains due.
func (s *State) RecordNotice(localOwnerID, villageURL, userID string, nowMs int64) {
	s.Version = stateVersion
	if s.Notices == nil {
		s.Notices = map[string]int64{}
	}
	s.Notices[ScopeKey(localOwnerID, villageURL, userID)] = nowMs
}

// Save atomically replaces the prompt state under a private mode.
func Save(path string, state State) error {
	if state.Decisions == nil {
		state.Decisions = map[string]Decision{}
	}
	if state.Notices == nil {
		state.Notices = map[string]int64{}
	}
	state.Version = stateVersion
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode local ownership prompt state for %q: %w; the prompt decision was not changed", path, err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create the local ownership prompt state directory %q: %w; the prompt decision was not changed; fix directory permissions and retry", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".claim-prompt-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create a temporary local ownership prompt state beside %q: %w; the prompt decision was not changed; fix directory permissions or free space and retry", path, err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("set private permissions on the temporary local ownership prompt state %q: %w; the destination was not changed", tmpPath, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write the temporary local ownership prompt state %q: %w; the destination was not changed; free disk space and retry", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync the temporary local ownership prompt state %q: %w; the destination was not changed; repair the filesystem and retry", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close the temporary local ownership prompt state %q: %w; the destination was not changed; repair the filesystem and retry", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("atomically replace the local ownership prompt state %q: %w; the previous file remains available; fix destination permissions and retry", path, err)
	}
	committed = true
	return nil
}
