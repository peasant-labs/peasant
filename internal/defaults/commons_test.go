package defaults

import "testing"

func TestResolveCommonsWebURLHonorsEnv(t *testing.T) {
	t.Setenv("PEASANT_COMMONS_URL", "https://commons.example.test")
	if got := ResolveCommonsWebURL().String(); got != "https://commons.example.test" {
		t.Fatalf("ResolveCommonsWebURL() = %q, want the env override", got)
	}
}

func TestResolveCommonsWebURLFallsBackToProduction(t *testing.T) {
	t.Setenv("PEASANT_COMMONS_URL", "")
	if got := ResolveCommonsWebURL().String(); got != productionCommonsWebURL {
		t.Fatalf("ResolveCommonsWebURL() = %q, want production %q", got, productionCommonsWebURL)
	}
}

func TestCommonsNoticeURLJoinsSinglePrivacy(t *testing.T) {
	cases := []struct {
		name, base, want string
	}{
		{"no-trailing-slash", "https://commons.example.test", "https://commons.example.test/privacy"},
		{"one-trailing-slash", "https://commons.example.test/", "https://commons.example.test/privacy"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("PEASANT_COMMONS_URL", c.base)
			if got := CommonsNoticeURL(); got != c.want {
				t.Fatalf("CommonsNoticeURL() = %q, want %q", got, c.want)
			}
		})
	}
}
