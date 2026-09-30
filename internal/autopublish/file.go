package autopublish

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/filelock"
)

// FileName is the rules file in the Peasant config directory. It sits beside
// config.yaml and the stored credential, so a hook bound to a config
// directory reads the rules of that directory.
const FileName = "hooks.yaml"

// fileVersion is the only layout this version reads and writes.
const fileVersion = 1

// lockWait bounds how long a change waits for another writer of the file.
const lockWait = 5 * time.Second

// document is the file's layout.
type document struct {
	Version     int    `yaml:"version"`
	AutoPublish []Rule `yaml:"autoPublish"`
}

// Path is the rules file of the config directory.
func Path(configDir defaults.ConfigDirPath) string {
	return filepath.Join(string(configDir), FileName)
}

// Load reads the rules at path, in file order. A missing file holds no rule.
// A file that cannot be read, or that holds an invalid or repeated rule, is an
// error: a rule decides who can read a transcript, so a rule that cannot be
// read is never guessed at.
func Load(path string) ([]Rule, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return []Rule{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the auto-publish rules in %s: %w; fix the file's permissions and retry", path, err)
	}
	return parse(path, raw)
}

func parse(path string, raw []byte) ([]Rule, error) {
	var doc document
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read the auto-publish rules in %s: %w; correct the file or remove it, and retry", path, err)
	}
	// A file written by hand may leave the version out; it is the only one.
	if doc.Version != fileVersion && doc.Version != 0 {
		return nil, fmt.Errorf("read the auto-publish rules in %s: version %d is not the version %d this Peasant reads; nothing was applied; upgrade Peasant, or rewrite the file", path, doc.Version, fileVersion)
	}
	rules := doc.AutoPublish
	if rules == nil {
		rules = []Rule{}
	}
	for _, rule := range rules {
		// A missing list is not an empty one: "events: []" pauses a rule on
		// purpose, and a forgotten key must not.
		if rule.Events == nil || rule.Collectives == nil {
			return nil, fmt.Errorf("read the auto-publish rules in %s: rule %q has no events or no collectives list; write events: [pre-push] (or [] to pause the rule) and collectives: [<collective id>], and retry", path, rule.ID)
		}
	}
	if err := validateRules(rules); err != nil {
		return nil, fmt.Errorf("read the auto-publish rules in %s: %w", path, err)
	}
	return rules, nil
}

func validateRules(rules []Rule) error {
	ids := make(map[string]struct{}, len(rules))
	for _, rule := range rules {
		if err := rule.Validate(); err != nil {
			return err
		}
		if _, duplicate := ids[rule.ID]; duplicate {
			return fmt.Errorf("rule %q is listed twice; give each rule its own identifier", rule.ID)
		}
		ids[rule.ID] = struct{}{}
	}
	return nil
}

// Update applies change to the rules at path and saves the result, holding the
// file's lock so a change from the terminal and one from the local web never
// overwrite each other. Nothing is written when change or validation fails.
func Update(path string, change func([]Rule) ([]Rule, error)) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, defaults.PrivateDirPerm); err != nil {
		return fmt.Errorf("save the auto-publish rules in %s: create %s: %w; nothing was changed", path, dir, err)
	}
	release, err := filelock.Acquire(path+".lock", time.Now().Add(lockWait))
	if err != nil {
		return fmt.Errorf("save the auto-publish rules in %s: another change holds the file: %w; nothing was changed; retry", path, err)
	}
	defer func() { _ = release() }()

	rules, err := Load(path)
	if err != nil {
		return err
	}
	changed, err := change(rules)
	if err != nil {
		return err
	}
	if err := validateRules(changed); err != nil {
		return fmt.Errorf("save the auto-publish rules in %s: %w; nothing was changed", path, err)
	}
	raw, err := yaml.Marshal(document{Version: fileVersion, AutoPublish: nonNil(changed)})
	if err != nil {
		return fmt.Errorf("save the auto-publish rules in %s: %w; nothing was changed", path, err)
	}
	return writeAtomic(path, raw)
}

// writeAtomic replaces path with raw through a temporary file beside it, so a
// reader sees the old rules or the new ones and never a partial file.
func writeAtomic(path string, raw []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hooks-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("save the auto-publish rules in %s: %w; nothing was changed", path, err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(defaults.PrivateFilePerm); err == nil {
		if _, err = tmp.Write(raw); err == nil {
			if err = tmp.Sync(); err == nil {
				err = tmp.Close()
			}
		}
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("save the auto-publish rules in %s: %w; the previous rules are unchanged", path, err)
	}
	return nil
}
