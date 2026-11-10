package onboarding_test

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"gopkg.in/yaml.v3"
)

func decodeFixture(t *testing.T, name string, data []byte, target any) {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			t.Fatalf("%s contains a second YAML document", name)
		}
		t.Fatalf("decode trailing %s: %v", name, err)
	}
}

func requireNames(t *testing.T, required []string, names []string) {
	t.Helper()
	if len(required) == 0 {
		t.Fatal("fixture requiredNames is empty")
	}
	got := map[string]bool{}
	for _, name := range names {
		if name == "" {
			t.Fatal("fixture case has an empty name")
		}
		if got[name] {
			t.Fatalf("duplicate fixture name %q", name)
		}
		got[name] = true
	}
	for _, name := range required {
		if !got[name] {
			t.Errorf("required name %q is missing", name)
		}
	}
	for name := range got {
		found := false
		for _, requiredName := range required {
			if requiredName == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("name %q is not in requiredNames", name)
		}
	}
}

func yamlEqual(got, want any) error {
	left, err := yaml.Marshal(got)
	if err != nil {
		return err
	}
	right, err := yaml.Marshal(want)
	if err != nil {
		return err
	}
	if !bytes.Equal(left, right) {
		return fmt.Errorf("got %s want %s", left, right)
	}
	return nil
}
