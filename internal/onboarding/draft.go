package onboarding

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
)

// Draft is the in-memory baseline and working pair for a setup session.
// Transient seeds are values that participate in the session but are not
// written to config.yaml. Discard and Rebuild copy the baseline and apply
// those seeds again. This type does not write a file.
type Draft struct {
	path     string
	baseline config.Config
	working  config.Config
	seeds    []func(*config.Config)
}

// OpenDraft copies loaded into an independent baseline and working pair.
// loaded is not mutated. path is recorded for error text; the file is not read.
func OpenDraft(path string, loaded *config.Config) (*Draft, error) {
	if path == "" {
		return nil, fmt.Errorf(
			"open setup draft: destination path is empty.\n" +
				"what: no config path was provided to edit.\n" +
				"where: onboarding.OpenDraft.\n" +
				"fix: pass the resolved config path the session should commit to.")
	}
	if loaded == nil {
		return nil, fmt.Errorf(
			"open setup draft for %q: loaded configuration is nil.\n"+
				"what: there is no baseline config to copy.\n"+
				"where: onboarding.OpenDraft.\n"+
				"fix: load the config before opening a draft.", path)
	}
	baseline, err := cloneDraftConfig(loaded)
	if err != nil {
		return nil, fmt.Errorf("open setup draft for %q: copy baseline: %w", path, err)
	}
	working, err := cloneDraftConfig(loaded)
	if err != nil {
		return nil, fmt.Errorf("open setup draft for %q: copy working set: %w", path, err)
	}
	return &Draft{path: path, baseline: baseline, working: working}, nil
}

// memoryDraftPath is the destination of a draft that has no file. A commit
// refuses it, so an in-memory draft can never write a file named for it.
const memoryDraftPath = "memory"

// DraftFrom builds a draft over two already-copied configurations. It is the
// in-memory form used when a caller probes field writes without a file.
func DraftFrom(baseline, working config.Config) *Draft {
	return &Draft{path: memoryDraftPath, baseline: baseline, working: working}
}

// Path is the destination recorded for the draft.
func (d *Draft) Path() string {
	if d == nil {
		return ""
	}
	return d.path
}

// Working is the configuration the session edits.
func (d *Draft) Working() *config.Config {
	if d == nil {
		return nil
	}
	return &d.working
}

// Baseline is the configuration as it was when the draft opened, plus any
// transient seeds applied since.
func (d *Draft) Baseline() *config.Config {
	if d == nil {
		return nil
	}
	return &d.baseline
}

// Dirty reports whether the working copy differs from the baseline. A
// comparison error is treated as dirty so a caller does not discard a value
// it could not prove equal.
func (d *Draft) Dirty() bool {
	equal, err := d.Equal()
	return err != nil || !equal
}

// Equal reports whether the persisted form of baseline and working match.
func (d *Draft) Equal() (bool, error) {
	if d == nil {
		return false, fmt.Errorf("compare setup draft: draft is nil")
	}
	return draftConfigsEqual(&d.baseline, &d.working)
}

// SeedTransient applies value to both copies and records it so Discard and
// Rebuild apply it again. apply must be non-nil and must not depend on the
// draft pointer.
func (d *Draft) SeedTransient(apply func(*config.Config)) error {
	if d == nil {
		return fmt.Errorf(
			"seed a transient setup value: draft is nil.\n" +
				"what: no setup draft was provided.\n" +
				"why: baseline and working state cannot be updated without an open draft.\n" +
				"where: onboarding.Draft.SeedTransient.\n" +
				"when: before fields mount.\n" +
				"means: no draft state was changed.\n" +
				"fix: open a draft, then seed the transient value.")
	}
	if apply == nil {
		return fmt.Errorf(
			"seed a transient setup value for %q: apply is nil.\n"+
				"what: the transient value has no setter.\n"+
				"why: a seed must write the same typed value into baseline and working.\n"+
				"where: onboarding.Draft.SeedTransient.\n"+
				"when: before the seed is recorded.\n"+
				"means: neither copy was changed.\n"+
				"fix: pass a function that sets the transient field on the given configuration.", d.path)
	}
	apply(&d.baseline)
	apply(&d.working)
	d.seeds = append(d.seeds, apply)
	return nil
}

// Discard drops every edit. The working copy becomes a fresh copy of the
// baseline, and every transient seed is applied again.
func (d *Draft) Discard() error {
	return d.Rebuild()
}

// Rebuild copies the baseline into the working configuration and applies
// transient seeds again. It does not write a file.
func (d *Draft) Rebuild() error {
	if d == nil {
		return fmt.Errorf(
			"rebuild setup draft: draft is nil.\n" +
				"what: there is no working copy to reset.\n" +
				"where: onboarding.Draft.Rebuild.\n" +
				"means: no configuration was changed.\n" +
				"fix: open a draft before rebuilding it.")
	}
	baseline, err := cloneDraftConfig(&d.baseline)
	if err != nil {
		return fmt.Errorf("rebuild setup draft for %q: %w", d.path, err)
	}
	for _, seed := range d.seeds {
		seed(&baseline)
	}
	d.working = baseline
	return nil
}

func draftConfigsEqual(left, right *config.Config) (bool, error) {
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

func cloneDraftConfig(src *config.Config) (config.Config, error) {
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
