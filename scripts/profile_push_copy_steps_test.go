package scripts_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// A profileStep is one filesystem action or check, held as data in the
// profile-script fixtures. The test performs a case's setup steps itself before
// it runs the production script, and the case's child steps run in the command
// the script profiles (this test binary, re-executed; see TestMain). No step is
// shell text.
//
// Paths are relative to the case root unless they start with one of the
// placeholders the profiled command receives from the script: {work}
// ($PROFILE_WORK), {summary} ($PROFILE_SUMMARY), {profile} ($PROFILE_JSON) and
// {trace} ($PROFILE_TRACE).
type profileStep struct {
	Op      profileStepOp `yaml:"op"`
	Path    string        `yaml:"path"`
	To      string        `yaml:"to"`
	Content *string       `yaml:"content"`
	Mode    *uint32       `yaml:"mode"`
	Value   *string       `yaml:"value"`
	Status  *int          `yaml:"status"`
}

type profileStepOp string

const (
	// mkdir path [mode]: create one directory; mode, when given, is exact.
	profileOpMkdir profileStepOp = "mkdir"
	// write path content: create or truncate a file and write content.
	profileOpWrite profileStepOp = "write"
	// symlink path to: make path a symbolic link to the absolute form of to.
	profileOpSymlink profileStepOp = "symlink"
	// hardlink path to: make path a second name for the file at to.
	profileOpHardlink profileStepOp = "hardlink"
	// rename path to: move path to to.
	profileOpRename profileStepOp = "rename"
	// removeAll path: remove path and everything below it.
	profileOpRemoveAll profileStepOp = "removeAll"
	// expectDir path: fail unless path is a directory.
	profileOpExpectDir profileStepOp = "expectDir"
	// expectSamePath path to: fail unless both resolve to the same path text.
	profileOpExpectSamePath profileStepOp = "expectSamePath"
	// expectArg value: fail unless the command's first argument is value.
	profileOpExpectArg profileStepOp = "expectArg"
	// exit status: end the command with status; must be the last step.
	profileOpExit profileStepOp = "exit"
)

// profileStepFields is the closed set of ops and the fields each one takes.
var profileStepFields = map[profileStepOp][]string{
	profileOpMkdir:          {"path", "mode?"},
	profileOpWrite:          {"path", "content"},
	profileOpSymlink:        {"path", "to"},
	profileOpHardlink:       {"path", "to"},
	profileOpRename:         {"path", "to"},
	profileOpRemoveAll:      {"path"},
	profileOpExpectDir:      {"path"},
	profileOpExpectSamePath: {"path", "to"},
	profileOpExpectArg:      {"value"},
	profileOpExit:           {"status"},
}

// profilePlaceholders maps each path placeholder to the environment variable
// the production script exports to the command it profiles.
var profilePlaceholders = map[string]string{
	"{work}":    "PROFILE_WORK",
	"{summary}": "PROFILE_SUMMARY",
	"{profile}": "PROFILE_JSON",
	"{trace}":   "PROFILE_TRACE",
}

// validateProfileSteps rejects a step list the runner could misread: an
// unknown op, a field the op does not take, a missing field, or an exit that is
// not last. where names the fixture case and list for the message.
func validateProfileSteps(steps []profileStep, where string) error {
	for i, step := range steps {
		allowed, known := profileStepFields[step.Op]
		if !known {
			return fmt.Errorf("%s step %d has op %q, which is not one of the profile-step ops; use one of %s (see profile_push_copy_steps_test.go)", where, i+1, step.Op, strings.Join(profileStepOpNames(), ", "))
		}
		given := map[string]bool{"path": step.Path != "", "to": step.To != "", "content": step.Content != nil, "mode": step.Mode != nil, "value": step.Value != nil, "status": step.Status != nil}
		takes := map[string]bool{}
		for _, field := range allowed {
			name, optional := strings.CutSuffix(field, "?")
			takes[name] = true
			if !optional && !given[name] {
				return fmt.Errorf("%s step %d (%s) is missing %q; op %s takes %v", where, i+1, step.Op, name, step.Op, allowed)
			}
		}
		for name, present := range given {
			if present && !takes[name] {
				return fmt.Errorf("%s step %d (%s) sets %q, which op %s does not take (it takes %v); remove the field or change the op", where, i+1, step.Op, name, step.Op, allowed)
			}
		}
		if step.Op == profileOpExit && i != len(steps)-1 {
			return fmt.Errorf("%s step %d is exit but %d steps follow it and would never run; make exit the last step", where, i+1, len(steps)-1-i)
		}
	}
	return nil
}

func profileStepOpNames() []string {
	names := make([]string, 0, len(profileStepFields))
	for op := range profileStepFields {
		names = append(names, string(op))
	}
	slices.Sort(names)
	return names
}

// profileStepRunner performs steps below root. lookup supplies the placeholder
// variables (nil during setup, when no placeholder is defined yet) and args the
// command's arguments.
type profileStepRunner struct {
	root   string
	lookup func(string) (string, bool)
	args   []string
}

// errProfileExit carries the status an exit step ends the command with.
type errProfileExit struct{ status int }

func (e errProfileExit) Error() string { return fmt.Sprintf("exit %d", e.status) }

func (r profileStepRunner) resolve(path string) (string, error) {
	for placeholder, variable := range profilePlaceholders {
		rest, found := strings.CutPrefix(path, placeholder)
		if !found {
			continue
		}
		if r.lookup == nil {
			return "", fmt.Errorf("path %q uses %s, which only the profiled command receives; setup steps take paths relative to the case root", path, placeholder)
		}
		value, ok := r.lookup(variable)
		if !ok || value == "" {
			return "", fmt.Errorf("path %q uses %s but %s is unset or empty in the profiled command; pass the matching option to the script in this case", path, placeholder, variable)
		}
		return value + rest, nil
	}
	if strings.HasPrefix(path, "{") || filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q is neither relative to the case root nor led by one of the placeholders %v", path, placeholderNames())
	}
	return filepath.Join(r.root, path), nil
}

func placeholderNames() []string {
	names := make([]string, 0, len(profilePlaceholders))
	for name := range profilePlaceholders {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// run performs steps in order. An exit step returns errProfileExit.
func (r profileStepRunner) run(steps []profileStep) error {
	for i, step := range steps {
		if err := r.step(step); err != nil {
			var exit errProfileExit
			if errors.As(err, &exit) {
				return err
			}
			return fmt.Errorf("step %d (%s %s): %w", i+1, step.Op, step.Path, err)
		}
	}
	return nil
}

func (r profileStepRunner) step(step profileStep) error {
	if step.Op == profileOpExit {
		return errProfileExit{status: *step.Status}
	}
	if step.Op == profileOpExpectArg {
		if len(r.args) == 0 || r.args[0] != *step.Value {
			return fmt.Errorf("the profiled command's arguments are %q, want the first to be %q; the script must pass its trailing arguments through unchanged", r.args, *step.Value)
		}
		return nil
	}
	path, err := r.resolve(step.Path)
	if err != nil {
		return err
	}
	var to string
	if step.To != "" {
		if to, err = r.resolve(step.To); err != nil {
			return err
		}
	}
	switch step.Op {
	case profileOpMkdir:
		if err := os.Mkdir(path, 0o777); err != nil {
			return err
		}
		if step.Mode != nil {
			return os.Chmod(path, fs.FileMode(*step.Mode))
		}
		return nil
	case profileOpWrite:
		return os.WriteFile(path, []byte(*step.Content), 0o666)
	case profileOpSymlink:
		return os.Symlink(to, path)
	case profileOpHardlink:
		return os.Link(to, path)
	case profileOpRename:
		return os.Rename(path, to)
	case profileOpRemoveAll:
		return os.RemoveAll(path)
	case profileOpExpectDir:
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("%s is not a directory (%v); the script must create it before it runs the command", path, err)
		}
		return nil
	case profileOpExpectSamePath:
		if path != to {
			return fmt.Errorf("%s resolves to %q, want %q", step.Path, path, to)
		}
		return nil
	}
	return fmt.Errorf("op %q has no runner; add it to profileStepRunner.step", step.Op)
}
