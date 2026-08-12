package settings

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
)

// Draft is a mutable editing session over the loaded configuration. It holds a
// deep-copied baseline (the config as it was when the flow opened) and a working
// copy the fields edit through their accessors. Dirty tracking is a
// baseline-vs-working compare; Discard resets the working copy to baseline; and
// Commit preserves an existing clean source byte-for-byte or routes changed and
// missing configs through the ONE existing atomic save path with the same drift
// discipline the kickstart wizard uses, so the settings flow never invents a
// second way to persist configuration.
type Draft struct {
	path     string
	baseline config.Config
	working  config.Config

	// transientResets re-applies values intentionally omitted from config YAML
	// after Discard deep-copies the baseline through YAML. SeedInitial is the
	// only way to register one of these values, so baseline and working always
	// start from the same typed value.
	transientResets []func(*config.Config)

	// expectedBytes is the exact file content observed when the draft opened.
	// Commit compares the file against it and fails closed if another process
	// changed the file since - the CheckSnapshot / ExpectedBytes discipline the
	// kickstart wizard's SaveTo uses.
	expectedBytes  []byte
	expectedExists bool
}

// SeedInitial applies value through the same typed accessor to both the
// baseline and working copies of d. It is intended for settings that
// participate in a settings presentation but are persisted somewhere other
// than config.yaml. The draft and both accessor functions are validated before
// either copy is mutated.
func SeedInitial[T comparable](d *Draft, acc Accessor[T], value T) error {
	if d == nil {
		return fmt.Errorf(
			"seed settings initial value: draft is nil.\n" +
				"what: no settings draft was provided for paired initialization.\n" +
				"why: baseline and working state cannot be updated without an open draft.\n" +
				"where: settings.SeedInitial.\n" +
				"when: before fields mount for a settings presentation.\n" +
				"means: no draft state was changed.\n" +
				"fix: call settings.NewDraft successfully, then pass that draft to SeedInitial.")
	}
	if d.path == "" {
		return fmt.Errorf(
			"seed settings initial value: draft has no destination path.\n" +
				"what: the supplied draft is not a valid editing session.\n" +
				"why: it was not opened through settings.NewDraft with a resolved config path.\n" +
				"where: settings.SeedInitial.\n" +
				"when: before fields mount for a settings presentation.\n" +
				"means: neither baseline nor working state was changed.\n" +
				"fix: create the draft with settings.NewDraft and retry paired initialization.")
	}
	if acc.Get == nil {
		return fmt.Errorf(
			"seed settings initial value for %q: accessor Get is nil.\n"+
				"what: the typed accessor cannot read the initialized value.\n"+
				"why: its Get function was not supplied.\n"+
				"where: settings.SeedInitial.\n"+
				"when: validating the accessor before draft mutation.\n"+
				"means: neither baseline nor working state was changed.\n"+
				"fix: provide one Accessor with both Get and Set functions, then retry.", d.path)
	}
	if acc.Set == nil {
		return fmt.Errorf(
			"seed settings initial value for %q: accessor Set is nil.\n"+
				"what: the typed accessor cannot write the initialized value.\n"+
				"why: its Set function was not supplied.\n"+
				"where: settings.SeedInitial.\n"+
				"when: validating the accessor before draft mutation.\n"+
				"means: neither baseline nor working state was changed.\n"+
				"fix: provide one Accessor with both Get and Set functions, then retry.", d.path)
	}

	acc.Set(&d.baseline, value)
	acc.Set(&d.working, value)
	d.transientResets = append(d.transientResets, func(cfg *config.Config) {
		acc.Set(cfg, value)
	})
	return nil
}

// NewDraft opens a draft over loaded, editing the config persisted at path. It
// deep-copies loaded into an independent baseline and working copy (loaded is
// never mutated) and snapshots the on-disk bytes at path for drift detection.
func NewDraft(path string, loaded *config.Config) (*Draft, error) {
	if path == "" {
		return nil, fmt.Errorf(
			"open settings draft: destination path is empty.\n" +
				"what: no config path was provided to edit.\n" +
				"where: settings.NewDraft.\n" +
				"fix: pass the resolved --config path the flow should commit to.")
	}
	if loaded == nil {
		return nil, fmt.Errorf(
			"open settings draft for %q: loaded configuration is nil.\n"+
				"what: there is no baseline config to copy.\n"+
				"where: settings.NewDraft.\n"+
				"fix: load the config (config.Load) before opening a draft.", path)
	}
	baseline, err := cloneConfig(loaded)
	if err != nil {
		return nil, fmt.Errorf("open settings draft for %q: copy baseline: %w", path, err)
	}
	working, err := cloneConfig(loaded)
	if err != nil {
		return nil, fmt.Errorf("open settings draft for %q: copy working set: %w", path, err)
	}
	d := &Draft{path: path, baseline: baseline, working: working}
	current, err := os.ReadFile(path)
	switch {
	case err == nil:
		d.expectedBytes = current
		d.expectedExists = true
	case os.IsNotExist(err):
		d.expectedExists = false
	default:
		return nil, fmt.Errorf(
			"open settings draft for %q: read current file for drift detection: %w.\n"+
				"where: settings.NewDraft.\n"+
				"fix: ensure the config path is readable and retry.", path, err)
	}
	return d, nil
}

// Path is the destination the draft commits to.
func (d *Draft) Path() string { return d.path }

// Working is the config the fields edit. Accessors read and write through it.
func (d *Draft) Working() *config.Config { return &d.working }

// Baseline is the config as it was when the draft opened, used to drop edits
// (write a baseline value back into the working copy) and to compute Dirty.
func (d *Draft) Baseline() *config.Config { return &d.baseline }

// Dirty reports whether the working copy differs from the baseline.
func (d *Draft) Dirty() bool {
	equal, err := persistedConfigsEqual(&d.baseline, &d.working)
	return err != nil || !equal
}

// Discard drops every edit, resetting the working copy to the baseline.
func (d *Draft) Discard() error {
	baseline, err := cloneConfig(&d.baseline)
	if err != nil {
		return fmt.Errorf("discard settings draft for %q: %w", d.path, err)
	}
	for _, reset := range d.transientResets {
		reset(&baseline)
	}
	d.working = baseline
	return nil
}

// Commit first detects drift and fails closed if the destination changed. An
// existing semantically clean config is then a successful byte-preserving
// no-op, allowing Screen to emit SavedMsg for transient-only effects without
// discarding comments or formatting. A missing destination is still created;
// changed persisted settings delegate to config.SaveAtomic. Nothing is written
// on any error.
func (d *Draft) Commit() error {
	current, err := os.ReadFile(d.path)
	switch {
	case err == nil:
		if !d.expectedExists {
			return d.driftError("a config file appeared at %s after the settings flow opened against no file")
		}
		if !bytes.Equal(current, d.expectedBytes) {
			return d.driftError("the config file at %s changed after the settings flow opened")
		}
	case os.IsNotExist(err):
		if d.expectedExists {
			return d.driftError("the config file at %s was removed after the settings flow opened")
		}
	default:
		return fmt.Errorf(
			"commit settings: check %q for drift before writing: %w.\n"+
				"what: the current file could not be read to confirm it is unchanged.\n"+
				"where: settings.Draft.Commit.\n"+
				"means: no file was changed.\n"+
				"fix: ensure the config path is readable and re-run the settings flow.", d.path, err)
	}
	if d.expectedExists {
		equal, err := persistedConfigsEqual(&d.baseline, &d.working)
		if err != nil {
			return fmt.Errorf(
				"commit settings: compare persisted state for %q after drift validation: %w.\n"+
					"what: the draft could not determine whether config-backed settings changed.\n"+
					"why: baseline or working configuration could not be serialized for a semantic comparison.\n"+
					"where: settings.Draft.Commit clean-save decision.\n"+
					"when: after the exact-byte drift check and before any atomic replacement.\n"+
					"means: the existing config bytes, comments, ordering, and formatting remain unchanged.\n"+
					"fix: correct the unsupported configuration value, reopen the editor, and retry ctrl+s.",
				d.path, err)
		}
		if equal {
			return nil
		}
	}
	if err := config.SaveAtomic(d.path, &d.working); err != nil {
		return err
	}
	// The freshly-written file is now the drift baseline for any later commit.
	written, err := os.ReadFile(d.path)
	if err == nil {
		d.expectedBytes = written
		d.expectedExists = true
	}
	return nil
}

func persistedConfigsEqual(left, right *config.Config) (bool, error) {
	leftBytes, err := yaml.Marshal(left)
	if err != nil {
		return false, fmt.Errorf("marshal baseline config: %w", err)
	}
	rightBytes, err := yaml.Marshal(right)
	if err != nil {
		return false, fmt.Errorf("marshal working config: %w", err)
	}
	return bytes.Equal(leftBytes, rightBytes), nil
}

// driftError formats the fail-closed drift message with the standard
// what/why/where/fix shape.
func (d *Draft) driftError(what string) error {
	return fmt.Errorf("%s.\n"+
		"why: committing would overwrite an edit made outside this flow.\n"+
		"where: settings.Draft.Commit.\n"+
		"means: nothing was written.\n"+
		"fix: re-run the settings flow to review the current file, then apply your changes again.",
		fmt.Sprintf(what, d.path))
}

// cloneConfig deep-copies a config.Config through a YAML round-trip. Every
// config field is exported with a yaml tag, so the round-trip reproduces the
// value exactly (maps and slices included) without sharing backing storage with
// the original.
func cloneConfig(src *config.Config) (config.Config, error) {
	data, err := yaml.Marshal(src)
	if err != nil {
		return config.Config{}, fmt.Errorf("marshal config for deep copy: %w", err)
	}
	var dst config.Config
	if err := yaml.Unmarshal(data, &dst); err != nil {
		return config.Config{}, fmt.Errorf("unmarshal config for deep copy: %w", err)
	}
	return dst, nil
}
