// Command harvester-version-guard compares actual parsers at two Git revisions.
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

//go:embed probe_test.go.txt
var probe []byte

type versions struct{ AdapterVersion, IndexerVersion int }
type snapshot struct {
	Versions map[string]versions
	Outputs  map[string]json.RawMessage
}

func main() {
	base := flag.String("base", "", "exact reviewed base Git revision (required)")
	candidate := flag.String("candidate", "HEAD", "candidate Git revision; working edits are excluded")
	flag.Parse()
	if *base == "" {
		fail(fmt.Errorf("missing -base; select the reviewed base commit explicitly"))
	}
	if err := run(*base, *candidate); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "harvester version guard:", err)
	os.Exit(1)
}

func command(dir string, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %v in %s: %w\n%s\n%s", name, args, dir, err, stderr.Bytes(), out)
	}
	return out, nil
}

func run(base, candidate string) error {
	rootBytes, err := command("", "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	root := strings.TrimSpace(string(rootBytes))
	baseID, err := command(root, "git", "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return err
	}
	candidateID, err := command(root, "git", "rev-parse", "--verify", candidate+"^{commit}")
	if err != nil {
		return err
	}
	fmt.Printf("Comparing %s -> %s (committed trees only)\n", strings.TrimSpace(string(baseID)), strings.TrimSpace(string(candidateID)))
	temp, err := os.MkdirTemp("", "peasant-version-guard-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	baseTree, candidateTree := filepath.Join(temp, "base"), filepath.Join(temp, "candidate")
	for tree, id := range map[string]string{baseTree: string(baseID), candidateTree: string(candidateID)} {
		if err := os.Mkdir(tree, 0700); err != nil {
			return err
		}
		archive, err := command(root, "git", "archive", strings.TrimSpace(id))
		if err != nil {
			return err
		}
		cmd := exec.Command("tar", "-x", "-C", tree)
		cmd.Stdin = bytes.NewReader(archive)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("extract guarded revision: %w: %s", err, out)
		}
	}
	// Preserve both input corpora. Removed or edited fixtures cannot hide an old
	// behavior, and additions only demand a bump if the parsers actually differ.
	// Materialize native SQLite with one shared fixture builder, never with
	// two implementations that could construct different input databases.
	builder := filepath.Join(temp, "fixture-builder")
	if err := os.CopyFS(builder, os.DirFS(filepath.Join(candidateTree, "internal/ingest/testfixture"))); err != nil {
		return err
	}
	nativeCorpora := make(map[string][]byte)
	for _, corpus := range []string{baseTree, candidateTree} {
		nativeCorpora[corpus], err = os.ReadFile(filepath.Join(corpus, "internal/ingest/testfixture/testdata/opencode_sqlite.yaml"))
		if err != nil {
			return err
		}
	}
	var failures []string
	for _, corpus := range []string{baseTree, candidateTree} {
		old, err := capture(baseTree, corpus, candidateTree, temp, builder, nativeCorpora[corpus])
		if err != nil {
			return err
		}
		current, err := capture(candidateTree, corpus, candidateTree, temp, builder, nativeCorpora[corpus])
		if err != nil {
			return err
		}
		failures = append(failures, compare(old, current)...)
	}
	sort.Strings(failures)
	last := ""
	for _, failure := range failures {
		if failure != last {
			fmt.Fprintln(os.Stderr, failure)
			last = failure
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("parser behavior changed without the affected revision increasing; bump the named registry field after reviewing the change")
	}
	fmt.Println("PASS: all observed adapter/indexer changes carry a version increase")
	return nil
}

func capture(tree, corpus, fallbackCorpus, temp, builder string, nativeCorpus []byte) (snapshot, error) {
	var result snapshot
	err := filepath.WalkDir(builder, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(builder, path)
		if err != nil {
			return err
		}
		target := filepath.Join(tree, "internal/ingest/testfixture", relative)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if relative == "testdata/opencode_sqlite.yaml" {
			data = nativeCorpus
		}
		return os.WriteFile(target, data, 0600)
	})
	if err != nil {
		return result, err
	}
	probeDir := filepath.Join(tree, "internal", "ingest")
	if err := os.MkdirAll(probeDir, 0700); err != nil {
		return result, err
	}
	if err := os.WriteFile(filepath.Join(probeDir, "harvester_guard_probe_test.go"), probe, 0600); err != nil {
		return result, err
	}
	// This is the sole historical bridge: before per-harness registration,
	// adapter baseline 1 and CurrentIndexVersion described these same parsers.
	versionSource := `package ingest_test
import "github.com/peasant-labs/peasant/internal/ingest"
func parserVersions() map[string]versions {
 result := make(map[string]versions)
 for harness := range ingest.DefaultAdapterRegistry {
  target := ingest.HarvesterVersionRegistry[harness]
  result[string(harness)] = versions{target.AdapterVersion, target.IndexerVersion}
 }
 return result
}`
	if _, err := os.Stat(filepath.Join(tree, "internal/ingest/harvester_versions.go")); os.IsNotExist(err) {
		versionSource = strings.Replace(versionSource, "target := ingest.HarvesterVersionRegistry[harness]", "", 1)
		versionSource = strings.Replace(versionSource, "target.AdapterVersion, target.IndexerVersion", "1, ingest.CurrentIndexVersion", 1)
	}
	if err := os.WriteFile(filepath.Join(probeDir, "harvester_guard_versions_test.go"), []byte(versionSource), 0600); err != nil {
		return result, err
	}
	output := filepath.Join(temp, "snapshot.json")
	cmd := exec.Command("go", "test", "-race", "-count=1", "-run", "^TestHarvesterGuardCapture$", "./internal/ingest")
	cmd.Dir = tree
	cmd.Env = append(os.Environ(), "PEASANT_GUARD_CORPUS="+corpus, "PEASANT_GUARD_FALLBACK_CORPUS="+fallbackCorpus, "PEASANT_GUARD_OUTPUT="+output, "PEASANT_GUARD_NATIVE="+filepath.Join(temp, "native"))
	if out, err := cmd.CombinedOutput(); err != nil {
		return result, fmt.Errorf("cannot compare %s against corpus %s; restore compatible probe APIs/fixtures before claiming a guard result: %w\n%s", filepath.Base(tree), filepath.Base(corpus), err, out)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		return result, err
	}
	err = json.Unmarshal(data, &result)
	return result, err
}

func compare(base, candidate snapshot) []string {
	var failures []string
	for name, previous := range base.Outputs {
		current, exists := candidate.Outputs[name]
		if !exists {
			failures = append(failures, "missing candidate observation: "+name)
			continue
		}
		if bytes.Equal(previous, current) {
			continue
		}
		parts := strings.SplitN(name, "/", 3)
		oldVersion, newVersion := base.Versions[parts[0]], candidate.Versions[parts[0]]
		old, next := oldVersion.AdapterVersion, newVersion.AdapterVersion
		if parts[1] == "IndexerVersion" {
			old, next = oldVersion.IndexerVersion, newVersion.IndexerVersion
		}
		if next <= old {
			var before, after any
			_ = json.Unmarshal(previous, &before)
			_ = json.Unmarshal(current, &after)
			failures = append(failures, fmt.Sprintf("%s changed: %s %d -> %d (increase required); %s", name, parts[1], old, next, difference("output", before, after)))
		} else {
			fmt.Printf("changed %s: %d -> %d\n", name, old, next)
		}
	}
	return failures
}

func difference(path string, before, after any) string {
	if old, ok := before.(map[string]any); ok {
		if current, ok := after.(map[string]any); ok {
			keys := make(map[string]bool)
			for key := range old {
				keys[key] = true
			}
			for key := range current {
				keys[key] = true
			}
			ordered := make([]string, 0, len(keys))
			for key := range keys {
				ordered = append(ordered, key)
			}
			sort.Strings(ordered)
			for _, key := range ordered {
				if !reflect.DeepEqual(old[key], current[key]) {
					return difference(path+"."+key, old[key], current[key])
				}
			}
		}
	}
	if old, ok := before.([]any); ok {
		if current, ok := after.([]any); ok && len(old) == len(current) {
			for n := range old {
				if !reflect.DeepEqual(old[n], current[n]) {
					return difference(fmt.Sprintf("%s[%d]", path, n), old[n], current[n])
				}
			}
		}
	}
	return fmt.Sprintf("%s: %.160v -> %.160v", path, before, after)
}
