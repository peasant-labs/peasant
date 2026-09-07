package testutil

import "testing"

func TestLoadProfileFixtureFamilies(t *testing.T) {
	t.Parallel()

	loaders := map[ProfileFixtureFamily]func() (ProfileFixtureSet, error){
		ProfileFixtureFamilyContract:  LoadProfileContractFixtures,
		ProfileFixtureFamilyPush:      LoadProfilePushFixtures,
		ProfileFixtureFamilyRedaction: LoadProfileRedactionFixtures,
		ProfileFixtureFamilyCLI:       LoadProfileCLIFixtures,
	}

	for family, load := range loaders {
		family, load := family, load
		t.Run(string(family), func(t *testing.T) {
			t.Parallel()
			fixtures, err := load()
			if err != nil {
				t.Fatalf("load %s fixtures: %v", family, err)
			}
			if fixtures.Family != family {
				t.Fatalf("fixture family = %q, want %q", fixtures.Family, family)
			}
			if len(fixtures.Cases) == 0 {
				t.Fatalf("%s fixtures have no cases", family)
			}
			if err := ValidateRequiredNames(fixtures.Manifest, fixtures.Names(), string(family)); err != nil {
				t.Fatalf("validate %s names: %v", family, err)
			}
		})
	}
}

func TestProfileFixturesRejectDeletedCaseByName(t *testing.T) {
	t.Parallel()

	fixtures, err := LoadProfilePushFixtures()
	if err != nil {
		t.Fatalf("LoadProfilePushFixtures: %v", err)
	}
	if len(fixtures.Cases) < 2 {
		t.Fatalf("push fixture needs at least 2 cases for deletion proof")
	}
	if err := ValidateRequiredNames(fixtures.Manifest, fixtures.Names()[1:], string(fixtures.Family)); err == nil {
		t.Fatalf("ValidateRequiredNames accepted a deleted profile push case")
	}
}
