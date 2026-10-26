package push_test

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/schema"
)

// TestCollectiveAudienceAsksForNoWiderAudience pins the narrowing both
// collectives-only doors use: a first publication opens private with no
// license, and an update asks for no change of visibility or license, so the
// audience the transcript has on Village is kept. The caller's configuration
// is not changed.
func TestCollectiveAudienceAsksForNoWiderAudience(t *testing.T) {
	t.Parallel()
	cfg := config.BaseConfig()
	cfg.Push.Visibility = schema.VisibilityPublic
	cfg.Push.License = schema.LicenseCCBY
	narrowed, runCfg := push.CollectiveAudience(cfg, push.PipelineConfig{
		Visibility: schema.VisibilityPublic, License: schema.LicenseCC0, ChangeVisibility: true, ChangeLicense: true,
	})
	if narrowed.Push.License != "" || runCfg.Visibility != schema.VisibilityPrivate || runCfg.License != "" || runCfg.ChangeVisibility || runCfg.ChangeLicense {
		t.Fatalf("narrowed = license %q, run %+v; want private, no license, and no change asked", narrowed.Push.License, runCfg)
	}
	if cfg.Push.License != schema.LicenseCCBY {
		t.Fatal("CollectiveAudience changed the caller's configuration")
	}
}
