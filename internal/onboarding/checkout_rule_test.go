package onboarding_test

import (
	_ "embed"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/checkout_rule.yaml
var checkoutRuleData []byte

type checkoutRuleDocument struct {
	RequiredNames []string           `yaml:"requiredNames"`
	Cases         []checkoutRuleCase `yaml:"cases"`
}

type checkoutRuleCase struct {
	Name     string                `yaml:"name"`
	Sessions []checkoutRuleSession `yaml:"sessions"`
	Expected checkoutRuleExpected  `yaml:"expected"`
}

type checkoutRuleSession struct {
	ID       string `yaml:"id"`
	Branch   string `yaml:"branch"`
	Chosen   bool   `yaml:"chosen"`
	Explicit bool   `yaml:"explicit"`
}

type checkoutRuleExpected struct {
	Whole    bool     `yaml:"whole"`
	Branches []string `yaml:"branches"`
	Sessions []string `yaml:"sessions"`
}

// TestDeriveCheckout pins the one derivation of a checkout's saved rule. The
// terminal and the web both call it, so its cases are the specification of what
// choosing a branch, every branch, or a checkout with no branch saves.
func TestDeriveCheckout(t *testing.T) {
	t.Parallel()
	var document checkoutRuleDocument
	if err := testutil.DecodeNamedFixtureYAML(checkoutRuleData, &document); err != nil {
		t.Fatalf("decode testdata/checkout_rule.yaml: %v", err)
	}
	for _, testCase := range document.Cases {
		if len(testCase.Sessions) == 0 {
			t.Fatalf("checkout rule case %q has no sessions; a checkout with none proves nothing", testCase.Name)
		}
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			sessions := make([]onboarding.CheckoutSession, len(testCase.Sessions))
			for index, session := range testCase.Sessions {
				sessions[index] = onboarding.CheckoutSession{
					ID: session.ID, Branch: session.Branch, Chosen: session.Chosen, ExplicitSession: session.Explicit,
				}
			}
			got := onboarding.DeriveCheckout(sessions)
			want := onboarding.CheckoutRule{
				Whole: testCase.Expected.Whole, Branches: testCase.Expected.Branches, Sessions: testCase.Expected.Sessions,
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("DeriveCheckout = %+v, want %+v", got, want)
			}
		})
	}
}
