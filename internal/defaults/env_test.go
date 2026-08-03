package defaults

import "testing"

func TestEnvVarString(t *testing.T) {
	cases := []EnvVar{
		EnvXDGConfigHome,
		EnvXDGDataHome,
		EnvXDGStateHome,
		EnvGoPrivate,
	}

	for _, env := range cases {
		got := env.String()
		if got == "" {
			t.Fatalf("%#v.String() is empty", env)
		}
		if EnvVar(got) != env {
			t.Fatalf("%#v.String() = %q, want round-trip to same EnvVar", env, got)
		}
	}
}
