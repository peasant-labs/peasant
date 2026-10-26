package settings

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
)

// TestFieldWritesReportsWhatEachFieldOwns probes a registry whose fields each
// own one part of the configuration: a toggle, a radio inside a hidden
// section, and a field whose value config.yaml never holds. Each visit must
// show exactly the field's own write, on a working copy no other field
// touched.
func TestFieldWritesReportsWhatEachFieldOwns(t *testing.T) {
	t.Parallel()
	branch := Toggle("branch", "send the branch", Accessor[bool]{
		Get: func(c *config.Config) bool { return c.Push.Fields.GitBranch },
		Set: func(c *config.Config, v bool) { c.Push.Fields.GitBranch = v },
	})
	license := Radio("license", "license", Accessor[config.License]{
		Get: func(c *config.Config) config.License { return c.Push.License },
		Set: func(c *config.Config, v config.License) { c.Push.License = v },
	}, Option[config.License]{Label: "none", Value: ""}, Option[config.License]{Label: "cc0", Value: config.LicenseCC0})
	retention := Radio("retention", "retention", Accessor[int]{
		Get: func(c *config.Config) int { return c.ClaudeRetentionDays },
		Set: func(c *config.Config, v int) { c.ClaudeRetentionDays = v },
	}, Option[int]{Label: "30 days", Value: 30})
	registry := Registry{Sections: []Section{
		{Key: "fields", Fields: []Field{branch, nil}},
		{Key: "license", When: func(*Draft) bool { return false }, Fields: []Field{license, retention}},
	}}

	working := func() config.Config {
		var cfg config.Config
		cfg.User.Email = "kept@example.test"
		cfg.Push.Fields.GitBranch = true
		cfg.Push.License = config.LicenseCC0
		cfg.ClaudeRetentionDays = 30
		return cfg
	}
	var visited []string
	registry.FieldWrites(func() config.Config { return config.Config{} }, working, func(field Field, written *config.Config) {
		visited = append(visited, field.Key())
		want := working()
		switch field.Key() {
		case "branch":
			want.Push.Fields.GitBranch = false
		case "license":
			want.Push.License = ""
		case "retention":
			want.ClaudeRetentionDays = 0
		}
		if written.User.Email != want.User.Email || written.Push.Fields.GitBranch != want.Push.Fields.GitBranch ||
			written.Push.License != want.Push.License || written.ClaudeRetentionDays != want.ClaudeRetentionDays {
			t.Errorf("field %q wrote %+v, want %+v", field.Key(), *written, want)
		}
	})
	if got := len(visited); got != 3 || visited[0] != "branch" || visited[1] != "license" || visited[2] != "retention" {
		t.Fatalf("visited %v, want every non-nil field in presentation order, hidden ones included", visited)
	}
}
