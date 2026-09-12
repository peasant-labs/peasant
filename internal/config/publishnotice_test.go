package config

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"sort"
	"testing"

	schema "github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/publish_consent_phrases.yaml
var publishConsentPhraseFixtureData []byte

// requiredPublishConsentPhraseCases is the manifest the corpus must carry
// exactly: one row per license the contract offers, keyed by a stable name,
// plus the no-license row. A missing name fails loudly; a count would churn on
// every legitimate addition. The license COVERAGE (that these names map onto
// schema.AllLicenses ∪ {""}) is asserted separately from the contract itself.
var requiredPublishConsentPhraseCases = []string{"cc-by", "cc-by-sa", "cc0", "no-license"}

type publishConsentPhraseDocument struct {
	Cases []publishConsentPhraseCase `yaml:"cases"`
}

type publishConsentPhraseCase struct {
	Name    string  `yaml:"name"`
	License License `yaml:"license"`
	Phrase  string  `yaml:"phrase"`
}

func loadPublishConsentPhraseFixture(t *testing.T) publishConsentPhraseDocument {
	t.Helper()
	var doc publishConsentPhraseDocument
	dec := yaml.NewDecoder(bytes.NewReader(publishConsentPhraseFixtureData))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode publish_consent_phrases.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		t.Fatalf("publish_consent_phrases.yaml must hold exactly one document; got a second (%v)", err)
	}

	// Required-NAME manifest: the set of case names must be exactly the manifest.
	got := make([]string, 0, len(doc.Cases))
	for _, c := range doc.Cases {
		got = append(got, c.Name)
	}
	assertNamesMatch(t, got, requiredPublishConsentPhraseCases)
	return doc
}

func assertNamesMatch(t *testing.T, got, want []string) {
	t.Helper()
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if fmt.Sprint(g) != fmt.Sprint(w) {
		t.Fatalf("case names differ: got %v; want %v (a case was added, renamed or deleted without updating the manifest)", g, w)
	}
	if len(g) != len(dedupe(g)) {
		t.Fatalf("case names must be unique: %v", got)
	}
}

func dedupe(in []string) []string {
	seen := map[string]struct{}{}
	out := in[:0:0]
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// TestPublishConsentPhraseMatchesFixture holds the helper to the fixture rows.
func TestPublishConsentPhraseMatchesFixture(t *testing.T) {
	doc := loadPublishConsentPhraseFixture(t)
	for _, c := range doc.Cases {
		if got := PublishConsentPhrase(c.License); got != c.Phrase {
			t.Errorf("PublishConsentPhrase(%q) = %q, want %q", c.License, got, c.Phrase)
		}
	}
}

// TestPublishConsentPhraseCoversContract fails until every license the contract
// offers has a non-empty phrase AND a fixture row, and until the fixture covers
// exactly schema.AllLicenses plus the no-license choice. A license added to
// schema.AllLicenses goes red here until publishnotice.go and the fixture cover it.
func TestPublishConsentPhraseCoversContract(t *testing.T) {
	doc := loadPublishConsentPhraseFixture(t)

	// The switch answers every contract member.
	for _, l := range schema.AllLicenses {
		if PublishConsentPhrase(l) == "" {
			t.Errorf("PublishConsentPhrase(%q) is empty; every schema.AllLicenses member needs a phrase", l)
		}
	}

	// The fixture's license set equals schema.AllLicenses ∪ {""}.
	want := map[License]struct{}{"": {}}
	for _, l := range schema.AllLicenses {
		want[l] = struct{}{}
	}
	got := map[License]struct{}{}
	for _, c := range doc.Cases {
		got[c.License] = struct{}{}
	}
	if fmt.Sprint(sortedLicenseKeys(got)) != fmt.Sprint(sortedLicenseKeys(want)) {
		t.Errorf("fixture licenses %v != schema.AllLicenses ∪ {\"\"} %v",
			sortedLicenseKeys(got), sortedLicenseKeys(want))
	}
}

func sortedLicenseKeys(m map[License]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, string(k))
	}
	sort.Strings(out)
	return out
}
