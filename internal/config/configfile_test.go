package config

import (
	"context"
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/configfile.yaml
var configFileYAML []byte

type configFileWant struct {
	BytesNil               *bool  `yaml:"bytesNil"`
	DocumentEmpty          *bool  `yaml:"documentEmpty"`
	RedactionLevel         string `yaml:"redactionLevel"`
	ReadError              string `yaml:"readError"`
	SwapOK                 bool   `yaml:"swapOK"`
	SwapChanged            bool   `yaml:"swapChanged"`
	Fresh                  string `yaml:"fresh"`
	FreshNil               bool   `yaml:"freshNil"`
	SwapError              string `yaml:"swapError"`
	FileEquals             string `yaml:"fileEquals"`
	FileAbsent             bool   `yaml:"fileAbsent"`
	SecondRefusedWhileHeld bool   `yaml:"secondRefusedWhileHeld"`
	SecondOpensAfterClose  bool   `yaml:"secondOpensAfterClose"`
	CloseTwiceOK           bool   `yaml:"closeTwiceOK"`
}

type configFileConcurrent struct {
	Write  string `yaml:"write"`
	Remove bool   `yaml:"remove"`
}

type configFileCase struct {
	Name        string               `yaml:"name"`
	Scenario    string               `yaml:"scenario"`
	Absent      bool                 `yaml:"absent"`
	File        string               `yaml:"file"`
	Concurrent  configFileConcurrent `yaml:"concurrent"`
	Replacement string               `yaml:"replacement"`
	Want        configFileWant       `yaml:"want"`
}

type configFileFixture struct {
	RequiredNames []string         `yaml:"requiredNames"`
	Cases         []configFileCase `yaml:"cases"`
}

func TestConfigFile(t *testing.T) {
	var fixture configFileFixture
	if err := testutil.DecodeNamedFixtureYAML(configFileYAML, &fixture); err != nil {
		t.Fatalf("decode configfile.yaml: %v", err)
	}
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "peasant", "config.yaml")
			if !row.Absent {
				writeConfigFile(t, path, row.File)
			}
			switch row.Scenario {
			case "read":
				runConfigFileRead(t, path, row)
			case "swap":
				runConfigFileSwap(t, path, row)
			case "contend":
				runConfigFileContend(t, path, row)
			case "close-twice":
				runConfigFileCloseTwice(t, path, row)
			case "closed-handle":
				runConfigFileClosedHandle(t, path, row)
			default:
				t.Fatalf("row %s has unknown scenario %q", row.Name, row.Scenario)
			}
		})
	}
}

func writeConfigFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func openConfigFile(t *testing.T, path string) *ConfigFile {
	t.Helper()
	handle, err := OpenConfigFile(path, time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatalf("OpenConfigFile: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })
	return handle
}

func runConfigFileRead(t *testing.T, path string, row configFileCase) {
	t.Helper()
	handle := openConfigFile(t, path)
	data, doc, cfg, err := handle.Read(context.Background())
	if row.Want.ReadError != "" {
		if err == nil || !strings.Contains(err.Error(), row.Want.ReadError) {
			t.Fatalf("Read error = %v, want it to contain %q", err, row.Want.ReadError)
		}
		return
	}
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if row.Want.BytesNil != nil && (data == nil) != *row.Want.BytesNil {
		t.Errorf("Read bytes nil = %t, want %t", data == nil, *row.Want.BytesNil)
	}
	if row.Want.DocumentEmpty != nil && (doc.Kind == 0) != *row.Want.DocumentEmpty {
		t.Errorf("Read document empty = %t, want %t", doc.Kind == 0, *row.Want.DocumentEmpty)
	}
	if row.Want.RedactionLevel != "" && cfg.Redaction.Level.String() != row.Want.RedactionLevel {
		t.Errorf("redaction level = %s, want %s", cfg.Redaction.Level, row.Want.RedactionLevel)
	}
	if !row.Absent && string(data) != row.File {
		t.Errorf("Read bytes = %q, want %q", data, row.File)
	}
}

func runConfigFileSwap(t *testing.T, path string, row configFileCase) {
	t.Helper()
	handle := openConfigFile(t, path)
	data, _, _, err := handle.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	switch {
	case row.Concurrent.Remove:
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	case row.Concurrent.Write != "":
		writeConfigFile(t, path, row.Concurrent.Write)
	}
	swapErr := handle.Swap(data, []byte(row.Replacement))
	want := row.Want
	switch {
	case want.SwapOK:
		if swapErr != nil {
			t.Fatalf("Swap: %v", swapErr)
		}
	case want.SwapChanged:
		if !errors.Is(swapErr, ErrConfigChanged) {
			t.Fatalf("Swap error = %v, want ErrConfigChanged", swapErr)
		}
		var changed *ConfigChangedError
		if !errors.As(swapErr, &changed) {
			t.Fatalf("Swap error %T carries no fresh bytes", swapErr)
		}
		if want.FreshNil != (changed.Fresh == nil) {
			t.Errorf("fresh nil = %t, want %t", changed.Fresh == nil, want.FreshNil)
		}
		if !want.FreshNil && string(changed.Fresh) != want.Fresh {
			t.Errorf("fresh = %q, want %q", changed.Fresh, want.Fresh)
		}
	case want.SwapError != "":
		if swapErr == nil || !strings.Contains(swapErr.Error(), want.SwapError) {
			t.Fatalf("Swap error = %v, want it to contain %q", swapErr, want.SwapError)
		}
		if errors.Is(swapErr, ErrConfigChanged) {
			t.Fatalf("Swap error = %v, want a refusal that is not ErrConfigChanged", swapErr)
		}
	default:
		t.Fatalf("row %s states no swap expectation", row.Name)
	}
	assertConfigFile(t, path, want)
}

func assertConfigFile(t *testing.T, path string, want configFileWant) {
	t.Helper()
	got, err := os.ReadFile(path)
	if want.FileAbsent {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("file should be absent, read err = %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("read the file back: %v", err)
	}
	if want.FileEquals != "" && string(got) != want.FileEquals {
		t.Errorf("file = %q, want %q", got, want.FileEquals)
	}
}

func runConfigFileContend(t *testing.T, path string, row configFileCase) {
	t.Helper()
	first := openConfigFile(t, path)
	_, err := OpenConfigFile(path, time.Now().Add(75*time.Millisecond))
	if got := err != nil; got != row.Want.SecondRefusedWhileHeld {
		t.Fatalf("second handle refused while held = %t (err %v), want %t", got, err, row.Want.SecondRefusedWhileHeld)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	second, err := OpenConfigFile(path, time.Now().Add(5*time.Second))
	if (err == nil) != row.Want.SecondOpensAfterClose {
		t.Fatalf("second handle after Close err = %v, want success = %t", err, row.Want.SecondOpensAfterClose)
	}
	if second != nil {
		_ = second.Close()
	}
}

func runConfigFileCloseTwice(t *testing.T, path string, row configFileCase) {
	t.Helper()
	handle := openConfigFile(t, path)
	for i := 0; i < 2; i++ {
		if err := handle.Close(); (err == nil) != row.Want.CloseTwiceOK {
			t.Fatalf("Close #%d err = %v, want success = %t", i+1, err, row.Want.CloseTwiceOK)
		}
	}
	other, err := OpenConfigFile(path, time.Now().Add(5*time.Second))
	if (err == nil) != row.Want.SecondOpensAfterClose {
		t.Fatalf("handle after double Close err = %v, want success = %t", err, row.Want.SecondOpensAfterClose)
	}
	if other != nil {
		_ = other.Close()
	}
}

func runConfigFileClosedHandle(t *testing.T, path string, row configFileCase) {
	t.Helper()
	handle := openConfigFile(t, path)
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := handle.Read(context.Background()); err == nil || !strings.Contains(err.Error(), row.Want.ReadError) {
		t.Errorf("Read after Close error = %v, want it to contain %q", err, row.Want.ReadError)
	}
	if err := handle.Swap([]byte(row.File), []byte(row.File)); err == nil || !strings.Contains(err.Error(), row.Want.SwapError) {
		t.Errorf("Swap after Close error = %v, want it to contain %q", err, row.Want.SwapError)
	}
}
