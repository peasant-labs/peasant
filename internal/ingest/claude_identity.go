package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/schema"
)

// mineClaudeIdentity adds structural evidence to the same read discovery already
// performs. No message content participates in admission or parent selection.
func mineClaudeIdentity(data []byte, evidence *ClaudeTranscriptEvidence) {
	beginClaudeIdentity(data, evidence)
	scanner := newJSONLRecordScanner(data, defaults.MaxJSONLRecordBytes)
	for scanner.Scan() {
		var value map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
			evidence.NativeAgentValid = false
			continue
		}
		addClaudeIdentityRecord(value, evidence)
	}
	complete := scanner.Err() == nil && len(scanner.Oversized()) == 0
	evidence.NativeAgentValid = evidence.NativeAgentValid && evidence.NativeAgentID != "" && complete
}

func beginClaudeIdentity(data []byte, evidence *ClaudeTranscriptEvidence) {
	evidence.ProofVersion = ClaudeIdentityProofVersion
	digest := sha256.Sum256(data)
	evidence.ContentDigest = hex.EncodeToString(digest[:])
	evidence.NativeAgentValid = true
}

func addClaudeIdentityRecord(value map[string]any, evidence *ClaudeTranscriptEvidence) {
	var record ClaudeIdentityRecord
	record.SessionID, _ = value["sessionId"].(string)
	if record.SessionID == "" {
		return
	}
	record.UUID, _ = value["uuid"].(string)
	record.ParentUUID, _ = value["parentUuid"].(string)
	record.Type, _ = value["type"].(string)
	record.Subtype, _ = value["subtype"].(string)
	if result, ok := value["toolUseResult"].(map[string]any); ok {
		record.SpawnAgentID, _ = result["agentId"].(string)
	}
	evidence.Records = append(evidence.Records, record)
	agent, _ := value["agentId"].(string)
	sidechain, _ := value["isSidechain"].(bool)
	if agent == "" || !sidechain {
		evidence.NativeAgentValid = false
	}
	if evidence.NativeAgentID == "" {
		evidence.NativeAgentID = agent
	} else if evidence.NativeAgentID != agent {
		evidence.NativeAgentValid = false
	}
}

func claudeRecordOwner(evidence ClaudeTranscriptEvidence) (SessionID, bool) {
	var owner string
	for _, record := range evidence.Records {
		if owner == "" {
			owner = record.SessionID
		} else if owner != record.SessionID {
			return "", false
		}
	}
	id, err := NewSessionID(owner)
	return id, err == nil
}

func claudeProjectDirectory(path ResolvedPath) string {
	dir := filepath.Dir(path.String())
	for dir != "." && dir != filepath.Dir(dir) {
		if filepath.Base(dir) == defaults.DirSubagents.String() {
			return filepath.Dir(filepath.Dir(dir))
		}
		dir = filepath.Dir(dir)
	}
	return filepath.Dir(path.String())
}

func claudeSourcePath(path ResolvedPath, owner SessionID) ResolvedPath {
	return ResolvedPath(filepath.Join(claudeProjectDirectory(path), owner.String()+defaults.ExtJSONL.String()))
}

func claudeLayoutParent(path ResolvedPath) (SessionID, bool) {
	dir := filepath.Dir(path.String())
	for dir != "." && dir != filepath.Dir(dir) {
		if filepath.Base(dir) == defaults.DirSubagents.String() {
			id, err := NewSessionID(filepath.Base(filepath.Dir(dir)))
			return id, err == nil
		}
		dir = filepath.Dir(dir)
	}
	return "", false
}

func (a *ClaudeAdapter) claudeIdentitySnapshot(path ResolvedPath) (ClaudeTranscriptEvidence, bool) {
	if data, err := a.fs.ReadFile(path.String()); err == nil {
		var snapshot ClaudeTranscriptEvidence
		mineClaudeIdentity(data, &snapshot)
		return snapshot, true
	}
	return ClaudeTranscriptEvidence{}, false
}

// claudeHistoryLink proves a mixed artifact against a real source snapshot.
// Copied records must match structural coordinates; a chain may continue from
// that snapshot's tail/boundary. A local-command divergence with re-stamped
// shared history also proves a fork without inspecting the command's text.
func claudeHistoryLink(records []ClaudeIdentityRecord, own, source SessionID, snapshot []ClaudeIdentityRecord) bool {
	byUUID := make(map[string]ClaudeIdentityRecord, len(snapshot))
	for _, record := range snapshot {
		if record.UUID != "" {
			byUUID[record.UUID] = record
		}
	}
	var ownPresent, restamped, command, sourcePresent, transition bool
	for _, record := range snapshot {
		sourcePresent = sourcePresent || record.SessionID == source.String()
	}
	foreign := make([]ClaudeIdentityRecord, 0)
	for _, record := range records {
		if record.SessionID == own.String() {
			ownPresent = true
			if prior, ok := byUUID[record.UUID]; ok && claudeSameCoordinate(prior, record) {
				restamped = true
			}
		}
		if record.SessionID == source.String() {
			foreign = append(foreign, record)
			command = command || record.Type == "system" && record.Subtype == "local_command"
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
	if !ownPresent || !sourcePresent {
		return false
	}
	if len(foreign) == 0 || len(byUUID) == 0 {
		return false
	}
	if !restamped && !transition {
		return false
	}
	// Each foreign record is a copied coordinate or extends the proved chain.
	linked := make(map[string]bool, len(byUUID)+len(foreign))
	for i, record := range snapshot {
		if i == len(snapshot)-1 || record.Type == "system" && record.Subtype == "compact_boundary" {
			linked[record.UUID] = true
		}
	}
	seenRestamped := false
	for _, record := range records {
		if record.SessionID == own.String() {
			if prior, exists := byUUID[record.UUID]; exists && claudeSameCoordinate(prior, record) {
				linked[record.UUID] = true
				seenRestamped = true
			}
			continue
		}
		if record.SessionID != source.String() {
			continue
		}
		prior, copied := byUUID[record.UUID]
		if copied && !claudeSameCoordinate(prior, record) {
			return false
		}
		systemBoundary := record.Type == "system" && (record.Subtype == "local_command" || record.Subtype == "compact_boundary")
		forkBoundary := seenRestamped && command && systemBoundary
		chainLinked := record.ParentUUID != "" && linked[record.ParentUUID]
		if !copied && !forkBoundary && !chainLinked {
			return false
		}
		if record.UUID == "" {
			return false
		}
		linked[record.UUID] = true
	}
	return true
}

func claudeSameCoordinate(left, right ClaudeIdentityRecord) bool {
	return left.Type == right.Type && left.Subtype == right.Subtype && left.ParentUUID == right.ParentUUID
}

func (a *ClaudeAdapter) linkClaudeNativeAgents(sessions []DiscoveredSession, evidence map[ResolvedPath]ClaudeTranscriptEvidence) []DiscoveredSession {
	byID := make(map[SessionID]int, len(sessions))
	for i := range sessions {
		byID[sessions[i].SessionID] = i
	}
	for i := range sessions {
		child := &sessions[i]
		proof := evidence[child.SourcePath]
		if !proof.NativeAgentValid || child.SessionID.String() != defaults.ClaudeSubagentPrefix+proof.NativeAgentID {
			continue
		}
		owner, ok := claudeRecordOwner(proof)
		ownerIndex, exists := byID[owner]
		if !ok || !exists || owner == child.SessionID {
			continue
		}
		if sessions[ownerIndex].SourcePath != claudeSourcePath(child.SourcePath, owner) {
			continue
		}
		if child.ParentUUID != nil && *child.ParentUUID != owner {
			layoutIndex, known := byID[*child.ParentUUID]
			ownerRecords := evidence[sessions[ownerIndex].SourcePath].Records
			if !claudeNativeSpawn(ownerRecords, proof.NativeAgentID) && (!known || !claudeHistoryLink(evidence[sessions[layoutIndex].SourcePath].Records, *child.ParentUUID, owner, ownerRecords)) {
				continue
			}
		}
		child.ParentUUID = &owner
		child.Origin, child.Signal = sessionorigin.Classify(sessionorigin.Evidence{HasParent: true})
	}
	// Existing ID admission collapses duplicated subagent trees. Also collapse
	// identical root-agent/tree copies, but never use content to choose an owner.
	kept := make(map[SessionID]int)
	out := sessions[:0]
	for _, session := range sessions {
		proof := evidence[session.SourcePath]
		if index, exists := kept[session.SessionID]; exists && strings.HasPrefix(session.SessionID.String(), defaults.ClaudeSubagentPrefix) {
			prior := evidence[out[index].SourcePath]
			owner, ownerOK := claudeRecordOwner(proof)
			priorOwner, priorOwnerOK := claudeRecordOwner(prior)
			currentParentMatches := session.ParentUUID != nil && owner == *session.ParentUUID
			priorParentMatches := out[index].ParentUUID != nil && priorOwner == *out[index].ParentUUID
			currentProved := proof.NativeAgentValid && ownerOK && currentParentMatches
			priorProved := prior.NativeAgentValid && priorOwnerOK && priorParentMatches
			if currentProved && !priorProved {
				out[index] = session
			} else if proof.ContentDigest != prior.ContentDigest {
				// As with subtree admission, never stitch different snapshots or
				// let a second copy overwrite the selected identity. Native source
				// order supplies a deterministic tie-break, not content text.
				slog.Warn("claude discovery: duplicate agent snapshot left out; the first proved artifact keeps its identity; inspect the native copies if their owners disagree",
					"agent_id", session.SessionID, "kept", out[index].SourcePath, "left_out", session.SourcePath)
			}
			continue
		}
		kept[session.SessionID] = len(out)
		out = append(out, session)
	}
	for i := range out {
		// Rebuild associations once, from the selected artifact and proved
		// owner, rather than leaving duplicate or stale layout paths behind.
		out[i].SubagentPaths = nil
	}
	byID = make(map[SessionID]int, len(out))
	for i := range out {
		byID[out[i].SessionID] = i
	}
	for i := range out {
		if out[i].ParentUUID != nil {
			if parent, exists := byID[*out[i].ParentUUID]; exists {
				out[parent].SubagentPaths = append(out[parent].SubagentPaths, out[i].SourcePath)
			}
		}
	}
	return out
}

func claudeNativeSpawn(records []ClaudeIdentityRecord, agentID string) bool {
	for _, record := range records {
		if record.SpawnAgentID == agentID {
			return true
		}
	}
	return false
}

// verifyClaudeIdentity runs on the captured bytes, not the discovery cache. A
// changed source cannot inherit a cached admission from a different transcript.
func (a *ClaudeAdapter) verifyClaudeIdentity(data []byte, session DiscoveredSession) (map[string]bool, []schema.SessionRelationship, error) {
	return a.verifyClaudeIdentityAgainst(data, session, true)
}

func (a *ClaudeAdapter) verifyClaudeIdentityAgainst(data []byte, session DiscoveredSession, native bool) (map[string]bool, []schema.SessionRelationship, error) {
	var proof ClaudeTranscriptEvidence
	mineClaudeIdentity(data, &proof)
	allowed := map[string]bool{session.SessionID.String(): true}
	if session.ParentUUID != nil {
		allowed[session.ParentUUID.String()] = true
	}
	refuse := func() (map[string]bool, []schema.SessionRelationship, error) {
		return nil, nil, fmt.Errorf("Claude metadata capture for %s at %s: transcript sessionId disagrees with the discovered session or parent identity and no structural source linkage proves it; no capture was written; restore the matching source and rerun peasant ingest", session.SessionID, session.SourcePath)
	}
	if native && filepath.Base(session.SourcePath.String()) != session.SessionID.String()+defaults.ExtJSONL.String() {
		return refuse()
	}
	var relationships []schema.SessionRelationship
	if strings.HasPrefix(session.SessionID.String(), defaults.ClaudeSubagentPrefix) && proof.NativeAgentID != "" {
		owner, ok := claudeRecordOwner(proof)
		parentMatches := session.ParentUUID != nil && owner == *session.ParentUUID
		identityMatches := session.SessionID.String() == defaults.ClaudeSubagentPrefix+proof.NativeAgentID
		ownerMatches := identityMatches && parentMatches
		if !proof.NativeAgentValid || !ok || !ownerMatches {
			return refuse()
		}
		ownerProof, available := a.claudeIdentitySnapshot(claudeSourcePath(session.SourcePath, owner))
		if !available {
			return refuse()
		}
		if layout, present := claudeLayoutParent(session.SourcePath); present && layout != owner && !claudeNativeSpawn(ownerProof.Records, proof.NativeAgentID) {
			layoutProof, available := a.claudeIdentitySnapshot(claudeSourcePath(session.SourcePath, layout))
			if !available || !claudeHistoryLink(layoutProof.Records, layout, owner, ownerProof.Records) {
				return refuse()
			}
		}
		target := schema.SessionID(owner)
		relationships = append(relationships, schema.SessionRelationship{Kind: schema.SessionRelationshipStartedBy, TargetState: schema.RelationshipTargetKnown, TargetLocalID: &target, Evidence: schema.EvidenceNativeTyped})
	}
	foreign := make(map[string]bool)
	for _, record := range proof.Records {
		if !allowed[record.SessionID] {
			foreign[record.SessionID] = true
		}
	}
	ids := make([]string, 0, len(foreign))
	for id := range foreign {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		source, err := NewSessionID(id)
		if err != nil || strings.HasPrefix(session.SessionID.String(), defaults.ClaudeSubagentPrefix) {
			return refuse()
		}
		path := claudeSourcePath(session.SourcePath, source)
		snapshot, available := a.claudeIdentitySnapshot(path)
		if !available {
			return refuse()
		}
		if !claudeHistoryLink(proof.Records, session.SessionID, source, snapshot.Records) {
			return refuse()
		}
		allowed[id] = true
		target := schema.SessionID(source)
		relationships = append(relationships, schema.SessionRelationship{Kind: schema.SessionRelationshipContextFrom, TargetState: schema.RelationshipTargetKnown, TargetLocalID: &target, Evidence: schema.EvidenceNativeTyped, Anchor: &schema.PublicSourceAnchor{Kind: schema.PublicSourceAnchorGeneral}})
	}
	if err := schema.ValidateSessionRelationships(relationships); err != nil {
		return nil, nil, fmt.Errorf("Claude metadata capture for %s: structural provenance cannot be represented: %w", session.SessionID, err)
	}
	return allowed, relationships, nil
}

// Retained bytes are already a managed capture, not a new native file. Reuse
// only the source relations the last-good metadata proved, and still require
// the captured record chain to connect the artifact to each recorded source.
func (a *ClaudeAdapter) verifyRetainedClaudeIdentity(data []byte, original *UnifiedMetadata) (map[string]bool, []schema.SessionRelationship, error) {
	var proof ClaudeTranscriptEvidence
	mineClaudeIdentity(data, &proof)
	allowed := map[string]bool{original.SessionID.String(): true}
	if original.ParentUUID != nil {
		allowed[original.ParentUUID.String()] = true
	}
	if strings.HasPrefix(original.SessionID.String(), defaults.ClaudeSubagentPrefix) && proof.NativeAgentID != "" {
		owner, ok := claudeRecordOwner(proof)
		parentMatches := original.ParentUUID != nil && owner == *original.ParentUUID
		identityMatches := original.SessionID.String() == defaults.ClaudeSubagentPrefix+proof.NativeAgentID
		ownerMatches := parentMatches && identityMatches
		if !proof.NativeAgentValid || !ok || !ownerMatches {
			return nil, nil, fmt.Errorf("Claude retained metadata capture for %s: native agent identity disagrees with its recorded owner; keep the last-good capture and restore matching native input", original.SessionID)
		}
	}
	relationships := append([]schema.SessionRelationship(nil), original.Relationships...)
	for _, relationship := range original.Relationships {
		if relationship.Kind != schema.SessionRelationshipContextFrom || relationship.TargetLocalID == nil || relationship.Anchor == nil {
			continue
		}
		if relationship.TargetState != schema.RelationshipTargetKnown && relationship.TargetState != schema.RelationshipTargetKnownRetained {
			continue
		}
		if relationship.Evidence != schema.EvidenceNativeTyped && relationship.Evidence != schema.EvidenceRetainedLastGood {
			continue
		}
		ownUUIDs, foreignUUIDs := make(map[string]bool), make(map[string]bool)
		for _, record := range proof.Records {
			if record.UUID == "" {
				continue
			}
			if record.SessionID == original.SessionID.String() {
				ownUUIDs[record.UUID] = true
			}
			if record.SessionID == relationship.TargetLocalID.String() {
				foreignUUIDs[record.UUID] = true
			}
		}
		for _, record := range proof.Records {
			ownTransition := record.SessionID == original.SessionID.String() && foreignUUIDs[record.ParentUUID]
			foreignTransition := record.SessionID == relationship.TargetLocalID.String() && ownUUIDs[record.ParentUUID]
			if ownTransition || foreignTransition {
				allowed[relationship.TargetLocalID.String()] = true
			}
		}
	}
	for _, record := range proof.Records {
		if !allowed[record.SessionID] {
			// Legacy metadata has no captured relationship proof. It may still
			// recover from an available native source, never from a guessed link.
			return a.verifyClaudeIdentityAgainst(data, DiscoveredSession{
				SessionID: original.SessionID, ParentUUID: original.ParentUUID, SourcePath: ResolvedPath(original.Source.FilePath),
			}, false)
		}
	}
	for i := range relationships {
		if relationships[i].TargetLocalID != nil && (relationships[i].TargetState == schema.RelationshipTargetKnown || relationships[i].TargetState == schema.RelationshipTargetKnownRetained) {
			relationships[i].TargetState = schema.RelationshipTargetKnownRetained
			relationships[i].Evidence = schema.EvidenceRetainedLastGood
		}
	}
	return allowed, relationships, nil
}
