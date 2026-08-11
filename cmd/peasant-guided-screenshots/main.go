//go:build guided_screenshots

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const screenshotCommandTimeout = 90 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("peasant-guided-screenshots", flag.ContinueOnError)
	flags.SetOutput(stderr)
	allowDirty := flags.Bool("allow-dirty", false, "generate development evidence from a dirty worktree")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "peasant guided screenshots: unexpected arguments %q; fix: pass only --allow-dirty when intentionally capturing uncommitted work\n", flags.Args())
		return 2
	}

	repository, err := readRepositoryState()
	if err != nil {
		printActionable(stderr, "read the Peasant repository state", err, "run this command from a Peasant git worktree")
		return 1
	}
	if repository.dirty && !*allowDirty {
		printActionable(
			stderr,
			"refuse to label uncommitted code as commit evidence",
			fmt.Errorf("the Peasant worktree has tracked or untracked changes"),
			"commit the intended changes first, or pass --allow-dirty for disposable development evidence",
		)
		return 1
	}
	document, err := decodeCaptureDocument(captureFixtureData)
	if err != nil {
		printActionable(stderr, "load the guided screenshot fixture", err, "correct cmd/peasant-guided-screenshots/testdata/captures.yaml and rerun")
		return 1
	}
	sheets, err := renderSheets(document)
	if err != nil {
		printActionable(stderr, "render the mounted guided TUI states", err, "fix the fixture or mounted TUI regression, then rerun the manual harness")
		return 1
	}
	freezePath, err := findFreeze()
	if err != nil {
		printActionable(stderr, "locate the Freeze ANSI renderer", err, "enter `nix develop`, or set FREEZE_PATH to the Freeze executable")
		return 1
	}

	directoryName := "peasant-guided-final-" + repository.revision
	if repository.dirty {
		directoryName += "-dirty"
	}
	outputDirectory := filepath.Join(repository.root, "out", "test", "screenshots", directoryName)
	if err := os.MkdirAll(outputDirectory, 0o750); err != nil {
		printActionable(stderr, "create the screenshot output directory", err, "fix directory ownership or permissions and rerun")
		return 1
	}
	for _, sheet := range sheets {
		outputPath := filepath.Join(outputDirectory, string(sheet.fixture.Name)+".png")
		if err := rasterizeSheet(freezePath, outputPath, sheet); err != nil {
			printActionable(stderr, "rasterize the mounted ANSI contact sheet", err, "verify Freeze is available in `nix develop`, then rerun")
			return 1
		}
		fmt.Fprintln(stdout, outputPath)
	}
	return 0
}

type repositoryState struct {
	root     string
	revision string
	dirty    bool
}

func readRepositoryState() (repositoryState, error) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		return repositoryState{}, fmt.Errorf("read current directory: %w", err)
	}
	root, err := runGit(workingDirectory, "rev-parse", "--show-toplevel")
	if err != nil {
		return repositoryState{}, err
	}
	moduleData, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return repositoryState{}, fmt.Errorf("read Peasant module declaration at %q: %w", root, err)
	}
	if !bytes.HasPrefix(moduleData, []byte("module github.com/peasant-labs/peasant\n")) {
		return repositoryState{}, fmt.Errorf("git root %q is not the Peasant module", root)
	}
	revision, err := runGit(root, "rev-parse", "--short=8", "HEAD")
	if err != nil {
		return repositoryState{}, err
	}
	status, err := runGit(root, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return repositoryState{}, err
	}
	return repositoryState{root: root, revision: revision, dirty: status != ""}, nil
}

func runGit(directory string, arguments ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", append([]string{"-C", directory}, arguments...)...)
	output, err := command.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("git %s timed out after 10 seconds", strings.Join(arguments, " "))
	}
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func findFreeze() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("FREEZE_PATH")); configured != "" {
		path, err := exec.LookPath(configured)
		if err != nil {
			return "", fmt.Errorf("FREEZE_PATH=%q is not executable: %w", configured, err)
		}
		return path, nil
	}
	path, err := exec.LookPath("freeze")
	if err != nil {
		return "", fmt.Errorf("freeze is not on PATH: %w", err)
	}
	return path, nil
}

func rasterizeSheet(freezePath, outputPath string, sheet renderedSheet) error {
	outputDirectory := filepath.Dir(outputPath)
	ansiFile, err := os.CreateTemp(outputDirectory, "."+string(sheet.fixture.Name)+"-*.ansi")
	if err != nil {
		return fmt.Errorf("create temporary ANSI input for %q: %w", sheet.fixture.Name, err)
	}
	ansiPath := ansiFile.Name()
	defer os.Remove(ansiPath)
	if err := ansiFile.Chmod(0o600); err != nil {
		_ = ansiFile.Close()
		return fmt.Errorf("restrict temporary ANSI input %q: %w", ansiPath, err)
	}
	if _, err := ansiFile.WriteString(sheet.ansi); err != nil {
		_ = ansiFile.Close()
		return fmt.Errorf("write temporary ANSI input %q: %w", ansiPath, err)
	}
	if err := ansiFile.Close(); err != nil {
		return fmt.Errorf("close temporary ANSI input %q: %w", ansiPath, err)
	}

	pngFile, err := os.CreateTemp(outputDirectory, "."+string(sheet.fixture.Name)+"-*.png")
	if err != nil {
		return fmt.Errorf("reserve temporary PNG output for %q: %w", sheet.fixture.Name, err)
	}
	temporaryPNG := pngFile.Name()
	if err := pngFile.Close(); err != nil {
		return fmt.Errorf("close temporary PNG reservation %q: %w", temporaryPNG, err)
	}
	defer os.Remove(temporaryPNG)

	ctx, cancel := context.WithTimeout(context.Background(), screenshotCommandTimeout)
	defer cancel()
	command := exec.CommandContext(
		ctx,
		freezePath,
		ansiPath,
		"--config", "base",
		"--output", temporaryPNG,
		"--width", fmt.Sprint(sheet.fixture.Viewport.Width),
		"--height", fmt.Sprint(sheet.fixture.Viewport.Height),
		"--background", "#171614",
		"--padding", "28",
		"--margin", "0",
		"--border.radius", "0",
		"--border.width", "0",
		"--shadow.blur", "0",
		"--font.family", "JetBrains Mono",
		"--font.size", "11",
		"--line-height", "1.2",
		"--font.ligatures=false",
	)
	output, err := command.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("Freeze timed out after %s while rendering %q", screenshotCommandTimeout, sheet.fixture.Name)
	}
	if err != nil {
		return fmt.Errorf("Freeze render %q: %w: %s", sheet.fixture.Name, err, strings.TrimSpace(string(output)))
	}
	if err := validatePNG(temporaryPNG, sheet.fixture.Viewport); err != nil {
		return fmt.Errorf("validate Freeze output for %q: %w", sheet.fixture.Name, err)
	}
	if err := os.Rename(temporaryPNG, outputPath); err != nil {
		return fmt.Errorf("publish screenshot %q to %q: %w", sheet.fixture.Name, outputPath, err)
	}
	return nil
}

func validatePNG(path string, viewport viewportFixture) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open PNG %q: %w", path, err)
	}
	defer file.Close()
	configuration, err := png.DecodeConfig(file)
	if err != nil {
		return fmt.Errorf("decode PNG header %q: %w", path, err)
	}
	if configuration.Width != viewport.Width || configuration.Height != viewport.Height {
		return fmt.Errorf("PNG dimensions=%dx%d, want %dx%d", configuration.Width, configuration.Height, viewport.Width, viewport.Height)
	}
	return nil
}

func printActionable(writer io.Writer, what string, cause error, fix string) {
	fmt.Fprintf(
		writer,
		"guided screenshot generation failed\nwhat: %s.\nwhy: %v.\nwhere: cmd/peasant-guided-screenshots.\nwhen: generating the manually requested TUI evidence set.\nmeans: the requested screenshot set is incomplete.\nfix: %s.\n",
		what,
		cause,
		fix,
	)
}
