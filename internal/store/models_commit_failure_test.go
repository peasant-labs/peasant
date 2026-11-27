package store_test

import (
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/model_commit_failure.yaml
var modelCommitFailureYAML []byte

func TestSyncModelsCommitFailure(t *testing.T) {
	var f struct {
		ModelID     string `yaml:"model_id"`
		ProviderKey string `yaml:"provider_key"`
		DisplayName string `yaml:"display_name"`
		LastSynced  string `yaml:"last_synced"`
	}
	if err := yaml.Unmarshal(modelCommitFailureYAML, &f); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := openTestStore(t)
	pool := s.PoolForTest()
	denied := 0
	auth := sqlite.AuthorizeFunc(func(action sqlite.Action) sqlite.AuthResult {
		if action.Type() == sqlite.OpTransaction && action.Operation() == "COMMIT" {
			denied++
			return sqlite.AuthResultDeny
		}
		return sqlite.AuthResultOK
	})
	authorizeCommitFailure(t, pool, auth)
	err := s.SyncModels(ctx, []ingest.ModelInfo{{ModelID: f.ModelID, ProviderKey: f.ProviderKey, DisplayName: f.DisplayName, LastSynced: f.LastSynced}})
	if denied != 1 {
		t.Fatalf("COMMIT denials = %d, want 1; error: %v", denied, err)
	}
	if err == nil || !strings.Contains(err.Error(), "COMMIT") {
		t.Errorf("expected COMMIT error, got %v", err)
	}
	model, err := s.GetModel(ctx, f.ModelID, f.ProviderKey)
	if err != nil || model != nil {
		t.Errorf("after rollback model = %v, error %v; want absent", model, err)
	}
}
