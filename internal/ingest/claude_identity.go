package ingest

import (
	"context"
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
	spawnCalls := make(map[string]bool)
	for scanner.Scan() {
		var value map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
			evidence.NativeAgentValid = false
			continue
		}
		addClaudeIdentityRecord(value, evidence, spawnCalls)
	}
	complete := scanner.Err() == nil && len(scanner.Oversized()) == 0
	evidence.NativeAgentValid = evidence.NativeAgentValid && evidence.NativeAgentID != "" && complete
}

func beginClaudeIdentity(data []byte, evidence *ClaudeTranscriptEvidence) {
	evidence.ProofVersion = ClaudeIdentityProofVersion
	digest := sha256.Sum256(data)
	evidence.ContentDigest = hex.EncodeToString(digest[:])
	evidence.NativeAgentValid = true
	evidence.Records = nil
	evidence.ControlRecordSeen = false
}

func addClaudeIdentityRecord(value map[string]any, evidence *ClaudeTranscriptEvidence, spawnCalls map[string]bool) {
	var record ClaudeIdentityRecord
	record.SessionID, _ = value["sessionId"].(string)
	record.Type, _ = value["type"].(string)
	if record.SessionID == "" {
		if record.Type == "summary" || record.Type == "file-history-snapshot" || isClaudeControlRecordType(record.Type) {
			evidence.ControlRecordSeen = true
		}
		return
	}
	record.UUID, _ = value["uuid"].(string)
	record.ParentUUID, _ = value["parentUuid"].(string)
	record.Subtype, _ = value["subtype"].(string)
	record.AgentID, _ = value["agentId"].(string)
	if raw, present := value["isSidechain"]; present {
		sidechain, _ := raw.(bool)
		record.Sidechain = &sidechain
	}
	record.SpawnAgentID = claudeCorrelatedNativeSpawn(value, spawnCalls)
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

// Older child transcripts may omit isSidechain. Matching layout and every
// native agent ID retain that existing flow, but an explicit false/malformed
// flag or conflicting native agent identity never becomes legacy proof.
func claudeLegacyAgentProof(proof ClaudeTranscriptEvidence, agentID string) bool {
	if len(proof.Records) == 0 {
		return false
	}
	for _, record := range proof.Records {
		if record.AgentID != agentID || record.Sidechain != nil {
			return false
		}
	}
	return true
}

func claudeContradictorySidechain(proof ClaudeTranscriptEvidence) bool {
	for _, record := range proof.Records {
		if record.Sidechain != nil && !*record.Sidechain {
			return true
		}
	}
	return false
}

func claudeAgentAdmissionProof(session DiscoveredSession, proof ClaudeTranscriptEvidence) bool {
	owner, ok := claudeRecordOwner(proof)
	if !ok || session.ParentUUID == nil || owner != *session.ParentUUID {
		return false
	}
	if proof.NativeAgentValid {
		return session.SessionID.String() == defaults.ClaudeSubagentPrefix+proof.NativeAgentID
	}
	layout, hasLayout := claudeLayoutParent(session.SourcePath)
	if !hasLayout || layout != owner || !claudeLegacyAgentProof(proof, proof.NativeAgentID) {
		return false
	}
	return proof.NativeAgentID == "" || session.SessionID.String() == defaults.ClaudeSubagentPrefix+proof.NativeAgentID
}

// A generic tool result may contain arbitrary JSON. Only a result correlated
// with a native Task/Agent call supplies spawn evidence; prose and uncorrelated
// agentId fields never authorize a layout override.
func claudeCorrelatedNativeSpawn(value map[string]any, calls map[string]bool) string {
	message, ok := value["message"].(map[string]any)
	if !ok {
		return ""
	}
	blocks, _ := message["content"].([]any)
	role, _ := message["role"].(string)
	recordType, _ := value["type"].(string)
	var correlated bool
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := block["type"].(string)
		if recordType == "assistant" && role == "assistant" && kind == "tool_use" {
			name, _ := block["name"].(string)
			id, _ := block["id"].(string)
			if id != "" && (name == "Task" || name == "Agent") {
				calls[id] = true
			}
		}
		if recordType == "user" && role == "user" && kind == "tool_result" {
			id, _ := block["tool_use_id"].(string)
			correlated = correlated || calls[id]
		}
	}
	if correlated {
		if result, ok := value["toolUseResult"].(map[string]any); ok {
			agent, _ := result["agentId"].(string)
			return agent
		}
	}
	return ""
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

func claudeNamesSession(records []ClaudeIdentityRecord, owner SessionID) bool {
	for _, record := range records {
		if record.SessionID == owner.String() {
			return true
		}
	}
	return false
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

// claudeIdentitySnapshotSource reads the structural proof of one candidate
// source file. It is the one filesystem-shaped dependency the retained identity
// kernel has, so a caller that is not a *ClaudeAdapter (a candidate builder
// carrying its own FileSystem) threads it explicitly instead of reimplementing
// the kernel. A missing, malformed, or foreign-owned snapshot reports false and
// the kernel refuses. A file that exists and holds a recognized control record
// but no session id reports true with empty Records; callers that need a named
// owner treat that as a rotated file, never as a named owner.
type claudeIdentitySnapshotSource func(path ResolvedPath) (ClaudeTranscriptEvidence, bool)

// claudeIdentitySnapshotFrom builds the snapshot accessor over a FileSystem. It
// is the free-function form of ClaudeAdapter.claudeIdentitySnapshot, so the
// adapter and the candidate builder read structural proof exactly the same way.
func claudeIdentitySnapshotFrom(fs FileSystem) claudeIdentitySnapshotSource {
	return func(path ResolvedPath) (ClaudeTranscriptEvidence, bool) {
		if data, err := fs.ReadFile(path.String()); err == nil {
			if err := validateRetainedJSONL(context.Background(), data); err != nil {
				return ClaudeTranscriptEvidence{}, false
			}
			var snapshot ClaudeTranscriptEvidence
			mineClaudeIdentity(data, &snapshot)
			owner, err := NewSessionID(strings.TrimSuffix(filepath.Base(path.String()), defaults.ExtJSONL.String()))
			if err != nil {
				return ClaudeTranscriptEvidence{}, false
			}
			if claudeNamesSession(snapshot.Records, owner) {
				return snapshot, true
			}
			if snapshot.ControlRecordSeen && len(snapshot.Records) == 0 {
				// The file exists and holds a recognized control record, but no
				// record carries a session id: a rotated or truncated owner
				// transcript that kept only summary and file-history records. A
				// zero-byte, blank, or junk file proves nothing and stays
				// unavailable.
				return snapshot, true
			}
		}
		return ClaudeTranscriptEvidence{}, false
	}
}

func (a *ClaudeAdapter) claudeIdentitySnapshot(path ResolvedPath) (ClaudeTranscriptEvidence, bool) {
	return claudeIdentitySnapshotFrom(a.fs)(path)
}

// claudeHistoryLink proves a mixed artifact against a real source snapshot.
// Copied records must match structural coordinates; a chain may continue from
// that snapshot's tail/boundary. An uncopied record may also prove a segment
// start at a compact boundary, or anchor at any coordinate the snapshot holds.
// An empty-uuid mirror is ignored. Uncopied records otherwise must connect to
// proved coordinates; their subtype alone proves no fork.
func claudeHistoryLink(records []ClaudeIdentityRecord, own, source SessionID, snapshot []ClaudeIdentityRecord) bool {
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
			if prior, ok := byUUID[record.UUID]; ok && claudeSameCoordinate(prior, record) {
				restamped = true
			}
		}
		if record.SessionID == source.String() {
			foreign = append(foreign, record)
		}
	}
	// A real Claude fork can write the forked prompt under the parent's session
	// id while its parentUuid names an existing record in the parent snapshot.
	// The new session's own chain then starts independently, so no own record
	// links back to that prompt. Only a NEW foreign record proves that
	// continuation: a copied record is not evidence of a fork, and a record
	// whose parent is absent from the snapshot proves nothing.
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
	if !ownPresent || !sourcePresent {
		return false
	}
	if len(foreign) == 0 || len(byUUID) == 0 {
		return false
	}
	if !restamped && !transition && !continuedFromSnapshot {
		return false
	}
	// Each foreign record is a copied coordinate, extends the proved chain,
	// proves a compact-boundary segment start, or anchors at a snapshot
	// coordinate; an empty-uuid mirror is ignored.
	linked := make(map[string]bool, len(byUUID)+len(foreign))
	for i, record := range snapshot {
		if i == len(snapshot)-1 || record.Type == "system" && record.Subtype == "compact_boundary" {
			linked[record.UUID] = true
		}
	}
	for _, record := range records {
		if record.SessionID == own.String() {
			if prior, exists := byUUID[record.UUID]; exists && claudeSameCoordinate(prior, record) {
				linked[record.UUID] = true
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
		if record.UUID == "" {
			// A copied mirror or control record can carry no coordinate of its
			// own. It cannot extend the chain, but it must not invalidate a link
			// the surrounding records prove.
			continue
		}
		if record.Type == "system" && record.Subtype == "compact_boundary" {
			// A compact boundary starts a new native segment with no parent.
			// It is the harness's own continuation marker, so it is proof of a
			// segment start rather than a disconnected record.
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
		if !claudeNamesSession(evidence[sessions[ownerIndex].SourcePath].Records, owner) {
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
			currentProved := claudeAgentAdmissionProof(session, proof)
			priorProved := claudeAgentAdmissionProof(out[index], prior)
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
	return verifyClaudeIdentityAgainst(data, session, native, a.claudeIdentitySnapshot)
}

func verifyClaudeIdentityAgainst(data []byte, session DiscoveredSession, native bool, snapshot claudeIdentitySnapshotSource) (map[string]bool, []schema.SessionRelationship, error) {
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
	if strings.HasPrefix(session.SessionID.String(), defaults.ClaudeSubagentPrefix) && claudeContradictorySidechain(proof) {
		return refuse()
	}
	if strings.HasPrefix(session.SessionID.String(), defaults.ClaudeSubagentPrefix) && proof.NativeAgentID != "" {
		owner, ok := claudeRecordOwner(proof)
		// The artifact must never name itself as its owner, whether the owner
		// was derived or the recorded parent names the same session.
		if !ok || owner.String() == session.SessionID.String() {
			return refuse()
		}
		parentMatches := session.ParentUUID != nil && owner == *session.ParentUUID
		identityMatches := session.SessionID.String() == defaults.ClaudeSubagentPrefix+proof.NativeAgentID
		// A legacy root agent file records its owner in every record's sessionId
		// but predates the recorded parent field. Derive the owner when the
		// native owner source exists and names that owner; a rotated owner file
		// that retains no session records is the same file identity.
		derivedOwner := session.ParentUUID == nil
		ownerMatches := identityMatches && (parentMatches || derivedOwner)
		layout, hasLayout := claudeLayoutParent(session.SourcePath)
		legacyLayout := hasLayout && layout == owner && claudeLegacyAgentProof(proof, proof.NativeAgentID)
		nativeProof := proof.NativeAgentValid || legacyLayout
		if !nativeProof || !ok || !ownerMatches {
			return refuse()
		}
		// A matching native parent directory is itself independent owner proof.
		// Its root transcript may have rotated while its session remains stored;
		// scheduling, not extraction, enforces the stored-parent dependency.
		if !hasLayout || layout != owner {
			ownerProof, available := snapshot(claudeSourcePath(session.SourcePath, owner))
			if !available {
				return refuse()
			}
			if hasLayout && !claudeNativeSpawn(ownerProof.Records, proof.NativeAgentID) {
				layoutProof, available := snapshot(claudeSourcePath(session.SourcePath, layout))
				if !available || !claudeHistoryLink(layoutProof.Records, layout, owner, ownerProof.Records) {
					return refuse()
				}
			}
		}
		if derivedOwner {
			allowed[owner.String()] = true
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
		snapshotEvidence, available := snapshot(path)
		if !available {
			return refuse()
		}
		if !claudeHistoryLink(proof.Records, session.SessionID, source, snapshotEvidence.Records) {
			return refuse()
		}
		allowed[id] = true
	}
	if len(ids) == 1 {
		target, _ := NewSessionID(ids[0]) // validated against the source above
		relationships = append(relationships, schema.SessionRelationship{Kind: schema.SessionRelationshipContextFrom, TargetState: schema.RelationshipTargetKnown, TargetLocalID: &target, Evidence: schema.EvidenceNativeTyped, Anchor: &schema.PublicSourceAnchor{Kind: schema.PublicSourceAnchorGeneral}})
	} else if len(ids) > 1 {
		// The public graph permits one target per kind. Keep all proved source
		// IDs and copied coordinates in the local evidence/native capture, and
		// report the existing conflict state instead of guessing a primary source.
		relationships = append(relationships, schema.SessionRelationship{Kind: schema.SessionRelationshipContextFrom, TargetState: schema.RelationshipTargetConflictingCurrentNativeEvidence, Evidence: schema.EvidenceConflict})
	}
	if err := schema.ValidateSessionRelationships(relationships); err != nil {
		return nil, nil, fmt.Errorf("Claude metadata capture for %s: structural provenance cannot be represented: %w", session.SessionID, err)
	}
	return allowed, relationships, nil
}

// Stored bytes are already a managed capture, not a new native file. Reuse
// only the source relations the last-good metadata proved, and still require
// the captured record chain to connect the artifact to each recorded source.
func (a *ClaudeAdapter) verifyRetainedClaudeIdentity(data []byte, original *UnifiedMetadata, contentHash string) (map[string]bool, []schema.SessionRelationship, error) {
	return verifyRetainedClaudeIdentityWith(data, original, contentHash, a.claudeIdentitySnapshot)
}

// verifyRetainedClaudeIdentityWith is the data-in form of the retained identity
// proof. The legacy fallback for metadata with no captured relationship proof
// consults structural sibling snapshots through the supplied accessor only,
// never the captured file itself; a caller that carries its own FileSystem
// threads that accessor rather than duplicating the kernel.
func verifyRetainedClaudeIdentityWith(data []byte, original *UnifiedMetadata, contentHash string, snapshot claudeIdentitySnapshotSource) (map[string]bool, []schema.SessionRelationship, error) {
	var proof ClaudeTranscriptEvidence
	mineClaudeIdentity(data, &proof)
	allowed := map[string]bool{original.SessionID.String(): true}
	if original.ParentUUID != nil {
		allowed[original.ParentUUID.String()] = true
	}
	if strings.HasPrefix(original.SessionID.String(), defaults.ClaudeSubagentPrefix) && claudeContradictorySidechain(proof) {
		return nil, nil, fmt.Errorf("Claude stored metadata capture for %s: explicit native sidechain evidence contradicts an agent artifact; keep the last-good capture and restore matching native input", original.SessionID)
	}
	var derivedAgentOwner *SessionID
	if strings.HasPrefix(original.SessionID.String(), defaults.ClaudeSubagentPrefix) && proof.NativeAgentID != "" {
		owner, ok := claudeRecordOwner(proof)
		// The artifact must never name itself as its owner, whether the owner
		// was derived or the recorded parent names the same session.
		if !ok || owner.String() == original.SessionID.String() {
			return nil, nil, fmt.Errorf("Claude stored metadata capture for %s: native agent identity disagrees with its recorded owner; keep the last-good capture and restore matching native input", original.SessionID)
		}
		parentMatches := original.ParentUUID != nil && owner == *original.ParentUUID
		identityMatches := original.SessionID.String() == defaults.ClaudeSubagentPrefix+proof.NativeAgentID
		// A legacy root agent artifact stores its owner in every record's
		// sessionId but has no recorded parent. Derive the owner when the
		// recorded relationship is bound to these bytes, or when the native
		// owner source still exists and names that owner.
		derivedOwner := original.ParentUUID == nil
		ownerMatches := identityMatches && (parentMatches || derivedOwner)
		legacyProof := claudeLegacyAgentProof(proof, proof.NativeAgentID)
		nativeProof := proof.NativeAgentValid || legacyProof
		if !nativeProof || !ok || !ownerMatches {
			return nil, nil, fmt.Errorf("Claude stored metadata capture for %s: native agent identity disagrees with its recorded owner; keep the last-good capture and restore matching native input", original.SessionID)
		}
		if derivedOwner {
			recordedProof := false
			if original.ContentHash != "" && original.ContentHash == contentHash {
				for _, relationship := range original.Relationships {
					if relationship.Kind != schema.SessionRelationshipStartedBy || relationship.TargetLocalID == nil || relationship.TargetLocalID.String() != owner.String() {
						continue
					}
					if relationship.Evidence == schema.EvidenceNativeTyped || relationship.Evidence == schema.EvidenceRetainedLastGood {
						recordedProof = true
						break
					}
				}
			}
			if !recordedProof {
				_, available := snapshot(claudeSourcePath(ResolvedPath(original.Source.FilePath), owner))
				if !available {
					return nil, nil, fmt.Errorf("Claude stored metadata capture for %s: native agent identity disagrees with its recorded owner; keep the last-good capture and restore matching native input", original.SessionID)
				}
			}
			allowed[owner.String()] = true
			id := owner
			derivedAgentOwner = &id
		}
	}
	relationships := append([]schema.SessionRelationship(nil), original.Relationships...)
	if derivedAgentOwner != nil {
		target := schema.SessionID(*derivedAgentOwner)
		already := false
		for _, relationship := range relationships {
			if relationship.Kind != schema.SessionRelationshipStartedBy {
				continue
			}
			if relationship.TargetLocalID == nil {
				return nil, nil, fmt.Errorf("Claude stored metadata capture for %s: the recorded started_by owner is unavailable; keep the last-good capture and restore matching native input", original.SessionID)
			}
			if *relationship.TargetLocalID == target {
				already = true
				break
			}
			return nil, nil, fmt.Errorf("Claude stored metadata capture for %s: the recorded started_by owner %s disagrees with the transcript owner %s; keep the last-good capture and restore matching native input", original.SessionID, *relationship.TargetLocalID, target)
		}
		if !already {
			relationships = append([]schema.SessionRelationship{{Kind: schema.SessionRelationshipStartedBy, TargetState: schema.RelationshipTargetKnown, TargetLocalID: &target, Evidence: schema.EvidenceNativeTyped}}, relationships...)
		}
	}
	for _, relationship := range original.Relationships {
		contextConflict := relationship.Kind == schema.SessionRelationshipContextFrom && relationship.TargetState == schema.RelationshipTargetConflictingCurrentNativeEvidence
		if contextConflict && relationship.Evidence == schema.EvidenceConflict {
			// A multi-source last-good capture has no single public target. Its
			// persisted transcript digest binds all structural IDs to the exact
			// bytes that acquisition proved; changed bytes must acquire anew.
			if original.ContentHash == "" || original.ContentHash != contentHash {
				return nil, nil, fmt.Errorf("Claude stored metadata capture for %s: multi-source provenance belongs to different captured bytes; keep the last-good capture and reacquire intact native input", original.SessionID)
			}
			for _, record := range proof.Records {
				allowed[record.SessionID] = true
			}
		}
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
			return verifyClaudeIdentityAgainst(data, DiscoveredSession{
				SessionID: original.SessionID, ParentUUID: original.ParentUUID, SourcePath: ResolvedPath(original.Source.FilePath),
			}, false, snapshot)
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
