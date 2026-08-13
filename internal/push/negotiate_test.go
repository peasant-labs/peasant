package push

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// newNegotiatePipeline builds a minimal real Pipeline whose only live dependency
// for negotiate() is the publisher (the village HTTP double) and stderr. The
// pipeline SUT is NOT mocked — only the village transport is.
func newNegotiatePipeline(pub *testutil.StubPublisher, stderr *bytes.Buffer) *Pipeline {
	return NewPipeline(
		&testutil.StubPushStore{},
		pub,
		&auth.Credentials{VillageURL: "https://village.example", APIKey: "k"},
		&config.Config{},
		testutil.NewMemFS(),
		PipelineConfig{},
		nil,
		stderr,
	)
}

// cliVersion is the contract version this build of the CLI emits.
var cliVersion = defaults.PublishSchemaVersion

// --- negotiate() matrix (FAILS until L3) ---

func TestNegotiate_Within_EmitsAtCLIVersion(t *testing.T) {
	pub := &testutil.StubPublisher{SchemaVersionResp: &schema.SchemaVersionResponse{
		MinPushContractVersion: schema.PushContractVersion("0.0.1"),
		PushContractVersion:    cliVersion, // current == cli → within
	}}
	var stderr bytes.Buffer
	p := newNegotiatePipeline(pub, &stderr)

	emit, _, err := p.negotiate(context.Background())
	if err != nil {
		t.Fatalf("within-window must not error, got: %v", err)
	}
	if emit != cliVersion {
		t.Errorf("emit: got %q, want CLI version %q", emit, cliVersion)
	}
	if pub.SchemaVersionCalls != 1 {
		t.Errorf("GetSchemaVersion (was dead) must be called exactly once by the preflight; got %d", pub.SchemaVersionCalls)
	}
}

func TestNegotiate_OlderThanMin_AbortsUpgradeCLI(t *testing.T) {
	pub := &testutil.StubPublisher{SchemaVersionResp: &schema.SchemaVersionResponse{
		MinPushContractVersion: schema.PushContractVersion("0.2.0"),
		PushContractVersion:    schema.PushContractVersion("0.5.0"),
	}}
	var stderr bytes.Buffer
	p := newNegotiatePipeline(pub, &stderr)

	emit, _, err := p.negotiate(context.Background())
	if err == nil {
		t.Fatalf("CLI older than Min must abort; got emit=%q nil err", emit)
	}
	msg := err.Error()
	// Actionable: what mismatched, the floor version, and upgrade the CLI.
	for _, want := range []string{"older", "0.2.0", "upgrade the peasant CLI"} {
		if !strings.Contains(msg, want) {
			t.Errorf("upgrade-CLI error missing %q; got: %s", want, msg)
		}
	}
	if strings.Contains(strings.ToLower(msg), "upgrade the village") {
		t.Errorf("error must NEVER instruct upgrading the village; got: %s", msg)
	}
}

func TestNegotiate_AheadOfCurrent_DowngradeEmitsWithWarning(t *testing.T) {
	current := schema.PushContractVersion("0.0.5") // below cli 0.1.1, same major
	pub := &testutil.StubPublisher{SchemaVersionResp: &schema.SchemaVersionResponse{
		MinPushContractVersion: schema.PushContractVersion("0.0.1"),
		PushContractVersion:    current,
	}}
	var stderr bytes.Buffer
	p := newNegotiatePipeline(pub, &stderr)

	emit, _, err := p.negotiate(context.Background())
	if err != nil {
		t.Fatalf("CLI ahead (downgradable) must not error, got: %v", err)
	}
	if emit != current {
		t.Errorf("downgrade-emit: got %q, want village current %q", emit, current)
	}
	warn := stderr.String()
	for _, want := range []string{"ahead", "emitting v0.0.5"} {
		if !strings.Contains(warn, want) {
			t.Errorf("downgrade warning missing %q; got: %s", want, warn)
		}
	}
	if strings.Contains(strings.ToLower(warn), "upgrade the village") {
		t.Errorf("warning must NEVER instruct upgrading the village; got: %s", warn)
	}
	// One-line warning.
	if n := strings.Count(strings.TrimRight(warn, "\n"), "\n"); n != 0 {
		t.Errorf("downgrade warning must be one line; got %d newlines: %s", n, warn)
	}
}

func TestNegotiate_Unadvertised_ProceedsAtCLIVersion(t *testing.T) {
	pub := &testutil.StubPublisher{SchemaVersionResp: &schema.SchemaVersionResponse{}} // empty window
	var stderr bytes.Buffer
	p := newNegotiatePipeline(pub, &stderr)

	emit, _, err := p.negotiate(context.Background())
	if err != nil {
		t.Fatalf("unadvertised window must not error, got: %v", err)
	}
	if emit != cliVersion {
		t.Errorf("emit: got %q, want CLI version %q (back-compat passthrough)", emit, cliVersion)
	}
}

// NOTE: the pure semver/window helper tests (CompareSemver, ClassifyContract,
// CanDowngrade) moved with the helpers to internal/village/negotiate_test.go
// so this file retains only the push-pipeline orchestration matrix.
