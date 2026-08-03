package e2e

import (
	"errors"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
)

// TestMaximumConfigPath_IsRefusedNotSilentlyWeakened is the CONFIG-DRIVEN
// counterpart to the flag-driven Maximum coverage. The real push / harvest /
// ingest pipelines take no `--redaction` flag: they read the loaded config, so
// this drives that exact chain with a `redaction: { level: maximum }` config.
//
// It asserts the decision by CALLING production rather than restating the rule. An
// earlier version of this test re-derived the policy inline, which meant it kept
// passing while defending a rule the product no longer had - the same "green test
// over dormant behaviour" shape it exists to prevent.
//
// The rule it now holds: a configured maximum level is REFUSED, not quietly
// downgraded, and refused UNCONDITIONALLY. Maximum's distinguishing behaviour is
// anonymising code identifiers in transcript content, and the parser that needs
// is linked in only when Peasant is built with cgo - so offering it would make
// the same configuration run on a locally-built Peasant and dead-end on a
// released one. Refusing on every build is what keeps redaction independent of
// how the binary was compiled. Running at a weaker level instead would publish
// content the user believes was anonymised, and every weaker level protects
// less, so there is no safe substitute and nothing runs.
//
// Untagged, so it runs and asserts in BOTH the CGO=1 and CGO=0 test legs. It no
// longer needs to branch on redact.MaximumAvailable at all: the level never
// reaches a redactor, which is the point.
func TestMaximumConfigPath_IsRefusedNotSilentlyWeakened(t *testing.T) {
	const yamlContent = "redaction:\n  level: maximum\n"

	memfs := testutil.NewMemFS()
	if err := memfs.WriteFile("/etc/peasant/config.yaml", []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("setup WriteFile: %v", err)
	}

	cfg, err := config.Load("/etc/peasant/config.yaml", memfs, testutil.DefaultGitResolver())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// Load must still ACCEPT the level: it is a real level, the engine supports it,
	// and the refusal has to be able to quote back what the user configured.
	if cfg.Redaction.Level != redact.Maximum {
		t.Fatalf("the loaded config must keep the configured level intact, got %q: a refusal that could not name what the user "+
			"asked for would be unactionable", cfg.Redaction.Level)
	}

	// The production decision every consumer makes.
	if config.RedactionLevelSupported(cfg.Redaction.Level) {
		t.Fatal("a configured maximum level must not be treated as supported: no code path anonymises transcript content, " +
			"so running would protect less than the user chose")
	}

	// And the refusal has to be actionable rather than a bare failure.
	refusal := &config.UnsupportedRedactionLevelError{
		Level:     cfg.Redaction.Level,
		Source:    "your configuration file /etc/peasant/config.yaml",
		Operation: "village push",
		Step:      "while building the redactor, before any session was uploaded",
		Impact:    "Nothing was published and nothing was recorded as published.",
	}
	if !errors.Is(refusal, config.ErrUnsupportedRedactionLevel) {
		t.Error("the refusal must classify as config.ErrUnsupportedRedactionLevel")
	}
	message := refusal.Error()
	for _, want := range []string{
		redact.Maximum.String(),
		"/etc/peasant/config.yaml",
		"redaction.level",
		config.RecommendedRedactionLevel.String(),
		"Nothing was published",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal must state %q; got:\n%s", want, message)
		}
	}

	// The levels that ARE supported must construct a redactor in every build,
	// including the CGO=0 leg. That is what makes the refusal safe to rely on:
	// there is no supported level that fails at construction.
	for _, level := range config.SupportedRedactionLevels {
		redactor, buildErr := redact.NewRedactor(level, nil, redact.XDGPaths{})
		if buildErr != nil {
			t.Errorf("the supported level %q must construct in every build, got: %v", level, buildErr)
		}
		if redactor == nil {
			t.Errorf("the supported level %q returned a nil redactor without an error", level)
		}
	}

	// The library keeps its own contract, unchanged: only the product's policy
	// moved, and the public redaction module still supports the level. Asked for it directly it
	// either constructs (cgo) or fails closed and actionably (no cgo).
	direct, directErr := redact.NewRedactor(redact.Maximum, nil, redact.XDGPaths{})
	if redact.MaximumAvailable {
		if directErr != nil {
			t.Fatalf("MaximumAvailable=true: NewRedactor(maximum) failed: %v", directErr)
		}
		if direct == nil {
			t.Fatal("MaximumAvailable=true: nil redactor returned without error")
		}
		return
	}
	if directErr == nil {
		t.Fatal("MaximumAvailable=false: NewRedactor(maximum) returned no error " +
			"(silent weaker redaction through the library seam is forbidden)")
	}
	if direct != nil {
		t.Fatalf("MaximumAvailable=false: expected nil redactor (fail-closed), got %v", direct)
	}
	low := strings.ToLower(directErr.Error())
	for _, want := range []string{redact.Maximum.String(), "cgo", redact.Standard.String()} {
		if !strings.Contains(low, want) {
			t.Errorf("actionable library-path error must mention %q; got: %v", want, directErr.Error())
		}
	}
}
