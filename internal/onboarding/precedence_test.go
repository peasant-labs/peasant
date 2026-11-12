package onboarding_test

import (
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/precedence.yaml
var precedenceData []byte

type precedenceAnswer struct {
	Key       string `yaml:"key"`
	Token     string `yaml:"token"`
	Bool      *bool  `yaml:"bool"`
	Selection *struct {
		Mode string `yaml:"mode"`
	} `yaml:"selection"`
}

type precedenceCase struct {
	Name             string             `yaml:"name"`
	StartMode        string             `yaml:"startMode"`
	StartAutoIngest  *bool              `yaml:"startAutoIngest"`
	StartShare       string             `yaml:"startShare"`
	StartAutoPublish *bool              `yaml:"startAutoPublish"`
	Answers          []precedenceAnswer `yaml:"answers"`
	WantMode         string             `yaml:"wantMode"`
	WantAutoIngest   *bool              `yaml:"wantAutoIngest"`
	WantShare        string             `yaml:"wantShare"`
	WantAutoPublish  *bool              `yaml:"wantAutoPublish"`
	DuplicatePath    bool               `yaml:"duplicatePath"`
	IDAnswer         bool               `yaml:"idAnswer"`
}

type precedenceDocument struct {
	RequiredNames []string         `yaml:"requiredNames"`
	Cases         []precedenceCase `yaml:"cases"`
}

func TestPrecedence(t *testing.T) {
	t.Parallel()
	var document precedenceDocument
	decodeFixture(t, "precedence.yaml", precedenceData, &document)
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Name
	}
	requireNames(t, document.RequiredNames, names)
	for _, row := range document.Cases {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			if row.DuplicatePath {
				assertDuplicatePathRefused(t)
				return
			}
			if row.IDAnswer {
				assertIDAnswerWritten(t)
				return
			}
			model := onboarding.BuildModel(onboarding.Options{Scope: onboarding.ScopeFull})
			cfg := config.BaseConfig()
			if row.StartMode != "" {
				cfg.Selection.Mode = config.SelectionMode(row.StartMode)
			}
			if row.StartAutoIngest != nil {
				cfg.Selection.AutoIngestNewBranches = *row.StartAutoIngest
			}
			if row.StartShare != "" || strings.Contains(row.Name, "share") || row.Name == "auto-publish-on-sets-share-later" || row.Name == "keep-local-clears-auto-publish-intent" {
				cfg.Push.SharePreference = config.SharePreference(row.StartShare)
			}
			if row.StartAutoPublish != nil {
				cfg.Push.AutoPublishIntent = *row.StartAutoPublish
			}
			answers := onboarding.Answers{}
			for _, answer := range row.Answers {
				key, err := onboarding.NewFieldKey(answer.Key)
				if err != nil {
					t.Fatal(err)
				}
				value, err := precedenceValue(answer)
				if err != nil {
					t.Fatal(err)
				}
				answers[key] = value
			}
			if err := model.ApplyAnswers(cfg, answers, onboarding.WriteContext{Config: cfg, Answers: answers}); err != nil {
				t.Fatal(err)
			}
			if row.WantMode != "" && string(cfg.Selection.Mode) != row.WantMode {
				t.Errorf("mode = %s, want %s", cfg.Selection.Mode, row.WantMode)
			}
			if row.WantAutoIngest != nil && cfg.Selection.AutoIngestNewBranches != *row.WantAutoIngest {
				t.Errorf("auto-ingest = %t, want %t", cfg.Selection.AutoIngestNewBranches, *row.WantAutoIngest)
			}
			if row.WantShare != "" || row.Name == "keep-local-clears-auto-publish-intent" {
				if string(cfg.Push.SharePreference) != row.WantShare {
					t.Errorf("share = %q, want %q", cfg.Push.SharePreference, row.WantShare)
				}
			}
			if row.WantAutoPublish != nil && cfg.Push.AutoPublishIntent != *row.WantAutoPublish {
				t.Errorf("auto-publish = %t, want %t", cfg.Push.AutoPublishIntent, *row.WantAutoPublish)
			}
		})
	}
}

func precedenceValue(answer precedenceAnswer) (schema.LocalSettingValue, error) {
	switch {
	case answer.Selection != nil:
		return onboarding.SelectionMappingValue(config.SelectionMode(answer.Selection.Mode), nil)
	case answer.Bool != nil:
		return onboarding.BoolValue(*answer.Bool), nil
	default:
		return onboarding.StringValue(answer.Token), nil
	}
}

func assertIDAnswerWritten(t *testing.T) {
	t.Helper()
	model := onboarding.BuildModel(onboarding.Options{Scope: onboarding.ScopeFull})
	field, ok := model.Field(onboarding.FieldSelection)
	if !ok {
		t.Fatal("missing selection field")
	}
	cfg := config.BaseConfig()
	raw, err := onboarding.SelectionIDsValue([]string{"node-1"})
	if err != nil {
		t.Fatal(err)
	}
	err = field.Write(cfg, onboarding.WriteContext{Config: cfg}, raw)
	if err != nil {
		t.Fatalf("id answer write: %v", err)
	}
	if cfg.Selection.Mode != config.SelectionModeSelected {
		t.Fatalf("id answer mode = %s, want selected", cfg.Selection.Mode)
	}
}

func assertDuplicatePathRefused(t *testing.T) {
	t.Helper()
	first := onboarding.Choice(onboarding.FieldPublication, "one", "", onboarding.Lens[string]{Path: []string{"push", "license"}}, nil)
	second := onboarding.Choice(onboarding.FieldLicense, "two", "", onboarding.Lens[string]{Path: []string{"push", "license"}}, nil)
	_, err := onboarding.ModelFromStops(onboarding.ScopeFull, []onboarding.Stop{{
		Key:    onboarding.FlowLicense,
		Fields: []onboarding.Field{first, second},
	}}, nil)
	if err == nil || !strings.Contains(err.Error(), "share YAML path") {
		t.Fatalf("duplicate YAML path error = %v", err)
	}
}
