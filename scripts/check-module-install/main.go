// check-module-install verifies the documented go install @version distribution.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/mod/module"
	modulezip "golang.org/x/mod/zip"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/cases.yaml
var fixtureBytes []byte

const modulePath = "github.com/peasant-labs/peasant"
const driverPath = "third_party/zombiezen-sqlite/"

type installCase struct {
	Name          string `yaml:"name"`
	Mode          string `yaml:"mode"`
	Succeeds      bool   `yaml:"succeeds"`
	ErrorContains string `yaml:"error_contains"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func loadCases() ([]installCase, error) {
	var fixture struct {
		Cases []installCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(fixtureBytes))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing installation fixture: %v", err)
	}
	required := map[string]string{
		"the audited driver is installed from the real module zip": "complete",
		"an omitted audited driver cannot install":                 "omit-driver",
	}
	seen := make(map[string]bool)
	for _, c := range fixture.Cases {
		mode, ok := required[c.Name]
		if !ok || seen[c.Name] || c.Mode != mode {
			return nil, fmt.Errorf("invalid named module installation obligation %q", c.Name)
		}
		if c.Succeeds != (c.Mode == "complete") || (c.Mode == "omit-driver" && c.ErrorContains != "third_party/zombiezen-sqlite") {
			return nil, fmt.Errorf("invalid installation expectation %q", c.Name)
		}
		seen[c.Name] = true
	}
	for name := range required {
		if !seen[name] {
			return nil, fmt.Errorf("missing module installation fixture %q", name)
		}
	}
	return fixture.Cases, nil
}

func run() (result error) {
	cases, err := loadCases()
	if err != nil {
		return err
	}
	repo, err := os.Getwd()
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "peasant-module-install-*")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, os.RemoveAll(work)) }()
	source := filepath.Join(work, "source")
	if err := copyTrackedSource(repo, source); err != nil {
		return err
	}
	cacheResult, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		return err
	}
	originalCache := strings.TrimSpace(string(cacheResult))
	cache := filepath.Join(work, "cache")
	// Reuse the public checksum cache, without mutating the user's module cache.
	sumCache := filepath.Join(originalCache, "cache", "download", "sumdb")
	if _, err := os.Stat(sumCache); err == nil {
		if err := copyTree(sumCache, filepath.Join(cache, "cache", "download", "sumdb")); err != nil {
			return err
		}
	}
	proxy := filepath.Join(work, "proxy")
	if err := os.MkdirAll(proxy, 0o755); err != nil {
		return err
	}
	for _, c := range cases {
		if err := installOne(work, source, cache, originalCache, proxy, c); err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
		fmt.Println(c.Name + ": passed")
	}
	return nil
}

func copyTrackedSource(repo, dest string) error {
	command := exec.Command("git", "ls-files", "-z")
	command.Dir = repo
	names, err := command.Output()
	if err != nil {
		return err
	}
	for _, name := range strings.Split(string(names), "\x00") {
		if name == "" {
			continue
		}
		source := filepath.Join(repo, name)
		info, err := os.Lstat(source)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(source)
			if err != nil {
				return err
			}
			destPath := filepath.Join(dest, name)
			if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(target, destPath); err != nil {
				return err
			}
			continue // The real Go module zipper omits symbolic links.
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("module source must be a regular tracked file: %s", name)
		}
		if err := copyFile(source, filepath.Join(dest, name)); err != nil {
			return err
		}
	}
	return nil
}

func copyTree(source, dest string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("checksum cache entry is not regular: %s", path)
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		return copyFile(path, filepath.Join(dest, relative))
	})
}

func copyFile(source, dest string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	output, err := os.Create(dest)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func installOne(work, source, cache, originalCache, proxy string, c installCase) error {
	digest := sha256.Sum256([]byte(work + c.Name))
	version := fmt.Sprintf("v0.0.0-20261001000000-%x", digest[:6])
	mod := module.Version{Path: modulePath, Version: version}
	directory := filepath.Join(proxy, filepath.FromSlash(modulePath), "@v")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	zipPath := filepath.Join(directory, version+".zip")
	output, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	zipErr := modulezip.CreateFromDir(output, mod, source)
	closeErr := output.Close()
	if zipErr != nil {
		return zipErr
	}
	if closeErr != nil {
		return closeErr
	}
	if c.Mode == "omit-driver" {
		if err := omitDriver(zipPath, mod); err != nil {
			return err
		}
	}
	archive, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	driverPresent := false
	for _, file := range archive.File {
		if file.Name == modulePath+"@"+version+"/"+driverPath+"sqlite.go" {
			driverPresent = true
		}
	}
	if err := archive.Close(); err != nil {
		return err
	}
	if driverPresent != (c.Mode == "complete") {
		return fmt.Errorf("real module zip driver presence=%t, expected mode=%s", driverPresent, c.Mode)
	}
	if _, err := modulezip.CheckZip(mod, zipPath); err != nil {
		return err
	}
	info, err := json.Marshal(struct {
		Version string
		Time    time.Time
	}{version, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, version+".info"), info, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, "list"), []byte(version+"\n"), 0o644); err != nil {
		return err
	}
	if err := copyFile(filepath.Join(source, "go.mod"), filepath.Join(directory, version+".mod")); err != nil {
		return err
	}
	bin := filepath.Join(work, "bin-"+c.Mode)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "install", modulePath+"/cmd/peasant@"+version)
	command.Dir = work
	// Kill the owned installer process group on timeout, including compiler
	// children, before removing its temporary module cache and binary directory.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = 5 * time.Second
	oldProxy := os.Getenv("GOPROXY")
	if oldProxy == "" {
		oldProxy = "https://proxy.golang.org,direct"
	}
	proxyValue := "file://" + filepath.ToSlash(proxy) + ",file://" + filepath.ToSlash(filepath.Join(originalCache, "cache", "download"))
	if c.Mode == "complete" {
		proxyValue += "," + oldProxy
	}
	command.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0", "GOFLAGS="+strings.TrimSpace(os.Getenv("GOFLAGS")+" -modcacherw"), "GOMODCACHE="+cache, "GOBIN="+bin, "GOPROXY="+proxyValue, "GONOSUMDB="+modulePath)
	result, installErr := command.CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("installation budget exhausted: %w", ctx.Err())
	}
	if c.Succeeds {
		if installErr != nil {
			return fmt.Errorf("go install failed: %w\n%s", installErr, result)
		}
		executable, err := os.Stat(filepath.Join(bin, "peasant"))
		if err != nil {
			return err
		}
		if !executable.Mode().IsRegular() || executable.Size() == 0 {
			return fmt.Errorf("go install did not produce the CLI")
		}
	} else if installErr == nil || !strings.Contains(string(result), c.ErrorContains) {
		return fmt.Errorf("omitted-driver install did not fail for the required reason: %v\n%s", installErr, result)
	}
	return nil
}

func omitDriver(path string, mod module.Version) (result error) {
	source, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, source.Close()) }()
	temp := path + ".omitted"
	file, err := os.Create(temp)
	if err != nil {
		return err
	}
	fileClosed := false
	defer func() {
		if !fileClosed {
			result = errors.Join(result, file.Close())
		}
	}()
	target := zip.NewWriter(file)
	targetClosed := false
	defer func() {
		if !targetClosed {
			result = errors.Join(result, target.Close())
		}
	}()
	prefix := mod.Path + "@" + mod.Version + "/" + driverPath
	for _, entry := range source.File {
		if strings.HasPrefix(entry.Name, prefix) {
			continue
		}
		input, err := entry.Open()
		if err != nil {
			return err
		}
		output, err := target.CreateHeader(&entry.FileHeader)
		if err != nil {
			return errors.Join(err, input.Close())
		}
		_, copyErr := io.Copy(output, input)
		if err := errors.Join(copyErr, input.Close()); err != nil {
			return err
		}
	}
	// Finalize and close both outputs before renaming.
	targetClosed = true
	if err := target.Close(); err != nil {
		return err
	}
	fileClosed = true
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temp, path)
}
