package e2e

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// secretPattern is a high-confidence secret/PII signature. Patterns are precise
// (anchored lengths) to avoid false positives on legitimate fixture content
// (e.g. "task-id" must NOT trip the OpenAI "sk-" rule).
type secretPattern struct {
	name string
	re   *regexp.Regexp
}

var secretPatterns = []secretPattern{
	{"github-token", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`)},
	{"aws-access-key", regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{"openai-key", regexp.MustCompile(`sk-[A-Za-z0-9]{20,}`)},
	{"slack-token", regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"google-api-key", regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`)},
	{"private-key-header", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	{"email", regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)},
}

// homePathRe finds unix/mac home paths; the gate fails on any home directory
// other than the synthetic "user" (i.e. a leaked real username).
var (
	homePathRe    = regexp.MustCompile(`/home/([A-Za-z0-9_.-]+)`)
	macHomePathRe = regexp.MustCompile(`/Users/([A-Za-z0-9_.-]+)`)
)

// testdataRoot is the directory holding ALL committed e2e fixtures
// (claude-fixture/, codex-fixture/, and any future fixture). The always-on gates
// walk this root so a new fixture is covered automatically — zero per-fixture
// test edits (PROPOSAL D3). It is the parent of FixtureSourcePath (claude-fixture).
func testdataRoot() string {
	return filepath.Dir(FixtureSourcePath())
}

// TestFixture_NoSecrets is the ALWAYS-ON, level-independent gate (runs in
// `make check`): it scans EVERY committed fixture file under testdata/ for
// secrets/PII. The fixtures are committed data, so this is the standing
// guarantee that no secret ever lands in the repo through them. It is
// intentionally NOT build-tagged.
func TestFixture_NoSecrets(t *testing.T) {
	indexes := loadFixtureIndexes(t)
	files := collectFixtureFiles(t, indexes)
	if len(files) == 0 {
		t.Fatal("no-secrets gate found no fixture files — fixtures missing?")
	}

	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read fixture %s: %v", path, err)
		}
		text := string(data)

		// Resolve the owning fixture index. Top-level flat fixtures (directly
		// under testdataRoot(), with no fixture-index.yaml — e.g.
		// legacy-raw-jsonl.transcript.jsonl) are owned by no index; they are
		// synthetic, so NO real home path is allowed in them at all (allowedHome
		// stays "").
		m := indexForPath(indexes, path)
		var rel, allowedHome string
		if m != nil {
			rel = mustRel(m.Root, path)
			allowedHome = strings.TrimPrefix(m.Pinned.ScrubbedHome, "/home/")
			if allowedHome == "" || strings.Contains(allowedHome, "/") {
				t.Fatalf("%s: pinned.scrubbed_home must be a single /home/<user> path, got %q", m.Path, m.Pinned.ScrubbedHome)
			}
		} else {
			rel = mustRel(testdataRoot(), path)
		}

		for _, p := range secretPatterns {
			if match := p.re.FindString(text); match != "" {
				t.Errorf("SECRET/PII leak in fixture %s: matched %s pattern: %q", rel, p.name, redactForLog(match))
			}
		}
		for _, hm := range homePathRe.FindAllStringSubmatch(text, -1) {
			if hm[1] != allowedHome {
				if allowedHome == "" {
					t.Errorf("real home path in flat fixture %s: /home/%s (no real home path allowed)", rel, hm[1])
				} else {
					t.Errorf("real home path in fixture %s: /home/%s (only /home/%s is allowed)", rel, hm[1], allowedHome)
				}
			}
		}
		for _, hm := range macHomePathRe.FindAllStringSubmatch(text, -1) {
			t.Errorf("mac home path in fixture %s: /Users/%s (real username leak)", rel, hm[1])
		}
	}
}

// TestFixture_StructureSanity verifies each committed fixture still has the shape
// the harness depends on. Each immediate fixture directory must carry a
// fixture-index.yaml; adding another provider fixture is therefore data-first.
func TestFixture_StructureSanity(t *testing.T) {
	for _, m := range loadFixtureIndexes(t) {
		assertFixtureShape(t, m.Path)
	}
}

// TestNoSecretsGate_DetectsKnownSecrets proves the gate is NOT vacuous: each
// pattern matches a representative planted secret, and the legitimate fixture
// token "task-id" does NOT trip the OpenAI rule. Guards against a regex regression
// silently disabling the security gate.
func TestNoSecretsGate_DetectsKnownSecrets(t *testing.T) {
	mustMatch := map[string]string{
		"github-token":       "ghp_0123456789abcdef0123456789abcdef0123",
		"aws-access-key":     "AKIAIOSFODNN7EXAMPLE",
		"openai-key":         "sk-abcdefghijklmnopqrstuvwxyz0123",
		"private-key-header": "-----BEGIN RSA PRIVATE KEY-----",
		"jwt":                "eyJhbGciOiJIUzI1NiationhdGExMjM0.eyJzdWIiOiIxMjM0NTY3.abcDEFghiJKLmnoPQRstuv",
		"email":              "someone@example.com",
	}
	byName := make(map[string]*regexp.Regexp, len(secretPatterns))
	for _, p := range secretPatterns {
		byName[p.name] = p.re
	}
	for name, sample := range mustMatch {
		re, ok := byName[name]
		if !ok {
			t.Errorf("pattern %q missing from gate", name)
			continue
		}
		if !re.MatchString(sample) {
			t.Errorf("gate pattern %q failed to match planted secret %q", name, sample)
		}
	}
	// Negative: a legit fixture token must NOT match the OpenAI rule.
	if byName["openai-key"].MatchString("use the --task-id flag") {
		t.Error("openai-key pattern false-positives on 'task-id'")
	}
}

func collectFixtureFiles(t *testing.T, indexes []*fixtureIndex) []string {
	t.Helper()
	var files []string
	for _, m := range indexes {
		err := filepath.WalkDir(m.Root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk fixture dir %s: %v", m.Root, err)
		}
	}
	// ALSO scan top-level FLAT fixture files directly under testdataRoot() that
	// live outside any index Root (e.g. legacy-raw-jsonl.transcript.jsonl, a
	// committed source fixture for the production-encrypted migrate-on-read
	// path). Index Roots are subdirectories, so flat files never overlap them and
	// cannot be scanned twice. Restricted to *.json/*.jsonl so unrelated or binary
	// files are not pulled in.
	root := testdataRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read testdata root %s: %v", root, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if name := e.Name(); strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".jsonl") {
			files = append(files, filepath.Join(root, name))
		}
	}
	return files
}

// indexForPath returns the fixture index whose Root owns path, or nil for a
// top-level FLAT fixture file (directly under testdataRoot(), owned by no index).
func indexForPath(indexes []*fixtureIndex, path string) *fixtureIndex {
	for _, m := range indexes {
		if path == m.Root || strings.HasPrefix(path, m.Root+string(filepath.Separator)) {
			return m
		}
	}
	return nil
}

func mustRel(base, path string) string {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}
	return rel
}

// redactForLog truncates a matched secret so the gate's own failure message does
// not echo the full value into CI logs.
func redactForLog(s string) string {
	if len(s) <= 8 {
		return s[:len(s)/2] + "…"
	}
	return s[:8] + "…"
}
