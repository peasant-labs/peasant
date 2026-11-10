package main

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

func TestDocumentedHarnessRoster(t *testing.T) {
	data, err := os.ReadFile("../../docs/bestiary.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	const begin = "<!-- harness-sets:begin -->"
	const end = "<!-- harness-sets:end -->"
	if strings.Count(text, begin) != 1 || strings.Count(text, end) != 1 {
		t.Fatal("expected one harness roster")
	}
	_, rest, _ := strings.Cut(text, begin)
	block, _, ok := strings.Cut(rest, end)
	if !ok {
		t.Fatal("missing roster end")
	}
	rows := strings.Split(strings.TrimSpace(block), "\n")
	if len(rows) != 4 {
		t.Fatal("roster must contain four labeled lines")
	}
	parse := func(i int, label string) []string {
		t.Helper()
		head, values, ok := strings.Cut(rows[i], ": ")
		if !ok || head != label {
			t.Fatalf("invalid roster line %q", rows[i])
		}
		return strings.Split(values, ", ")
	}
	toStrings := func(h []schema.Harness) []string {
		out := make([]string, 0, len(h))
		for _, x := range h {
			out = append(out, string(x))
		}
		return out
	}
	if got, want := parse(0, "known"), toStrings(schema.Harnesses()); !reflect.DeepEqual(got, want) {
		t.Errorf("known = %v, want %v", got, want)
	}
	if got, want := parse(1, "declared"), toStrings(schema.AllHarnesses); !reflect.DeepEqual(got, want) {
		t.Errorf("declared = %v, want %v", got, want)
	}
	got := parse(2, "harvestable")
	want := make([]string, 0, len(ingest.DefaultAdapterRegistry))
	for h := range ingest.DefaultAdapterRegistry {
		want = append(want, string(h))
	}
	slices.Sort(got)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("harvestable = %v, want %v", got, want)
	}
	if got := parse(3, "deprecated"); !reflect.DeepEqual(got, []string{string(schema.HarnessGeminiCLI)}) {
		t.Errorf("deprecated = %v", got)
	}
}
