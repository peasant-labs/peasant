package push_test

// Peasant back-compat round-trip over the shared versioned golden corpus. The
// peasant side OWNS the current contract shape (TranscriptContent +
// SessionDetailPayload), so it asserts the current corpus decodes into those
// types and round-trips (marshal -> unmarshal -> equal). The corpus is the
// single source of truth; it now ships inside the schema module and is read
// through the embedded schema.ContractCorpusFS accessor (embed root
// testdata/contract) rather than an on-disk path.

import (
	"encoding/json"
	"io/fs"
	"path"
	"reflect"
	"testing"

	"github.com/peasant-labs/schema"
)

// corpusRoot is the directory prefix inside schema.ContractCorpusFS (the embed
// is rooted at the module, so entries are addressed as testdata/contract/...).
const corpusRoot = "testdata/contract"

func readCorpus(t *testing.T, version, validity, name string) []byte {
	t.Helper()
	p := path.Join(corpusRoot, version, validity, name)
	b, err := fs.ReadFile(schema.ContractCorpusFS, p)
	if err != nil {
		t.Fatalf("read corpus %s from schema.ContractCorpusFS: %v", p, err)
	}
	return b
}

// The current content.json decodes into a TranscriptContent envelope and
// round-trips losslessly.
func TestContract_Current_TranscriptContent_RoundTrip(t *testing.T) {
	raw := readCorpus(t, "current", "valid", "content.json")

	var env schema.TranscriptContent
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode TranscriptContent: %v", err)
	}
	if env.ContractVersion != schema.PushContractVersion("0.1.1") {
		t.Errorf("contractVersion: got %q, want 0.1.1", env.ContractVersion)
	}
	if env.Kind != schema.ContentKindSessionDetail {
		t.Errorf("kind: got %q, want %q", env.Kind, schema.ContentKindSessionDetail)
	}
	if env.SessionDetail == nil {
		t.Fatal("sessionDetail is nil")
	}
	if env.SessionDetail.Harness != schema.HarnessClaudeCode {
		t.Errorf("sessionDetail.harness: got %q, want claude-code", env.SessionDetail.Harness)
	}
	if env.SessionDetail.Scorecard == nil {
		t.Error("current corpus must carry a scorecard")
	}

	// Round-trip: re-marshal + re-decode must be stable.
	reBytes, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	var env2 schema.TranscriptContent
	if err := json.Unmarshal(reBytes, &env2); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if !reflect.DeepEqual(env, env2) {
		t.Errorf("TranscriptContent round-trip not stable:\n got=%+v\nwant=%+v", env2, env)
	}
}

// The current metadata.json decodes into a PublishRequest with the unified
// harness key and round-trips.
func TestContract_Current_PublishRequest_RoundTrip(t *testing.T) {
	raw := readCorpus(t, "current", "valid", "metadata.json")

	var req schema.PublishRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("decode PublishRequest: %v", err)
	}
	if req.Model.Harness != schema.HarnessClaudeCode {
		t.Errorf("model.harness: got %q, want claude-code", req.Model.Harness)
	}
	if !req.Model.Harness.IsKnown() {
		t.Errorf("model.harness %q must be a known harness", req.Model.Harness)
	}

	reBytes, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	var req2 schema.PublishRequest
	if err := json.Unmarshal(reBytes, &req2); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if !reflect.DeepEqual(req, req2) {
		t.Errorf("PublishRequest round-trip not stable")
	}
}

// corpusVersionDirs enumerates the version directories under the corpus root so
// new versions (e.g. legacy-metadata-field) are auto-covered without editing a
// hardcoded list. Non-directory entries (e.g. the corpus README) are skipped.
func corpusVersionDirs(t *testing.T) []string {
	t.Helper()
	entries, err := fs.ReadDir(schema.ContractCorpusFS, corpusRoot)
	if err != nil {
		t.Fatalf("read corpus dir %s from schema.ContractCorpusFS: %v", corpusRoot, err)
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() {
			versions = append(versions, e.Name())
		}
	}
	if len(versions) == 0 {
		t.Fatalf("no corpus version directories found under %s", corpusRoot)
	}
	return versions
}

// Every corpus version's content + metadata is loadable/decodable peasant-side
// (sanity that the shared corpus stays parseable against the current types).
func TestContract_AllVersions_Loadable(t *testing.T) {
	for _, v := range corpusVersionDirs(t) {
		t.Run(v, func(t *testing.T) {
			content := readCorpus(t, v, "valid", "content.json")
			var any1 any
			if err := json.Unmarshal(content, &any1); err != nil {
				t.Errorf("%s valid content.json must be valid JSON: %v", v, err)
			}
			meta := readCorpus(t, v, "valid", "metadata.json")
			var req schema.PublishRequest
			if err := json.Unmarshal(meta, &req); err != nil {
				t.Errorf("%s valid metadata.json must decode into PublishRequest: %v", v, err)
			}
		})
	}
}
