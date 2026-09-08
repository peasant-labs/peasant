package main

import (
	"bytes"
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"gopkg.in/yaml.v3"
)

func TestDryRunCommandsPreserveExistingFiles(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "managed")
	native := filepath.Join(directory, "native")
	dbPath := defaults.ResolveDBFilePathWith(directory).String()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sessions, _ := LoadHarvestIndexSelectionFixtures(t)
	session := sessions[0]
	seedHarvestIndexSession(t, db, output, session, make(map[string][]byte))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	configPath := writeTestConfigFile(t, directory)
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Output.BasePath = output
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	writeTestCredentialsFor(t, directory, "http://127.0.0.1:1")
	if err := os.MkdirAll(native, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(native, string(session.ID)+".jsonl"), []byte(session.Transcript), 0600); err != nil {
		t.Fatal(err)
	}
	before := dryRunFileState(t, directory)
	run := func(args ...string) string {
		t.Helper()
		root := buildRootCommand()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(append([]string{"--config", configPath, "--data-dir", directory, "--config-dir", directory, "--state-dir", directory}, args...))
		if err := root.Execute(); err != nil {
			t.Fatalf("dry-run %v: %v\n%s", args, err, &out)
		}
		if after := dryRunFileState(t, directory); !reflect.DeepEqual(before, after) {
			t.Fatalf("dry-run %v changed source, managed, database, sidecar or config files", args)
		}
		return out.String()
	}
	run("harvest", "--dry-run", "--source-harness", "claude-code", "--source-path", native, "--output", output, "--include-active", "--json")
	forecast := run("harvest", "index", "--dry-run", "--output", output, "--session", string(session.ID), "--json")
	if !strings.Contains(forecast, string(session.ID)) {
		t.Fatalf("dry-run lost the stored stale-session forecast: %s", forecast)
	}
	run("village", "push", "--dry-run", "--timing", "--json")

	// An active WAL is a prerequisite, never silently omitted from the view.
	db, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	before = dryRunFileState(t, directory)
	if readOnly, err := store.OpenReadOnly(dbPath); err == nil {
		readOnly.Close()
		t.Fatal("dry-run accepted active WAL state")
	}
	if after := dryRunFileState(t, directory); !reflect.DeepEqual(before, after) {
		t.Fatal("refusing active WAL changed existing files")
	}
}

func dryRunFileState(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	state := make(map[string][32]byte)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			state[path] = sha256.Sum256([]byte("directory"))
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		state[path] = sha256.Sum256(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}
