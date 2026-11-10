package ingest

import "sort"

// ClaudeIdentityChainLink is one structural coordinate kept for a lineage
// proof. Type and subtype are part of the coordinate: a copied record whose
// type or subtype disagrees is not the same record, and a foreign
// compact-boundary record is a segment start even when it has no parent.
// Session, uuid and parent alone do not decide either question.
type ClaudeIdentityChainLink struct {
	SessionID  string `json:"sessionId"`
	UUID       string `json:"uuid"`
	ParentUUID string `json:"parentUuid"`
	Type       string `json:"type"`
	Subtype    string `json:"subtype"`
}

// SealClaudeIdentity derives the compact structural facts from Records when a
// per-line list is present, then drops that list. A fresh mine and a cache
// write both seal before anything reads or stores the facts. A value whose
// Records field is nil is already sealed; sealing it again does not recompute.
func SealClaudeIdentity(evidence *ClaudeTranscriptEvidence) {
	if evidence == nil || evidence.Records == nil {
		return
	}
	evidence.SessionIDs = distinctSessionIDs(evidence.Records)
	evidence.SpawnAgentIDs = distinctSpawnAgentIDs(evidence.Records)
	evidence.LegacyAgentProof = legacyAgentProofFromRecords(evidence.Records, evidence.NativeAgentID)
	evidence.ContradictorySidechain = contradictorySidechainFromRecords(evidence.Records)
	evidence.Chain = compactIdentityChain(evidence.Records)
	evidence.Records = nil
}

func distinctSessionIDs(records []ClaudeIdentityRecord) []string {
	seen := make(map[string]struct{}, 4)
	for _, record := range records {
		seen[record.SessionID] = struct{}{}
	}
	return sortedSet(seen)
}

func distinctSpawnAgentIDs(records []ClaudeIdentityRecord) []string {
	seen := make(map[string]struct{})
	for _, record := range records {
		if record.SpawnAgentID == "" {
			continue
		}
		seen[record.SpawnAgentID] = struct{}{}
	}
	return sortedSet(seen)
}

func sortedSet(seen map[string]struct{}) []string {
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func legacyAgentProofFromRecords(records []ClaudeIdentityRecord, agentID string) bool {
	if len(records) == 0 {
		return false
	}
	for _, record := range records {
		if record.AgentID != agentID || record.Sidechain != nil {
			return false
		}
	}
	return true
}

func contradictorySidechainFromRecords(records []ClaudeIdentityRecord) bool {
	for _, record := range records {
		if record.Sidechain != nil && !*record.Sidechain {
			return true
		}
	}
	return false
}

// compactIdentityChain keeps the first and last occurrence of each structural
// key, in original order. A later identical copy can witness a parent
// transition that the first copy cannot, and the last copy is that witness
// when one exists. Dropping every later copy changes the lineage answer.
// Keeping both endpoints does not: a repeated foreign record cannot newly
// fail a walk the first copy passed, and the last own copy is the transition
// witness. The last kept link for a uuid is that uuid's last coordinate.
func compactIdentityChain(records []ClaudeIdentityRecord) []ClaudeIdentityChainLink {
	type key struct {
		sessionID, uuid, parentUUID, typ, subtype string
	}
	type span struct {
		first, last int
	}
	order := make([]key, 0)
	seen := make(map[key]*span)
	for index, record := range records {
		item := key{record.SessionID, record.UUID, record.ParentUUID, record.Type, record.Subtype}
		if existing, ok := seen[item]; ok {
			existing.last = index
			continue
		}
		seen[item] = &span{first: index, last: index}
		order = append(order, item)
	}
	type placed struct {
		index int
		key   key
	}
	kept := make([]placed, 0, len(order)*2)
	for _, item := range order {
		span := seen[item]
		kept = append(kept, placed{index: span.first, key: item})
		if span.last != span.first {
			kept = append(kept, placed{index: span.last, key: item})
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].index < kept[j].index })
	chain := make([]ClaudeIdentityChainLink, len(kept))
	for index, item := range kept {
		chain[index] = ClaudeIdentityChainLink{
			SessionID: item.key.sessionID, UUID: item.key.uuid, ParentUUID: item.key.parentUUID,
			Type: item.key.typ, Subtype: item.key.subtype,
		}
	}
	return chain
}

// identitySessionIDs returns the distinct session ids. An unsealed value still
// carrying the per-line list is derived from that list, so a caller that has
// not sealed yet answers the same question.
func (e ClaudeTranscriptEvidence) identitySessionIDs() []string {
	if e.Records != nil {
		return distinctSessionIDs(e.Records)
	}
	return e.SessionIDs
}

func (e ClaudeTranscriptEvidence) identitySpawnAgentIDs() []string {
	if e.Records != nil {
		return distinctSpawnAgentIDs(e.Records)
	}
	return e.SpawnAgentIDs
}

func (e ClaudeTranscriptEvidence) identityChain() []ClaudeIdentityChainLink {
	if e.Records != nil {
		return compactIdentityChain(e.Records)
	}
	return e.Chain
}

func (e ClaudeTranscriptEvidence) identityLegacyAgentProof(agentID string) bool {
	if e.Records != nil {
		return legacyAgentProofFromRecords(e.Records, agentID)
	}
	return e.LegacyAgentProof && agentID == e.NativeAgentID
}

func (e ClaudeTranscriptEvidence) identityContradictorySidechain() bool {
	if e.Records != nil {
		return contradictorySidechainFromRecords(e.Records)
	}
	return e.ContradictorySidechain
}

// ClaudeIdentityChainPersisted reports whether a cached row can be the owner
// snapshot or the layout-parent artifact in a stored lineage proof. Both of
// those arguments are root transcripts. A subagent row is the child being
// linked; the stored proof never passes its chain to the lineage walk.
func ClaudeIdentityChainPersisted(scope ClaudeEvidenceScope) bool {
	return scope == ClaudeEvidenceScopeRoot
}
