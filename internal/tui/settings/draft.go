package settings

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/onboarding"
)

// Draft is a mutable editing session over the loaded configuration. The
// baseline and working pair, transient seeds, and discard/rebuild live in the
// neutral draft. This type keeps the on-disk drift snapshot and the one atomic
// commit path.
type Draft struct {
	core           *onboarding.Draft
	expectedBytes  []byte
	expectedExists bool
}

// SeedInitial applies value through the same typed accessor to both the
// baseline and working copies of d. It is intended for settings that
// participate in a settings presentation but are persisted somewhere other
// than config.yaml. The draft and both accessor functions are validated before
// either copy is mutated.
func SeedInitial[T comparable](d *Draft, acc Accessor[T], value T) error {
	if d == nil || d.core == nil {
		return fmt.Errorf(
			"seed settings initial value: draft is nil.\n" +
				"what: no settings draft was provided for paired initialization.\n" +
				"why: baseline and working state cannot be updated without an open draft.\n" +
				"where: settings.SeedInitial.\n" +
				"when: before fields mount for a settings presentation.\n" +
				"means: no draft state was changed.\n" +
				"fix: call settings.NewDraft successfully, then pass that draft to SeedInitial.")
	}
	if d.core.Path() == "" {
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
				"fix: provide one Accessor with both Get and Set functions, then retry.", d.core.Path())
	}
	if acc.Set == nil {
		return fmt.Errorf(
			"seed settings initial value for %q: accessor Set is nil.\n"+
				"what: the typed accessor cannot write the initialized value.\n"+
				"why: its Set function was not supplied.\n"+
				"where: settings.SeedInitial.\n"+
				"when: validating the accessor before draft mutation.\n"+
				"means: neither baseline nor working state was changed.\n"+
				"fix: provide one Accessor with both Get and Set functions, then retry.", d.core.Path())
	}
	return d.core.SeedTransient(func(cfg *config.Config) {
		acc.Set(cfg, value)
	})
}

// NewDraft opens a draft over loaded, editing the config persisted at path. It
// deep-copies loaded into an independent baseline and working copy (loaded is
// never mutated) and snapshots the on-disk bytes at path for drift detection.
func NewDraft(path string, loaded *config.Config) (*Draft, error) {
	core, err := onboarding.OpenDraft(path, loaded)
	if err != nil {
		return nil, err
	}
	d := &Draft{core: core}
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
func (d *Draft) Path() string {
	if d == nil || d.core == nil {
		return ""
	}
	return d.core.Path()
}

// Working is the config the fields edit. Accessors read and write through it.
func (d *Draft) Working() *config.Config {
	if d == nil || d.core == nil {
		return nil
	}
	return d.core.Working()
}

// Baseline is the config as it was when the draft opened, used to drop edits
// (write a baseline value back into the working copy) and to compute Dirty.
func (d *Draft) Baseline() *config.Config {
	if d == nil || d.core == nil {
		return nil
	}
	return d.core.Baseline()
}

// Dirty reports whether the working copy differs from the baseline.
func (d *Draft) Dirty() bool {
	if d == nil || d.core == nil {
		return false
	}
	return d.core.Dirty()
}

// Discard drops every edit, resetting the working copy to the baseline and
// re-applying transient seeds.
func (d *Draft) Discard() error {
	if d == nil || d.core == nil {
		return fmt.Errorf("discard settings draft: draft is nil")
	}
	if err := d.core.Discard(); err != nil {
		return fmt.Errorf("discard settings draft for %q: %w", d.Path(), err)
	}
	return nil
}

// Commit first detects drift and fails closed if the destination changed. An
// existing semantically clean config is then a successful byte-preserving
// no-op, allowing Screen to emit SavedMsg for transient-only effects without
// discarding comments or formatting. A missing destination is still created;
// changed persisted settings delegate to config.SaveAtomic. Nothing is written
// on any error.
func (d *Draft) Commit() error {
	if d == nil || d.core == nil {
		return fmt.Errorf(
			"commit settings: draft is nil.\n" +
				"what: there is no editing session to commit.\n" +
				"where: settings.Draft.Commit.\n" +
				"means: no file was changed.\n" +
				"fix: open a draft with settings.NewDraft, then commit.")
	}
	current, err := os.ReadFile(d.Path())
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
				"fix: ensure the config path is readable and re-run the settings flow.", d.Path(), err)
	}
	if d.expectedExists {
		equal, err := d.core.Equal()
		if err != nil {
			return fmt.Errorf(
				"commit settings: compare persisted state for %q after drift validation: %w.\n"+
					"what: the draft could not determine whether config-backed settings changed.\n"+
					"why: baseline or working configuration could not be serialized for a semantic comparison.\n"+
					"where: settings.Draft.Commit clean-save decision.\n"+
					"when: after the exact-byte drift check and before any atomic replacement.\n"+
					"means: the existing config bytes, comments, ordering, and formatting remain unchanged.\n"+
					"fix: correct the unsupported configuration value, reopen the editor, and retry ctrl+s.",
				d.Path(), err)
		}
		if equal {
			return nil
		}
	}
	if err := config.SaveAtomic(d.Path(), d.Working()); err != nil {
		return err
	}
	written, err := os.ReadFile(d.Path())
	if err == nil {
		d.expectedBytes = written
		d.expectedExists = true
	}
	return nil
}

// cloneConfig deep-copies a config.Config through a YAML round-trip. Transient
// fields that are not in config.yaml are omitted and must be copied by the caller.
func cloneConfig(src *config.Config) (config.Config, error) {
	if src == nil {
		return config.Config{}, fmt.Errorf("clone config: source is nil")
	}
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

func (d *Draft) driftError(what string) error {
	return fmt.Errorf("%s.\n"+
		"why: committing would overwrite an edit made outside this flow.\n"+
		"where: settings.Draft.Commit.\n"+
		"means: nothing was written.\n"+
		"fix: re-run the settings flow to review the current file, then apply your changes again.",
		fmt.Sprintf(what, d.Path()))
}
