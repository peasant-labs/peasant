package pull_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/schema"
)

// TestPullManifest_Golden pins the REAL pull.PullManifest against the committed
// golden example embedded in the github.com/peasant-labs/schema module. That
// module's shape-pin test (TestPullManifestExample_ShapePin) cannot import internal/pull,
// so this is the cross-module half: it unmarshals the golden bytes into the
// actual PullManifest, asserts the load-bearing field invariants, and round-trips
// (marshal -> normalize -> byte-compare) to catch any drift between the struct's
// JSON tags and the committed example.
func TestPullManifest_Golden(t *testing.T) {
	raw := schema.PullManifestExampleJSON

	var m pull.PullManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal golden manifest into pull.PullManifest: %v", err)
	}

	// Field invariants the golden example demonstrates.
	if m.ManifestVersion != pull.PullManifestVersion {
		t.Errorf("manifestVersion = %d, want PullManifestVersion=%d", m.ManifestVersion, pull.PullManifestVersion)
	}
	if m.VillageHost == "" {
		t.Error("villageHost is empty")
	}
	if m.TranscriptID == "" {
		t.Error("transcriptId is empty")
	}
	// servedETag is the VERBATIM (quoted) transport token; servedBlobHash is the
	// RAW (unquoted) content identity — the inner of the former must equal the
	// latter.
	if want := `"` + m.ServedBlobHash + `"`; m.ServedETag != want {
		t.Errorf("servedETag = %q, want quoted form of servedBlobHash = %q", m.ServedETag, want)
	}
	if len(m.Annotations) == 0 {
		t.Error("golden manifest should carry >=1 annotation provenance entry")
	}
	for i, a := range m.Annotations {
		if a.ContentHash == "" || a.AuthorUserID == "" || a.AuthorUsername == "" {
			t.Errorf("annotation[%d] has an empty provenance field: %+v", i, a)
		}
	}

	// Round-trip: re-marshal the real struct, normalize, byte-compare to committed.
	remarshaled, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-marshal pull.PullManifest: %v", err)
	}
	var got, want bytes.Buffer
	if err := json.Indent(&got, remarshaled, "", "  "); err != nil {
		t.Fatalf("indent re-marshaled: %v", err)
	}
	if err := json.Indent(&want, bytes.TrimSpace(raw), "", "  "); err != nil {
		t.Fatalf("indent committed: %v", err)
	}
	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Errorf("real PullManifest round-trip drifted from golden example:\n--- got ---\n%s\n--- want ---\n%s",
			got.String(), want.String())
	}
}
