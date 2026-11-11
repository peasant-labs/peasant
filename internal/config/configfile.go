package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/filelock"
)

// ErrConfigChanged reports that the configuration file is no longer the one a
// caller read. Swap returns a *ConfigChangedError that matches it.
var ErrConfigChanged = errors.New("configuration changed since it was read")

// ConfigChangedError is what Swap returns when the file on disk is not the
// document the caller read. Fresh holds the bytes now on disk, so the caller can
// recompute against them under the lock it already holds. Fresh is nil when the
// file is now absent.
type ConfigChangedError struct {
	Path  string
	Fresh []byte
}

func (e *ConfigChangedError) Error() string {
	return fmt.Sprintf(
		"swap configuration %q: the file changed after it was read.\n"+
			"what: the bytes on disk differ from the bytes this save was computed from.\n"+
			"why: a writer that does not take the configuration lock replaced the file while this save held it.\n"+
			"where: config.ConfigFile.Swap.\n"+
			"when: immediately before the atomic replacement.\n"+
			"means: nothing was written.\n"+
			"fix: recompute the save against the current file and retry.",
		e.Path)
}

// Is makes errors.Is(err, ErrConfigChanged) true for this error.
func (e *ConfigChangedError) Is(target error) bool { return target == ErrConfigChanged }

// ConfigFile holds the exclusive advisory lock on one configuration file across
// a read, a comparison and an atomic replacement. Every writer that takes the
// lock sees the file as the previous holder left it. A writer that does not take
// the lock can still write, so Swap re-reads the file before it replaces it.
//
// OpenConfigFile takes the lock and Close releases it. Close is idempotent, so a
// caller defers it and still releases the lock on every early return.
type ConfigFile struct {
	path    string
	mu      sync.Mutex
	release filelock.ReleaseFunc
}

// OpenConfigFile takes the lock beside path, waiting no longer than deadline. A
// zero deadline waits filelock.DefaultWait, which is long for a request handler:
// pass a deadline. The parent directory is created when missing so a first save
// can lock before the file exists.
func OpenConfigFile(path string, deadline time.Time) (*ConfigFile, error) {
	if path == "" {
		return nil, fmt.Errorf(
			"open configuration lock: the path is empty.\n" +
				"what: no configuration path was given.\n" +
				"why: the lock lives beside the file it protects.\n" +
				"where: config.OpenConfigFile.\n" +
				"when: before the file was read.\n" +
				"means: no lock was taken and nothing was changed.\n" +
				"fix: pass the resolved configuration path.")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, defaults.PublicDirPerm); err != nil {
		return nil, fmt.Errorf(
			"open configuration lock for %q: create %q: %w.\n"+
				"what: the directory that holds the lock could not be created.\n"+
				"where: config.OpenConfigFile.\n"+
				"means: no lock was taken and nothing was changed.\n"+
				"fix: correct the directory permissions and retry.", path, dir, err)
	}
	release, err := filelock.Acquire(path+".lock", deadline)
	if err != nil {
		return nil, fmt.Errorf(
			"open configuration lock for %q: %w.\n"+
				"what: another writer holds the configuration lock, or the lock file could not be opened.\n"+
				"where: config.OpenConfigFile.\n"+
				"means: no lock was taken and nothing was changed.\n"+
				"fix: wait for the other change to finish, then retry.", path, err)
	}
	return &ConfigFile{path: path, release: release}, nil
}

// Path is the configuration file this handle protects.
func (f *ConfigFile) Path() string { return f.path }

// Read returns the file as it is now: its bytes (nil when the file is absent),
// its YAML document, and the configuration those bytes parse to. An absent file
// reads as an empty document and the configuration defaults. A file that is
// unreadable, holds more than one YAML document, or does not parse is refused
// and nothing is returned.
func (f *ConfigFile) Read(ctx context.Context) ([]byte, *yaml.Node, *Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.release == nil {
		return nil, nil, nil, f.closedError("read")
	}
	return ReadDocumentFile(ctx, f.path)
}

// Swap replaces the file with data when it still holds expected. It re-reads the
// file first: a mismatch returns a *ConfigChangedError that carries the fresh
// bytes, and nothing is written. A nil expected means the file was absent when
// read. The replacement is the atomic, validated write every save uses.
func (f *ConfigFile) Swap(expected, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.release == nil {
		return f.closedError("swap")
	}
	current, err := readConfigBytes(f.path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, expected) || (current == nil) != (expected == nil) {
		return &ConfigChangedError{Path: f.path, Fresh: current}
	}
	return SaveAtomicYAML(f.path, data)
}

// Close releases the lock. Calling it again is harmless.
func (f *ConfigFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.release == nil {
		return nil
	}
	release := f.release
	f.release = nil
	return release()
}

func (f *ConfigFile) closedError(op string) error {
	return fmt.Errorf(
		"%s configuration %q: the lock was already released.\n"+
			"what: this handle no longer holds the configuration lock.\n"+
			"why: Close was called before the %s.\n"+
			"where: config.ConfigFile.\n"+
			"means: the file was not touched.\n"+
			"fix: open a new handle with config.OpenConfigFile.", op, f.path, op)
}

// ReadDocumentFile reads the configuration file at path without taking the lock.
// It returns what ConfigFile.Read returns. A caller that only previews a save
// uses it, because the save that follows re-reads the file under the lock.
func ReadDocumentFile(ctx context.Context, path string) ([]byte, *yaml.Node, *Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("read configuration %q: %w", path, err)
	}
	data, err := readConfigBytes(path)
	if err != nil {
		return nil, nil, nil, err
	}
	doc, cfg, err := ParseDocument(data)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read configuration %q: %w", path, err)
	}
	return data, doc, cfg, nil
}

// ParseDocument turns configuration bytes into the YAML document an editor
// changes in place and the configuration the same bytes parse to. Nil bytes (an
// absent file) are an empty document and the defaults. Bytes that hold more than
// one YAML document are refused: Peasant reads only the first, and writing the
// edited first document back would drop the others.
func ParseDocument(data []byte) (*yaml.Node, *Config, error) {
	if data == nil {
		return &yaml.Node{}, BaseConfig(), nil
	}
	if err := requireOneDocument(data); err != nil {
		return nil, nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, nil, err
	}
	doc := &yaml.Node{}
	if err := yaml.Unmarshal(data, doc); err != nil {
		return nil, nil, fmt.Errorf("config: parse YAML document: %w", err)
	}
	return doc, cfg, nil
}

func readConfigBytes(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf(
			"read configuration %q: %w.\n"+
				"what: the configuration file could not be read.\n"+
				"where: config.ReadDocumentFile.\n"+
				"means: nothing was changed.\n"+
				"fix: correct the file permissions and retry.", path, err)
	}
	// An empty file reads as an empty, non-nil slice, which stays distinct from
	// an absent file (nil).
	return data, nil
}

func requireOneDocument(data []byte) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for count := 0; ; count++ {
		var document yaml.Node
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("config: parse YAML document: %w", err)
		}
		if count == 1 {
			return errors.New(
				"config: the file holds more than one YAML document; " +
					"Peasant reads only the first, and a change here would drop the others; " +
					"merge them into one document by hand")
		}
	}
}
