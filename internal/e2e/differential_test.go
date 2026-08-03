package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/redact"
)

// TestFixture_MaximumDifferential exercises the SAME redaction the ingest pipeline
// applies to transcript text (redact.RedactText, the per-string primitive inside
// RedactJSONLBytes), over the fixture's injected code block at Standard vs
// Maximum, and asserts the EMPIRICAL behavior of the pinned identifier
// (docs/e2e-fixture.md):
//
//   - Standard LEAVES the fenced block in place and runs the rules over it — the
//     pinned identifier and code structure both SURVIVE, and matched tokens
//     inside the block are replaced.
//   - Maximum AST-anonymizes identifiers while PRESERVING structure — the pinned
//     identifier is gone (idN placeholder) but `func`/`return` structure remains,
//     and the block is NOT masked.
//
// We isolate the injected block's own message text first, because the committed
// fixture ALSO contains pre-masked "<CODE_BLOCK>" literals (real code masked when
// the fixture was Standard-redacted at generation) which would otherwise conflate
// a whole-file check. Untagged so it runs in `make check`.
//
// THE DIFFERENTIAL REVERSED. This test carried "Standard masks / Maximum
// preserves-structure-and-anonymizes", which described shipped behaviour whose
// gating was inverted: AST anonymisation was the v2 replacement for v1 masking
// and was gated to Maximum, so once Maximum stopped being offered every remaining
// user fell through to the destructive path, and the stricter level preserved
// more than the looser one. Masking also ran before the rules, so nothing inside
// a block was ever scanned.
//
// The differential is now "Standard leaves code in place and redacts inside it /
// Maximum additionally anonymises identifiers", and the bullets above state it.
// They previously stated the OLD differential, with this paragraph correcting
// them underneath - so the file carried both claims and a reader met the false
// one first. The bullets are the summary somebody reads; a correction below them
// does not undo them.
func TestFixture_MaximumDifferential(t *testing.T) {
	text := injectedCodeBlockText(t)
	if !strings.Contains(text, PinnedCodeIdentifier) {
		t.Fatalf("could not locate the injected code block (identifier %q) in the fixture", PinnedCodeIdentifier)
	}

	std := redactText(t, redact.Standard, text)

	// Standard: the code is LEFT IN PLACE - identifier and structure survive - and
	// the rules run over it. Standard is available in every build mode, so this
	// half always runs.
	if !strings.Contains(std, PinnedCodeIdentifier) {
		t.Errorf("Standard: pinned identifier %q is gone. Below Maximum the code is left in place; anonymising it is "+
			"Maximum's job and masking it wholesale was the inverted behaviour", PinnedCodeIdentifier)
	}
	if strings.Contains(std, "<CODE_BLOCK>") {
		t.Errorf("Standard: the block was masked wholesale. That removes the artifact in a product for sharing coding "+
			"transcripts, and it runs before the rules so nothing inside is scanned:\n%s", std)
	}
	if !strings.Contains(std, "func ") {
		t.Errorf("Standard: code structure did not survive:\n%s", std)
	}

	// CGO=0 leg: Maximum is NOT compiled in (redact.MaximumAvailable == false).
	// This is NOT a silent skip — the negative
	// assertion IS the test: NewRedactor(Maximum) must hard-error actionably
	// instead of silently degrading. Asserting the error here keeps the most
	// security-critical path (privacy redaction) tested in the exact build mode
	// it changed. The full positive differential below runs under CGO=1.
	if !redact.MaximumAvailable {
		_, err := redact.NewRedactor(redact.Maximum, nil, redact.XDGPaths{})
		if err == nil {
			t.Fatal("MaximumAvailable=false but NewRedactor(Maximum) returned no error: " +
				"a no-cgo build must reject Maximum, never silently apply weaker redaction")
		}
		low := strings.ToLower(err.Error())
		for _, want := range []string{redact.Maximum.String(), "cgo", redact.Standard.String()} {
			if !strings.Contains(low, want) {
				t.Errorf("actionable Maximum-unavailable error must mention %q; got: %v", want, err.Error())
			}
		}
		return
	}

	// CGO=1 leg: full positive differential.
	mx := redactText(t, redact.Maximum, text)

	// Maximum: identifier anonymized (gone), structure preserved, NOT masked.
	if strings.Contains(mx, PinnedCodeIdentifier) {
		t.Errorf("Maximum: pinned identifier %q should be AST-anonymized, but survived:\n%s", PinnedCodeIdentifier, mx)
	}
	if !strings.Contains(mx, "func") || !strings.Contains(mx, "return") {
		t.Errorf("Maximum: expected preserved code structure (func/return), not found:\n%s", mx)
	}
}

// injectedCodeBlockText scans the root fixture for the message text block that
// contains the pinned identifier and returns its raw (pre-redaction) text.
func injectedCodeBlockText(t *testing.T) string {
	t.Helper()
	rootJSONL := filepath.Join(FixtureSourcePath(), FixtureSlugDir, FixtureRootSessionID+".jsonl")
	data, err := os.ReadFile(rootJSONL)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		msg, _ := m["message"].(map[string]any)
		if msg == nil {
			continue
		}
		blocks, _ := msg["content"].([]any)
		for _, b := range blocks {
			bm, _ := b.(map[string]any)
			if bm == nil {
				continue
			}
			if txt, ok := bm["text"].(string); ok && strings.Contains(txt, PinnedCodeIdentifier) {
				return txt
			}
		}
	}
	t.Fatalf("pinned identifier %q not found in any fixture message text", PinnedCodeIdentifier)
	return ""
}

func redactText(t *testing.T, level redact.RedactionLevel, text string) string {
	t.Helper()
	r, err := redact.NewRedactor(level, nil, redact.XDGPaths{})
	if err != nil {
		t.Fatalf("new redactor (%s): %v", level, err)
	}
	return r.RedactText(text)
}
