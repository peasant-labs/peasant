package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/cli/migrate_interrupt.yaml
var migrateInterruptYAML []byte

//go:embed testdata/cli/migrate_interrupt.manifest.yaml
var migrateInterruptManifestYAML []byte

type migrateInterruptCase struct {
	Name         string   `yaml:"name"`
	Mode         string   `yaml:"mode"`
	JSON         bool     `yaml:"json"`
	TextContains []string `yaml:"textContains"`
}

func loadMigrateInterruptFixtures(t *testing.T) []migrateInterruptCase {
	t.Helper()
	var fixture struct {
		Cases []migrateInterruptCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(migrateInterruptYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(migrateInterruptManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool)
	for _, c := range fixture.Cases {
		if c.Name == "" || present[c.Name] {
			t.Fatal("blank or duplicate name")
		}
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("migrate_interrupt", "case", manifest.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

type interruptProgressWriter struct {
	bytes.Buffer
	fired     bool
	interrupt func()
}

func (w *interruptProgressWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if !w.fired && strings.Contains(string(p), "phase convert-sessions: 1/1") {
		w.fired = true
		w.interrupt()
	}
	return n, err
}

func runMigrateInterruptCase(t *testing.T, c migrateInterruptCase, deliver func() error) {
	t.Helper()
	dir := t.TempDir()
	owned := filepath.Join(dir, "sync")
	seedMigrateCmdStore(t, dir, owned)
	config := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(config, []byte("version: 1\noutput:\n  basePath: "+owned+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	root := &cobra.Command{Use: "peasant"}
	root.PersistentFlags().String("config", "", "")
	root.PersistentFlags().String("data-dir", "", "")
	root.PersistentFlags().String("config-dir", "", "")
	cmd := BuildMigrateCommand()
	root.AddCommand(cmd)
	writer := &interruptProgressWriter{interrupt: func() {
		if deliver == nil {
			cancel()
		} else if err := deliver(); err != nil {
			t.Fatal(err)
		}
		// The production command's NotifyContext is the wake source. If
		// signal registration is removed, the SIGINT case cannot reach it.
		select {
		case <-cmd.Context().Done():
		case <-time.After(2 * time.Second):
			t.Fatal("interrupt did not cancel the mounted migration context")
		}
	}}
	var out bytes.Buffer
	root.SetOut(writer)
	root.SetErr(writer)
	args := []string{"migrate", "--confirm", "--data-dir", dir, "--config-dir", dir, "--config", config}
	if c.JSON {
		args = append(args, "--json")
		root.SetOut(&out)
	}
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if !writer.fired {
		t.Fatalf("conversion progress never reached cancellation boundary: %s", writer.String())
	}
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "peasant migrate --confirm") {
		t.Fatalf("interrupt error lacks cancel/resume: %v", err)
	}
	text := writer.String()
	if c.JSON {
		text = out.String()
	}
	for _, want := range c.TextContains {
		if !strings.Contains(text, want) {
			t.Errorf("partial report misses %q: %s", want, text)
		}
	}
	// A second real invocation resumes rather than re-converting the row.
	resumed, _, err := executeMigrateCmd(t, dir, owned, []string{"--confirm", "--json"})
	if err != nil || !strings.Contains(resumed, `"converted":0`) {
		t.Fatalf("resume=%s err=%v", resumed, err)
	}
}

func TestMigrateInterruptPartialReport(t *testing.T) {
	for _, c := range loadMigrateInterruptFixtures(t) {
		if c.Mode == "sigint" {
			continue
		} // the platform-tagged runner owns SIGINT
		if c.Mode != "context" {
			t.Fatalf("unknown interrupt mode %s", c.Mode)
		}
		t.Run(c.Name, func(t *testing.T) {
			runMigrateInterruptCase(t, c, nil)
		})
	}
}
