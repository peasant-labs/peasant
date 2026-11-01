package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// verifySeed is a real harvest of one synthetic Claude session under a
// test root, for the `harvest verify --content` damage tests.
type verifySeed struct {
	dir, configPath, sessionID string
}

func seedVerifyHarvest(t *testing.T) verifySeed {
	t.Helper()
	dir := t.TempDir()
	projects := t.TempDir()
	const id = "aaaaaaaa-0000-4000-8000-000000000682"
	var lines []string
	for i := range 6 {
		lines = append(lines,
			fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"request %d"},"uuid":"u-%d","sessionId":%q,"timestamp":"2026-05-30T04:%02d:00.000Z","cwd":"/tmp/proj","version":"1.0","userType":"external"}`, i, i, id, i),
			fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","model":"claude-test","content":[{"type":"text","text":"response %d"}]},"uuid":"a-%d","parentUuid":"u-%d","sessionId":%q,"timestamp":"2026-05-30T04:%02d:30.000Z","cwd":"/tmp/proj","version":"1.0"}`, i, i, i, id, i))
	}
	file := filepath.Join(projects, "-tmp-proj", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := fmt.Sprintf("version: 1\nsources:\n  claude-code:\n    enabled: true\n    paths:\n      - %s\n  opencode: {enabled: false}\n  cursor: {enabled: false}\n  codex: {enabled: false}\n  strike: {enabled: false}\n  pi: {enabled: false}\noutput:\n  basePath: %s\n", projects, filepath.Join(dir, "out"))
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	seed := verifySeed{dir: dir, configPath: configPath, sessionID: id}
	if out, err := seed.run(t); err != nil {
		t.Fatalf("seed harvest: %v\n%s", err, out)
	}
	if out, err := seed.run(t, "verify", "--content"); err != nil || !strings.Contains(out, "Status: PASSED") {
		t.Fatalf("precondition: verify --content on a clean harvest: %v\n%s", err, out)
	}
	return seed
}

func (s verifySeed) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return executeHarvestCmd(t, s.dir, append([]string{"--config=" + s.configPath}, args...))
}

func (s verifySeed) exec(t *testing.T, script string) {
	t.Helper()
	conn, err := sqlite.OpenConn(string(defaults.ResolveDBFilePathWith(s.dir)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := sqlitex.ExecuteScript(conn, script, nil); err != nil {
		t.Fatal(err)
	}
}
