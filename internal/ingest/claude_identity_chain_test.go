package ingest

import (
	"bytes"
	_ "embed"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/claude_identity_chain.yaml
var claudeIdentityChainYAML []byte

type claudeIdentityChainLinkFixture struct {
	SessionID  string `yaml:"sessionId"`
	UUID       string `yaml:"uuid"`
	ParentUUID string `yaml:"parentUuid"`
	Type       string `yaml:"type"`
	Subtype    string `yaml:"subtype"`
}

type claudeIdentityChainCase struct {
	Name     string                           `yaml:"name"`
	Own      string                           `yaml:"own"`
	Source   string                           `yaml:"source"`
	Want     bool                             `yaml:"want"`
	Records  []claudeIdentityChainLinkFixture `yaml:"records"`
	Snapshot []claudeIdentityChainLinkFixture `yaml:"snapshot"`
}

type claudeIdentityChainFixtures struct {
	RequiredNames []string                  `yaml:"requiredNames"`
	Cases         []claudeIdentityChainCase `yaml:"cases"`
}

func TestClaudeIdentityChainPreservesLineageAnswer(t *testing.T) {
	var fixtures claudeIdentityChainFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(claudeIdentityChainYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures.RequiredNames) == 0 {
		t.Fatal("lineage fixture declares no required names")
	}
	seen := make(map[string]struct{}, len(fixtures.Cases))
	for _, name := range fixtures.RequiredNames {
		if name == "" {
			t.Fatal("lineage fixture declares a blank required name")
		}
		if _, duplicate := seen[name]; duplicate {
			t.Fatalf("lineage fixture repeats required name %q", name)
		}
		seen[name] = struct{}{}
	}
	if len(seen) != len(fixtures.Cases) {
		t.Fatalf("lineage fixture has %d cases and %d required names", len(fixtures.Cases), len(seen))
	}
	for _, testCase := range fixtures.Cases {
		if _, ok := seen[testCase.Name]; !ok {
			t.Fatalf("lineage fixture case %q is not in the required name list", testCase.Name)
		}
		delete(seen, testCase.Name)
		t.Run(testCase.Name, func(t *testing.T) {
			own, err := NewSessionID(testCase.Own)
			if err != nil {
				t.Fatal(err)
			}
			source, err := NewSessionID(testCase.Source)
			if err != nil {
				t.Fatal(err)
			}
			records := chainFixtureRecords(testCase.Records)
			snapshot := chainFixtureRecords(testCase.Snapshot)
			full := claudeHistoryLinkRecords(records, own, source, snapshot)
			compact := claudeHistoryLink(compactIdentityChain(records), own, source, compactIdentityChain(snapshot))
			if full != testCase.Want {
				t.Fatalf("full record walk = %t, want %t", full, testCase.Want)
			}
			if compact != testCase.Want {
				t.Fatalf("compact chain walk = %t, want %t; the deduplicated chain changed the lineage answer", compact, testCase.Want)
			}
		})
	}
	for name := range seen {
		t.Errorf("required lineage case %q has no fixture row", name)
	}
}

func chainFixtureRecords(links []claudeIdentityChainLinkFixture) []ClaudeIdentityRecord {
	records := make([]ClaudeIdentityRecord, len(links))
	for i, link := range links {
		records[i] = ClaudeIdentityRecord{
			SessionID: link.SessionID, UUID: link.UUID, ParentUUID: link.ParentUUID,
			Type: link.Type, Subtype: link.Subtype,
		}
	}
	return records
}

// claudeHistoryLinkRecords is the pre-compaction walk. It exists so the compact
// chain can be proved against the walk it replaces, including the type,
// subtype, and later-copy cases a triple-only list gets wrong.
func claudeHistoryLinkRecords(records []ClaudeIdentityRecord, own, source SessionID, snapshot []ClaudeIdentityRecord) bool {
	byUUID := make(map[string]ClaudeIdentityRecord, len(snapshot))
	for _, record := range snapshot {
		if record.UUID != "" {
			byUUID[record.UUID] = record
		}
	}
	var ownPresent, restamped, sourcePresent, transition bool
	for _, record := range snapshot {
		sourcePresent = sourcePresent || record.SessionID == source.String()
	}
	foreign := make([]ClaudeIdentityRecord, 0)
	for _, record := range records {
		if record.SessionID == own.String() {
			ownPresent = true
			if prior, ok := byUUID[record.UUID]; ok && record.Type == prior.Type && record.Subtype == prior.Subtype && record.ParentUUID == prior.ParentUUID {
				restamped = true
			}
		}
		if record.SessionID == source.String() {
			foreign = append(foreign, record)
		}
	}
	continuedFromSnapshot := false
	for _, record := range foreign {
		if record.UUID == "" || record.ParentUUID == "" {
			continue
		}
		if _, copied := byUUID[record.UUID]; copied {
			continue
		}
		if _, ok := byUUID[record.ParentUUID]; ok {
			continuedFromSnapshot = true
			break
		}
	}
	foreignUUIDs := make(map[string]bool, len(foreign))
	for _, record := range records {
		if record.SessionID == own.String() && record.ParentUUID != "" && foreignUUIDs[record.ParentUUID] {
			transition = true
		}
		if record.SessionID == source.String() && record.UUID != "" {
			foreignUUIDs[record.UUID] = true
		}
	}
	if !ownPresent || !sourcePresent || len(foreign) == 0 || len(byUUID) == 0 {
		return false
	}
	if !restamped && !transition && !continuedFromSnapshot {
		return false
	}
	linked := make(map[string]bool, len(byUUID)+len(foreign))
	for i, record := range snapshot {
		if i == len(snapshot)-1 || record.Type == "system" && record.Subtype == "compact_boundary" {
			linked[record.UUID] = true
		}
	}
	for _, record := range records {
		if record.SessionID == own.String() {
			if prior, exists := byUUID[record.UUID]; exists && record.Type == prior.Type && record.Subtype == prior.Subtype && record.ParentUUID == prior.ParentUUID {
				linked[record.UUID] = true
			}
			continue
		}
		if record.SessionID != source.String() {
			continue
		}
		prior, copied := byUUID[record.UUID]
		if copied && (record.Type != prior.Type || record.Subtype != prior.Subtype || record.ParentUUID != prior.ParentUUID) {
			return false
		}
		if record.UUID == "" {
			continue
		}
		if record.Type == "system" && record.Subtype == "compact_boundary" {
			linked[record.UUID] = true
			continue
		}
		chainLinked := record.ParentUUID != "" && linked[record.ParentUUID]
		_, parentInSnapshot := byUUID[record.ParentUUID]
		if !copied && !chainLinked && !(record.ParentUUID != "" && parentInSnapshot) {
			return false
		}
		linked[record.UUID] = true
	}
	return true
}
